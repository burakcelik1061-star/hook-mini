package service

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func wantField(t *testing.T, err error, field string) {
	t.Helper()
	var ve *ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err = %v, want a *ValidationError", err)
	}
	if ve.Field != field {
		t.Errorf("error is about %q, want %q (%v)", ve.Field, field, err)
	}
}

func TestValidateEndpointURL(t *testing.T) {
	ok := []string{
		"https://example.com",
		"https://example.com/hooks/stripe",
		"http://example.com:8080/path?x=1&y=2",
		"HTTPS://Example.COM/x",
		"https://sub.domain.example.co.uk/a/b/c",
		"https://[2001:db8::1]:8443/hook",
		"https://1.2.3.4/hook",
		"https://example.com/ünïcode",
	}
	for _, u := range ok {
		if err := ValidateEndpointURL(u); err != nil {
			t.Errorf("ValidateEndpointURL(%q) = %v, want nil", u, err)
		}
	}

	bad := map[string]string{
		"":                                     "is required",
		"example.com/hook":                     "must start with",
		"ftp://example.com":                    "must start with",
		"javascript:alert(1)":                  "must start with",
		"file:///etc/passwd":                   "must start with",
		"https://":                             "must include a host",
		"https:///path":                        "must include a host",
		"https://user:pass@example.com/":       "credentials",
		"https://user@example.com/":            "credentials",
		"https://example.com/a b":              "whitespace",
		"https://example.com/\n":               "whitespace",
		"https://example.com/#frag":            "fragment",
		"https://example.com/#":                "fragment",
		"https://example.com:0/":               "port",
		"https://example.com:70000/":           "port",
		"https://example.com:abc/":             "valid URL",
		"https://" + strings.Repeat("a", 2100): "at most",
	}
	for u, want := range bad {
		err := ValidateEndpointURL(u)
		if err == nil {
			t.Errorf("ValidateEndpointURL(%q) accepted a bad URL", u)
			continue
		}
		wantField(t, err, "url")
		if !strings.Contains(err.Error(), want) {
			t.Errorf("ValidateEndpointURL(%q) = %q, want it to mention %q", u, err, want)
		}
	}
}

