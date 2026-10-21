package credential

import (
	"encoding/base64"
	"strings"
	"testing"
)

func TestNewAPIKeyShapeAndRoundTrip(t *testing.T) {
	k, err := NewAPIKey()
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Full) != keyLen {
		t.Fatalf("key length = %d, want %d", len(k.Full), keyLen)
	}
	prefix, ok := ParseAPIKey(k.Full)
	if !ok || prefix != k.Prefix {
		t.Fatalf("ParseAPIKey(%q) = %q, %v; want %q, true", k.Full, prefix, ok, k.Prefix)
	}
	if !strings.HasPrefix(k.Full, k.Prefix+"_") {
		t.Errorf("key %q does not start with its prefix %q", k.Full, k.Prefix)
	}
	if !KeyMatches(k.Full, k.Hash[:]) {
		t.Error("a key must match its own stored hash")
	}
	if strings.Contains(k.Prefix, k.Full[len(k.Prefix)+1:]) {
		t.Error("the prefix must not contain the secret part")
	}
}

func TestKeysAreUnique(t *testing.T) {
	seen := map[string]bool{}
	for range 200 {
		k, err := NewAPIKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[k.Full] {
			t.Fatalf("duplicate key generated: %s", k.Full)
		}
		seen[k.Full] = true
	}
}

func TestParseAPIKeyRejectsMalformedInput(t *testing.T) {
	good, _ := NewAPIKey()
	body := good.Full[len(keyScheme):]
	tests := map[string]string{
		"empty":             "",
		"wrong scheme":      "xx_" + body,
		"too short":         good.Full[:keyLen-1],
		"too long":          good.Full + "0",
		"missing separator": keyScheme + body[:prefixHexLen] + "0" + body[prefixHexLen+1:],
		"uppercase hex":     keyScheme + strings.ToUpper(body),
		"non hex in secret": good.Full[:keyLen-1] + "g",
		"non hex in prefix": keyScheme + "zzzzzzzz" + body[prefixHexLen:],
		"whitespace":        good.Full + " ",
		"bearer pasted":     "Bearer " + good.Full,
	}
	for name, in := range tests {
		if _, ok := ParseAPIKey(in); ok {
			t.Errorf("%s: ParseAPIKey(%q) accepted malformed input", name, in)
		}
	}
}

func TestKeyMatchesRejectsWrongKeyAndBadHash(t *testing.T) {
	a, _ := NewAPIKey()
	b, _ := NewAPIKey()
	if KeyMatches(b.Full, a.Hash[:]) {
		t.Error("a different key must not match")
	}
	if KeyMatches(a.Full, nil) || KeyMatches(a.Full, []byte("short")) {
		t.Error("a missing or truncated stored hash must not match")
	}
	// Flip one bit of the stored hash.
	h := a.Hash
	h[0] ^= 1
	if KeyMatches(a.Full, h[:]) {
		t.Error("a tampered hash must not match")
	}
}

func TestSigningSecret(t *testing.T) {
	a, err := NewSigningSecret()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := NewSigningSecret()
	if a == b {
		t.Error("two signing secrets were identical")
	}
	if !strings.HasPrefix(a, SigningSecretPrefix) {
		t.Errorf("secret %q lacks the %q prefix", a, SigningSecretPrefix)
	}
	// The database requires at least 24 characters; ours carry 256 bits.
	if len(a) < 24+len(SigningSecretPrefix) {
		t.Errorf("secret is only %d characters", len(a))
	}
	// Standard Webhooks libraries decode the part after "whsec_" as
	// standard base64; it must come out as the full 32-byte key.
	key, err := base64.StdEncoding.Strict().DecodeString(strings.TrimPrefix(a, SigningSecretPrefix))
	if err != nil || len(key) != 32 {
		t.Errorf("secret %q is not standard base64 of 32 bytes (%d bytes, err=%v)", a, len(key), err)
	}
}
