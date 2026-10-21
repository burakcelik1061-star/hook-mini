package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/hook-mini/internal/secretbox"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// reencryptBatch is how many endpoints one query of ReencryptSecrets reads.
// A variable only so a test can make batches small.
var reencryptBatch = 500

// ReencryptSecrets seals every endpoint secret that is not sealed with the
// current (first) key yet: plaintext written before encryption existed, or a
// value sealed with an older key. It returns how many endpoints it rewrote.
//
// It is idempotent and safe while the api is serving: a row is rewritten
// only if it still holds what was read, and a row that changed meanwhile (a
// secret rotation) is looked at again, so a plaintext secret that a rotation
// moved into previous_secret is not left behind.
func (s *Service) ReencryptSecrets(ctx context.Context) (int, error) {
	if s.keys == nil {
		return 0, errNoKeys
	}
	total := 0
	for {
		rewritten, raced, err := s.reencryptPass(ctx)
		total += rewritten
		if err != nil || !raced {
			return total, err
		}
	}
}

// reencryptPass walks every endpoint that needs re-encryption once. raced
// reports whether some row changed under it and needs another pass.
func (s *Service) reencryptPass(ctx context.Context) (rewritten int, raced bool, err error) {
	after := uuid.Nil
	for {
		rows, err := s.q.ListEndpointSecretsToReencrypt(ctx, store.ListEndpointSecretsToReencryptParams{
			AfterID: after, CurrentPrefix: s.keys.PrimaryPrefix(), BatchSize: int32(reencryptBatch),
		})
		if err != nil {
			return rewritten, raced, fmt.Errorf("list secrets to re-encrypt: %w", err)
		}
		for _, r := range rows {
			after = r.ID
			secret, err := s.reseal(r.ID, r.Secret)
			if err != nil {
				return rewritten, raced, fmt.Errorf("endpoint %s: secret: %w", r.ID, err)
			}
			var previous *string
			if r.PreviousSecret != nil {
				p, err := s.reseal(r.ID, *r.PreviousSecret)
				if err != nil {
					return rewritten, raced, fmt.Errorf("endpoint %s: previous secret: %w", r.ID, err)
				}
				previous = &p
			}
			n, err := s.q.ReplaceEndpointSecrets(ctx, store.ReplaceEndpointSecretsParams{
				ID: r.ID, NewSecret: secret, NewPreviousSecret: previous,
				OldSecret: r.Secret, OldPreviousSecret: r.PreviousSecret,
			})
			if err != nil {
				return rewritten, raced, fmt.Errorf("endpoint %s: write re-encrypted secret: %w", r.ID, err)
			}
			if n == 0 {
				raced = true // rotated or deleted meanwhile; the next pass decides
				continue
			}
			rewritten++
		}
		if len(rows) < reencryptBatch {
			return rewritten, raced, nil
		}
	}
}

// reseal returns value sealed with the current key: plaintext is sealed as it
// is, a sealed value is opened first (with whichever key sealed it).
func (s *Service) reseal(endpointID uuid.UUID, value string) (string, error) {
	plain := value
	if _, err := secretbox.KeyIDOf(value); !errors.Is(err, secretbox.ErrNotSealed) {
		plain, err = s.keys.Open(value, secretbox.EndpointContext(endpointID))
		if err != nil {
			return "", err
		}
	}
	return s.seal(endpointID, plain)
}

// CheckSecrets verifies that every signing secret in the database is sealed
// with one of the configured keys. The api and the worker call it at startup
// and refuse to run otherwise: with it, a running service implies that no
// secret is stored in plaintext, and a key removed too early is noticed
// before any delivery fails for it.
func (s *Service) CheckSecrets(ctx context.Context) error {
	if s.keys == nil {
		return errNoKeys
	}
	rows, err := s.q.CountEndpointSecretsByKey(ctx)
	if err != nil {
		return fmt.Errorf("count secrets by key: %w", err)
	}
	var plaintext int64
	var unknown []string
	for _, r := range rows {
		switch {
		case r.KeyID == "":
			plaintext += r.Secrets
		case !s.keys.Has(r.KeyID):
			unknown = append(unknown, fmt.Sprintf("%s (%d secrets)", r.KeyID, r.Secrets))
		}
	}
	var problems []string
	if plaintext > 0 {
		problems = append(problems, fmt.Sprintf("%d endpoint secrets are stored unencrypted: run the migrate command with SECRET_ENCRYPTION_KEYS set to encrypt them", plaintext))
	}
	if len(unknown) > 0 {
		problems = append(problems, "endpoint secrets are encrypted with keys missing from SECRET_ENCRYPTION_KEYS: "+
			strings.Join(unknown, ", ")+"; list those keys after the current one until the migrate command has re-encrypted everything")
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "; "))
	}
	return nil
}
