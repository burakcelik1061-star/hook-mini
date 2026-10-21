package service

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/hook-mini/internal/credential"
	"github.com/burakcelik1061-star/hook-mini/internal/safedial"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// EndpointInput is the data needed to register an endpoint.
type EndpointInput struct {
	URL         string
	Description string
}

// EndpointPatch changes an endpoint; a nil field is left as it is.
type EndpointPatch struct {
	URL         *string
	Description *string
}

// CreateEndpoint registers a subscriber endpoint and generates its signing
// secret. The returned value is the only place the secret is ever exposed to
// the caller; reads return the store struct too, but the HTTP layer never
// serialises it.
func (s *Service) CreateEndpoint(ctx context.Context, tenantID uuid.UUID, in EndpointInput) (store.Endpoint, error) {
	if err := s.validateDestination(in.URL); err != nil {
		return store.Endpoint{}, err
	}
	if err := validateDescription(in.Description); err != nil {
		return store.Endpoint{}, err
	}
	secret, err := credential.NewSigningSecret()
	if err != nil {
		return store.Endpoint{}, err
	}

	ep, err := s.q.CreateEndpoint(ctx, store.CreateEndpointParams{
		TenantID: tenantID, Url: in.URL, Description: in.Description, Secret: secret,
	})
	if err != nil {
		return store.Endpoint{}, fmt.Errorf("create endpoint: %w", err)
	}
	return ep, nil
}

// validateDestination checks the URL's syntax and then rejects destinations
// that are blocked without any DNS lookup (private IP literals, localhost).
// This is only the early, friendly check: the worker checks the resolved
// address again on every connection.
func (s *Service) validateDestination(raw string) error {
	if err := ValidateEndpointURL(raw); err != nil {
		return err
	}
	u, _ := url.Parse(raw) // cannot fail after ValidateEndpointURL
	if err := s.egress.CheckHost(u.Hostname()); err != nil {
		var be *safedial.BlockedError
		if errors.As(err, &be) {
			return invalid("url", "points to a destination that is not allowed (%s)", be.Reason)
		}
		return invalid("url", "points to a destination that is not allowed")
	}
	return nil
}

// SecretRotationGrace is how long the previous signing secret keeps working
// after a rotation.
const SecretRotationGrace = 24 * time.Hour

// RotateEndpointSecret gives an endpoint a new signing secret. For
// SecretRotationGrace requests are signed with both the new and the old
// secret, so the receiver can switch over without rejecting anything. The new
// secret is returned in the endpoint and, as at creation, shown only here.
func (s *Service) RotateEndpointSecret(ctx context.Context, tenantID, id uuid.UUID) (store.Endpoint, error) {
	secret, err := credential.NewSigningSecret()
	if err != nil {
		return store.Endpoint{}, err
	}
	ep, err := s.q.RotateEndpointSecret(ctx, store.RotateEndpointSecretParams{
		ID: id, TenantID: tenantID, NewSecret: secret, GraceMs: SecretRotationGrace.Milliseconds(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Endpoint{}, ErrNotFound
	}
	if err != nil {
		return store.Endpoint{}, fmt.Errorf("rotate secret: %w", err)
	}
	return ep, nil
}

// GetEndpoint returns one of the tenant's endpoints.
func (s *Service) GetEndpoint(ctx context.Context, tenantID, id uuid.UUID) (store.Endpoint, error) {
	ep, err := s.q.GetEndpoint(ctx, store.GetEndpointParams{ID: id, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Endpoint{}, ErrNotFound
	}
	if err != nil {
		return store.Endpoint{}, fmt.Errorf("get endpoint: %w", err)
	}
	return ep, nil
}

// ListEndpoints returns a page of the tenant's endpoints, newest first.
func (s *Service) ListEndpoints(ctx context.Context, tenantID uuid.UUID, page Page) (ListResult[store.Endpoint], error) {
	limit, ks, size, err := decodePage(page)
	if err != nil {
		return ListResult[store.Endpoint]{}, err
	}
	rows, err := s.q.ListEndpoints(ctx, store.ListEndpointsParams{
		TenantID: tenantID, AfterCreatedAt: ks.at, AfterID: ks.id, PageSize: limit,
	})
	if err != nil {
		return ListResult[store.Endpoint]{}, fmt.Errorf("list endpoints: %w", err)
	}
	return finishPage(rows, size, func(e store.Endpoint) (time.Time, uuid.UUID) { return e.CreatedAt, e.ID }), nil
}

// UpdateEndpoint changes the URL and/or description of an endpoint.
func (s *Service) UpdateEndpoint(ctx context.Context, tenantID, id uuid.UUID, p EndpointPatch) (store.Endpoint, error) {
	if p.URL == nil && p.Description == nil {
		return store.Endpoint{}, invalid("body", "must contain at least one of url, description")
	}
	if p.URL != nil {
		if err := s.validateDestination(*p.URL); err != nil {
			return store.Endpoint{}, err
		}
	}
	if p.Description != nil {
		if err := validateDescription(*p.Description); err != nil {
			return store.Endpoint{}, err
		}
	}

	ep, err := s.q.UpdateEndpoint(ctx, store.UpdateEndpointParams{
		ID: id, TenantID: tenantID, Url: p.URL, Description: p.Description,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return store.Endpoint{}, ErrNotFound
	}
	if err != nil {
		return store.Endpoint{}, fmt.Errorf("update endpoint: %w", err)
	}
	return ep, nil
}

// DeleteEndpoint removes an endpoint together with its delivery history.
func (s *Service) DeleteEndpoint(ctx context.Context, tenantID, id uuid.UUID) error {
	n, err := s.q.DeleteEndpoint(ctx, store.DeleteEndpointParams{ID: id, TenantID: tenantID})
	if err != nil {
		return fmt.Errorf("delete endpoint: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
