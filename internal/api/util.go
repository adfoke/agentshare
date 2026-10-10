package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/config"
)

// Bounds for JSON request bodies. Without one, a decoder reads the entire body
// into memory before any handler gets a chance to object to its size.
const (
	// maxTextBody covers POST /v1/text: at most 8 MiB of content, plus room for
	// JSON escaping and the surrounding fields.
	maxTextBody = 12 << 20
	// maxSmallBody covers the endpoints whose whole body is a handful of
	// fields. Nothing legitimate comes close.
	maxSmallBody = 64 << 10
)

// bindJSON decodes a JSON request body, refusing anything larger than maxBytes.
//
// The limit has to go on the reader rather than on the decoded value. A size
// check placed after the call is checking something that has already happened:
// by the time a decoder returns, it has read the whole body into memory. The
// endpoints this guards are all behind a token, but the cheapest credential the
// service issues should not be able to make the process allocate as much as it
// cares to send.
func bindJSON[T any](c *gin.Context, maxBytes int64, hint string) (*T, bool) {
	if maxBytes > 0 {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
	}
	var v T
	if err := c.ShouldBindJSON(&v); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(c, http.StatusRequestEntityTooLarge, codeTooLarge,
				fmt.Sprintf("request body exceeds the %d byte limit for this endpoint", tooLarge.Limit),
				hint)
			return nil, false
		}
		fail(c, http.StatusBadRequest, codeBadRequest, "invalid JSON body: "+err.Error(), hint)
		return nil, false
	}
	return &v, true
}

const ctxRequestID = "agentshare.request_id"

func (s *Server) now() time.Time { return s.clock() }

// ---------------------------------------------------------------- names

// sanitizeName reduces a client-supplied filename to something that is safe to
// store, log and echo back in a Content-Disposition header. The name is never
// used as a path, but "safe on a terminal" still matters: agentshare prints
// these in listings.
func sanitizeName(raw string) string {
	name := strings.TrimSpace(raw)
	name = strings.ReplaceAll(name, "\\", "/")
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	// A leading dot keeps a file out of casual `ls`; allow that, but a bare "."
	// or ".." is never a name. An empty result is reported as empty so the
	// caller can apply its own default (an explicit name and a multipart
	// filename do not get the same fallback).
	if name == "." || name == ".." {
		return ""
	}
	if runes := []rune(name); len(runes) > 200 {
		name = string(runes[:200])
	}
	return name
}

// contentDisposition builds a header that survives both ASCII-only clients and
// everything else (RFC 6266 + RFC 5987).
func contentDisposition(name string, inline bool) string {
	kind := "attachment"
	if inline {
		kind = "inline"
	}
	ascii := strings.Map(func(r rune) rune {
		if r > 0x20 && r < 0x7f && r != '"' && r != '\\' {
			return r
		}
		return '_'
	}, name)
	if ascii == "" {
		ascii = "file.bin"
	}
	// QueryEscape leaves nothing that needs quoting except "+", which stands
	// for a space in that encoding.
	encoded := strings.ReplaceAll(url.QueryEscape(name), "+", "%20")
	return fmt.Sprintf(`%s; filename="%s"; filename*=UTF-8''%s`, kind, ascii, encoded)
}

// ---------------------------------------------------------------- parsing

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstCSV(v string) string {
	if i := strings.IndexByte(v, ','); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}

func parseTags(groups ...[]string) []string {
	var out []string
	seen := map[string]bool{}
	for _, g := range groups {
		for _, raw := range g {
			for _, t := range strings.Split(raw, ",") {
				t = strings.TrimSpace(t)
				if t == "" || seen[t] {
					continue
				}
				seen[t] = true
				out = append(out, t)
			}
		}
	}
	if len(out) > 32 {
		out = out[:32]
	}
	return out
}

// resolveTTL turns the various spellings of a lifetime into an absolute unix
// expiry. Accepts "30m", "24h", "7d", "2w", a bare number of seconds, and the
// words 0/never/none for "keep forever".
//
// A lifetime above the operator's cap is refused rather than quietly shortened:
// silently storing a file for less time than the caller asked for is how you
// get data that disappears while everyone believes it is safe.
func (s *Server) resolveTTL(raw string) (*int64, error) {
	raw = strings.TrimSpace(raw)
	var d time.Duration
	if raw == "" {
		d = s.cfg.DefaultTTL
	} else {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			if n < 0 {
				return nil, errors.New("ttl must not be negative")
			}
			d = time.Duration(n) * time.Second
		} else {
			parsed, err := config.ParseDuration(raw)
			if err != nil {
				return nil, fmt.Errorf("ttl: %q is not a lifetime (try 30m, 24h, 7d, 2w, or seconds)", raw)
			}
			d = parsed
		}
	}

	if d == 0 {
		return nil, nil
	}
	if s.cfg.MaxTTL > 0 && d > s.cfg.MaxTTL {
		return nil, fmt.Errorf("ttl %s exceeds this server's maximum of %s",
			d, s.cfg.MaxTTL)
	}
	exp := s.now().Add(d).Unix()
	return &exp, nil
}

// ---------------------------------------------------------------- urls

// baseURL is the public prefix for links in responses. When no --base-url is
// configured it is derived from the request, and only then are the
// X-Forwarded-* headers consulted — and only when --behind-proxy is on, so an
// untrusted client cannot talk the server into printing URLs it does not own.
func (s *Server) baseURL(c *gin.Context) string {
	if s.cfg.BaseURL != "" {
		return strings.TrimRight(s.cfg.BaseURL, "/")
	}
	scheme := "http"
	if c.Request.TLS != nil {
		scheme = "https"
	}
	host := c.Request.Host
	if s.cfg.BehindProxy {
		if p := firstCSV(c.GetHeader("X-Forwarded-Proto")); p != "" {
			scheme = p
		}
		if h := firstCSV(c.GetHeader("X-Forwarded-Host")); h != "" {
			host = h
		}
	}
	if host == "" {
		host = s.cfg.Listen
	}
	return scheme + "://" + host
}

func (s *Server) absURL(c *gin.Context, path string) string {
	return s.baseURL(c) + path
}

// ---------------------------------------------------------------- misc

func timeStr(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.RFC3339) }

func timePtrStr(unix *int64) *string {
	if unix == nil {
		return nil
	}
	v := timeStr(*unix)
	return &v
}

func clampLimit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}
