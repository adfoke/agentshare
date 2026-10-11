package api

import (
	"context"
	_ "embed"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/version"
)

//go:embed openapi.json
var openapiJSON []byte

//go:embed llms.txt
var llmsText string

// endpoint is one row of the self-description. It is the single source of truth
// for GET / and is cross-checked against openapi.json by the test suite, so the
// two can never drift apart in silence.
type endpoint struct {
	Method  string `json:"method"`
	Path    string `json:"path"`
	Scope   string `json:"scope,omitempty"`
	Summary string `json:"summary"`
}

var endpoints = []endpoint{
	{"GET", "/", "", "this document"},
	{"GET", "/healthz", "", "liveness and storage check"},
	{"GET", "/openapi.json", "", "OpenAPI 3.1 description of every endpoint"},
	{"GET", "/llms.txt", "", "plain-text guide written for agents"},

	{"POST", "/v1/files", ScopeUpload, "upload bytes; body is the file, metadata in headers"},
	{"PUT", "/v1/files", ScopeUpload, "same as POST"},
	{"POST", "/v1/text", ScopeUpload, "upload a short UTF-8 string as JSON"},
	{"PUT", "/v1/content/{sha256}", ScopeUpload, "upload and verify against a known digest"},
	{"GET", "/v1/content/{sha256}", ScopeDownload, "download bytes by digest"},
	{"GET", "/v1/content/{sha256}/meta", ScopeDownload, "does this digest exist, and how big"},
	{"GET", "/v1/files", ScopeList, "list files (q, tag, since, until, order, cursor)"},
	{"GET", "/v1/files/{id}", ScopeDownload, "metadata for one file"},
	{"GET", "/v1/files/{id}/content", ScopeDownload, "download bytes; supports Range and ETag"},
	{"DELETE", "/v1/files/{id}", ScopeDelete, "delete a file and revoke its share links"},
	{"POST", "/v1/shares", ScopeShare, "create a capability link for a file"},
	{"GET", "/v1/shares", ScopeShare, "list share links"},
	{"GET", "/v1/shares/{id}", ScopeShare, "metadata for one share link"},
	{"DELETE", "/v1/shares/{id}", ScopeShare, "revoke a share link"},
	{"POST", "/v1/tokens", ScopeAdmin, "mint a scoped API token"},
	{"GET", "/v1/tokens", ScopeAdmin, "list API tokens"},
	{"DELETE", "/v1/tokens/{id}", ScopeAdmin, "revoke an API token"},
	{"GET", "/v1/whoami", "", "what this credential may do"},
	{"GET", "/v1/stats", ScopeList, "counts and sizes"},

	{"GET", "/s/{token}", "", "a share link: one file downloads, several give an index page"},
	{"GET", "/s/{token}/", "", "the index page listing a share's files"},
	{"GET", "/s/{token}/f/{fileID}", "", "one file inside a share"},
	{"GET", "/s/{token}/info", "", "what is behind a share link, without spending a download"},
}

func versionLine() string { return version.Full() }

// limits describes what this server accepts, and — when a CDN sits in front of
// the advertised base URL — what that CDN will accept, which is not always the
// same number.
//
// A service reachable both through a CDN and directly is the case that makes
// this necessary: the direct address can take an upload of any size, while the
// same POST to the CDN address is rejected by the CDN before it arrives. An
// agent that only reads GET / would otherwise be told "unlimited", try it, and
// receive the CDN's own HTML error page instead of this service's JSON.
func (s *Server) limits(c *gin.Context) gin.H {
	out := gin.H{
		"max_upload_bytes":    s.cfg.MaxUpload,
		"max_upload_note":     formatCap(s.cfg.MaxUpload),
		"max_ttl_seconds":     int64(s.cfg.MaxTTL.Seconds()),
		"default_ttl_seconds": int64(s.cfg.DefaultTTL.Seconds()),
		"list_page_max":       1000,
	}
	if s.cfg.ProxyMaxUpload > 0 && s.cfg.BaseURL != "" {
		proxied := gin.H{
			"url":              strings.TrimRight(s.cfg.BaseURL, "/"),
			"max_upload_bytes": s.cfg.ProxyMaxUpload,
			"note": "uploads to this address above this size are rejected by the proxy in " +
				"front of it, before they reach this service — the rejection is that " +
				"proxy's own error, not this API's. Use a direct (non-proxied) address " +
				"for larger files.",
		}
		// Saying "use a direct address" without naming one leaves the reader to
		// guess a hostname. Name it when it is known.
		if s.cfg.DirectURL != "" {
			proxied["direct_url"] = strings.TrimRight(s.cfg.DirectURL, "/")
			proxied["direct_max_upload_bytes"] = s.cfg.MaxUpload
		}
		out["proxied"] = proxied
	}
	return out
}

