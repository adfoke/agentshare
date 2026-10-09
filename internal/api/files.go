package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/adfoke/agentshare/internal/blob"
	"github.com/adfoke/agentshare/internal/config"
	"github.com/adfoke/agentshare/internal/id"
	"github.com/adfoke/agentshare/internal/store"
)

// ---------------------------------------------------------------- upload

// uploadFile stores the request body as a new file.
//
// POST /v1/files          multi-purpose upload; the body is the content
// PUT  /v1/files          same, for clients that prefer PUT
// PUT  /v1/content/<sha>  content-addressed upload; the digest is checked
//
// Metadata rides in headers (X-Filename, X-Tag, X-Ttl, X-Meta, X-Content-Type)
// with query parameters as an equally valid alternative, because some agent
// frameworks are far happier appending query strings than setting headers.
func (s *Server) uploadFile(c *gin.Context) {
	expected := strings.ToLower(firstNonEmpty(c.Param("sha256"), c.GetHeader("X-Sha256")))
	byHashOnly := c.Param("sha256") != ""
	if expected != "" && !isHex64(expected) {
		fail(c, http.StatusBadRequest, codeBadRequest,
			"sha256 must be 64 hexadecimal characters",
			"the digest is of the raw bytes, lowercase hex")
		return
	}

	tags := parseTags([]string{c.GetHeader("X-Tag")}, c.QueryArray("tag"))

	meta, ok := parseMeta(c)
	if !ok {
		return // parseMeta already wrote the response
	}

	expires, err := s.resolveTTL(firstNonEmpty(c.GetHeader("X-Ttl"), c.Query("ttl")))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, err.Error(),
			"omit the ttl to use the server default, or pass one within the cap")
		return
	}

	body, partName, err := s.uploadBody(c)
	if err != nil {
		return // response already written
	}

	// Name precedence: an explicit header/query beats the multipart part's own
	// filename, which in turn beats the fallback.
	name := sanitizeName(firstNonEmpty(
		c.GetHeader("X-Filename"), c.GetHeader("X-File-Name"),
		c.Query("name"), c.GetHeader("X-Name")))
	if name == "" {
		name = sanitizeName(partName)
	}
	switch {
	case name != "":
	case byHashOnly:
		name = expected[:16] // a digest upload with no name gets a stable one
	default:
		name = "file.bin"
	}

	tmp, err := s.blobs.Temp()
	if err != nil {
		failInternal(c, s.log, err, "cannot stage the upload")
		return
	}
	tmpPath := tmp.Name()
	keepTemp := false
	defer func() {
		_ = tmp.Close()
		if !keepTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	hasher := sha256.New()
	written, err := io.Copy(io.MultiWriter(tmp, hasher), body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(c, http.StatusRequestEntityTooLarge, codeTooLarge,
				"upload exceeds the server limit of "+strconv.FormatInt(tooLarge.Limit, 10)+" bytes",
				"split the upload, or ask the operator to raise --max-upload")
			return
		}
		// A client that hung up mid-body lands here too; there is nobody left
		// to read the response, but the temp file must still go.
		s.log.Warn("upload aborted", "err", err.Error(), "request_id", requestID(c))
		fail(c, http.StatusBadRequest, codeBadRequest, "could not read the request body: "+err.Error())
		return
	}
	if err := tmp.Sync(); err != nil {
		failInternal(c, s.log, err, "cannot flush the upload to disk")
		return
	}

	// Sniff while the staged file is still where it is: Commit renames it into
	// the blob store, after which there is nothing left to look at.
	contentType := s.contentTypeFor(c, tmpPath)

	sum := hex.EncodeToString(hasher.Sum(nil))
	if expected != "" && expected != sum {
		fail(c, http.StatusBadRequest, codeChecksum,
			"body digest is "+sum+", not the "+expected+" that was declared",
			"resend the exact bytes, or drop the declared digest to accept whatever arrives")
		return
	}

	existed, err := s.blobs.Commit(tmpPath, sum)
	if err != nil {
		failInternal(c, s.log, err, "cannot commit the upload to the blob store")
		return
	}
	keepTemp = true // Commit consumed or removed the temp file

	ctx := c.Request.Context()
	var prior *store.File
	if byHashOnly {
		prior, err = s.store.FileBySha(ctx, sum)
	} else {
		prior, err = s.store.FileByShaAndName(ctx, sum, name)
	}
	switch {
	case err == nil && prior != nil:
		// Identical bytes under the same name are the same file. Reporting the
		// existing record is what makes a retried upload harmless.
		s.finishUpload(c, prior, true, existed)
		return
	case err != nil && !errors.Is(err, store.ErrNotFound):
		failInternal(c, s.log, err, "cannot look up existing metadata")
		return
	}

	rec := &store.File{
		ID:          id.File(),
		Sha256:      sum,
		Size:        written,
		Name:        name,
		ContentType: contentType,
		Tags:        tags,
		Meta:        meta,
		Owner:       ownerID(c),
		CreatedAt:   s.now().Unix(),
		ExpiresAt:   expires,
	}
	if err := s.store.InsertFile(ctx, rec); err != nil {
		failInternal(c, s.log, err, "cannot record the upload")
		return
	}
	s.finishUpload(c, rec, false, existed)
}

