package secretbox

import (
	"bytes"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

func testKey(b byte) []byte { return bytes.Repeat([]byte{b}, KeySize) }

func ring(t *testing.T, keys ...[]byte) *Keyring {
	t.Helper()
	k, err := NewKeyring(keys...)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

var (
	ctxA = EndpointContext([16]byte{1})
	ctxB = EndpointContext([16]byte{2})
)

const secret = "whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw"

func TestSealOpenRoundTrip(t *testing.T) {
	k := ring(t, testKey(1))
	sealed, err := k.Seal(secret, ctxA)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sealed, "enc:v1:"+k.PrimaryID()+":") || k.PrimaryPrefix() != "enc:v1:"+k.PrimaryID()+":" {
		t.Errorf("sealed value %q does not carry the primary key id", sealed)
	}
	if strings.Contains(sealed, "whsec_") || strings.Contains(sealed, "MfKQ9r8G") {
		t.Errorf("the sealed value shows the secret: %q", sealed)
	}
	got, err := k.Open(sealed, ctxA)
	if err != nil || got != secret {
		t.Fatalf("Open = %q, %v; want the secret back", got, err)
	}
}

// Sealing the same secret twice gives different values: a fresh nonce every
// time (reusing a GCM nonce under one key would be catastrophic).
func TestEverySealUsesAFreshNonce(t *testing.T) {
	k := ring(t, testKey(1))
	seen := map[string]bool{}
	for range 100 {
		s, err := k.Seal(secret, ctxA)
		if err != nil {
			t.Fatal(err)
		}
		if seen[s] {
			t.Fatal("two seals of the same secret produced the same value")
		}
		seen[s] = true
	}
}

// A sealed secret is bound to its endpoint: copied onto another, it does not
// open.
func TestASealedValueOpensOnlyInItsOwnContext(t *testing.T) {
	k := ring(t, testKey(1))
	sealed, _ := k.Seal(secret, ctxA)
	if _, err := k.Open(sealed, ctxB); !errors.Is(err, ErrOpen) {
		t.Errorf("opened under another endpoint's context: err = %v", err)
	}
	if _, err := k.Open(sealed, nil); !errors.Is(err, ErrOpen) {
		t.Errorf("opened without a context: err = %v", err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	k := ring(t, testKey(1))
	sealed, _ := k.Seal(secret, ctxA)
	head := k.PrimaryPrefix()
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(sealed, head))
	if err != nil {
		t.Fatal(err)
	}
	for i := range raw {
		flipped := bytes.Clone(raw)
		flipped[i] ^= 0x01
		if _, err := k.Open(head+base64.RawURLEncoding.EncodeToString(flipped), ctxA); err == nil {
			t.Fatalf("a flipped bit at byte %d went unnoticed", i)
		}
	}
	for name, v := range map[string]string{
		"truncated":  sealed[:len(sealed)-10],
		"too short":  head + "AAAA",
		"not base64": head + "!!!!",
		"no key id":  "enc:v1:",
		"bad key id": "enc:v1:xyz:AAAA",
		"empty":      "",
	} {
		if _, err := k.Open(v, ctxA); err == nil {
			t.Errorf("%s: opened", name)
		}
	}
}

// Plaintext is refused, not passed through: the database must never hold it,
// and a reader that tolerated it would hide that it does.
func TestOpenRefusesPlaintext(t *testing.T) {
	k := ring(t, testKey(1))
	if _, err := k.Open(secret, ctxA); !errors.Is(err, ErrNotSealed) {
		t.Errorf("err = %v, want ErrNotSealed", err)
	}
}

// Key replacement: a new key seals, the old one still opens what it sealed,
// and without the old key those values are reported as such.
func TestSeveralKeys(t *testing.T) {
	old := ring(t, testKey(1))
	sealedOld, _ := old.Seal(secret, ctxA)

	both := ring(t, testKey(2), testKey(1))
	if got, err := both.Open(sealedOld, ctxA); err != nil || got != secret {
		t.Fatalf("the new keyring cannot open an old value: %q, %v", got, err)
	}
	sealedNew, _ := both.Seal(secret, ctxA)
	if id, _ := KeyIDOf(sealedNew); id != both.PrimaryID() || id == old.PrimaryID() {
		t.Errorf("sealed with key %s, want the new primary %s", id, both.PrimaryID())
	}
	if !both.Has(old.PrimaryID()) || both.Has("00000000") {
		t.Error("Has does not reflect the configured keys")
	}

	onlyNew := ring(t, testKey(2))
	if _, err := onlyNew.Open(sealedOld, ctxA); !errors.Is(err, ErrUnknownKey) {
		t.Errorf("err = %v, want ErrUnknownKey", err)
	}
}

func TestKeyIDIsStableAndDistinct(t *testing.T) {
	a1, a2, b := ring(t, testKey(1)), ring(t, testKey(1)), ring(t, testKey(2))
	if a1.PrimaryID() != a2.PrimaryID() {
		t.Error("the same key gave two ids")
	}
	if a1.PrimaryID() == b.PrimaryID() {
		t.Error("two keys share an id")
	}
	if len(a1.PrimaryID()) != 8 {
		t.Errorf("id %q is not 8 hex characters", a1.PrimaryID())
	}
}

func TestParseKeys(t *testing.T) {
	k1 := base64.StdEncoding.EncodeToString(testKey(1))
	k2 := base64.StdEncoding.EncodeToString(testKey(2))

	k, err := ParseKeys(" " + k2 + " , " + k1 + " ")
	if err != nil {
		t.Fatal(err)
	}
	if k.PrimaryID() != ring(t, testKey(2)).PrimaryID() || !k.Has(ring(t, testKey(1)).PrimaryID()) {
		t.Error("ParseKeys did not keep the order (first key seals)")
	}

	short := base64.StdEncoding.EncodeToString(testKey(1)[:16])
	for name, in := range map[string]string{
		"empty":       "",
		"only commas": " , ,",
		"not base64":  "not*base64",
		"too short":   short,
		"duplicate":   k1 + "," + k1,
	} {
		_, err := ParseKeys(in)
		if err == nil {
			t.Errorf("%s: accepted", name)
			continue
		}
		if strings.Contains(err.Error(), k1) || strings.Contains(err.Error(), short) {
			t.Errorf("%s: the error repeats the key: %v", name, err)
		}
	}
}

func TestGenerateKey(t *testing.T) {
	a, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := GenerateKey()
	if a == b {
		t.Error("two generated keys are equal")
	}
	if _, err := ParseKeys(a); err != nil {
		t.Errorf("a generated key is not accepted: %v", err)
	}
}
