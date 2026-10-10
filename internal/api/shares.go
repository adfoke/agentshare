package api

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/id"
	"github.com/adfoke/agentshare/internal/store"
)

// A share link is a capability URL: whoever holds /s/<token> may reach the
// files it covers, with no credential of their own. That is the whole mechanism
// by which someone who is not the owner ever gets bytes out of agentshare.
//
// A share covers one or more files. One file is the common case and its URL
// serves that file's bytes; more than one makes the URL an index page listing
// them, which is the shape a "here is a directory of things" hand-off needs.

const (
	// maxShareItems bounds a single share. Each member costs a row and a line
	// on the index page, and a share is meant to be a hand-off, not a mirror of
	// the whole store.
	maxShareItems = 500
)

type createShareRequest struct {
	// FileID is the single-file spelling, kept because it is what every
	// existing client sends and what the quickstart documents.
	FileID string `json:"file_id"`
	// FileIDs covers several files at once. Order is preserved: it is the order
	// the index page lists them in.
	FileIDs      []string `json:"file_ids"`
	Label        string   `json:"label"`
	TTL          string   `json:"ttl"`         // "24h", "7d", ...
	TTLSeconds   *int64   `json:"ttl_seconds"` // alternative spelling
	MaxDownloads *int64   `json:"max_downloads"`
}

// requestedFileIDs merges the two spellings into one ordered, deduplicated list.
func requestedFileIDs(req *createShareRequest, c *gin.Context) []string {
	raw := make([]string, 0, len(req.FileIDs)+1)
	if id := strings.TrimSpace(req.FileID); id != "" {
		raw = append(raw, id)
	}
	raw = append(raw, req.FileIDs...)
	// ?file_id= and ?file_ids=a,b are accepted too, for the callers that find a
	// query string easier to build than a body.
	if q := strings.TrimSpace(c.Query("file_id")); q != "" && len(raw) == 0 {
		raw = append(raw, q)
	}
	if qs := c.QueryArray("file_ids"); len(qs) > 0 && len(raw) == 0 {
		for _, q := range qs {
			for _, part := range strings.Split(q, ",") {
				raw = append(raw, part)
			}
		}
	}

	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, id := range raw {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out
}

func (s *Server) createShare(c *gin.Context) {
	req, ok := bindJSON[createShareRequest](c, maxSmallBody,
		`expected {"file_ids": ["f_...", "f_..."], "ttl": "7d", "max_downloads": 3}`)
	if !ok {
		return
	}

	ids := requestedFileIDs(req, c)
	if len(ids) == 0 {
		fail(c, http.StatusBadRequest, codeBadRequest, "no files named",
			"pass file_id for one file, or file_ids for several: "+
				`{"file_ids": ["f_...", "f_..."]}`)
		return
	}
	if len(ids) > maxShareItems {
		fail(c, http.StatusBadRequest, codeBadRequest,
			fmt.Sprintf("a share covers at most %d files, %d were named", maxShareItems, len(ids)),
			"split it into several shares")
		return
	}

	ctx := c.Request.Context()
	now := s.now()

	// Every member is checked before anything is written: a share that silently
	// drops the files it could not find is worse than one that refuses, because
	// the sender never learns the recipient is missing something.
	files := make([]*store.File, 0, len(ids))
	for _, id := range ids {
		f, err := s.store.FileByID(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			fail(c, http.StatusNotFound, codeNotFound, "no such file: "+id,
				"check GET /v1/files for ids; nothing was shared")
			return
		}
		if err != nil {
			failInternal(c, s.log, err, "cannot read the file record")
			return
		}
		if f.Expired(now) {
			fail(c, http.StatusGone, codeGone, "that file has expired: "+id,
				"upload it again, or leave it out of the share")
			return
		}
		files = append(files, f)
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
	// A share can never outlive the files it points at, otherwise the link
	// answers "gone" for no reason the recipient can understand. With several
	// files the earliest expiry is the one that matters.
	for _, f := range files {
		if f.ExpiresAt == nil {
			continue
		}
		if shareExpiry == nil || *shareExpiry > *f.ExpiresAt {
			v := *f.ExpiresAt
			shareExpiry = &v
		}
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

	// One named file is the single-file shape; more than one makes the link an
	// index page. Deciding it here, from what the caller asked for, is what
	// keeps the link's shape stable for as long as it lives.
	kind := store.ShareKindFile
	if len(ids) > 1 {
		kind = store.ShareKindSet
	}
	sh := &store.Share{
		ID:           id.Share(),
		Label:        strings.TrimSpace(req.Label),
		Owner:        ownerID(c),
		CreatedAt:    now.Unix(),
		ExpiresAt:    shareExpiry,
		MaxDownloads: max,
		Kind:         kind,
	}
	if err := s.store.InsertShare(ctx, sh, ids); err != nil {
		failInternal(c, s.log, err, "cannot create the share link")
		return
	}

	c.Header("Location", s.absURL(c, "/s/"+sh.ID))
	c.JSON(http.StatusCreated, gin.H{"share": s.shareDTO(c, sh, files...)})
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
		out = append(out, s.shareDTO(c, sh))
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
	files, err := s.store.ShareItems(c.Request.Context(), sh.ID)
	if err != nil {
		failInternal(c, s.log, err, "cannot read the share's files")
		return
	}
	c.JSON(http.StatusOK, gin.H{"share": s.shareDTO(c, sh, files...)})
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

// publicShare is what /s/<token> serves.
//
// Three shapes, decided by what the share covers:
//
//	one file      the file's bytes, so an existing single-file link is unchanged
//	several files an index page listing them
//	?list=1       the index page regardless, for a share of one
//
// The index is also what /s/<token>/ always serves, which is the URL a browser
// is left with after any relative navigation.
func (s *Server) publicShare(c *gin.Context) {
	sh, files, hidden, ok := s.resolveShare(c, strings.TrimSpace(c.Param("token")))
	if !ok {
		return
	}

	// The share's own kind decides, not the number of files still alive.
	// Deriving it from the live count would turn a listing into a download the
	// moment one member expired — same URL, same token, different thing, with
	// no way for the recipient to tell.
	forceList := truthy(c.Query("list")) || strings.HasSuffix(c.Request.URL.Path, "/")
	if sh.ServesBytes() && len(files) == 1 && !forceList {
		s.serveShareFile(c, sh, files[0])
		return
	}
	s.renderShareIndex(c, sh, files, hidden)
}

// publicShareFile serves one file out of a share. The file has to be a member:
// the whole point of the token is that it scopes what may be reached through it.
func (s *Server) publicShareFile(c *gin.Context) {
	sh, _, _, ok := s.resolveShare(c, strings.TrimSpace(c.Param("token")))
	if !ok {
		return
	}

	fileID := strings.TrimSpace(c.Param("fileID"))
	member, err := s.store.ShareCovers(c.Request.Context(), sh.ID, fileID)
	if err != nil {
		failInternal(c, s.log, err, "cannot check the share's membership")
		return
	}
	if !member {
		fail(c, http.StatusNotFound, codeNotFound, "that file is not part of this share",
			"the index page lists what is: "+shareListingURL(s, c, sh))
		return
	}

	f, err := s.store.FileByID(c.Request.Context(), fileID)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusGone, codeGone, "this file is no longer available")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the file record")
		return
	}
	if f.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "this file has expired",
			"ask the owner for a new link")
		return
	}
	s.serveShareFile(c, sh, f)
}

