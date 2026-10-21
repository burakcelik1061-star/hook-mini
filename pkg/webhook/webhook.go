// Package webhook signs and verifies webhooks in the Standard Webhooks format
// (https://www.standardwebhooks.com), which is what Hook Mini sends.
//
// A receiver verifies a request like this:
//
//	v, err := webhook.NewVerifier(os.Getenv("WEBHOOK_SECRET")) // "whsec_..."
//	...
//	body, _ := io.ReadAll(r.Body)
//	if err := v.Verify(r.Header, body); err != nil {
//		http.Error(w, "invalid signature", http.StatusUnauthorized)
//		return
//	}
//
// Every request carries three headers:
//
//	webhook-id         the event id, identical on every retry (deduplicate on it)
//	webhook-timestamp  Unix seconds when this attempt was signed
//	webhook-signature  "v1,<base64 HMAC-SHA256>", space separated if several
//
// The signed content is "<webhook-id>.<webhook-timestamp>.<raw body>". The
// timestamp is checked against a tolerance window so a captured request cannot
// be replayed later. Because the format is a published standard, receivers can
// equally use the official Standard Webhooks libraries in other languages.
package webhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Header names. HTTP header names are case-insensitive; the standard writes
// them in lower case.
const (
	HeaderID        = "Webhook-Id"
	HeaderTimestamp = "Webhook-Timestamp"
	HeaderSignature = "Webhook-Signature"
)

// SecretPrefix starts every signing secret.
const SecretPrefix = "whsec_"

// DefaultTolerance is how far a request's timestamp may be from the
// receiver's clock, in either direction.
const DefaultTolerance = 5 * time.Minute

// MaxSignatures is the most signatures Verify accepts in one header. A
// sender adds one per active secret (two during a rotation); the bound stops
// a forged header from making each request cost thousands of decodes.
const MaxSignatures = 16

// Verification errors. They are distinct so a receiver can log the reason;
// the sender should only ever be told "unauthorized".
var (
	ErrMissingHeaders    = errors.New("webhook: missing webhook-id, webhook-timestamp or webhook-signature header")
	ErrInvalidTimestamp  = errors.New("webhook: invalid webhook-timestamp header")
	ErrTimestampTooOld   = errors.New("webhook: timestamp is too old")
	ErrTimestampTooNew   = errors.New("webhook: timestamp is too far in the future")
	ErrNoMatch           = errors.New("webhook: no signature matches")
	ErrTooManySignatures = errors.New("webhook: too many signatures in the webhook-signature header")
	ErrInvalidSecret     = errors.New("webhook: secret must be whsec_ followed by base64")
)

// DecodeSecret returns the key bytes of a "whsec_<base64>" secret. Standard
// (padded) base64 is the format; unpadded URL-safe base64 is accepted too.
func DecodeSecret(secret string) ([]byte, error) {
	enc, ok := strings.CutPrefix(strings.TrimSpace(secret), SecretPrefix)
	if !ok || enc == "" {
		return nil, ErrInvalidSecret
	}
	for _, e := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		if key, err := e.DecodeString(enc); err == nil && len(key) > 0 {
			return key, nil
		}
	}
	return nil, ErrInvalidSecret
}

// Sign returns one signature, "v1,<base64>", for the given key.
func Sign(key []byte, id string, ts time.Time, body []byte) string {
	return "v1," + base64.StdEncoding.EncodeToString(mac(key, id, ts.Unix(), body))
}

// SignatureHeader signs with every key and joins the results with spaces,
// the form used while a secret is being rotated.
func SignatureHeader(keys [][]byte, id string, ts time.Time, body []byte) string {
	sigs := make([]string, len(keys))
	for i, k := range keys {
		sigs[i] = Sign(k, id, ts, body)
	}
	return strings.Join(sigs, " ")
}

func mac(key []byte, id string, unix int64, body []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(id))
	h.Write([]byte{'.'})
	h.Write([]byte(strconv.FormatInt(unix, 10)))
	h.Write([]byte{'.'})
	h.Write(body)
	return h.Sum(nil)
}

// Verifier checks incoming webhooks.
type Verifier struct {
	keys [][]byte
	// Tolerance bounds the clock difference accepted; zero means DefaultTolerance.
	Tolerance time.Duration
	// Now returns the current time; nil means time.Now. Tests set it.
	Now func() time.Time
}

// NewVerifier accepts one or more secrets. Pass both the old and the new
// secret while switching over after a rotation.
func NewVerifier(secrets ...string) (*Verifier, error) {
	if len(secrets) == 0 {
		return nil, ErrInvalidSecret
	}
	v := &Verifier{}
	for _, s := range secrets {
		k, err := DecodeSecret(s)
		if err != nil {
			return nil, err
		}
		v.keys = append(v.keys, k)
	}
	return v, nil
}

// Verify checks the headers and the raw request body. It must be given the
// body exactly as received, before any JSON decoding.
func (v *Verifier) Verify(h http.Header, body []byte) error {
	id, tsRaw, sigs := h.Get(HeaderID), h.Get(HeaderTimestamp), h.Get(HeaderSignature)
	if id == "" || tsRaw == "" || sigs == "" {
		return ErrMissingHeaders
	}
	unix, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil {
		return ErrInvalidTimestamp
	}

	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	tol := v.Tolerance
	if tol <= 0 {
		tol = DefaultTolerance
	}
	ts := time.Unix(unix, 0)
	switch n := now(); {
	case n.Sub(ts) > tol:
		return ErrTimestampTooOld
	case ts.Sub(n) > tol:
		return ErrTimestampTooNew
	}

	// Decode each signature once, not once per key, and refuse a header with
	// more signatures than any sender has reason to send.
	var candidates [][]byte
	n := 0
	for sig := range strings.FieldsSeq(sigs) {
		if n++; n > MaxSignatures {
			return ErrTooManySignatures
		}
		version, b64, ok := strings.Cut(sig, ",")
		if !ok || version != "v1" {
			continue // unknown versions are ignored, as the standard requires
		}
		if got, err := base64.StdEncoding.DecodeString(b64); err == nil {
			candidates = append(candidates, got)
		}
	}
	for _, key := range v.keys {
		expected := mac(key, id, unix, body)
		for _, got := range candidates {
			if hmac.Equal(got, expected) {
				return nil
			}
		}
	}
	return ErrNoMatch
}
