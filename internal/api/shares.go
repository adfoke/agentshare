package api

import (
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/id"
	"github.com/adfoke/agentshare/internal/store"
)

// A share link is a capability URL: whoever holds /s/<token> may download the
// file, with no credential of their own. That is the whole mechanism by which
// someone who is not the owner ever gets bytes out of agentshare.

type createShareRequest struct {
	FileID       string `json:"file_id"`
	Label        string `json:"label"`
	TTL          string `json:"ttl"`         // "24h", "7d", ...
	TTLSeconds   *int64 `json:"ttl_seconds"` // alternative spelling
	MaxDownloads *int64 `json:"max_downloads"`
}

func (s *Server) createShare(c *gin.Context) {
	req, ok := bindJSON[createShareRequest](c, maxSmallBody,
		`expected {"file_id": "f_...", "ttl": "7d", "max_downloads": 3}`)
	if !ok {
		return
	}
	fileID := strings.TrimSpace(req.FileID)
	if fileID == "" {
		// Allow ?file_id= too, for `curl -X POST ".../shares?file_id=..."`.
		fileID = strings.TrimSpace(c.Query("file_id"))
	}
	if fileID == "" {
		fail(c, http.StatusBadRequest, codeBadRequest, "file_id is required",
			"upload first (POST /v1/files) and share the id it returns")
		return
	}

	ctx := c.Request.Context()
	f, err := s.store.FileByID(ctx, fileID)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such file", "check GET /v1/files for ids")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the file record")
		return
	}
	if f.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "that file has expired; upload it again")
		return
	}

	ttlRaw := strings.TrimSpace(req.TTL)
	if ttlRaw == "" && req.TTLSeconds != nil {
		ttlRaw = strconv.FormatInt(*req.TTLSeconds, 10)
	}
	shareExpiry, err := s.resolveTTL(firstNonEmpty(ttlRaw, c.Query("ttl")))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	// A share can never outlive the file it points at, otherwise the link
	// answers 410 for no reason the recipient can understand.
	if f.ExpiresAt != nil && (shareExpiry == nil || *shareExpiry > *f.ExpiresAt) {
		v := *f.ExpiresAt
		shareExpiry = &v
	}

	max := req.MaxDownloads
	if max == nil {
		if raw := strings.TrimSpace(c.Query("max_downloads")); raw != "" {
			n, convErr := strconv.ParseInt(raw, 10, 64)
			if convErr != nil {
				fail(c, http.StatusBadRequest, codeBadRequest, "max_downloads must be an integer")
				return
			}
			max = &n
		}
	}
	if max != nil && *max <= 0 {
		fail(c, http.StatusBadRequest, codeBadRequest, "max_downloads must be positive",
			"omit the field entirely for an unlimited link")
		return
	}

	sh := &store.Share{
		ID:           id.Share(),
		FileID:       f.ID,
		Label:        strings.TrimSpace(req.Label),
		Owner:        ownerID(c),
		CreatedAt:    s.now().Unix(),
		ExpiresAt:    shareExpiry,
		MaxDownloads: max,
	}
	if err := s.store.InsertShare(ctx, sh); err != nil {
		failInternal(c, s.log, err, "cannot create the share link")
		return
	}

	c.Header("Location", s.absURL(c, "/s/"+sh.ID))
	c.JSON(http.StatusCreated, gin.H{"share": s.shareDTO(c, sh, f)})
}

func (s *Server) listShares(c *gin.Context) {
	limit, err := strconv.Atoi(firstNonEmpty(c.Query("limit"), "0"))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "limit must be an integer")
		return
	}
	shares, err := s.store.ListShares(c.Request.Context(), c.Query("file_id"), clampLimit(limit, 100, 1000))
	if err != nil {
		failInternal(c, s.log, err, "cannot list share links")
		return
	}
	out := make([]shareDTO, 0, len(shares))
	for _, sh := range shares {
		out = append(out, s.shareDTO(c, sh, nil))
	}
	c.JSON(http.StatusOK, gin.H{"shares": out, "count": len(out)})
}

