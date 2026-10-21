// Package secretboxtest provides a fixed keyring for tests. Only test files
// import it, so the key never reaches a production binary.
package secretboxtest

import (
	"bytes"

	"github.com/burakcelik1061-star/hook-mini/internal/secretbox"
)

// Key is the raw test key.
var Key = bytes.Repeat([]byte{0x42}, secretbox.KeySize)

// Keyring returns a keyring holding only Key.
func Keyring() *secretbox.Keyring {
	k, err := secretbox.NewKeyring(Key)
	if err != nil {
		panic(err)
	}
	return k
}
