package store

import (
	"crypto/rand"
	"encoding/base32"
	"strings"
)

// newID mints a row identifier: 16 random bytes, base32 without padding.
//
// Random rather than sequential because IDs travel to the web client and land
// in URLs, and an opaque one can't be walked. base32 rather than base64
// because the alphabet is case-insensitive and URL-safe with no escaping.
func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing means the OS entropy source is gone; there is no
		// sensible fallback that still deserves to be called an identifier.
		panic("store: no entropy for id generation: " + err.Error())
	}
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}
