package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/id"
	"github.com/adfoke/agentshare/internal/store"
)

// Tokens are how you avoid handing the root credential to every agent that
// needs to drop a file somewhere. Mint one per job, scope it, let it expire.

type createTokenRequest struct {
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	TTL        string   `json:"ttl"`
	TTLSeconds *int64   `json:"ttl_seconds"`
}

func (s *Server) createToken(c *gin.Context) {
	req, ok := bindJSON[createTokenRequest](c, maxSmallBody,
		`expected {"name": "nightly-ci", "scopes": ["upload","share"], "ttl": "30d"}`)
	if !ok {
		return
	}

	scopes, problem := scopesAllowed(req.Scopes)
	if problem != "" {
		fail(c, http.StatusBadRequest, codeBadRequest, problem,
			"scopes are: "+strings.Join(AllScopes, ", "))
		return
	}

	ttlRaw := strings.TrimSpace(req.TTL)
	if ttlRaw == "" && req.TTLSeconds != nil {
		ttlRaw = strconv.FormatInt(*req.TTLSeconds, 10)
	}
	expires, err := s.resolveTTL(firstNonEmpty(ttlRaw, c.Query("ttl")))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}

	secret := id.Secret()
	tok := &store.Token{
		ID:        id.Token(),
		Name:      strings.TrimSpace(req.Name),
		Scopes:    scopes,
		CreatedAt: s.now().Unix(),
		ExpiresAt: expires,
	}
	if err := s.store.InsertToken(c.Request.Context(), tok, store.TokenHash(secret)); err != nil {
		failInternal(c, s.log, err, "cannot create the token")
		return
	}

	c.JSON(http.StatusCreated, gin.H{
		"token": s.tokenDTO(tok, secret),
		"note":  "store this secret now: it is not recoverable, only revocable",
	})
}

func (s *Server) listTokens(c *gin.Context) {
	tokens, err := s.store.ListTokens(c.Request.Context())
	if err != nil {
		failInternal(c, s.log, err, "cannot list tokens")
		return
	}
	out := make([]tokenDTO, 0, len(tokens))
	for _, t := range tokens {
		out = append(out, s.tokenDTO(t, ""))
	}
	c.JSON(http.StatusOK, gin.H{"tokens": out, "count": len(out)})
}

func (s *Server) deleteToken(c *gin.Context) {
	target := c.Param("id")
	if caller := identity(c); caller != nil && caller.ID == target {
		fail(c, http.StatusBadRequest, codeBadRequest, "a token cannot revoke itself",
			"use the root token, or another admin token")
		return
	}
	err := s.store.DeleteToken(c.Request.Context(), target)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such token")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot revoke the token")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true, "token_id": target})
}

// whoami is the cheapest way for an agent to find out what it is allowed to do
// before it tries something that will 403.
func (s *Server) whoami(c *gin.Context) {
	id := identity(c)
	if id == nil {
		fail(c, http.StatusUnauthorized, codeUnauthorized, "missing API token")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"identity": id,
		"scopes":   id.Scopes,
		"server":   s.absURL(c, "/"),
	})
}

func (s *Server) stats(c *gin.Context) {
	ctx := c.Request.Context()
	st, err := s.store.StatsFor(ctx)
	if err != nil {
		failInternal(c, s.log, err, "cannot read the statistics")
		return
	}
	blobBytes, err := s.blobs.TotalBytes()
	if err != nil {
		failInternal(c, s.log, err, "cannot size the blob store")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"files":            st.Files,
		"files_bytes":      st.FilesBytes,
		"shares":           st.Shares,
		"tokens":           st.Tokens,
		"blobs_on_disk":    blobBytes,
		"blob_dir":         s.blobs.Root(),
		"max_upload_bytes": s.cfg.MaxUpload,
		"default_ttl":      s.cfg.DefaultTTL.String(),
		"uptime_seconds":   int64(s.now().Sub(s.started).Seconds()),
		"version":          versionLine(),
	})
}
