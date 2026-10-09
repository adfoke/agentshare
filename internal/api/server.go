// Package api is the HTTP surface: a plain JSON API with no HTML anywhere.
//
// The design assumption is that the caller is a program. Every response is
// JSON (except file bytes, which are exactly the bytes that were uploaded),
// every error carries a stable machine-readable code, and every resource
// representation carries absolute URLs so a client never has to build one.
package api

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/blob"
	"github.com/adfoke/agentshare/internal/config"
	"github.com/adfoke/agentshare/internal/id"
	"github.com/adfoke/agentshare/internal/store"
)

// Server wires configuration, storage and routing together.
type Server struct {
	cfg        *config.Config
	store      *store.Store
	blobs      *blob.Store
	log        *slog.Logger
	adminToken string
	started    time.Time
	clock      func() time.Time
	router     *gin.Engine
}

// New builds the router. adminToken is the resolved root credential.
func New(cfg *config.Config, st *store.Store, bl *blob.Store, log *slog.Logger, adminToken string) (*Server, error) {
	if !cfg.Debug {
		gin.SetMode(gin.ReleaseMode)
	}
	s := &Server{
		cfg:        cfg,
		store:      st,
		blobs:      bl,
		log:        log,
		adminToken: adminToken,
		started:    time.Now(),
		clock:      time.Now,
	}
	r := gin.New()
	r.RedirectTrailingSlash = false // a 301 on POST is a trap for API clients
	r.RedirectFixedPath = false
	r.HandleMethodNotAllowed = true
	if err := r.SetTrustedProxies(cfg.TrustedProxies); err != nil {
		return nil, fmt.Errorf("trusted proxies: %w", err)
	}
	r.Use(s.recoverPanic(), s.withRequestID(), s.accessLog(), s.securityHeaders(), s.cors())

	r.NoRoute(func(c *gin.Context) {
		fail(c, http.StatusNotFound, codeNotFound, "no such endpoint",
			"GET "+s.absURL(c, "/")+" lists every endpoint, or read "+s.absURL(c, "/openapi.json"))
	})
	r.NoMethod(func(c *gin.Context) {
		fail(c, http.StatusMethodNotAllowed, codeBadRequest,
			"method "+c.Request.Method+" is not allowed here",
			"see "+s.absURL(c, "/openapi.json")+" for the methods each path accepts")
	})

	// --- open, no credential required -------------------------------------
	r.GET("/", s.describe)
	r.GET("/healthz", s.health)
	r.GET("/openapi.json", s.openapi)
	r.GET("/llms.txt", s.llms)
	r.GET("/favicon.ico", func(c *gin.Context) { c.Status(http.StatusNoContent) })

	// Capability URLs: possession of the token is the credential.
	r.GET("/s/:token", s.publicDownload)
	r.HEAD("/s/:token", s.publicDownload)
	r.GET("/s/:token/info", s.publicShareInfo)

	// --- authenticated ----------------------------------------------------
	v1 := r.Group("/v1")

	v1.GET("/whoami", s.authenticate(), s.whoami)
	v1.GET("/stats", s.authenticate(), s.requireScope(ScopeList), s.stats)

	// Upload. POST and PUT are both accepted with a raw request body; a
	// multipart/form-data body is understood as well, taking the first file
	// part, because half the world's tooling reaches for -F first.
	v1.POST("/files", s.authenticate(), s.requireScope(ScopeUpload), s.uploadFile)
	v1.PUT("/files", s.authenticate(), s.requireScope(ScopeUpload), s.uploadFile)
	v1.POST("/text", s.authenticate(), s.requireScope(ScopeUpload), s.uploadText)

	v1.GET("/files", s.authenticate(), s.requireScope(ScopeList), s.listFiles)
	v1.GET("/files/:id", s.authenticate(), s.requireScope(ScopeDownload), s.fileMeta)
	v1.GET("/files/:id/content", s.authenticate(), s.requireScope(ScopeDownload), s.fileContent)
	v1.HEAD("/files/:id/content", s.authenticate(), s.requireScope(ScopeDownload), s.fileContent)
	v1.DELETE("/files/:id", s.authenticate(), s.requireScope(ScopeDelete), s.deleteFile)

	// Content-addressed access: no file id needed if you know the digest.
	v1.PUT("/content/:sha256", s.authenticate(), s.requireScope(ScopeUpload), s.uploadFile)
	v1.GET("/content/:sha256", s.authenticate(), s.requireScope(ScopeDownload), s.getContent)
	v1.HEAD("/content/:sha256", s.authenticate(), s.requireScope(ScopeDownload), s.getContent)
	v1.GET("/content/:sha256/meta", s.authenticate(), s.requireScope(ScopeDownload), s.contentMeta)

	v1.POST("/shares", s.authenticate(), s.requireScope(ScopeShare), s.createShare)
	v1.GET("/shares", s.authenticate(), s.requireScope(ScopeShare), s.listShares)
	v1.GET("/shares/:id", s.authenticate(), s.requireScope(ScopeShare), s.shareMeta)
	v1.DELETE("/shares/:id", s.authenticate(), s.requireScope(ScopeShare), s.deleteShare)

	v1.POST("/tokens", s.authenticate(), s.requireScope(ScopeAdmin), s.createToken)
	v1.GET("/tokens", s.authenticate(), s.requireScope(ScopeAdmin), s.listTokens)
	v1.DELETE("/tokens/:id", s.authenticate(), s.requireScope(ScopeAdmin), s.deleteToken)

	s.router = r
	return s, nil
}

