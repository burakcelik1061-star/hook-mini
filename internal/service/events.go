package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// IngestInput is an event submitted by a tenant.
type IngestInput struct {
	Type    string
	Payload json.RawMessage
	// IdempotencyKey, when set, makes the submission safe to retry: sending
	// the same key again returns the original event instead of creating a
	// second one.
	IdempotencyKey *string
}

// EventResult is an event with the number of deliveries it fanned out to.
type EventResult struct {
	Event         store.Event
	DeliveryCount int
	// Replayed is true when the event already existed for this idempotency
	// key, so nothing new was created.
	Replayed bool
}

// IngestEvent stores an event and queues one delivery per active endpoint.
//
// The event and its deliveries are written in a single transaction, which is
// the guarantee the whole service rests on: once this returns, the event is
// durable AND every delivery for it exists. There is no moment where an
// accepted event has lost its deliveries.
func (s *Service) IngestEvent(ctx context.Context, tenantID uuid.UUID, in IngestInput) (EventResult, error) {
	if err := ValidateEventType(in.Type); err != nil {
		return EventResult{}, err
	}
	if err := validatePayload(in.Payload); err != nil {
		return EventResult{}, err
	}
	if err := validateIdempotencyKey(in.IdempotencyKey); err != nil {
		return EventResult{}, err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return EventResult{}, fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }() // no-op after a successful commit
	q := s.q.WithTx(tx)

	ev, err := q.CreateEvent(ctx, store.CreateEventParams{
		TenantID: tenantID, Type: in.Type, Payload: in.Payload, IdempotencyKey: in.IdempotencyKey,
	})
	switch {
	case err == nil:
		deliveries, err := q.CreateDeliveriesForEvent(ctx, store.CreateDeliveriesForEventParams{
			EventID: ev.ID, TenantID: tenantID,
		})
		if err != nil {
			return EventResult{}, fmt.Errorf("fan out event: %w", err)
		}
		if err := tx.Commit(ctx); err != nil {
			return EventResult{}, fmt.Errorf("commit: %w", err)
		}
		return EventResult{Event: ev, DeliveryCount: len(deliveries)}, nil

	case errors.Is(err, pgx.ErrNoRows) && in.IdempotencyKey != nil:
		// The key already exists. Concurrent requests with the same key end
		// up here too: Postgres makes the second INSERT wait for the first
		// transaction, then skip, so exactly one event is ever created.
		return s.replay(ctx, q, tenantID, in)

	default:
		if ve := s.dataError(err, "payload"); ve != nil {
			return EventResult{}, ve
		}
		return EventResult{}, fmt.Errorf("create event: %w", err)
	}
}

func (s *Service) replay(ctx context.Context, q *store.Queries, tenantID uuid.UUID, in IngestInput) (EventResult, error) {
	row, err := q.GetEventForReplay(ctx, store.GetEventForReplayParams{
		TenantID: tenantID, IdempotencyKey: in.IdempotencyKey, Type: in.Type, Payload: in.Payload,
	})
	if err != nil {
		return EventResult{}, fmt.Errorf("load original event: %w", err)
	}
	if !row.Matches {
		return EventResult{}, ErrIdempotencyMismatch
	}
	n, err := q.CountDeliveriesForEvent(ctx, store.CountDeliveriesForEventParams{EventID: row.ID, TenantID: tenantID})
	if err != nil {
		return EventResult{}, fmt.Errorf("count deliveries: %w", err)
	}
	return EventResult{
		Event: store.Event{
			ID: row.ID, TenantID: row.TenantID, Type: row.Type, Payload: row.Payload,
			IdempotencyKey: row.IdempotencyKey, CreatedAt: row.CreatedAt,
		},
		DeliveryCount: int(n),
		Replayed:      true,
	}, nil
}

// GetEvent returns one of the tenant's events with its delivery count.
func (s *Service) GetEvent(ctx context.Context, tenantID, id uuid.UUID) (EventResult, error) {
	ev, err := s.q.GetEvent(ctx, store.GetEventParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return EventResult{}, ErrNotFound
	}
	if err != nil {
		return EventResult{}, fmt.Errorf("get event: %w", err)
	}
	n, err := s.q.CountDeliveriesForEvent(ctx, store.CountDeliveriesForEventParams{EventID: ev.ID, TenantID: tenantID})
	if err != nil {
		return EventResult{}, fmt.Errorf("count deliveries: %w", err)
	}
	return EventResult{Event: ev, DeliveryCount: int(n)}, nil
}

// ListEvents returns a page of the tenant's events, newest first.
func (s *Service) ListEvents(ctx context.Context, tenantID uuid.UUID, page Page) (ListResult[store.Event], error) {
	limit, ks, size, err := decodePage(page)
	if err != nil {
		return ListResult[store.Event]{}, err
	}
	rows, err := s.q.ListEvents(ctx, store.ListEventsParams{
		TenantID: tenantID, AfterCreatedAt: ks.at, AfterID: ks.id, PageSize: limit,
	})
	if err != nil {
		return ListResult[store.Event]{}, fmt.Errorf("list events: %w", err)
	}
	return finishPage(rows, size, func(e store.Event) (time.Time, uuid.UUID) { return e.CreatedAt, e.ID }), nil
}
