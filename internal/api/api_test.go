package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/adfoke/agentshare/internal/blob"
	"github.com/adfoke/agentshare/internal/config"
	"github.com/adfoke/agentshare/internal/store"
)

const testRootToken = "root-token-0123456789abcdef"

type testEnv struct {
	t      *testing.T
	srv    *httptest.Server
	router *Server // the real handler, so tests can inspect the routes it registered
	cfg    *config.Config
	store  *store.Store
	blobs  *blob.Store
	token  string
}

func newTestEnv(t *testing.T, tune func(*config.Config)) *testEnv {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{
		Listen:         "127.0.0.1:0",
		DataDir:        dir,
		MaxUpload:      1 << 20,
		CORSOrigin:     "*",
		TrustedProxies: []string{"127.0.0.1/32"},
		LogLevel:       "error",
		LogFormat:      "text",
	}
	if tune != nil {
		tune(cfg)
	}

	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	bl, err := blob.Open(dir)
	if err != nil {
		t.Fatalf("blob.Open: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	srv, err := New(cfg, st, bl, log, testRootToken)
	if err != nil {
		t.Fatalf("api.New: %v", err)
	}
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() {
		ts.Close()
		_ = st.Close()
	})
	return &testEnv{t: t, srv: ts, router: srv, cfg: cfg, store: st, blobs: bl, token: testRootToken}
}

type reqOpt func(*http.Request)

func withoutAuth() reqOpt {
	return func(r *http.Request) { r.Header.Del("Authorization") }
}

func withToken(tok string) reqOpt {
	return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
}

func withHeader(k, v string) reqOpt { return func(r *http.Request) { r.Header.Set(k, v) } }

func (e *testEnv) do(method, path string, body []byte, opts ...reqOpt) *http.Response {
	e.t.Helper()
	var rdr io.Reader
	if body != nil {
		rdr = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, e.srv.URL+path, rdr)
	if err != nil {
		e.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+e.token)
	for _, o := range opts {
		o(req)
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		e.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

func decode(t *testing.T, resp *http.Response, v any) {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("decoding %s: %v\nbody: %s", resp.Request.URL.Path, err, b)
	}
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func digestHex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blobCount is how many distinct blobs the store holds right now.
func (e *testEnv) blobCount() int {
	e.t.Helper()
	n := 0
	if err := e.blobs.Walk(func(string, int64, time.Time) error { n++; return nil }); err != nil {
		e.t.Fatal(err)
	}
	return n
}

// upload is the common case: raw body, a name, and the reply decoded.
func (e *testEnv) upload(content []byte, name string, extra ...reqOpt) (int, map[string]any) {
	e.t.Helper()
	opts := append([]reqOpt{withHeader("X-Filename", name)}, extra...)
	resp := e.do(http.MethodPost, "/v1/files", content, opts...)
	var out map[string]any
	decode(e.t, resp, &out)
	return resp.StatusCode, out
}

func fileOf(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	f, ok := out["file"].(map[string]any)
	if !ok {
		t.Fatalf("reply has no file object: %v", out)
	}
	return f
}

func failCode(t *testing.T, resp *http.Response) string {
	t.Helper()
	var out struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Hint    string `json:"hint"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	decode(t, resp, &out)
	if out.Error.Code == "" {
		t.Fatalf("error body has no code: %+v", out)
	}
	if out.Error.Hint == "" {
		t.Errorf("error %q carries no hint; every failure should say what to do next", out.Error.Code)
	}
	return out.Error.Code
}

// ---------------------------------------------------------------- discovery

func TestDescriptorIsSelfContained(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodGet, "/", nil, withoutAuth())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d", resp.StatusCode)
	}
	var doc map[string]any
	decode(t, resp, &doc)

	if doc["service"] != "agentshare" {
		t.Errorf("service = %v", doc["service"])
	}
	eps, err := json.Marshal(doc["endpoints"])
	if err != nil || !strings.Contains(string(eps), "/v1/files") {
		t.Errorf("descriptor does not list the file endpoints: %s", eps)
	}
	qs, err := json.Marshal(doc["quickstart"])
	if err != nil || !strings.Contains(string(qs), "/v1/files") {
		t.Errorf("descriptor has no usable quickstart: %s", qs)
	}
	if !strings.Contains(string(qs), e.srv.URL) {
		t.Errorf("quickstart commands should carry the caller's base url: %s", qs)
	}
}

// The descriptor and the OpenAPI document are two views of one API. If they
// disagree, one of them is lying to whoever reads it.
func TestOpenAPICoversEveryDeclaredEndpoint(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(openapiJSON, &doc); err != nil {
		t.Fatalf("openapi.json is not valid JSON: %v", err)
	}

	for _, ep := range endpoints {
		methods, ok := doc.Paths[ep.Path]
		if !ok {
			t.Errorf("openapi.json has no path %s (declared as %s %s)", ep.Path, ep.Method, ep.Path)
			continue
		}
		if _, ok := methods[strings.ToLower(ep.Method)]; !ok {
			t.Errorf("openapi.json lacks %s %s", ep.Method, ep.Path)
		}
	}

	declared := map[string]bool{}
	for _, ep := range endpoints {
		declared[ep.Method+" "+ep.Path] = true
	}
	httpMethods := map[string]bool{
		"get": true, "put": true, "post": true, "delete": true, "patch": true, "head": true, "options": true,
	}
	for path, methods := range doc.Paths {
		for method := range methods {
			if !httpMethods[method] {
				continue
			}
			// HEAD is documented in OpenAPI for completeness but is not worth
			// a line in the human-facing descriptor.
			if method == "head" {
				continue
			}
			if !declared[strings.ToUpper(method)+" "+path] {
				t.Errorf("openapi.json documents %s %s but the descriptor does not", strings.ToUpper(method), path)
			}
		}
	}
}

// The endpoint table in the descriptor, the OpenAPI document and the gin
// router are three separate hand-written lists of the same API. The first two
// are checked against each other above; this one checks both against the routes
// that are actually registered, which is the only one of the three the server
// can be wrong about without anyone noticing.
func TestRouterMatchesDeclaredEndpoints(t *testing.T) {
	e := newTestEnv(t, nil)

	// gin spells a parameter ":id"; the descriptor spells it "{id}".
	normalize := func(path string) string {
		parts := strings.Split(path, "/")
		for i, p := range parts {
			if strings.HasPrefix(p, ":") {
				parts[i] = "{" + strings.TrimPrefix(p, ":") + "}"
			}
		}
		return strings.Join(parts, "/")
	}

	registered := map[string]bool{}
	for _, r := range e.router.router.Routes() {
		registered[r.Method+" "+normalize(r.Path)] = true
	}
	if len(registered) == 0 {
		t.Fatal("the router registered no routes at all")
	}

	declared := map[string]bool{}
	for _, ep := range endpoints {
		declared[ep.Method+" "+ep.Path] = true
	}

	// Routes that exist for something other than the API itself, and so have no
	// business in the endpoint table an agent reads.
	notAPI := map[string]bool{
		// Every GET is also allowed as a HEAD; the descriptor does not repeat
		// each one.
		"HEAD ": true,
		// Browsers and scanners ask for this unprompted. Answering 204 keeps
		// those requests out of the 404 log without inventing an endpoint.
		"GET /favicon.ico": true,
	}

	for key := range registered {
		switch {
		case notAPI[key]:
		case strings.HasPrefix(key, "HEAD "):
		case !declared[key]:
			t.Errorf("route %s is served but missing from the descriptor agents read", key)
		}
	}
	for key := range declared {
		if !registered[key] {
			t.Errorf("the descriptor advertises %s but no such route is registered", key)
		}
	}
}

// HEAD is defined as "GET without the body", and callers reach for it
// constantly: `curl -I`, uptime monitors, link checkers. A 405 there reads as
// "the service is down" to every one of them, which is how this gap survived
// until someone happened to use -I.
func TestHeadMirrorsGet(t *testing.T) {
	e := newTestEnv(t, nil)
	_, up := e.upload([]byte("head probe"), "head.txt")
	id := fileOf(t, up)["id"].(string)

	for _, tc := range []struct {
		path     string
		needsKey bool
	}{
		{"/", false},
		{"/healthz", false},
		{"/openapi.json", false},
		{"/llms.txt", false},
		{"/v1/whoami", true},
		{"/v1/files", true},
		{"/v1/files/" + id, true},
		{"/v1/files/" + id + "/content", true},
	} {
		var opts []reqOpt
		if !tc.needsKey {
			opts = append(opts, withoutAuth())
		}
		resp := e.do(http.MethodHead, tc.path, nil, opts...)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("HEAD %s = %d, want 200", tc.path, resp.StatusCode)
		}
		if body := readBody(t, resp); body != "" {
			t.Errorf("HEAD %s sent a body: %q", tc.path, body)
		}
	}

	// A method name is not a way around authentication.
	resp := e.do(http.MethodHead, "/v1/files", nil, withoutAuth())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("HEAD /v1/files without a token = %d, want 401", resp.StatusCode)
	}
	resp.Body.Close()

	// Nor is it a way to spend someone else's download budget: a HEAD on a
	// share link is a metadata check, not a download.
	created := struct {
		Share map[string]any `json:"share"`
	}{}
	decode(t, e.do(http.MethodPost, "/v1/shares",
		mustJSON(t, map[string]any{"file_id": id, "max_downloads": 1}),
		withHeader("Content-Type", "application/json")), &created)
	token := created.Share["id"].(string)

	for i := 0; i < 3; i++ {
		resp = e.do(http.MethodHead, "/s/"+token, nil, withoutAuth())
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("HEAD on a share link = %d, want 200", resp.StatusCode)
		}
		resp.Body.Close()
	}
	// The single credit must survive those, so a real download still works.
	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("HEAD requests spent the share's download budget: GET = %d, want 200", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUnknownRouteAndMethod(t *testing.T) {
	e := newTestEnv(t, nil)

	resp := e.do(http.MethodGet, "/v1/nope", nil, withoutAuth())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown route = %d, want 404", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeNotFound {
		t.Fatalf("code = %s", code)
	}

	resp = e.do(http.MethodPatch, "/v1/files", nil)
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("PATCH /v1/files = %d, want 405", resp.StatusCode)
	}
	failCode(t, resp)
}

// ---------------------------------------------------------------- auth

func TestAuthAndScopes(t *testing.T) {
	e := newTestEnv(t, nil)

	resp := e.do(http.MethodGet, "/v1/files", nil, withoutAuth())
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token = %d, want 401", resp.StatusCode)
	}
	failCode(t, resp)

	resp = e.do(http.MethodGet, "/v1/files", nil, withToken("ash_definitely-not-a-real-token"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bad token = %d, want 401", resp.StatusCode)
	}
	failCode(t, resp)

	// X-API-Key is an accepted spelling.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/whoami", nil)
	req.Header.Set("X-API-Key", testRootToken)
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("X-API-Key did not authenticate: %d", resp.StatusCode)
	}
	var who struct {
		Identity Identity `json:"identity"`
	}
	decode(t, resp, &who)
	if who.Identity.Type != "root" || !who.Identity.has(ScopeAdmin) {
		t.Fatalf("root identity looks wrong: %+v", who.Identity)
	}
}

func TestScopeEnforcement(t *testing.T) {
	e := newTestEnv(t, nil)

	resp := e.do(http.MethodPost, "/v1/tokens", mustJSON(t, map[string]any{
		"name":   "uploader",
		"scopes": []string{"upload"},
	}), withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create token = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var created struct {
		Token tokenDTO `json:"token"`
	}
	decode(t, resp, &created)
	if created.Token.Secret == "" {
		t.Fatal("the creating call must return the secret exactly once")
	}

	// What the token may do.
	resp = e.do(http.MethodPost, "/v1/files", []byte("scoped"), withToken(created.Token.Secret),
		withHeader("X-Filename", "scoped.txt"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload with an upload-scoped token = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	// What it may not.
	resp = e.do(http.MethodGet, "/v1/files", nil, withToken(created.Token.Secret))
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("list with an upload-only token = %d, want 403", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeForbidden {
		t.Fatalf("code = %s", code)
	}

	// Revocation is immediate.
	resp = e.do(http.MethodDelete, "/v1/tokens/"+created.Token.ID, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke = %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = e.do(http.MethodPost, "/v1/files", []byte("x"), withToken(created.Token.Secret),
		withHeader("X-Filename", "x.txt"))
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token still works: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// A token cannot revoke itself out from under the caller.
	resp = e.do(http.MethodPost, "/v1/tokens", mustJSON(t, map[string]any{"scopes": []string{"admin"}}),
		withHeader("Content-Type", "application/json"))
	var admin struct {
		Token tokenDTO `json:"token"`
	}
	decode(t, resp, &admin)
	resp = e.do(http.MethodDelete, "/v1/tokens/"+admin.Token.ID, nil, withToken(admin.Token.Secret))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("self-revocation = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	if resp = e.do(http.MethodPost, "/v1/tokens", mustJSON(t, map[string]any{
		"scopes": []string{"teleport"},
	}), withHeader("Content-Type", "application/json")); resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("unknown scope accepted: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------- uploads

func TestUploadDownloadRoundTrip(t *testing.T) {
	e := newTestEnv(t, nil)
	content := []byte("hello agentshare\n")

	status, out := e.upload(content, "notes.txt")
	if status != http.StatusCreated {
		t.Fatalf("upload = %d, want 201: %v", status, out)
	}
	f := fileOf(t, out)
	if f["sha256"] != digestHex(content) {
		t.Fatalf("sha256 = %v, want %s", f["sha256"], digestHex(content))
	}
	if f["size"].(float64) != float64(len(content)) {
		t.Fatalf("size = %v", f["size"])
	}
	if out["deduplicated"] != false {
		t.Fatalf("a first upload is not a dedup: %v", out)
	}
	// curl sends no content type at all for `-T`; the stored type must come
	// from looking at the bytes. Sniffing after the staging file has been moved
	// into the blob store silently yields octet-stream for everything, which is
	// how this assertion earns its place.
	if got := f["content_type"]; got != "text/plain" {
		t.Fatalf("content_type = %v, want text/plain (sniffed)", got)
	}

	id := f["id"].(string)
	urls := f["urls"].(map[string]any)
	if !strings.HasPrefix(urls["content"].(string), e.srv.URL) {
		t.Fatalf("urls are not absolute: %v", urls)
	}

	resp := e.do(http.MethodGet, "/v1/files/"+id+"/content", nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("download = %d", resp.StatusCode)
	}
	if got := readBody(t, resp); got != string(content) {
		t.Fatalf("downloaded %q, want %q", got, content)
	}
	if etag := resp.Header.Get("ETag"); etag != `"`+digestHex(content)+`"` {
		t.Fatalf("ETag = %q", etag)
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.Contains(cd, "notes.txt") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	// Byte ranges: half the point of a file service.
	req, _ := http.NewRequest(http.MethodGet, e.srv.URL+"/v1/files/"+id+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("Range", "bytes=0-4")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("range request = %d, want 206", resp.StatusCode)
	}
	if got := readBody(t, resp); got != "hello" {
		t.Fatalf("range body = %q, want %q", got, "hello")
	}
	if cr := resp.Header.Get("Content-Range"); !strings.HasPrefix(cr, "bytes 0-4/") {
		t.Fatalf("Content-Range = %q", cr)
	}

	// A client that already has the bytes pays nothing for asking again.
	req, _ = http.NewRequest(http.MethodGet, e.srv.URL+"/v1/files/"+id+"/content", nil)
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("If-None-Match", `"`+digestHex(content)+`"`)
	resp, err = e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("If-None-Match = %d, want 304", resp.StatusCode)
	}
	resp.Body.Close()

	// HEAD must not transfer the body but must describe it.
	resp = e.do(http.MethodHead, "/v1/files/"+id+"/content", nil)
	if resp.StatusCode != http.StatusOK || resp.Header.Get("Content-Length") == "" {
		t.Fatalf("HEAD = %d, Content-Length = %q", resp.StatusCode, resp.Header.Get("Content-Length"))
	}
	resp.Body.Close()
}

func TestUploadIsIdempotentAndContentAddressed(t *testing.T) {
	e := newTestEnv(t, nil)
	content := []byte("same bytes")

	_, first := e.upload(content, "a.txt")
	id1 := fileOf(t, first)["id"].(string)

	status, second := e.upload(content, "a.txt")
	if status != http.StatusOK || second["deduplicated"] != true {
		t.Fatalf("retried upload = %d %v, want 200 deduplicated", status, second)
	}
	if got := fileOf(t, second)["id"].(string); got != id1 {
		t.Fatalf("retried upload made a new record: %s vs %s", got, id1)
	}

	// Same bytes, different name: a new record sharing one blob.
	status, third := e.upload(content, "b.txt")
	if status != http.StatusCreated {
		t.Fatalf("same bytes under a new name = %d, want 201", status)
	}
	if got := fileOf(t, third)["id"].(string); got == id1 {
		t.Fatal("a different name should be a different file")
	}
	if got := fileOf(t, third)["sha256"]; got != digestHex(content) {
		t.Fatalf("digest changed: %v", got)
	}
	// One blob on disk, two records pointing at it.
	if n := e.blobCount(); n != 1 {
		t.Fatalf("blob store holds %d blobs, want 1", n)
	}
}

func TestUploadRejectsBadChecksum(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodPost, "/v1/files", []byte("real content"),
		withHeader("X-Filename", "f.txt"),
		withHeader("X-Sha256", digestHex([]byte("different content"))))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("mismatched digest = %d, want 400", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeChecksum {
		t.Fatalf("code = %s, want %s", code, codeChecksum)
	}
	// The rejected bytes must not have been stored.
	if n := e.blobCount(); n != 0 {
		t.Fatalf("%d blobs were stored despite the checksum failure", n)
	}
}

func TestUploadTooLarge(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config) { c.MaxUpload = 64 })
	resp := e.do(http.MethodPost, "/v1/files", bytes.Repeat([]byte("x"), 500),
		withHeader("X-Filename", "big.bin"))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized upload = %d, want 413", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeTooLarge {
		t.Fatalf("code = %s", code)
	}
}

func TestMultipartUpload(t *testing.T) {
	e := newTestEnv(t, nil)

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormFile("file", "from-form.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("multipart body")); err != nil {
		t.Fatal(err)
	}
	_ = mw.WriteField("note", "ignored")
	mw.Close()

	resp := e.do(http.MethodPost, "/v1/files", buf.Bytes(),
		withHeader("Content-Type", mw.FormDataContentType()))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("multipart upload = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var out map[string]any
	decode(t, resp, &out)
	f := fileOf(t, out)
	if got := f["name"]; got != "from-form.txt" {
		t.Fatalf("name = %v, want the part's filename", got)
	}
	// The envelope's own content type describes the envelope. Storing it would
	// label every multipart upload "multipart/form-data".
	if got := f["content_type"]; got == "multipart/form-data" {
		t.Fatalf("content_type leaked the envelope: %v", got)
	}
}

func TestTextUpload(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodPost, "/v1/text",
		mustJSON(t, map[string]any{"name": "build.log", "content": "all green\n", "tags": []string{"ci"}}),
		withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("text upload = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var out map[string]any
	decode(t, resp, &out)
	f := fileOf(t, out)
	if f["name"] != "build.log" {
		t.Fatalf("name = %v", f["name"])
	}
	if f["content_type"] != "text/plain; charset=utf-8" {
		t.Fatalf("content_type = %v", f["content_type"])
	}

	resp = e.do(http.MethodGet, "/v1/files/"+f["id"].(string)+"/content", nil)
	if got := readBody(t, resp); got != "all green\n" {
		t.Fatalf("round trip = %q", got)
	}

	// An empty body is a mistake, not a zero-byte file.
	resp = e.do(http.MethodPost, "/v1/text", mustJSON(t, map[string]any{"name": "empty"}),
		withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty text = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestContentAddressedAccess(t *testing.T) {
	e := newTestEnv(t, nil)
	content := []byte("addressed by digest")
	sum := digestHex(content)

	// PUT with the right digest.
	resp := e.do(http.MethodPut, "/v1/content/"+sum, content)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("PUT by digest = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	resp.Body.Close()

	// PUT with the wrong digest.
	resp = e.do(http.MethodPut, "/v1/content/"+digestHex([]byte("other")), content)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("wrong digest on PUT = %d, want 400", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeChecksum {
		t.Fatalf("code = %s", code)
	}

	// GET by digest.
	resp = e.do(http.MethodGet, "/v1/content/"+sum, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET by digest = %d", resp.StatusCode)
	}
	if got := readBody(t, resp); got != string(content) {
		t.Fatalf("body = %q", got)
	}

	// Existence check without transferring anything.
	resp = e.do(http.MethodGet, "/v1/content/"+sum+"/meta", nil)
	var meta struct {
		Exists bool `json:"exists"`
	}
	decode(t, resp, &meta)
	if !meta.Exists {
		t.Fatal("meta says the digest does not exist")
	}

	resp = e.do(http.MethodGet, "/v1/content/"+digestHex([]byte("never uploaded"))+"/meta", nil)
	decode(t, resp, &meta)
	if meta.Exists {
		t.Fatal("meta claims a digest that was never stored")
	}

	resp = e.do(http.MethodGet, "/v1/content/"+digestHex([]byte("never uploaded")), nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown digest download = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/v1/content/nothex", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("malformed digest = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

// ---------------------------------------------------------------- listing

func TestListAndPagination(t *testing.T) {
	e := newTestEnv(t, nil)
	for i, name := range []string{"one.txt", "two.txt", "three.txt"} {
		if status, _ := e.upload([]byte(strings.Repeat(name, i+1)), name); status != http.StatusCreated {
			t.Fatalf("upload %s = %d", name, status)
		}
	}

	var page struct {
		Files      []map[string]any `json:"files"`
		Count      int              `json:"count"`
		NextCursor string           `json:"next_cursor"`
		HasMore    bool             `json:"has_more"`
	}
	resp := e.do(http.MethodGet, "/v1/files?limit=2", nil)
	decode(t, resp, &page)
	if page.Count != 2 || !page.HasMore || page.NextCursor == "" {
		t.Fatalf("page 1 = %+v", page)
	}

	resp = e.do(http.MethodGet, "/v1/files?limit=2&cursor="+page.NextCursor, nil)
	decode(t, resp, &page)
	if page.Count != 1 || page.HasMore {
		t.Fatalf("page 2 = %+v", page)
	}

	resp = e.do(http.MethodGet, "/v1/files?q=three", nil)
	decode(t, resp, &page)
	if page.Count != 1 || page.Files[0]["name"] != "three.txt" {
		t.Fatalf("search = %+v", page)
	}

	resp = e.do(http.MethodGet, "/v1/files?cursor=not-a-cursor", nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad cursor = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestDeleteFile(t *testing.T) {
	e := newTestEnv(t, nil)
	content := []byte("delete me")
	_, out := e.upload(content, "gone.txt")
	id := fileOf(t, out)["id"].(string)

	resp := e.do(http.MethodDelete, "/v1/files/"+id, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("delete = %d", resp.StatusCode)
	}
	var del struct {
		Deleted             bool  `json:"deleted"`
		ReferencesRemaining int64 `json:"references_remaining"`
		BlobReclaimed       bool  `json:"blob_reclaimed"`
	}
	decode(t, resp, &del)
	if !del.Deleted {
		t.Fatal("delete did not report success")
	}
	if del.ReferencesRemaining != 0 {
		t.Fatalf("references_remaining = %d after deleting the only record, want 0",
			del.ReferencesRemaining)
	}

	resp = e.do(http.MethodGet, "/v1/files/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("metadata survived the delete: %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/v1/files/"+id+"/content", nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("content survived the delete: %d", resp.StatusCode)
	}
	resp.Body.Close()

	// The bytes are still on disk, and that is the point: deciding here that
	// the content is unreferenced and deleting it takes two steps that a
	// concurrent upload of the same bytes can land between, which would destroy
	// an upload that is still referenced. The sweep answers the same question
	// from a snapshot, so it does the deleting.
	if !e.blobs.Exists(digestHex(content)) {
		t.Fatal("the delete request removed the content itself; a concurrent " +
			"re-upload of the same bytes could have lost its file")
	}
	if del.BlobReclaimed {
		t.Error("the response claims reclamation, but reclamation is the sweep's job")
	}

	// The sweep reclaims it, because nothing references those bytes any more.
	res, err := e.blobs.GC(func(string) bool { return false }, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("the sweep removed %d blobs, want the one now unreferenced", res.Removed)
	}
	if e.blobs.Exists(digestHex(content)) {
		t.Fatal("the content survived the sweep that should have reclaimed it")
	}

	resp = e.do(http.MethodDelete, "/v1/files/"+id, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("double delete = %d, want 404", resp.StatusCode)
	}
	resp.Body.Close()
}

// A body limit has to be enforced while reading. A check placed after the
// decoder returns is a check on something that has already happened, and the
// allocation is the attack: the least privileged credential the service issues
// should not be able to make the process buffer as much as it likes.
func TestJSONEndpointsBoundTheirBodies(t *testing.T) {
	e := newTestEnv(t, nil)

	// The body has to be valid JSON up to the point where it runs past the
	// limit. A body that is malformed instead fails as a syntax error first,
	// which is a 400 and proves nothing about the size bound.
	oversizedJSON := func(field string, size int) []byte {
		body := []byte(`{"` + field + `":"`)
		for len(body) < size {
			body = append(body, 'x')
		}
		return append(body, '"', '}')
	}

	// 256 KiB is far past anything these endpoints legitimately receive, and
	// well past the 64 KiB they are allowed.
	for _, tc := range []struct{ path, field string }{
		{"/v1/shares", "file_id"},
		{"/v1/tokens", "scopes"},
	} {
		body := oversizedJSON(tc.field, 256<<10)
		resp := e.do(http.MethodPost, tc.path, body,
			withHeader("Content-Type", "application/json"))
		if resp.StatusCode != http.StatusRequestEntityTooLarge {
			t.Errorf("POST %s with a %d byte body = %d, want 413",
				tc.path, len(body), resp.StatusCode)
			resp.Body.Close()
			continue
		}
		if code := failCode(t, resp); code != codeTooLarge {
			t.Errorf("POST %s error code = %s, want %s", tc.path, code, codeTooLarge)
		}
	}

	// /v1/text carries content, so it gets a larger allowance — but it is still
	// bounded, and a body past the bound is refused while it is being read.
	huge := oversizedJSON("content", 13<<20)
	resp := e.do(http.MethodPost, "/v1/text", huge,
		withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("POST /v1/text with a %d byte body = %d, want 413", len(huge), resp.StatusCode)
	}
	resp.Body.Close()

	// A body that fits is still accepted, so the limit is not simply refusing
	// everything.
	if status, _ := e.upload([]byte("small"), "small.txt"); status != http.StatusCreated {
		t.Fatalf("an ordinary small upload = %d, want 201", status)
	}
	resp = e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_id": "f_nope"}),
		withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("a well-formed small body = %d, want 404 (unknown file), not a size error",
			resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUploadRejectsMalformedMeta(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodPost, "/v1/files", []byte("x"),
		withHeader("X-Filename", "x.txt"), withHeader("X-Meta", "not json"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad X-Meta = %d, want 400", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodPost, "/v1/files", []byte("x"),
		withHeader("X-Filename", "x.txt"), withHeader("X-Meta", `{"job":"nightly","n":7}`))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("good X-Meta = %d", resp.StatusCode)
	}
	var out map[string]any
	decode(t, resp, &out)
	meta, ok := fileOf(t, out)["meta"].(map[string]any)
	if !ok || meta["job"] != "nightly" {
		t.Fatalf("meta did not survive the round trip: %v", fileOf(t, out)["meta"])
	}
}

func TestTTLCap(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config) { c.MaxTTL = time.Hour })

	resp := e.do(http.MethodPost, "/v1/files", []byte("x"), withHeader("X-Filename", "x.txt"),
		withHeader("X-Ttl", "30d"))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("ttl above the cap = %d, want 400 rather than a silent shortening", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodPost, "/v1/files", []byte("y"), withHeader("X-Filename", "y.txt"),
		withHeader("X-Ttl", "10m"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("ttl within the cap = %d", resp.StatusCode)
	}
	var out map[string]any
	decode(t, resp, &out)
	if fileOf(t, out)["expires_at"] == nil {
		t.Fatal("expires_at should be set")
	}
}

// ---------------------------------------------------------------- shares

func TestShareLifecycle(t *testing.T) {
	e := newTestEnv(t, nil)
	content := []byte("shared bytes")
	_, up := e.upload(content, "report.pdf")
	id := fileOf(t, up)["id"].(string)

	resp := e.do(http.MethodPost, "/v1/shares",
		mustJSON(t, map[string]any{"file_id": id, "label": "for bob", "ttl": "1h", "max_downloads": 2}),
		withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create share = %d: %s", resp.StatusCode, readBody(t, resp))
	}
	var created struct {
		Share map[string]any `json:"share"`
	}
	decode(t, resp, &created)
	token := created.Share["id"].(string)
	url := created.Share["url"].(string)
	if !strings.HasSuffix(url, "/s/"+token) {
		t.Fatalf("share url = %s", url)
	}

	// The whole point: no credential needed.
	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("public download = %d", resp.StatusCode)
	}
	if got := readBody(t, resp); got != string(content) {
		t.Fatalf("shared bytes = %q", got)
	}

	// Info does not spend a credit.
	resp = e.do(http.MethodGet, "/s/"+token+"/info", nil, withoutAuth())
	var info struct {
		Share struct {
			Downloads     int64  `json:"downloads"`
			DownloadsLeft int64  `json:"downloads_left"`
			MaxDownloads  *int64 `json:"max_downloads"`
		} `json:"share"`
		File struct {
			Name   string `json:"name"`
			Sha256 string `json:"sha256"`
		} `json:"file"`
	}
	decode(t, resp, &info)
	if info.Share.Downloads != 1 || info.Share.DownloadsLeft != 1 {
		t.Fatalf("after one download: %+v", info.Share)
	}
	if info.File.Name != "report.pdf" || info.File.Sha256 != digestHex(content) {
		t.Fatalf("info file summary: %+v", info.File)
	}

	// Second download is the last one the budget allows.
	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("second download = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("third download = %d, want 403 once the budget is spent", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeForbidden {
		t.Fatalf("code = %s", code)
	}

	// Revocation.
	resp = e.do(http.MethodDelete, "/v1/shares/"+token, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("revoke share = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("revoked share still resolves: %d", resp.StatusCode)
	}
	resp.Body.Close()
}

func TestUnknownShareToken(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodGet, "/s/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil, withoutAuth())
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown share = %d, want 404", resp.StatusCode)
	}
	failCode(t, resp)
}

func TestExpiredFileAndShare(t *testing.T) {
	e := newTestEnv(t, nil)
	past := time.Now().Add(-time.Minute).Unix()

	_, up := e.upload([]byte("short lived"), "temp.txt")
	id := fileOf(t, up)["id"].(string)

	resp := e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_id": id}),
		withHeader("Content-Type", "application/json"))
	var created struct {
		Share map[string]any `json:"share"`
	}
	decode(t, resp, &created)
	token := created.Share["id"].(string)

	// Age both out directly; waiting on a real clock would slow the suite down
	// without testing anything the timestamps do not already say.
	if _, err := e.store.DB().Exec(`UPDATE files SET expires_at = ? WHERE id = ?`, past, id); err != nil {
		t.Fatal(err)
	}
	if _, err := e.store.DB().Exec(`UPDATE shares SET expires_at = ? WHERE id = ?`, past, token); err != nil {
		t.Fatal(err)
	}

	resp = e.do(http.MethodGet, "/v1/files/"+id, nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired file metadata = %d, want 410", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/v1/files/"+id+"/content", nil)
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired content = %d, want 410", resp.StatusCode)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("expired share = %d, want 410", resp.StatusCode)
	}
	if code := failCode(t, resp); code != codeGone {
		t.Fatalf("code = %s", code)
	}
}