// uploadBody selects the byte stream out of a raw or multipart request, and
// applies the size cap. The second result is the filename carried by a
// multipart part, if any.
func (s *Server) uploadBody(c *gin.Context) (io.Reader, string, error) {
	raw := c.Request.Body
	if s.cfg.MaxUpload > 0 {
		raw = http.MaxBytesReader(c.Writer, c.Request.Body, s.cfg.MaxUpload)
		c.Request.Body = raw
	}

	ct := strings.ToLower(c.ContentType())
	if !strings.HasPrefix(ct, "multipart/form-data") {
		return raw, "", nil
	}

	mr, err := c.Request.MultipartReader()
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "malformed multipart body", err.Error())
		return nil, "", err
	}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			fail(c, http.StatusBadRequest, codeBadRequest,
				"multipart body contained no file part",
				"send one file part, or post the raw bytes with `curl -T`")
			return nil, "", err
		}
		if err != nil {
			fail(c, http.StatusBadRequest, codeBadRequest, "malformed multipart body", err.Error())
			return nil, "", err
		}
		if part.FileName() == "" && part.FormName() != "file" {
			continue // a text field, not the payload
		}
		return part, part.FileName(), nil
	}
}

// contentTypeFor prefers what the client declared, and only falls back to
// sniffing when the client said nothing useful. Two of those useless answers
// are the common ones: curl sends octet-stream for anything it does not
// recognise, and a multipart upload's own content type describes the envelope,
// not the file inside it.
func (s *Server) contentTypeFor(c *gin.Context, path string) string {
	declared := firstNonEmpty(c.GetHeader("X-Content-Type"), c.ContentType())
	switch strings.ToLower(strings.TrimSpace(strings.Split(declared, ";")[0])) {
	case "", "application/octet-stream", "multipart/form-data":
		// Nothing worth keeping: fall through and sniff the bytes.
	default:
		return declared
	}
	f, err := os.Open(path)
	if err != nil {
		return "application/octet-stream"
	}
	defer f.Close()
	buf := make([]byte, 512)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return "application/octet-stream"
	}
	return strings.TrimSpace(strings.Split(http.DetectContentType(buf[:n]), ";")[0])
}

func (s *Server) finishUpload(c *gin.Context, f *store.File, deduped, blobExisted bool) {
	status := http.StatusCreated
	if deduped {
		status = http.StatusOK
	}
	c.Header("Location", s.absURL(c, "/v1/files/"+f.ID))
	c.Header("X-File-Id", f.ID)
	c.Header("X-Checksum-Sha256", f.Sha256)
	if f.ExpiresAt != nil {
		c.Header("X-Expires-At", timeStr(*f.ExpiresAt))
	}
	c.JSON(status, gin.H{
		"file":         s.fileDTO(c, f),
		"deduplicated": deduped,
		"blob_existed": blobExisted,
	})
}