func TestValidateEventType(t *testing.T) {
	for _, s := range []string{"invoice.paid", "a", "user_created", "order-shipped", "a.b.c", "v2.user.signed-up", "x1"} {
		if err := ValidateEventType(s); err != nil {
			t.Errorf("ValidateEventType(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", "Invoice.Paid", "invoice..paid", ".invoice", "invoice.", "invoice paid", "inv/oice", "é", strings.Repeat("a", 129), "a._b"} {
		wantField(t, ValidateEventType(s), "type")
	}
}

func TestValidatePayload(t *testing.T) {
	for _, s := range []string{`{}`, `{"a":1}`, ` {"a":[1,2,{"b":null}]} `, `{"nested":{"deep":true}}`} {
		if err := validatePayload(json.RawMessage(s)); err != nil {
			t.Errorf("validatePayload(%s) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{``, `null`, `[]`, `[1]`, `"x"`, `42`, `true`, `{`, `{"a":}`, `{"a":1} {"b":2}`} {
		wantField(t, validatePayload(json.RawMessage(s)), "payload")
	}
}

func TestValidateIdempotencyKey(t *testing.T) {
	ok := func(s string) *string { return &s }
	if err := validateIdempotencyKey(nil); err != nil {
		t.Errorf("a missing key must be fine: %v", err)
	}
	for _, s := range []string{"a", "order-1001", "3f6c1c2a-8e4b-4a8e-9a43-2b5f2f3d9c11", strings.Repeat("k", 255), "a_b.c:d~e"} {
		if err := validateIdempotencyKey(ok(s)); err != nil {
			t.Errorf("validateIdempotencyKey(%q) = %v, want nil", s, err)
		}
	}
	for _, s := range []string{"", strings.Repeat("k", 256), "has space", "tab\t", "new\nline", "ünï", "del\x7f"} {
		wantField(t, validateIdempotencyKey(ok(s)), "Idempotency-Key")
	}
}

func TestValidateNameAndDescription(t *testing.T) {
	if err := validateName("name", "Acme", 10); err != nil {
		t.Error(err)
	}
	wantField(t, validateName("name", "   ", 10), "name")
	wantField(t, validateName("name", strings.Repeat("a", 11), 10), "name")
	// Length is counted in characters, not bytes.
	if err := validateName("name", strings.Repeat("ö", 10), 10); err != nil {
		t.Errorf("10 two-byte characters must fit a limit of 10: %v", err)
	}

	if err := validateDescription(strings.Repeat("ö", 500)); err != nil {
		t.Errorf("500 characters must be allowed: %v", err)
	}
	wantField(t, validateDescription(strings.Repeat("ö", 501)), "description")
}

func TestCursorRoundTrip(t *testing.T) {
	at := time.Date(2026, 10, 21, 13, 45, 12, 123456000, time.UTC)
	id := uuid.New()

	_, ks, size, err := decodePage(Page{Limit: 7, Cursor: EncodeCursor(at, id)})
	if err != nil {
		t.Fatal(err)
	}
	if size != 7 {
		t.Errorf("page size = %d, want 7", size)
	}
	if ks.at == nil || !ks.at.Equal(at) || ks.id == nil || *ks.id != id {
		t.Errorf("cursor did not round trip: got %v %v, want %v %v", ks.at, ks.id, at, id)
	}
}

func TestDecodePageLimitsAndDefaults(t *testing.T) {
	limit, ks, size, err := decodePage(Page{})
	if err != nil || size != DefaultPageSize || limit != int32(DefaultPageSize+1) || ks.at != nil || ks.id != nil {
		t.Errorf("defaults: limit=%d size=%d ks=%+v err=%v", limit, size, ks, err)
	}
	if _, _, _, err := decodePage(Page{Limit: MaxPageSize}); err != nil {
		t.Errorf("the maximum page size must be accepted: %v", err)
	}
	for _, n := range []int{-1, MaxPageSize + 1, 1 << 30} {
		_, _, _, err := decodePage(Page{Limit: n})
		wantField(t, err, "limit")
	}
}

func TestDecodePageRejectsBadCursors(t *testing.T) {
	enc := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	bad := []string{
		"not base64 !!!",
		"AAAA",
		enc(`not json`),
		enc(`{}`),
		enc(`{"t":1,"i":"00000000-0000-0000-0000-000000000000"}`), // nil id
		enc(`{"t":-1,"i":"` + uuid.NewString() + `"}`),            // before 1970
		enc(`{"t":9223372036854775807,"i":"` + uuid.NewString() + `"}`),
		enc(`{"t":1,"i":"not-a-uuid"}`),
	}
	for _, c := range bad {
		_, _, _, err := decodePage(Page{Cursor: c})
		wantField(t, err, "cursor")
	}
}

func TestFinishPage(t *testing.T) {
	type row struct {
		at time.Time
		id uuid.UUID
	}
	key := func(r row) (time.Time, uuid.UUID) { return r.at, r.id }
	mk := func(n int) []row {
		rows := make([]row, n)
		for i := range rows {
			rows[i] = row{at: time.Unix(int64(1000-i), 0), id: uuid.New()}
		}
		return rows
	}

	// Exactly a full page and nothing more: no next cursor.
	res := finishPage(mk(3), 3, key)
	if len(res.Items) != 3 || res.NextCursor != "" {
		t.Errorf("full last page: %d items, cursor %q", len(res.Items), res.NextCursor)
	}

	// One extra row means another page exists; the cursor points at the last
	// row actually returned, not the extra one.
	rows := mk(4)
	res = finishPage(rows, 3, key)
	if len(res.Items) != 3 || res.NextCursor == "" {
		t.Fatalf("expected 3 items and a cursor, got %d, %q", len(res.Items), res.NextCursor)
	}
	_, ks, _, err := decodePage(Page{Cursor: res.NextCursor})
	if err != nil || *ks.id != rows[2].id {
		t.Errorf("cursor points at %v, want the 3rd row %v (err=%v)", ks.id, rows[2].id, err)
	}

	if res := finishPage[row](nil, 3, key); len(res.Items) != 0 || res.NextCursor != "" {
		t.Errorf("empty result: %+v", res)
	}
}
