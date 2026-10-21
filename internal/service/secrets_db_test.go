package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/burakcelik1061-star/hook-mini/internal/database"
	"github.com/burakcelik1061-star/hook-mini/internal/secretbox"
	"github.com/burakcelik1061-star/hook-mini/internal/secretbox/secretboxtest"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

// Encryption at rest of the endpoint signing secrets.

func TestSecretsAreStoredEncrypted(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	ep := newEndpoint(t, s, tn.ID)
	rotated, err := s.RotateEndpointSecret(ctx(), tn.ID, ep.ID)
	if err != nil {
		t.Fatal(err)
	}

	var secret, previous string
	if err := testPool.QueryRow(ctx(), `SELECT secret, previous_secret FROM endpoints WHERE id = $1`, ep.ID).Scan(&secret, &previous); err != nil {
		t.Fatal(err)
	}
	prefix := secretboxtest.Keyring().PrimaryPrefix()
	for name, stored := range map[string]string{"secret": secret, "previous_secret": previous} {
		if !strings.HasPrefix(stored, prefix) {
			t.Errorf("%s is not sealed with the current key: %.20q", name, stored)
		}
		for _, plain := range []string{ep.Secret, rotated.Secret, strings.TrimPrefix(ep.Secret, "whsec_"), strings.TrimPrefix(rotated.Secret, "whsec_")} {
			if strings.Contains(stored, plain) {
				t.Errorf("%s holds a secret in plaintext", name)
			}
		}
	}
	// The caller still gets the secrets themselves, once.
	if !strings.HasPrefix(ep.Secret, "whsec_") || !strings.HasPrefix(rotated.Secret, "whsec_") {
		t.Errorf("the caller did not get plaintext secrets: %.12q %.12q", ep.Secret, rotated.Secret)
	}
	if open(t, ep.ID, secret) != rotated.Secret || open(t, ep.ID, previous) != ep.Secret {
		t.Error("the stored values do not open to the secrets the caller got")
	}
}

// Without keys nothing is written: there is no plaintext fallback.
func TestNoKeysNoSecrets(t *testing.T) {
	s := newService(t)
	tn := newTenant(t, s)
	ep := newEndpoint(t, s, tn.ID)
	keyless := New(testPool, s.log)

	if _, err := keyless.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/x"}); !errors.Is(err, errNoKeys) {
		t.Errorf("create without keys: err = %v, want errNoKeys", err)
	}
	if _, err := keyless.RotateEndpointSecret(ctx(), tn.ID, ep.ID); !errors.Is(err, errNoKeys) {
		t.Errorf("rotate without keys: err = %v, want errNoKeys", err)
	}
	if n := count(t, `SELECT count(*) FROM endpoints WHERE tenant_id = $1`, tn.ID); n != 1 {
		t.Errorf("%d endpoints, want only the one created with keys", n)
	}
	if _, err := keyless.ReencryptSecrets(ctx()); !errors.Is(err, errNoKeys) {
		t.Errorf("re-encrypt without keys: %v", err)
	}
	if err := keyless.CheckSecrets(ctx()); !errors.Is(err, errNoKeys) {
		t.Errorf("check without keys: %v", err)
	}
}

// ---- re-encryption and the startup check, in a database of their own: both
// look at every endpoint, and other packages' tests share the main one ----

func scratchService(t *testing.T, keys *secretbox.Keyring) (*Service, *pgxpool.Pool) {
	t.Helper()
	base := os.Getenv("TEST_DATABASE_URL")
	if base == "" {
		t.Skip("TEST_DATABASE_URL not set")
	}
	c := context.Background()
	admin, err := pgx.Connect(c, base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	name := "hookmini_secrets_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	if _, err := admin.Exec(c, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(base)
	u.Path = "/" + name
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if _, err := database.Migrate(c, log, u.String()); err != nil {
		t.Fatal(err)
	}
	pool, err := database.Connect(c, log, u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
	})
	return New(pool, log, WithSecretKeys(keys)), pool
}

