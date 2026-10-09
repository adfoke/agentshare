package store

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// ErrBadCursor marks a page cursor that did not come from this server.
var ErrBadCursor = errors.New("invalid cursor")

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// EncodeCursor packs the sort key of the last row of a page. It is opaque on
// purpose: clients must treat it as a token, not parse it.
func EncodeCursor(createdAt int64, id string) string {
	raw := strconv.FormatInt(createdAt, 10) + ":" + id
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeCursor unpacks a cursor produced by EncodeCursor.
func DecodeCursor(s string) (int64, string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return 0, "", fmt.Errorf("%w: %v", ErrBadCursor, err)
	}
	at, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return 0, "", ErrBadCursor
	}
	n, err := strconv.ParseInt(at, 10, 64)
	if err != nil {
		return 0, "", ErrBadCursor
	}
	return n, id, nil
}