// uploadText stores a short UTF-8 string without making the caller build a
// request body. Handy for logs, snippets and one-line reports.
func (s *Server) uploadText(c *gin.Context) {
	var req struct {
		Name        string   `json:"name"`
		Content     string   `json:"content"`
		Tags        []string `json:"tags"`
		TTLSeconds  *int64   `json:"ttl_seconds"`
		ContentType string   `json:"content_type"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "invalid JSON body: "+err.Error(),
			`expected {"name": "...", "content": "..."}`)
		return
	}
	if req.Content == "" {
		fail(c, http.StatusBadRequest, codeBadRequest, "content must not be empty")
		return
	}
	const maxText = 8 << 20
	if len(req.Content) > maxText {
		fail(c, http.StatusRequestEntityTooLarge, codeTooLarge,
			"text upload is limited to 8 MiB",
			"use POST /v1/files with the content as the request body for anything larger")
		return
	}

	name := sanitizeName(req.Name)
	if name == "" {
		name = "text.txt"
	}
	expires, err := s.resolveTTL(ttlString(req.TTLSeconds))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, err.Error())
		return
	}
	ct := req.ContentType
	if ct == "" {
		ct = "text/plain; charset=utf-8"
	}

	data := []byte(req.Content)
	sum := sha256.Sum256(data)

	tmp, err := s.blobs.Temp()
	if err != nil {
		failInternal(c, s.log, err, "cannot stage the upload")
		return
	}
	tmpPath := tmp.Name()
	keep := false
	defer func() {
		_ = tmp.Close()
		if !keep {
			_ = os.Remove(tmpPath)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		failInternal(c, s.log, err, "cannot stage the upload")
		return
	}
	digest := hex.EncodeToString(sum[:])
	if _, err := s.blobs.Commit(tmpPath, digest); err != nil {
		failInternal(c, s.log, err, "cannot commit the upload")
		return
	}
	keep = true

	ctx := c.Request.Context()
	if prior, err := s.store.FileByShaAndName(ctx, digest, name); err == nil && prior != nil {
		s.finishUpload(c, prior, true, true)
		return
	}
	rec := &store.File{
		ID:          id.File(),
		Sha256:      digest,
		Size:        int64(len(data)),
		Name:        name,
		ContentType: ct,
		Tags:        parseTags(req.Tags),
		Meta:        "{}",
		Owner:       ownerID(c),
		CreatedAt:   s.now().Unix(),
		ExpiresAt:   expires,
	}
	if err := s.store.InsertFile(ctx, rec); err != nil {
		failInternal(c, s.log, err, "cannot record the upload")
		return
	}
	s.finishUpload(c, rec, false, true)
}

func ttlString(seconds *int64) string {
	if seconds == nil {
		return ""
	}
	return strconv.FormatInt(*seconds, 10)
}

// ---------------------------------------------------------------- read

// fileMeta returns the JSON description of one file.
func (s *Server) fileMeta(c *gin.Context) {
	f, err := s.store.FileByID(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such file",
			"list what is available with GET /v1/files")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the file record")
		return
	}
	if f.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "this file has expired",
			"the record is kept until the janitor sweeps it; ask the owner to re-upload")
		return
	}
	c.JSON(http.StatusOK, gin.H{"file": s.fileDTO(c, f)})
}

// fileContent streams the bytes of a file addressed by its id.
func (s *Server) fileContent(c *gin.Context) {
	f, err := s.store.FileByID(c.Request.Context(), c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such file",
			"list what is available with GET /v1/files")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot read the file record")
		return
	}
	if f.Expired(s.now()) {
		fail(c, http.StatusGone, codeGone, "this file has expired")
		return
	}
	if c.Request.Method == http.MethodGet {
		if err := s.store.BumpFileDownloads(c.Request.Context(), f.ID); err != nil {
			s.log.Debug("cannot bump download counter", "err", err.Error(), "file_id", f.ID)
		}
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

// getContent serves bytes by digest. No metadata row is required: if the bytes
// are on disk, they are downloadable.
func (s *Server) getContent(c *gin.Context) {
	sha := strings.ToLower(strings.TrimSpace(c.Param("sha256")))
	if !isHex64(sha) {
		fail(c, http.StatusBadRequest, codeBadRequest, "sha256 must be 64 hexadecimal characters")
		return
	}
	spec := contentSpec{Sha256: sha, Name: sha, ContentType: "", ModTime: time.Unix(0, 0)}
	if f, err := s.store.FileBySha(c.Request.Context(), sha); err == nil && f != nil {
		spec.Name = f.Name
		spec.ContentType = f.ContentType
		spec.ModTime = time.Unix(f.CreatedAt, 0)
		spec.FileID = f.ID
		if c.Request.Method == http.MethodGet {
			if err := s.store.BumpFileDownloads(c.Request.Context(), f.ID); err != nil {
				s.log.Debug("cannot bump download counter", "err", err.Error(), "file_id", f.ID)
			}
		}
	}
	s.serveBlob(c, spec)
}

// contentMeta answers "do you have these bytes?" without transferring them.
func (s *Server) contentMeta(c *gin.Context) {
	sha := strings.ToLower(strings.TrimSpace(c.Param("sha256")))
	if !isHex64(sha) {
		fail(c, http.StatusBadRequest, codeBadRequest, "sha256 must be 64 hexadecimal characters")
		return
	}
	ctx := c.Request.Context()
	if f, err := s.store.FileBySha(ctx, sha); err == nil && f != nil {
		c.JSON(http.StatusOK, gin.H{"exists": true, "file": s.fileDTO(c, f)})
		return
	}
	size, err := s.blobs.Stat(sha)
	if errors.Is(err, blob.ErrNotFound) {
		c.JSON(http.StatusOK, gin.H{"exists": false, "sha256": sha})
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot stat the blob")
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"exists": true,
		"sha256": sha,
		"size":   size,
		"urls":   gin.H{"content": s.absURL(c, "/v1/content/"+sha)},
	})
}

// contentSpec is what the byte-serving path needs to know.
type contentSpec struct {
	Name        string
	ContentType string
	Size        int64
	ModTime     time.Time
	Sha256      string
	FileID      string
}

// serveBlob is the single place that writes file bytes to a client. Every
// download route funnels through it so range handling, caching headers and
// content-disposition stay identical everywhere.
func (s *Server) serveBlob(c *gin.Context, spec contentSpec) {
	fh, err := s.blobs.Open(spec.Sha256)
	if errors.Is(err, blob.ErrNotFound) {
		if spec.FileID != "" {
			// A record claims these bytes exist, so their absence is a fault
			// state, not a wrong URL.
			fail(c, http.StatusGone, codeGone,
				"the content for this record is no longer on disk",
				"the owner must upload it again")
			return
		}
		fail(c, http.StatusNotFound, codeNotFound, "no content stored under that digest",
			"check the digest, or upload with PUT /v1/content/<sha256>")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot open the stored content")
		return
	}
	defer fh.Close()

	ct := spec.ContentType
	if ct == "" {
		head := make([]byte, 512)
		n, _ := io.ReadFull(fh, head)
		if n > 0 {
			ct = strings.TrimSpace(strings.Split(http.DetectContentType(head[:n]), ";")[0])
		}
		if _, err := fh.Seek(0, io.SeekStart); err != nil {
			failInternal(c, s.log, err, "cannot rewind the stored content")
			return
		}
	}
	if ct == "" {
		ct = "application/octet-stream"
	}

	etag := `"` + spec.Sha256 + `"`
	h := c.Writer.Header()
	h.Set("Content-Type", ct)
	h.Set("ETag", etag)
	h.Set("X-Checksum-Sha256", spec.Sha256)
	h.Set("Cache-Control", "private, no-store")
	h.Set("Accept-Ranges", "bytes")
	if spec.FileID != "" {
		h.Set("X-File-Id", spec.FileID)
	}
	if spec.Size > 0 {
		h.Set("X-File-Size", strconv.FormatInt(spec.Size, 10))
	}
	// Attachment by default: a file service should hand back a file, not render
	// it. `?inline=1` (alias `?view=1`) asks for the other behaviour.
	inline := truthy(c.Query("inline")) || truthy(c.Query("view"))
	h.Set("Content-Disposition", contentDisposition(spec.Name, inline))

	// ServeContent takes it from here: it evaluates If-None-Match against the
	// ETag set above, plus If-Match, If-Modified-Since, If-Range and Range.
	http.ServeContent(c.Writer, c.Request, spec.Name, spec.ModTime, fh)
}

// ---------------------------------------------------------------- listing

func (s *Server) listFiles(c *gin.Context) {
	ctx := c.Request.Context()

	limit, err := strconv.Atoi(firstNonEmpty(c.Query("limit"), "0"))
	if err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "limit must be an integer")
		return
	}

	filter := store.ListFilter{
		Query:   c.Query("q"),
		Tag:     c.Query("tag"),
		Owner:   c.Query("owner"),
		Include: truthy(c.Query("include_expired")),
		Order:   c.Query("order"),
		Limit:   clampLimit(limit, 100, 1000),
		Cursor:  c.Query("cursor"),
	}
	if filter.Order != "" && !strings.EqualFold(filter.Order, "asc") && !strings.EqualFold(filter.Order, "desc") {
		fail(c, http.StatusBadRequest, codeBadRequest, "order must be asc or desc")
		return
	}
	if filter.Since, err = parseTimeParam(c.Query("since")); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "since: "+err.Error())
		return
	}
	if filter.Until, err = parseTimeParam(c.Query("until")); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "until: "+err.Error())
		return
	}

	files, next, err := s.store.ListFiles(ctx, filter)
	if errors.Is(err, store.ErrBadCursor) {
		fail(c, http.StatusBadRequest, codeBadRequest, "invalid cursor",
			"cursors are opaque; use the next_cursor from a previous response verbatim")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot list files")
		return
	}

	out := make([]fileDTO, 0, len(files))
	for _, f := range files {
		out = append(out, s.fileDTO(c, f))
	}
	c.JSON(http.StatusOK, gin.H{
		"files":       out,
		"count":       len(out),
		"next_cursor": next,
		"has_more":    next != "",
	})
}

// ---------------------------------------------------------------- delete

func (s *Server) deleteFile(c *gin.Context) {
	ctx := c.Request.Context()
	res, err := s.store.DeleteFile(ctx, c.Param("id"))
	if errors.Is(err, store.ErrNotFound) {
		fail(c, http.StatusNotFound, codeNotFound, "no such file")
		return
	}
	if err != nil {
		failInternal(c, s.log, err, "cannot delete the file record")
		return
	}

	deleted := true
	if res.BlobFreed {
		if err := s.blobs.Remove(res.File.Sha256); err != nil {
			// The record is gone either way; say so, and let the janitor pick
			// the bytes up later rather than failing the request.
			s.log.Warn("cannot remove blob", "err", err.Error(), "sha256", res.File.Sha256)
			deleted = false
		}
	}
	c.JSON(http.StatusOK, gin.H{
		"deleted":     true,
		"file_id":     res.File.ID,
		"sha256":      res.File.Sha256,
		"blob_freed":  res.BlobFreed && deleted,
		"share_links": "any share links for this file were revoked with it",
	})
}

// ---------------------------------------------------------------- helpers

func ownerID(c *gin.Context) string {
	if id := identity(c); id != nil {
		return id.ID
	}
	return ""
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

func truthy(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "y", "on":
		return true
	}
	return false
}

// parseTimeParam accepts unix seconds or an RFC3339 timestamp, because both
// spellings show up in agent-generated URLs.
func parseTimeParam(v string) (*int64, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return nil, nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return &n, nil
	}
	// Accept durations relative to now as well: since=24h means "the last day".
	// The config parser is reused so 7d and 2w work here too.
	if d, err := config.ParseDuration(v); err == nil && d > 0 {
		n := time.Now().Add(-d).Unix()
		return &n, nil
	}
	t, err := time.Parse(time.RFC3339, v)
	if err != nil {
		return nil, errors.New("want unix seconds, RFC3339, or a window like 24h")
	}
	n := t.Unix()
	return &n, nil
}

func parseMeta(c *gin.Context) (string, bool) {
	raw := strings.TrimSpace(c.GetHeader("X-Meta"))
	if raw == "" {
		return "{}", true
	}
	if len(raw) > 4096 {
		fail(c, http.StatusBadRequest, codeBadRequest, "X-Meta is limited to 4096 bytes")
		return "", false
	}
	if !strings.HasPrefix(raw, "{") || !json.Valid([]byte(raw)) {
		fail(c, http.StatusBadRequest, codeBadRequest,
			"X-Meta must be a JSON object",
			`for example: X-Meta: {"job":"nightly","run":42}`)
		return "", false
	}
	var probe map[string]any
	if err := json.Unmarshal([]byte(raw), &probe); err != nil {
		fail(c, http.StatusBadRequest, codeBadRequest, "X-Meta must be a JSON object")
		return "", false
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, []byte(raw)); err != nil {
		return "{}", true
	}
	return buf.String(), true
}