func keyring(t *testing.T, raw ...[]byte) *secretbox.Keyring {
	t.Helper()
	k, err := secretbox.NewKeyring(raw...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var (
	oldKey = bytes.Repeat([]byte{0x01}, secretbox.KeySize)
	newKey = bytes.Repeat([]byte{0x02}, secretbox.KeySize)
)

// plaintextEndpoint writes an endpoint the way versions before encryption
// did: secret (and optionally previous_secret) in plaintext.
func plaintextEndpoint(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID, secret string, previous *string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `INSERT INTO endpoints (tenant_id, url, secret, previous_secret, previous_secret_expires_at)
		VALUES ($1, 'https://example.invalid/' || gen_random_uuid(), $2, $3::text, CASE WHEN $3::text IS NULL THEN NULL ELSE now() + interval '1 day' END)
		RETURNING id`, tenant, secret, previous).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func stored(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) (string, *string) {
	t.Helper()
	var secret string
	var previous *string
	if err := pool.QueryRow(context.Background(), `SELECT secret, previous_secret FROM endpoints WHERE id = $1`, id).Scan(&secret, &previous); err != nil {
		t.Fatal(err)
	}
	return secret, previous
}

const (
	plainA = "whsec_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	plainB = "whsec_BBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBBB="
)

func TestReencryptSecrets(t *testing.T) {
	keys := keyring(t, newKey, oldKey) // new seals, old still opens
	s, pool := scratchService(t, keys)
	tn, err := s.CreateTenant(ctx(), "Secrets")
	if err != nil {
		t.Fatal(err)
	}

	// 1. plaintext with a plaintext previous secret (from before encryption);
	prev := plainB
	legacy := plaintextEndpoint(t, pool, tn.ID, plainA, &prev)
	// 2. sealed with the old key;
	oldSvc := New(pool, s.log, WithSecretKeys(keyring(t, oldKey)))
	sealedOld, err := oldSvc.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/old"})
	if err != nil {
		t.Fatal(err)
	}
	// 3. already sealed with the new key.
	current, err := s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/new"})
	if err != nil {
		t.Fatal(err)
	}
	currentBefore, _ := stored(t, pool, current.ID)

	if err := s.CheckSecrets(ctx()); err == nil || !strings.Contains(err.Error(), "2 endpoint secrets are stored unencrypted") {
		t.Errorf("CheckSecrets before re-encryption: %v, want the 2 plaintext secrets reported", err)
	}

	n, err := s.ReencryptSecrets(ctx())
	if err != nil || n != 2 {
		t.Fatalf("ReencryptSecrets = %d, %v; want 2 endpoints rewritten", n, err)
	}

	want := map[uuid.UUID][2]string{legacy: {plainA, plainB}, sealedOld.ID: {sealedOld.Secret, ""}, current.ID: {current.Secret, ""}}
	newOnly := keyring(t, newKey)
	for id, w := range want {
		secret, previous := stored(t, pool, id)
		got, err := newOnly.Open(secret, secretbox.EndpointContext(id))
		if err != nil || got != w[0] {
			t.Errorf("endpoint %s: secret opens with the new key alone to %.10q (%v), want %.10q", id, got, err, w[0])
		}
		if w[1] != "" {
			if previous == nil {
				t.Fatalf("endpoint %s lost its previous secret", id)
			}
			if got, err := newOnly.Open(*previous, secretbox.EndpointContext(id)); err != nil || got != w[1] {
				t.Errorf("endpoint %s: previous secret opens to %.10q (%v)", id, got, err)
			}
		}
	}
	if after, _ := stored(t, pool, current.ID); after != currentBefore {
		t.Error("a secret already sealed with the current key was rewritten")
	}

	// Idempotent, and the database now passes the check even without the old key.
	if n, err := s.ReencryptSecrets(ctx()); err != nil || n != 0 {
		t.Errorf("second ReencryptSecrets = %d, %v; want 0", n, err)
	}
	if err := New(pool, s.log, WithSecretKeys(newOnly)).CheckSecrets(ctx()); err != nil {
		t.Errorf("CheckSecrets after re-encryption, old key removed: %v", err)
	}
}

// More endpoints than one batch: every batch is visited.
func TestReencryptWalksEveryBatch(t *testing.T) {
	saved := reencryptBatch
	reencryptBatch = 2
	t.Cleanup(func() { reencryptBatch = saved })

	keys := keyring(t, newKey)
	s, pool := scratchService(t, keys)
	tn, err := s.CreateTenant(ctx(), "Batches")
	if err != nil {
		t.Fatal(err)
	}
	var ids []uuid.UUID
	for range 5 {
		ids = append(ids, plaintextEndpoint(t, pool, tn.ID, plainA, nil))
	}
	if n, err := s.ReencryptSecrets(ctx()); err != nil || n != 5 {
		t.Fatalf("ReencryptSecrets = %d, %v; want all 5 endpoints in batches of 2", n, err)
	}
	for _, id := range ids {
		if secret, _ := stored(t, pool, id); !strings.HasPrefix(secret, keys.PrimaryPrefix()) {
			t.Errorf("endpoint %s was left unencrypted", id)
		}
	}
}

// A rotation that lands between reading a row and writing it back must not be
// undone, and a plaintext secret it moved into previous_secret must not stay
// behind.
func TestReencryptionDoesNotUndoAConcurrentRotation(t *testing.T) {
	keys := keyring(t, newKey)
	s, pool := scratchService(t, keys)
	tn, err := s.CreateTenant(ctx(), "Race")
	if err != nil {
		t.Fatal(err)
	}
	id := plaintextEndpoint(t, pool, tn.ID, plainA, nil)

	var rotated store.Endpoint
	racing := *s
	racing.q = store.New(&interceptDB{DBTX: pool, name: "ReplaceEndpointSecrets", before: func() {
		var err error
		if rotated, err = s.RotateEndpointSecret(ctx(), tn.ID, id); err != nil {
			t.Fatalf("rotate: %v", err)
		}
	}})
	if _, err := racing.ReencryptSecrets(ctx()); err != nil {
		t.Fatal(err)
	}

	secret, previous := stored(t, pool, id)
	ctxID := secretbox.EndpointContext(id)
	if got, err := keys.Open(secret, ctxID); err != nil || got != rotated.Secret {
		t.Errorf("the rotation's new secret was overwritten (got %.10q, %v)", got, err)
	}
	if previous == nil {
		t.Fatal("the previous secret is gone")
	}
	if got, err := keys.Open(*previous, ctxID); err != nil || got != plainA {
		t.Errorf("the old secret, moved to previous_secret by the rotation, is not sealed: %.10q, %v", *previous, err)
	}
}

func TestCheckSecretsNamesWhatIsWrong(t *testing.T) {
	s, pool := scratchService(t, keyring(t, newKey))
	if err := s.CheckSecrets(ctx()); err != nil {
		t.Fatalf("an empty database fails the check: %v", err)
	}
	tn, err := s.CreateTenant(ctx(), "Check")
	if err != nil {
		t.Fatal(err)
	}
	ok, err := s.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/ok"})
	if err != nil {
		t.Fatal(err)
	}
	// Rotated, so a sealed previous_secret is looked at too.
	if _, err := s.RotateEndpointSecret(ctx(), tn.ID, ok.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.CheckSecrets(ctx()); err != nil {
		t.Errorf("sealed secrets fail the check: %v", err)
	}

	// Sealed with a key that is not configured (removed too early).
	oldSvc := New(pool, s.log, WithSecretKeys(keyring(t, oldKey)))
	if _, err := oldSvc.CreateEndpoint(ctx(), tn.ID, EndpointInput{URL: "https://example.invalid/old"}); err != nil {
		t.Fatal(err)
	}
	oldID := keyring(t, oldKey).PrimaryID()
	err = s.CheckSecrets(ctx())
	if err == nil || !strings.Contains(err.Error(), oldID+" (1 secrets)") || strings.Contains(err.Error(), "unencrypted") {
		t.Errorf("unknown key: %v, want it named with its count", err)
	}
	// With that key listed (after the current one) it passes again.
	if err := New(pool, s.log, WithSecretKeys(keyring(t, newKey, oldKey))).CheckSecrets(ctx()); err != nil {
		t.Errorf("with the old key configured: %v", err)
	}

	// Plaintext, in either column.
	prev := plainB
	plaintextEndpoint(t, pool, tn.ID, plainA, &prev)
	err = New(pool, s.log, WithSecretKeys(keyring(t, newKey, oldKey))).CheckSecrets(ctx())
	if err == nil || !strings.Contains(err.Error(), "2 endpoint secrets are stored unencrypted") {
		t.Errorf("plaintext: %v, want both columns counted", err)
	}
	if strings.Contains(err.Error(), plainA) || strings.Contains(err.Error(), plainB) {
		t.Error("the error repeats a secret")
	}
}