func TestDeletingFileRevokesItsShares(t *testing.T) {
	e := newTestEnv(t, nil)
	_, up := e.upload([]byte("doomed"), "doomed.txt")
	id := fileOf(t, up)["id"].(string)

	resp := e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_id": id}),
		withHeader("Content-Type", "application/json"))
	var created struct {
		Share map[string]any `json:"share"`
	}
	decode(t, resp, &created)
	token := created.Share["id"].(string)

	resp = e.do(http.MethodDelete, "/v1/files/"+id, nil)
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/s/"+token, nil, withoutAuth())
	if resp.StatusCode == http.StatusOK {
		t.Fatal("a share link outlived the file it pointed at")
	}
	resp.Body.Close()
}

func TestShareRequiresExistingFile(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_id": "f_nope"}),
		withHeader("Content-Type", "application/json"))
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("share of an unknown file = %d, want 404", resp.StatusCode)
	}
	failCode(t, resp)
}

// ---------------------------------------------------------------- misc

func TestHealthStatsAndCORS(t *testing.T) {
	e := newTestEnv(t, nil)
	e.upload([]byte("one"), "one.txt")

	resp := e.do(http.MethodGet, "/healthz", nil, withoutAuth())
	var health map[string]any
	decode(t, resp, &health)
	if health["status"] != "ok" {
		t.Fatalf("health = %v", health)
	}

	resp = e.do(http.MethodGet, "/v1/stats", nil)
	var stats map[string]any
	decode(t, resp, &stats)
	if stats["files"].(float64) != 1 {
		t.Fatalf("stats = %v", stats)
	}

	resp = e.do(http.MethodOptions, "/v1/files", nil, withoutAuth())
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("preflight = %d, want 204", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight is missing CORS headers: %v", resp.Header)
	}
	resp.Body.Close()

	resp = e.do(http.MethodGet, "/", nil, withoutAuth())
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Error("security headers are missing from an ordinary response")
	}
	if resp.Request.Host == "" || resp.Header.Get("X-Request-Id") == "" {
		t.Error("every response should carry a request id")
	}
	resp.Body.Close()
}

