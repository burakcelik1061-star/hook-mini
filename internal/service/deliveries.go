package service

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// ConflictError means the request is valid but the resource is in a state
// where it cannot be carried out. Code is stable and machine-readable.
type ConflictError struct {
	Code    string
	Message string
}

func (e *ConflictError) Error() string { return e.Message }

// Conflict codes.
const (
	ConflictNotReplayable    = "delivery_not_replayable"
	ConflictEndpointDisabled = "endpoint_disabled"
)

// Delivery statuses, as stored.
var deliveryStatuses = map[string]bool{"pending": true, "in_flight": true, "succeeded": true, "dead": true}

// DeliveryFilter narrows a delivery listing. Nil/empty fields do not filter.
type DeliveryFilter struct {
	EndpointID *uuid.UUID
	EventID    *uuid.UUID
	Status     string
}

// DeliveryDetail is a delivery with its attempt history.
type DeliveryDetail struct {
	Delivery store.Delivery
	Attempts []store.DeliveryAttempt
}

// ListDeliveries returns a page of the tenant's deliveries, newest first.
func (s *Service) ListDeliveries(ctx context.Context, tenantID uuid.UUID, f DeliveryFilter, page Page) (ListResult[store.Delivery], error) {
	if f.Status != "" && !deliveryStatuses[f.Status] {
		return ListResult[store.Delivery]{}, invalid("status", "must be one of pending, in_flight, succeeded, dead")
	}
	limit, ks, size, err := decodePage(page)
	if err != nil {
		return ListResult[store.Delivery]{}, err
	}
	var status *string
	if f.Status != "" {
		status = &f.Status
	}
	rows, err := s.q.ListDeliveries(ctx, store.ListDeliveriesParams{
		TenantID: tenantID, EndpointID: f.EndpointID, EventID: f.EventID, Status: status,
		AfterCreatedAt: ks.at, AfterID: ks.id, PageSize: limit,
	})
	if err != nil {
		return ListResult[store.Delivery]{}, fmt.Errorf("list deliveries: %w", err)
	}
	return finishPage(rows, size, func(d store.Delivery) (time.Time, uuid.UUID) { return d.CreatedAt, d.ID }), nil
}

// GetDelivery returns one delivery with every attempt made so far.
func (s *Service) GetDelivery(ctx context.Context, tenantID, id uuid.UUID) (DeliveryDetail, error) {
	d, err := s.q.GetDelivery(ctx, store.GetDeliveryParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return DeliveryDetail{}, ErrNotFound
	}
	if err != nil {
		return DeliveryDetail{}, fmt.Errorf("get delivery: %w", err)
	}
	attempts, err := s.q.ListDeliveryAttempts(ctx, store.ListDeliveryAttemptsParams{DeliveryID: id, TenantID: tenantID})
	if err != nil {
		return DeliveryDetail{}, fmt.Errorf("list attempts: %w", err)
	}
	return DeliveryDetail{Delivery: d, Attempts: attempts}, nil
}

// ReplayDelivery sends a finished (dead or succeeded) delivery again with a
// fresh retry budget. The attempt history is kept; the new attempts are
// appended to it.
//
// It refuses a delivery that is still pending or in flight, and one whose
// endpoint is disabled (the worker would just abandon it again), with a
// ConflictError saying why.
func (s *Service) ReplayDelivery(ctx context.Context, tenantID, id uuid.UUID) (store.Delivery, error) {
	cur, err := s.q.GetDelivery(ctx, store.GetDeliveryParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Delivery{}, ErrNotFound
	}
	if err != nil {
		return store.Delivery{}, fmt.Errorf("get delivery: %w", err)
	}
	ep, err := s.q.GetEndpoint(ctx, store.GetEndpointParams{ID: cur.EndpointID, TenantID: tenantID})
	if err != nil {
		return store.Delivery{}, fmt.Errorf("get endpoint: %w", err)
	}
	if ep.Status != "active" {
		return store.Delivery{}, &ConflictError{
			Code:    ConflictEndpointDisabled,
			Message: "the endpoint is disabled; enable it before replaying its deliveries",
		}
	}

	// The status guard inside the UPDATE, not the read above, is what makes
	// this safe: two simultaneous replays both pass the read, but only one
	// finds the row dead or succeeded.
	d, err := s.q.ReplayDelivery(ctx, store.ReplayDeliveryParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Delivery{}, &ConflictError{
			Code:    ConflictNotReplayable,
			Message: "only a succeeded or dead delivery can be replayed; this one is still being delivered",
		}
	}
	if err != nil {
		return store.Delivery{}, fmt.Errorf("replay delivery: %w", err)
	}
	return d, nil
}

// DisableEndpoint stops deliveries to an endpoint. Deliveries that become due
// while it is disabled end as dead and can be replayed after enabling.
// Disabling an already disabled endpoint changes nothing.
func (s *Service) DisableEndpoint(ctx context.Context, tenantID, id uuid.UUID) (store.Endpoint, error) {
	ep, err := s.q.DisableEndpointForTenant(ctx, store.DisableEndpointForTenantParams{
		ID: id, TenantID: tenantID, Reason: "disabled manually",
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return s.GetEndpoint(ctx, tenantID, id) // already disabled, or not found
	}
	if err != nil {
		return store.Endpoint{}, fmt.Errorf("disable endpoint: %w", err)
	}
	return ep, nil
}

// EnableEndpoint resumes deliveries to a disabled endpoint and resets its
// failure counter.
func (s *Service) EnableEndpoint(ctx context.Context, tenantID, id uuid.UUID) (store.Endpoint, error) {
	ep, err := s.q.EnableEndpoint(ctx, store.EnableEndpointParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Endpoint{}, ErrNotFound
	}
	if err != nil {
		return store.Endpoint{}, fmt.Errorf("enable endpoint: %w", err)
	}
	return ep, nil
}
