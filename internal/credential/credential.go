// Package credential creates and checks the two kinds of secret Hook Mini
// hands out: API keys (a client authenticates to us) and endpoint signing
// secrets (we sign requests to a client's endpoint).
package credential

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
)

const (
	keyScheme    = "hm_"
	prefixHexLen = 8  // random, non-secret, shown in dashboards and used for lookup
	secretHexLen = 64 // 256 bits
	// keyLen is the exact length of a well-formed API key:
	// "hm_" + 8 hex + "_" + 64 hex.
	keyLen = len(keyScheme) + prefixHexLen + 1 + secretHexLen

	// SigningSecretPrefix marks webhook signing secrets so they are easy to
	// recognise in logs and secret scanners.
	SigningSecretPrefix = "whsec_"
)

// APIKey is a freshly generated key. Full is shown to the user exactly once;
// only Prefix and Hash are stored.
type APIKey struct {
	Full   string
	Prefix string
	Hash   [sha256.Size]byte
}

// NewAPIKey generates a key from the OS random source.
func NewAPIKey() (APIKey, error) {
	p, s := make([]byte, prefixHexLen/2), make([]byte, secretHexLen/2)
	if _, err := rand.Read(p); err != nil {
		return APIKey{}, fmt.Errorf("generate key prefix: %w", err)
	}
	if _, err := rand.Read(s); err != nil {
		return APIKey{}, fmt.Errorf("generate key secret: %w", err)
	}
	prefix := keyScheme + hex.EncodeToString(p)
	full := prefix + "_" + hex.EncodeToString(s)
	return APIKey{Full: full, Prefix: prefix, Hash: HashKey(full)}, nil
}

// ParseAPIKey checks that raw has the exact shape of a key and returns its
// lookup prefix. It does not prove the key is valid, only that it is worth a
// database lookup: malformed input is rejected without touching the database.
func ParseAPIKey(raw string) (prefix string, ok bool) {
	if len(raw) != keyLen || !strings.HasPrefix(raw, keyScheme) {
		return "", false
	}
	body := raw[len(keyScheme):]
	if body[prefixHexLen] != '_' {
		return "", false
	}
	if !isLowerHex(body[:prefixHexLen]) || !isLowerHex(body[prefixHexLen+1:]) {
		return "", false
	}
	return keyScheme + body[:prefixHexLen], true
}

// HashKey returns the SHA-256 of a full key. SHA-256 rather than a password
// hash is deliberate: keys are 256-bit random values, so there is nothing to
// brute-force, and a slow hash would only add latency to every request.
func HashKey(raw string) [sha256.Size]byte { return sha256.Sum256([]byte(raw)) }

// KeyMatches compares raw against a stored hash in constant time.
func KeyMatches(raw string, storedHash []byte) bool {
	h := HashKey(raw)
	return subtle.ConstantTimeCompare(h[:], storedHash) == 1
}

// NewSigningSecret returns a random secret for signing webhook requests.
func NewSigningSecret() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate signing secret: %w", err)
	}
	return SigningSecretPrefix + base64.RawURLEncoding.EncodeToString(b), nil
}

func isLowerHex(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