func TestShareResponseIsNotIndexable(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodGet, "/s/aaaaaaaaaaaaaaaaaaaaaaaaaa", nil, withoutAuth())
	if got := resp.Header.Get("X-Robots-Tag"); !strings.Contains(got, "noindex") {
		t.Fatalf("share responses should be non-indexable, got %q", got)
	}
	resp.Body.Close()
}

func TestLLMsTxtCarriesTheCallersBaseURL(t *testing.T) {
	e := newTestEnv(t, nil)
	resp := e.do(http.MethodGet, "/llms.txt", nil, withoutAuth())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("llms.txt = %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, e.srv.URL) {
		t.Fatalf("llms.txt should have {{BASE}} substituted:\n%s", body)
	}
	if strings.Contains(body, "{{BASE}}") {
		t.Fatal("llms.txt still contains an unsubstituted placeholder")
	}
}

// The advertised upload limit has to describe every address the service is
// reachable on, not just the one this process configured. Getting this wrong is
// invisible from the server's side: an upload above the proxy's limit is
// refused by the proxy and never arrives, so the server sees nothing wrong
// while the caller gets an HTML error page from something they were never told
// about.
func TestDescriptorReportsTheProxysUploadCap(t *testing.T) {
	t.Run("no proxy configured", func(t *testing.T) {
		e := newTestEnv(t, func(c *config.Config) { c.MaxUpload = 0 })
		resp := e.do(http.MethodGet, "/", nil, withoutAuth())
		var doc struct {
			Limits map[string]any `json:"limits"`
		}
		decode(t, resp, &doc)

		if got := doc.Limits["max_upload_bytes"].(float64); got != 0 {
			t.Fatalf("max_upload_bytes = %v, want 0 (unlimited)", got)
		}
		if _, ok := doc.Limits["proxied"]; ok {
			t.Fatal("no proxy is configured, so there is no proxied cap to advertise")
		}
	})

	t.Run("proxy configured", func(t *testing.T) {
		e := newTestEnv(t, func(c *config.Config) {
			c.MaxUpload = 0
			c.ProxyMaxUpload = 100 << 20
			c.BaseURL = "https://files.example.com"
		})
		resp := e.do(http.MethodGet, "/", nil, withoutAuth())
		var doc struct {
			Limits map[string]any `json:"limits"`
		}
		decode(t, resp, &doc)

		proxied, ok := doc.Limits["proxied"].(map[string]any)
		if !ok {
			t.Fatalf("a proxy cap was configured but not advertised: %v", doc.Limits)
		}
		if got := proxied["url"]; got != "https://files.example.com" {
			t.Errorf("proxied.url = %v, want the configured base url", got)
		}
		if got := proxied["max_upload_bytes"].(float64); got != float64(100<<20) {
			t.Errorf("proxied.max_upload_bytes = %v, want %d", got, 100<<20)
		}
		if note, _ := proxied["note"].(string); note == "" {
			t.Error("the proxied cap needs an explanation; a bare number looks like a limit this server enforces")
		}
		if _, ok := proxied["direct_url"]; ok {
			t.Error("no direct address was configured, so none should be named")
		}
	})

	t.Run("proxy configured with a direct address", func(t *testing.T) {
		// "use a direct address for larger files" is only actionable if the
		// reader is told which address that is.
		e := newTestEnv(t, func(c *config.Config) {
			c.MaxUpload = 0
			c.ProxyMaxUpload = 100 << 20
			c.BaseURL = "https://files.example.com"
			c.DirectURL = "https://dl.files.example.com"
		})
		resp := e.do(http.MethodGet, "/", nil, withoutAuth())
		var doc struct {
			Limits map[string]any `json:"limits"`
		}
		decode(t, resp, &doc)

		proxied, ok := doc.Limits["proxied"].(map[string]any)
		if !ok {
			t.Fatalf("no proxied cap advertised: %v", doc.Limits)
		}
		if got := proxied["direct_url"]; got != "https://dl.files.example.com" {
			t.Errorf("direct_url = %v, want the configured direct address", got)
		}
		if got := proxied["direct_max_upload_bytes"].(float64); got != 0 {
			t.Errorf("direct_max_upload_bytes = %v, want the server's own cap (0 = unlimited)", got)
		}
	})

	t.Run("proxy cap without a base url", func(t *testing.T) {
		// Nothing names the address the cap applies to, so advertising it would
		// only confuse: the reader cannot tell which URL it refers to.
		e := newTestEnv(t, func(c *config.Config) {
			c.ProxyMaxUpload = 100 << 20
			c.BaseURL = ""
		})
		resp := e.do(http.MethodGet, "/", nil, withoutAuth())
		var doc struct {
			Limits map[string]any `json:"limits"`
		}
		decode(t, resp, &doc)
		if _, ok := doc.Limits["proxied"]; ok {
			t.Fatal("a proxied cap was advertised without a base url to attach it to")
		}
	})
}

func TestConfiguredBaseURLWins(t *testing.T) {
	e := newTestEnv(t, func(c *config.Config) { c.BaseURL = "https://files.example.com" })
	_, out := e.upload([]byte("x"), "x.txt")
	urls := fileOf(t, out)["urls"].(map[string]any)
	if got := urls["self"].(string); !strings.HasPrefix(got, "https://files.example.com/") {
		t.Fatalf("base-url was ignored: %s", got)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
