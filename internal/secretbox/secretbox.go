// Package secretbox encrypts the endpoint signing secrets kept in the
// database, so that a leaked dump or backup does not hand out the means to
// forge webhooks for every tenant.
//
// Secrets are sealed with AES-256-GCM under a key from the environment, never
// from the database. A sealed value looks like
//
//	enc:v1:<key id>:<base64url(nonce || ciphertext || tag)>
//
// which cannot be mistaken for a plaintext "whsec_..." secret. The key id
// names the key that sealed it, so several keys can be configured at once:
// the first one seals, all of them open. That is what makes replacing a key
// possible without downtime (add the new key first, re-encrypt, then drop
// the old one).
//
// Every value is bound to a context (the endpoint's id) as additional
// authenticated data: a sealed secret copied onto another endpoint does not
// open there.
package secretbox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// KeySize is the length of an encryption key in bytes (AES-256).
const KeySize = 32

const (
	prefix  = "enc:v1:"
	idBytes = 4 // a key id is 8 hex characters
)

// Errors from Open. They never contain the value or the key.
var (
	ErrNotSealed  = errors.New("secretbox: value is not encrypted")
	ErrUnknownKey = errors.New("secretbox: value was encrypted with a key that is not configured")
	ErrMalformed  = errors.New("secretbox: malformed encrypted value")
	ErrOpen       = errors.New("secretbox: value cannot be decrypted (wrong context, or tampered with)")
)

type key struct {
	id   string
	aead cipher.AEAD
}

// Keyring holds the configured keys. The first one seals; every one opens.
// It is safe for concurrent use.
type Keyring struct {
	keys []key
}

// NewKeyring builds a keyring from raw keys, the sealing key first.
func NewKeyring(raw ...[]byte) (*Keyring, error) {
	if len(raw) == 0 {
		return nil, errors.New("at least one encryption key is required")
	}
	k := &Keyring{}
	seen := map[string]bool{}
	for i, r := range raw {
		if len(r) != KeySize {
			return nil, fmt.Errorf("encryption key %d is %d bytes, want %d", i+1, len(r), KeySize)
		}
		id := keyID(r)
		if seen[id] {
			return nil, fmt.Errorf("encryption key %d is listed twice", i+1)
		}
		seen[id] = true
		block, err := aes.NewCipher(r)
		if err != nil {
			return nil, fmt.Errorf("encryption key %d: %w", i+1, err)
		}
		aead, err := cipher.NewGCM(block)
		if err != nil {
			return nil, fmt.Errorf("encryption key %d: %w", i+1, err)
		}
		k.keys = append(k.keys, key{id: id, aead: aead})
	}
	return k, nil
}

// ParseKeys builds a keyring from a comma-separated list of standard base64
// keys, as found in SECRET_ENCRYPTION_KEYS. Errors never repeat a key.
func ParseKeys(s string) (*Keyring, error) {
	var raw [][]byte
	for i, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		b, err := base64.StdEncoding.DecodeString(part)
		if err != nil {
			return nil, fmt.Errorf("encryption key %d is not valid base64", i+1)
		}
		raw = append(raw, b)
	}
	return NewKeyring(raw...)
}

// GenerateKey returns a new random key in the form ParseKeys accepts.
func GenerateKey() (string, error) {
	b := make([]byte, KeySize)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(b), nil
}

// keyID derives a short, stable name for a key. It reveals nothing useful
// about the key (a truncated hash of a random 256-bit value).
func keyID(raw []byte) string {
	h := sha256.Sum256(append([]byte("hook-mini secretbox key id\x00"), raw...))
	return hex.EncodeToString(h[:idBytes])
}

// PrimaryID is the id of the key that seals.
func (k *Keyring) PrimaryID() string { return k.keys[0].id }

// Has reports whether the key with this id is configured.
func (k *Keyring) Has(id string) bool {
	for _, key := range k.keys {
		if key.id == id {
			return true
		}
	}
	return false
}

// PrimaryPrefix is how every value sealed with the primary key starts.
func (k *Keyring) PrimaryPrefix() string { return prefix + k.PrimaryID() + ":" }

// Seal encrypts plaintext under the primary key, bound to context.
func (k *Keyring) Seal(plaintext string, context []byte) (string, error) {
	pk := k.keys[0]
	nonce := make([]byte, pk.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("secretbox: nonce: %w", err)
	}
	sealed := pk.aead.Seal(nonce, nonce, []byte(plaintext), context)
	return prefix + pk.id + ":" + base64.RawURLEncoding.EncodeToString(sealed), nil
}

// KeyIDOf returns the id of the key that sealed value, or ErrNotSealed.
func KeyIDOf(value string) (string, error) {
	rest, ok := strings.CutPrefix(value, prefix)
	if !ok {
		return "", ErrNotSealed
	}
	id, _, ok := strings.Cut(rest, ":")
	if !ok || len(id) != 2*idBytes {
		return "", ErrMalformed
	}
	return id, nil
}

// EndpointContext is the context an endpoint's signing secrets are sealed
// with. It binds the secret to that one endpoint (and to this use).
func EndpointContext(endpointID [16]byte) []byte {
	return append([]byte("hook-mini endpoint signing secret\x00"), endpointID[:]...)
}

// Open decrypts a value Seal produced with the same context. A plaintext
// value is an error, not passed through: the database must never hold one.
func (k *Keyring) Open(value string, context []byte) (string, error) {
	id, err := KeyIDOf(value)
	if err != nil {
		return "", err
	}
	var aead cipher.AEAD
	for _, key := range k.keys {
		if key.id == id {
			aead = key.aead
			break
		}
	}
	if aead == nil {
		return "", ErrUnknownKey
	}
	sealed, err := base64.RawURLEncoding.DecodeString(value[len(prefix)+len(id)+1:])
	if err != nil || len(sealed) < aead.NonceSize()+aead.Overhead() {
		return "", ErrMalformed
	}
	nonce, ciphertext := sealed[:aead.NonceSize()], sealed[aead.NonceSize():]
	plain, err := aead.Open(nil, nonce, ciphertext, context)
	if err != nil {
		return "", ErrOpen
	}
	return string(plain), nil
}
