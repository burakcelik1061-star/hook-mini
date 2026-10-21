package webhook

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The example from the Standard Webhooks specification. The expected
// signature was also recomputed independently (Python hmac/hashlib) before
// being written here.
const (
	specSecret  = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"
	specID      = "msg_p5jXN8AQM9LWM0D4loKWxJek"
	specTS      = 1614265330
	specBody    = `{"test": 2432232314}`
	specSigning = "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="
)

func specHeaders(sig string) http.Header {
	h := http.Header{}
	h.Set(HeaderID, specID)
	h.Set(HeaderTimestamp, strconv.Itoa(specTS))
	h.Set(HeaderSignature, sig)
	return h
}

func specVerifier(t *testing.T, secrets ...string) *Verifier {
	t.Helper()
	if len(secrets) == 0 {
		secrets = []string{specSecret}
	}
	v, err := NewVerifier(secrets...)
	if err != nil {
		t.Fatal(err)
	}
	v.Now = func() time.Time { return time.Unix(specTS, 0) }
	return v
}

func TestSignMatchesTheSpecificationExample(t *testing.T) {
	key, err := DecodeSecret(specSecret)
	if err != nil {
		t.Fatal(err)
	}
	if got := Sign(key, specID, time.Unix(specTS, 0), []byte(specBody)); got != specSigning {
		t.Fatalf("Sign = %s, want %s", got, specSigning)
	}
}

func TestVerifyAcceptsTheSpecificationExample(t *testing.T) {
	if err := specVerifier(t).Verify(specHeaders(specSigning), []byte(specBody)); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

func TestVerifyRejectsTampering(t *testing.T) {
	v := specVerifier(t)
	body := []byte(specBody)

	cases := map[string]struct {
		h    http.Header
		body []byte
		want error
	}{
		"changed body":     {specHeaders(specSigning), []byte(`{"test": 2432232315}`), ErrNoMatch},
		"reformatted body": {specHeaders(specSigning), []byte(`{"test":2432232314}`), ErrNoMatch},
		// (Changing the character before "=" would not do: its last two bits
		// are padding and decode to the same bytes.)
		"changed signature": {specHeaders("v1,h0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), body, ErrNoMatch},
		"unknown version":   {specHeaders("v2,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), body, ErrNoMatch},
		"no version":        {specHeaders("g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE="), body, ErrNoMatch},
		"garbage signature": {specHeaders("v1,not-base64!!"), body, ErrNoMatch},
		"missing signature": {specHeaders(""), body, ErrMissingHeaders},
		"bad timestamp":     {func() http.Header { h := specHeaders(specSigning); h.Set(HeaderTimestamp, "yesterday"); return h }(), body, ErrInvalidTimestamp},
		"missing id":        {func() http.Header { h := specHeaders(specSigning); h.Del(HeaderID); return h }(), body, ErrMissingHeaders},
		"other id":          {func() http.Header { h := specHeaders(specSigning); h.Set(HeaderID, "msg_other"); return h }(), body, ErrNoMatch},
		"timestamp shifted": {func() http.Header {
			h := specHeaders(specSigning)
			h.Set(HeaderTimestamp, strconv.Itoa(specTS+1))
			return h
		}(), body, ErrNoMatch},
	}
	for name, c := range cases {
		if err := v.Verify(c.h, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}

	// The wrong secret never verifies.
	other := specVerifier(t, "whsec_"+strings.Repeat("A", 32))
	if err := other.Verify(specHeaders(specSigning), body); !errors.Is(err, ErrNoMatch) {
		t.Errorf("wrong secret: %v", err)
	}
}

func TestVerifyEnforcesTheTimestampWindow(t *testing.T) {
	key, _ := DecodeSecret(specSecret)
	v := specVerifier(t)
	now := time.Unix(specTS, 0)

	for name, c := range map[string]struct {
		ts   time.Time
		want error
	}{
		"4m59s old":       {now.Add(-(5*time.Minute - time.Second)), nil},
		"5m01s old":       {now.Add(-(5*time.Minute + time.Second)), ErrTimestampTooOld},
		"a day old":       {now.Add(-24 * time.Hour), ErrTimestampTooOld},
		"4m59s in future": {now.Add(5*time.Minute - time.Second), nil},
		"5m01s in future": {now.Add(5*time.Minute + time.Second), ErrTimestampTooNew},
	} {
		h := http.Header{}
		h.Set(HeaderID, specID)
		h.Set(HeaderTimestamp, strconv.FormatInt(c.ts.Unix(), 10))
		h.Set(HeaderSignature, Sign(key, specID, c.ts, []byte(specBody)))
		if err := v.Verify(h, []byte(specBody)); !errors.Is(err, c.want) {
			t.Errorf("%s: err = %v, want %v", name, err, c.want)
		}
	}
}

// During a rotation the sender signs with both secrets; a receiver that knows
// either one accepts the request.
func TestRotationWithTwoSignatures(t *testing.T) {
	oldKey, _ := DecodeSecret(specSecret)
	newSecret := "whsec_" + "dGhpcy1pcy1hLW5ldy1zZWNyZXQtMzItYnl0ZXMhIQ==" // 32 bytes
	newKey, err := DecodeSecret(newSecret)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Unix(specTS, 0)
	header := SignatureHeader([][]byte{newKey, oldKey}, specID, ts, []byte(specBody))
	if n := len(strings.Fields(header)); n != 2 {
		t.Fatalf("header has %d signatures: %q", n, header)
	}

	for name, secrets := range map[string][]string{
		"receiver still on the old secret": {specSecret},
		"receiver already on the new one":  {newSecret},
		"receiver configured with both":    {newSecret, specSecret},
	} {
		if err := specVerifier(t, secrets...).Verify(specHeaders(header), []byte(specBody)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := specVerifier(t, "whsec_"+strings.Repeat("B", 32)).Verify(specHeaders(header), []byte(specBody)); !errors.Is(err, ErrNoMatch) {
		t.Errorf("an unrelated secret verified: %v", err)
	}
}

func TestDecodeSecret(t *testing.T) {
	for _, s := range []string{specSecret, "whsec_aGVsbG8gd29ybGQ=", "whsec_aGVsbG8gd29ybGQ", "whsec_-_8=", " whsec_aGk= "} {
		if _, err := DecodeSecret(s); err != nil {
			t.Errorf("DecodeSecret(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "whsec_", "MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", "whsec_***", "sk_test_123"} {
		if _, err := DecodeSecret(s); !errors.Is(err, ErrInvalidSecret) {
			t.Errorf("DecodeSecret(%q) accepted", s)
		}
	}
	if _, err := NewVerifier(); !errors.Is(err, ErrInvalidSecret) {
		t.Error("NewVerifier with no secret must fail")
	}
}