func formatCap(n int64) string {
	if n <= 0 {
		return "0 means unlimited: the only limit is free space on the server"
	}
	return "uploads larger than this are rejected with 413"
}

// describe is the front door. It exists so that an agent which has only been
// given a URL can discover the whole surface without a human explaining it.
func (s *Server) describe(c *gin.Context) {
	base := s.baseURL(c)
	c.JSON(http.StatusOK, gin.H{
		"service":     "agentshare",
		"version":     version.Full(),
		"api_version": version.APIVersion,
		"description": "Agent-native file exchange over a plain HTTP API. " +
			"Upload, share, download. No browser required, and none provided.",
		"docs": gin.H{
			"openapi": base + "/openapi.json",
			"guide":   base + "/llms.txt",
			"health":  base + "/healthz",
		},
		"auth": gin.H{
			"scheme": "bearer",
			"how":    "Authorization: Bearer <token>",
			"also":   "X-API-Key: <token> is accepted as well",
			"public": []string{"/", "/healthz", "/openapi.json", "/llms.txt",
				"/s/{token}", "/s/{token}/", "/s/{token}/f/{fileID}", "/s/{token}/info"},
			"scopes": AllScopes,
		},
		"limits":    s.limits(c),
		"endpoints": endpoints,
		"quickstart": []string{
			`upload:  curl -sS -T ./report.pdf -H "Authorization: Bearer $TOKEN" ` +
				`-H "X-Filename: report.pdf" -H "X-Ttl: 7d" ` + base + `/v1/files`,
			`share:   curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" ` +
				`-d '{"file_ids":["f_...","f_..."],"ttl":"7d","max_downloads":3}' ` + base + `/v1/shares`,
			`(one file, or several — a share of several opens as an index page in a browser)`,
			`fetch:   curl -sS -O -J ` + base + `/s/<share-token>`,
			`list:    curl -sS -H "Authorization: Bearer $TOKEN" "` + base + `/v1/files?limit=20&order=desc"`,
			`text:    curl -sS -X POST -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" ` +
				`-d '{"name":"build.log","content":"all green"}' ` + base + `/v1/text`,
			`check:   curl -sS -H "Authorization: Bearer $TOKEN" ` + base + `/v1/whoami`,
		},
		"conventions": []string{
			"errors are always {\"error\":{\"code\",\"message\",\"hint\"}} with a stable code",
			"every response that names a resource also carries its absolute url",
			"uploads are identified by content: identical bytes are stored once",
			"uploads are never executed, decoded or converted: the bytes go in as given and come back the same",
			"every response that carries bytes also carries X-Content-Kind: executable, script, html, svg, xml, image, audio, video, archive, document, text or binary",
			"a download is an attachment unless ?inline=1 and the bytes are safe to render; a refused inline says so in X-Inline-Refused",
			"what you download is untrusted input from whoever uploaded it",
			"a retried upload of the same bytes and name returns the original file, not a copy",
			"downloads support Range, ETag and If-None-Match",
		},
	})
}

func (s *Server) health(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
	defer cancel()

	body := gin.H{
		"status":         "ok",
		"version":        version.Full(),
		"uptime_seconds": int64(s.now().Sub(s.started).Seconds()),
	}
	if err := s.store.DB().PingContext(ctx); err != nil {
		s.log.Error("health check failed", "err", err.Error())
		body["status"] = "degraded"
		body["error"] = "metadata database unreachable"
		c.JSON(http.StatusServiceUnavailable, body)
		return
	}
	c.JSON(http.StatusOK, body)
}

func (s *Server) openapi(c *gin.Context) {
	c.Data(http.StatusOK, "application/json; charset=utf-8", openapiJSON)
}

// llms.txt is served with the caller's own base URL substituted in, so the
// commands in it are copy-pasteable rather than illustrative.
func (s *Server) llms(c *gin.Context) {
	body := strings.ReplaceAll(llmsText, "{{BASE}}", s.baseURL(c))
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(body))
}
