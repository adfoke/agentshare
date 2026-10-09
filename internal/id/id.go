// Package id generates the short, URL-safe, randomly-drawn identifiers used
// for files, share links, tokens and request ids.
//
// The alphabet is Crockford-style base32 (no i, l, o, u) so identifiers survive
// being copied by hand out of a terminal log. Because 256 is an exact multiple
// of 32, taking a random byte modulo the alphabet size introduces no bias.
package id

import (
	"crypto/rand"
	"strings"
)

const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"

func draw(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		// crypto/rand failing means the process has no usable entropy source;
		// there is no safe way to continue handing out identifiers.
		panic("agentshare: crypto/rand failed: " + err.Error())
	}
	var sb strings.Builder
	sb.Grow(n)
	for _, b := range buf {
		sb.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	return sb.String()
}

// File returns an opaque handle for a stored file, e.g. f_9k3m.... It is short
// enough to paste, and never reused.
func File() string { return "f_" + draw(20) }

// Share returns the capability token embedded in a public share URL
// (/s/<token>). 26 symbols of a 32-symbol alphabet is 130 bits.
func Share() string { return draw(26) }

// Token returns the public portion of an API token id, e.g. t_ab12....
func Token() string { return "t_" + draw(16) }

// Secret returns a full API token secret. The ash_ prefix makes leaked tokens
// easy to grep for in logs and repositories.
func Secret() string { return "ash_" + draw(40) }

// Request returns a correlation id for the access log and the X-Request-Id
// response header.
func Request() string { return draw(16) }
