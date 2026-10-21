package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/burakcelik1061-star/hook-mini/internal/credential"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// Principal is the authenticated caller of an API request.
type Principal struct {
	TenantID uuid.UUID
	KeyID    uuid.UUID
}

// Authenticate resolves a raw API key to the tenant that owns it. Every
// failure returns ErrUnauthorized, with no hint of why: a caller must not be
// able to tell a malformed key from an unknown or revoked one.
func (s *Service) Authenticate(ctx context.Context, raw string) (Principal, error) {
	prefix, ok := credential.ParseAPIKey(raw)
	if !ok {
		return Principal{}, ErrUnauthorized
	}

	key, err := s.q.GetActiveAPIKeyByPrefix(ctx, prefix)
	if errors.Is(err, pgx.ErrNoRows) {
		return Principal{}, ErrUnauthorized
	}
	if err != nil {
		return Principal{}, fmt.Errorf("look up api key: %w", err)
	}
	if !credential.KeyMatches(raw, key.KeyHash) {
		return Principal{}, ErrUnauthorized
	}

	// Recording "last used" is bookkeeping: it must never fail a request.
	if err := s.q.TouchAPIKey(ctx, key.ID); err != nil {
		s.log.WarnContext(ctx, "could not update api key last_used_at", "key_id", key.ID, "error", err)
	}
	return Principal{TenantID: key.TenantID, KeyID: key.ID}, nil
}

// CreateTenant registers a new tenant. There is deliberately no HTTP endpoint
// for this: tenants are created by an operator through the admin command.
func (s *Service) CreateTenant(ctx context.Context, name string) (store.Tenant, error) {
	if err := validateName("name", name, maxNameLen); err != nil {
		return store.Tenant{}, err
	}
	t, err := s.q.CreateTenant(ctx, strings.TrimSpace(name))
	if err != nil {
		return store.Tenant{}, fmt.Errorf("create tenant: %w", err)
	}
	return t, nil
}

// IssueAPIKey creates a key for a tenant and returns it together with the
// full key text. That text exists only in the return value: the database
// keeps a hash, so it can never be shown again.
func (s *Service) IssueAPIKey(ctx context.Context, tenantID uuid.UUID, name string) (store.ApiKey, string, error) {
	if err := validateName("name", name, maxKeyNameLen); err != nil {
		return store.ApiKey{}, "", err
	}

	// The prefix is 32 random bits, so a collision is possible (if unlikely);
	// retrying with a new key is the correct response.
	for attempt := 0; attempt < 5; attempt++ {
		k, err := credential.NewAPIKey()
		if err != nil {
			return store.ApiKey{}, "", err
		}
		row, err := s.q.CreateAPIKey(ctx, store.CreateAPIKeyParams{
			TenantID: tenantID, Name: strings.TrimSpace(name), Prefix: k.Prefix, KeyHash: k.Hash[:],
		})
		switch {
		case err == nil:
			return row, k.Full, nil
		case pgViolation(err, pgUniqueViolation, "api_keys_prefix_key"):
			continue
		case pgViolation(err, pgForeignKeyViolation, ""):
			return store.ApiKey{}, "", ErrNotFound
		default:
			return store.ApiKey{}, "", fmt.Errorf("create api key: %w", err)
		}
	}
	return store.ApiKey{}, "", errors.New("could not generate a unique api key prefix")
}

// RevokeAPIKey disables a key. Revoking an unknown or already revoked key
// reports ErrNotFound.
func (s *Service) RevokeAPIKey(ctx context.Context, tenantID, keyID uuid.UUID) error {
	_, err := s.q.RevokeAPIKey(ctx, store.RevokeAPIKeyParams{ID: keyID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("revoke api key: %w", err)
	}
	return nil
}