// serveShareFile accounts for one download and streams the bytes.
func (s *Server) serveShareFile(c *gin.Context, sh *store.Share, f *store.File) {
	if c.Request.Method == http.MethodGet {
		granted, err := s.store.ReserveShareDownload(c.Request.Context(), sh.ID)
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

// publicShareInfo lets a recipient check what is behind a link — the file list,
// sizes, digests, remaining downloads — without spending a download credit.
func (s *Server) publicShareInfo(c *gin.Context) {
	sh, files, hidden, ok := s.resolveShare(c, strings.TrimSpace(c.Param("token")))
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

	items := make([]gin.H, 0, len(files))
	for _, f := range files {
		items = append(items, gin.H{
			"id":           f.ID,
			"name":         f.Name,
			"size":         f.Size,
			"sha256":       f.Sha256,
			"content_type": f.ContentType,
			"created_at":   timeStr(f.CreatedAt),
			"url":          s.absURL(c, "/s/"+sh.ID+"/f/"+f.ID),
		})
	}

	body := gin.H{
		"share": gin.H{
			"id":             sh.ID,
			"label":          sh.Label,
			"created_at":     timeStr(sh.CreatedAt),
			"expires_at":     timePtrStr(sh.ExpiresAt),
			"downloads":      sh.Downloads,
			"max_downloads":  sh.MaxDownloads,
			"downloads_left": remaining,
			"file_count":     len(files),
			"url":            s.absURL(c, "/s/"+sh.ID),
			"index_url":      shareListingURL(s, c, sh),
		},
		"files": items,
	}
	if hidden > 0 {
		body["hidden_expired"] = hidden
	}
	// A single-file share answers with the same keys it always did, so a client
	// written against the old shape keeps working.
	if len(files) == 1 {
		f := files[0]
		body["file"] = gin.H{
			"name":         f.Name,
			"size":         f.Size,
			"sha256":       f.Sha256,
			"content_type": f.ContentType,
			"created_at":   timeStr(f.CreatedAt),
		}
	}
	c.JSON(http.StatusOK, body)
}

// resolveShare validates a capability token and writes the failure response
// itself. It returns ok=false when the caller should stop.
//
// The live member files come back with it, and `hidden` counts members that
// have expired — a listing is allowed to show fewer files than the share was
// created with, as long as it says so. A share whose every member has gone is
// reported as gone rather than as an empty listing, because the recipient
// cannot do anything about an empty page and "this link no longer has anything
// in it" is the useful sentence.
func (s *Server) resolveShare(c *gin.Context, token string) (sh *store.Share, files []*store.File, hidden int, ok bool) {
	if token == "" {
		fail(c, http.StatusNotFound, codeNotFound, "no such share link")
		return nil, nil, 0, false
	}
	ctx := c.Request.Context()

	sh, err := s.store.ShareByID(ctx, token)
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such share link",
			"share links are not guessable; ask the sender to re-send the exact URL")
		return nil, nil, 0, false
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the share link")
		return nil, nil, 0, false
	}
	if sh.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "this share link has expired",
			"ask the owner for a new link")
		return nil, nil, 0, false
	}

	files, err = s.store.ShareItems(ctx, sh.ID)
	if err != nil {
		failInternal(c, s.log, err, "cannot read the share's files")
		return nil, nil, 0, false
	}
	if len(files) == 0 {
		// It may be empty because everything expired, or because the files were
		// deleted outright. The recipient gets the same answer either way: the
		// link no longer resolves to anything.
		fail(c, http.StatusGone, codeGone, "this share link no longer has any files",
			"the files were deleted or expired; ask the owner for a new link")
		return nil, nil, 0, false
	}

	// Members that exist but have expired are counted, not listed.
	hidden = sh.ItemCount - len(files)
	if hidden < 0 {
		hidden = 0
	}
	return sh, files, hidden, true
}
