package api

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// Scopes an agent can hold. "admin" implies all of them.
const (
	ScopeUpload   = "upload"
	ScopeDownload = "download"
	ScopeList     = "list"
	ScopeDelete   = "delete"
	ScopeShare    = "share"
	ScopeAdmin    = "admin"
)

// AllScopes is what the root token holds, and the set a caller may ask for.
var AllScopes = []string{ScopeUpload, ScopeDownload, ScopeList, ScopeDelete, ScopeShare, ScopeAdmin}

// Identity is the caller behind a request.
type Identity struct {
	ID     string   `json:"id"`
	Name   string   `json:"name"`
	Type   string   `json:"type"` // root | token
	Scopes []string `json:"scopes"`
}

func (i *Identity) has(scope string) bool {
	for _, s := range i.Scopes {
		if s == scope || s == ScopeAdmin {
			return true
		}
	}
	return false
}

const identityKey = "agentshare.identity"

// identity returns the caller, or nil for an unauthenticated route.
func identity(c *gin.Context) *Identity {
	if v, ok := c.Get(identityKey); ok {
		if id, ok := v.(*Identity); ok {
			return id
		}
	}
	return nil
}

func requestID(c *gin.Context) string {
	if v, ok := c.Get(ctxRequestID); ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// bearerToken pulls the credential out of the request. Three spellings are
// accepted because agent tooling is inconsistent about which one it emits:
//
//	Authorization: Bearer ash_...
//	Authorization: ash_...
//	X-API-Key: ash_...
func bearerToken(c *gin.Context) string {
	if v := strings.TrimSpace(c.GetHeader("Authorization")); v != "" {
		if len(v) > 7 && strings.EqualFold(v[:7], "bearer ") {
			return strings.TrimSpace(v[7:])
		}
		return v
	}
	return strings.TrimSpace(c.GetHeader("X-API-Key"))
}

// authenticate resolves the credential and stops the request when it is absent
// or unusable.
func (s *Server) authenticate() gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := bearerToken(c)
		if raw == "" {
			fail(c, http.StatusUnauthorized, codeUnauthorized,
				"missing API token",
				"send `Authorization: Bearer <token>`; get one from your administrator, "+
					"or read GET / for the endpoint list")
			return
		}

		if s.adminToken != "" &&
			subtle.ConstantTimeCompare([]byte(raw), []byte(s.adminToken)) == 1 {
			c.Set(identityKey, &Identity{
				ID: "root", Name: "admin", Type: "root", Scopes: AllScopes,
			})
			c.Next()
			return
		}

		tok, err := s.store.TokenBySecret(c.Request.Context(), raw)
		if err != nil {
			// A wrong token and an expired one look the same from outside: the
			// server should not help a guesser tell them apart.
			fail(c, http.StatusUnauthorized, codeUnauthorized,
				"invalid API token",
				"check the token value; expired or revoked tokens are rejected here too")
			return
		}
		if tok.Expired(s.now()) {
			fail(c, http.StatusUnauthorized, codeUnauthorized, "API token expired",
				"ask for a new token")
			return
		}
		s.store.TouchToken(c.Request.Context(), tok.ID)
		c.Set(identityKey, &Identity{
			ID: tok.ID, Name: tok.Name, Type: "token", Scopes: tok.Scopes,
		})
		c.Next()
	}
}

// requireScope must run after authenticate.
func (s *Server) requireScope(scope string) gin.HandlerFunc {
	return func(c *gin.Context) {
		id := identity(c)
		if id == nil {
			fail(c, http.StatusUnauthorized, codeUnauthorized, "missing API token")
			return
		}
		if !id.has(scope) {
			fail(c, http.StatusForbidden, codeForbidden,
				"token lacks the "+scope+" scope",
				"this token has: "+strings.Join(id.Scopes, ", "))
			return
		}
		c.Next()
	}
}

// scopesAllowed validates a scope list supplied by a caller creating a token.
func scopesAllowed(want []string) ([]string, string) {
	if len(want) == 0 {
		return nil, "at least one scope is required"
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(want))
	for _, s := range want {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" {
			continue
		}
		if !isKnownScope(s) {
			return nil, "unknown scope " + s + " (known: " + strings.Join(AllScopes, ", ") + ")"
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil, "at least one scope is required"
	}
	return out, ""
}

func isKnownScope(s string) bool {
	for _, k := range AllScopes {
		if k == s {
			return true
		}
	}
	return false
}
