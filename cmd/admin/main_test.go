package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/burakcelik1061-star/hook-mini/internal/service"
	"github.com/burakcelik1061-star/hook-mini/internal/store"
)

type fakeAdmin struct {
	tenant      store.Tenant
	issued      []string
	revokeErr   error
	issueErr    error
	revokedKeys []uuid.UUID
}

func (f *fakeAdmin) CreateTenant(_ context.Context, name string) (store.Tenant, error) {
	f.tenant = store.Tenant{ID: uuid.New(), Name: name, CreatedAt: time.Now()}
	return f.tenant, nil
}

func (f *fakeAdmin) IssueAPIKey(_ context.Context, tenantID uuid.UUID, name string) (store.ApiKey, string, error) {
	if f.issueErr != nil {
		return store.ApiKey{}, "", f.issueErr
	}
	f.issued = append(f.issued, name)
	return store.ApiKey{ID: uuid.New(), TenantID: tenantID, Name: name}, "hm_testkey", nil
}

func (f *fakeAdmin) RevokeAPIKey(_ context.Context, _, keyID uuid.UUID) error {
	f.revokedKeys = append(f.revokedKeys, keyID)
	return f.revokeErr
}

func TestCheckArgs(t *testing.T) {
	bad := [][]string{
		nil,
		{"nope"},
		{"bootstrap"},
		{"bootstrap", "a", "b", "c"},
		{"create-tenant"},
		{"create-tenant", "a", "b"},
		{"create-key", "only-one"},
		{"revoke-key", "a"},
	}
	for _, args := range bad {
		var u usageError
		if err := checkArgs(args); !errors.As(err, &u) {
			t.Errorf("checkArgs(%q) = %v, want a usage error", args, err)
		}
	}
	good := [][]string{
		{"bootstrap", "Acme"},
		{"bootstrap", "Acme", "ci"},
		{"create-tenant", "Acme"},
		{"create-key", "id", "ci"},
		{"revoke-key", "t", "k"},
	}
	for _, args := range good {
		if err := checkArgs(args); err != nil {
			t.Errorf("checkArgs(%q) = %v, want nil", args, err)
		}
	}
}

func TestBootstrapPrintsTenantAndKeyOnce(t *testing.T) {
	f := &fakeAdmin{}
	var out bytes.Buffer
	if err := run(context.Background(), []string{"bootstrap", "Acme Inc"}, &out, f); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"tenant_id: " + f.tenant.ID.String(), "name:      Acme Inc", "api_key:   hm_testkey", "cannot be shown again"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if len(f.issued) != 1 || f.issued[0] != "default" {
		t.Errorf("issued keys = %v, want one key named default", f.issued)
	}
}

func TestCreateKeyRejectsBadTenantID(t *testing.T) {
	var u usageError
	err := run(context.Background(), []string{"create-key", "not-a-uuid", "ci"}, &bytes.Buffer{}, &fakeAdmin{})
	if !errors.As(err, &u) {
		t.Fatalf("err = %v, want a usage error", err)
	}
}

func TestCreateKeyForUnknownTenant(t *testing.T) {
	f := &fakeAdmin{issueErr: service.ErrNotFound}
	err := run(context.Background(), []string{"create-key", uuid.NewString(), "ci"}, &bytes.Buffer{}, f)
	if err == nil || !strings.Contains(err.Error(), "no such tenant") {
		t.Fatalf("err = %v, want a friendly 'no such tenant'", err)
	}
}

func TestRevokeKey(t *testing.T) {
	f := &fakeAdmin{}
	var out bytes.Buffer
	keyID := uuid.New()
	if err := run(context.Background(), []string{"revoke-key", uuid.NewString(), keyID.String()}, &out, f); err != nil {
		t.Fatal(err)
	}
	if len(f.revokedKeys) != 1 || f.revokedKeys[0] != keyID || !strings.Contains(out.String(), keyID.String()) {
		t.Errorf("revoked %v, output %q", f.revokedKeys, out.String())
	}

	f.revokeErr = service.ErrNotFound
	err := run(context.Background(), []string{"revoke-key", uuid.NewString(), uuid.NewString()}, &out, f)
	if err == nil || !strings.Contains(err.Error(), "no active key") {
		t.Errorf("err = %v, want a friendly 'no active key'", err)
	}
}