func (s *Server) shareMeta(c *gin.Context) {
	sh, err := s.store.ShareByID(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such share link")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the share link")
		return
	}
	var f *store.File
	if got, err := s.store.FileByID(c.Request.Context(), sh.FileID); err == nil {
		f = got
	}
	c.JSON(http.StatusOK, gin.H{"share": s.shareDTO(c, sh, f)})
}

func (s *Server) deleteShare(c *gin.Context) {
	err := s.store.DeleteShare(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such share link")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot revoke the share link")
		return
	}
	c.JSON(http.StatusOK, gin.H{"deleted": true, "share_id": c.Param("id")})
}

// ---------------------------------------------------------------- public

// publicDownload serves /s/<token>. No credential is required; the token is
// the credential.
func (s *Server) publicDownload(c *gin.Context) {
	ctx := c.Request.Context()
	token := strings.TrimSpace(c.Param("token"))

	sh, f, ok := s.resolveShare(c, token)
	if !ok {
		return
	}

	if c.Request.Method == http.MethodGet {
		granted, err := s.store.ReserveShareDownload(ctx, sh.ID)
		if err != nil {
			failInternal(c, s.log, err, "cannot account for the download")
			return
		}
		if !granted {
			fail(c, http.StatusForbidden, codeForbidden,
				"this share link has reached its download limit",
				"ask the owner for a new link")
			return
		}
	}

	c.Header("X-Share-Id", sh.ID)
	if sh.MaxDownloads != nil {
		c.Header("X-Share-Downloads", strconv.FormatInt(sh.Downloads+1, 10))
		c.Header("X-Share-Max-Downloads", strconv.FormatInt(*sh.MaxDownloads, 10))
	}
	s.serveBlob(c, contentSpec{
		Name:        f.Name,
		ContentType: f.ContentType,
		Size:        f.Size,
		ModTime:     time.Unix(f.CreatedAt, 0),
		Sha256:      f.Sha256,
		FileID:      f.ID,
	})
}

// publicShareInfo lets a recipient check what is behind a link — name, size,
// digest, remaining downloads — without spending a download credit.
func (s *Server) publicShareInfo(c *gin.Context) {
	sh, f, ok := s.resolveShare(c, strings.TrimSpace(c.Param("token")))
	if !ok {
		return
	}
	remaining := int64(-1)
	if sh.MaxDownloads != nil {
		remaining = *sh.MaxDownloads - sh.Downloads
		if remaining < 0 {
			remaining = 0
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"share": gin.H{
			"id":             sh.ID,
			"label":          sh.Label,
			"created_at":     timeStr(sh.CreatedAt),
			"expires_at":     timePtrStr(sh.ExpiresAt),
			"downloads":      sh.Downloads,
			"max_downloads":  sh.MaxDownloads,
			"downloads_left": remaining,
			"url":            s.absURL(c, "/s/"+sh.ID),
		},
		"file": gin.H{
			"name":         f.Name,
			"size":         f.Size,
			"sha256":       f.Sha256,
			"content_type": f.ContentType,
			"created_at":   timeStr(f.CreatedAt),
		},
	})
}

// resolveShare validates a capability token and writes the failure response
// itself. It returns ok=false when the caller should stop.
func (s *Server) resolveShare(c *gin.Context, token string) (*store.Share, *store.File, bool) {
	if token == "" {
		fail(c, http.StatusNotFound, codeNotFound, "no such share link")
		return nil, nil, false
	}
	ctx := c.Request.Context()

	sh, err := s.store.ShareByID(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such share link",
			"share links are not guessable; ask the sender to re-send the exact URL")
		return nil, nil, false
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the share link")
		return nil, nil, false
	}
	if sh.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "this share link has expired",
			"ask the owner for a new link")
		return nil, nil, false
	}

	f, err := s.store.FileByID(ctx, sh.FileID)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusGone, codeGone, "the shared file is no longer available")
		return nil, nil, false
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the shared file")
		return nil, nil, false
	}
	if f.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "the shared file has expired",
			"ask the owner for a new link")
		return nil, nil, false
	}
	return sh, f, true
}