// Handler returns the root http.Handler.
func (s *Server) Handler() http.Handler { return s.router }

// ---------------------------------------------------------------- middleware

func (s *Server) recoverPanic() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic recovered",
					"err", fmt.Sprint(rec),
					"path", c.Request.URL.Path,
					"request_id", requestID(c),
					"stack", string(debug.Stack()))
				if !c.Writer.Written() {
					fail(c, http.StatusInternalServerError, codeInternal, "internal error",
						"retry once; the request id identifies this in the server log")
				}
				c.Abort()
			}
		}()
		c.Next()
	}
}

func (s *Server) withRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		rid := strings.TrimSpace(c.GetHeader("X-Request-Id"))
		if rid == "" || len(rid) > 128 {
			rid = id.Request()
		}
		c.Set(ctxRequestID, rid)
		c.Header("X-Request-Id", rid)
		c.Next()
	}
}

func (s *Server) accessLog() gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()

		path := c.Request.URL.Path
		// A share URL is a bearer capability; it has no business sitting in a
		// log line that other people can read.
		if strings.HasPrefix(path, "/s/") {
			path = "/s/<redacted>"
		}

		attrs := []any{
			"method", c.Request.Method,
			"path", path,
			"status", c.Writer.Status(),
			"bytes", c.Writer.Size(),
			"dur_ms", float64(time.Since(start).Microseconds()) / 1000,
			"ip", c.ClientIP(),
			"request_id", requestID(c),
			"caller", callerName(c),
		}
		if ua := c.Request.UserAgent(); ua != "" {
			attrs = append(attrs, "ua", ua)
		}
		if len(c.Errors) > 0 {
			attrs = append(attrs, "err", c.Errors.String())
		}

		// 5xx is our fault. 401/403 is worth noticing — it is what a credential
		// being probed looks like. Everything else at 4xx is the client's own
		// mistake, and a public listener sees enough scanner traffic (404s) that
		// logging it at warn would bury the lines that matter.
		switch code := c.Writer.Status(); {
		case code >= 500:
			s.log.Error("request", attrs...)
		case code == http.StatusUnauthorized || code == http.StatusForbidden:
			s.log.Warn("request", attrs...)
		default:
			s.log.Info("request", attrs...)
		}
	}
}

func callerName(c *gin.Context) string {
	if id := identity(c); id != nil {
		return id.ID
	}
	return ""
}

func (s *Server) securityHeaders() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if strings.HasPrefix(c.Request.URL.Path, "/s/") {
			// A share link may be indexed by accident if it ever lands in a
			// page; make sure it cannot be followed.
			h.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		}
		c.Next()
	}
}

func (s *Server) cors() gin.HandlerFunc {
	origin := s.cfg.CORSOrigin
	if origin == "" {
		return func(c *gin.Context) { c.Next() }
	}
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", origin)
		h.Set("Access-Control-Allow-Methods", "GET, HEAD, POST, PUT, DELETE, OPTIONS")
		h.Set("Access-Control-Allow-Headers",
			"Authorization, Content-Type, X-API-Key, X-Filename, X-Tag, X-Ttl, X-Meta, X-Sha256, X-Request-Id, Range")
		h.Set("Access-Control-Expose-Headers",
			"Content-Length, Content-Range, ETag, Location, X-Checksum-Sha256, X-File-Id, X-File-Size, X-Request-Id")
		h.Set("Access-Control-Max-Age", "86400")
		if origin != "*" {
			h.Add("Vary", "Origin")
		}
		if c.Request.Method == http.MethodOptions {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}
