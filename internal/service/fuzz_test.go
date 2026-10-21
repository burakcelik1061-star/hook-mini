package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// Fuzz tests. Under a plain `go test` they run only their seed inputs (and
// any regression inputs saved in testdata/fuzz); to actually fuzz:
//
//	TEST_DATABASE_URL=... go test -fuzz=FuzzIngestEvent -fuzztime=30s ./internal/service
//
// The property they share is the one that matters at the API boundary: bad
// input must come back as a *ValidationError (a 400), never as a database
// error (a 500). Validators that disagree with Postgres about what is valid
// turn a client mistake into a server failure.

// fuzzService returns a service with one tenant for the whole fuzz run.
func fuzzService(f *testing.F) (*Service, uuid.UUID) {
	f.Helper()
	s := newService(f)
	tn, err := s.CreateTenant(context.Background(), "fuzz "+uuid.NewString()[:8])
	if err != nil {
		f.Fatal(err)
	}
	f.Cleanup(func() { _, _ = s.q.DeleteTenant(context.Background(), tn.ID) })
	return s, tn.ID
}

// expectClientError fails the fuzz run when err is neither nil nor something
// the HTTP layer reports as a 4xx.
func expectClientError(t *testing.T, what string, err error, extra ...error) {
	t.Helper()
	if err == nil {
		return
	}
	var ve *ValidationError
	if errors.As(err, &ve) {
		return
	}
	for _, e := range extra {
		if errors.Is(err, e) {
			return
		}
	}
	t.Fatalf("%s: a client mistake became a server error: %v", what, err)
}

func FuzzCreateEndpoint(f *testing.F) {
	s, tenant := fuzzService(f)
	for _, seed := range [][2]string{
		{"https://example.invalid/hook", "billing"},
		{"https://example.invalid/a\vb", ""},        // vertical tab
		{"https://example.invalid/a b", ""},         // no-break space
		{"https://example.invalid/a b", ""},         // em space
		{"https://example.invalid/\x00", ""},        // NUL
		{"https://example.invalid/ok", "a\x00b"},    // NUL in the description
		{"https://example.invalid/ok", "tab\tnl\n"}, // control characters
		{"https://example.invalid/ok", "\xff\xfe"},  // invalid UTF-8
		{"https://[::1]:8080/", ""},                 // refused destination
		{"https://example.invalid:99999/", ""},
		{"HTTP://EXAMPLE.invalid", ""},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, url, description string) {
		ep, err := s.CreateEndpoint(context.Background(), tenant, EndpointInput{URL: url, Description: description})
		expectClientError(t, "CreateEndpoint", err)
		if err == nil {
			_ = s.DeleteEndpoint(context.Background(), tenant, ep.ID)
		}
	})
}

func FuzzUpdateEndpoint(f *testing.F) {
	s, tenant := fuzzService(f)
	ep, err := s.CreateEndpoint(context.Background(), tenant, EndpointInput{URL: "https://example.invalid/base"})
	if err != nil {
		f.Fatal(err)
	}
	f.Add("https://example.invalid/new", "d")
	f.Add("https://example.invalid/a b", "x\x00")
	f.Fuzz(func(t *testing.T, url, description string) {
		_, err := s.UpdateEndpoint(context.Background(), tenant, ep.ID, EndpointPatch{URL: &url, Description: &description})
		expectClientError(t, "UpdateEndpoint", err)
	})
}

func FuzzIngestEvent(f *testing.F) {
	s, tenant := fuzzService(f)
	for _, seed := range []struct{ typ, payload, key string }{
		{"invoice.paid", `{"amount":42}`, "k1"},
		{"a.b", `{"s":"\u0000"}`, ""},    // NUL escape: valid JSON, rejected by jsonb
		{"a.b", `{"s":"\ud800"}`, ""},    // lone surrogate
		{"a.b", `{"\u0000":1}`, ""},      // NUL escape in a key
		{"a.b", `{"n":1e999999999}`, ""}, // huge exponent
		{"a.b", `{"n":123456789012345678901234567890}`, ""},
		{"a.b", `{"a":{"a":{"a":{"a":{}}}}}`, ""},
		{"a.b", `{"a":1,"a":2}`, ""},    // duplicate keys
		{"a.b", "{\"s\":\"\xff\"}", ""}, // invalid UTF-8 inside a string
		{"a.b", `{}`, "key with space"},
		{"a.b", `{}`, "\x00"},
		{"A.B", `{}`, ""},
		{"a.b", `[1]`, ""},
		{"a.b", ``, ""},
	} {
		f.Add(seed.typ, seed.payload, seed.key)
	}
	f.Fuzz(func(t *testing.T, typ, payload, key string) {
		var k *string
		if key != "" {
			k = &key
		}
		_, err := s.IngestEvent(context.Background(), tenant, IngestInput{Type: typ, Payload: json.RawMessage(payload), IdempotencyKey: k})
		expectClientError(t, "IngestEvent", err, ErrIdempotencyMismatch)
	})
}

func FuzzDecodePage(f *testing.F) {
	for _, seed := range []string{"", "garbage", EncodeCursor(time.Now(), uuid.New()), "e30", "eyJ0IjoxfQ", "AAAA"} {
		f.Add(seed, 10)
	}
	f.Fuzz(func(t *testing.T, cursor string, limit int) {
		_, ks, size, err := decodePage(Page{Cursor: cursor, Limit: limit})
		if err != nil {
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("decodePage returned %T, want a ValidationError", err)
			}
			return
		}
		if size < 1 || size > MaxPageSize {
			t.Fatalf("accepted page size %d", size)
		}
		if ks.at != nil && (ks.at.Before(time.Unix(0, 0)) || ks.at.Year() > 9999) {
			t.Fatalf("accepted cursor time %v, which Postgres would reject or that no row has", ks.at)
		}
	})
}
