package api

import (
	"net/http"
	"strings"
	"testing"
)

// Classification is a pure function, so it can be pinned precisely. Every magic
// number here is a claim about the real world, which is exactly the kind of
// claim that silently rots.
func TestClassifyBytes(t *testing.T) {
	tar := make([]byte, 300)
	copy(tar[257:], "ustar")

	cases := []struct {
		name       string
		head       []byte
		kind       string
		executable bool
		scriptable bool
	}{
		{"elf", []byte("\x7fELF\x02\x01\x01\x00"), KindExecutable, true, false},
		{"windows pe", []byte("MZ\x90\x00\x03"), KindExecutable, true, false},
		{"mach-o 64", []byte("\xcf\xfa\xed\xfe\x07\x00\x00\x01"), KindExecutable, true, false},
		{"universal binary / java class", []byte("\xca\xfe\xba\xbe\x00\x00\x00\x34"), KindExecutable, true, false},
		{"webassembly", []byte("\x00asm\x01\x00\x00\x00"), KindExecutable, true, false},
		{"shebang script", []byte("#!/bin/sh\necho hi\n"), KindScript, true, false},
		{"html", []byte("<!DOCTYPE html><html><body>"), KindHTML, false, true},
		{"html after whitespace", []byte("\n\t  <html><body>"), KindHTML, false, true},
		{"html after a byte order mark", []byte("\xef\xbb\xbf<html>"), KindHTML, false, true},
		{"html starting at a script tag", []byte("<script>alert(1)</script>"), KindHTML, false, true},
		{"svg", []byte(`<svg xmlns="http://www.w3.org/2000/svg"></svg>`), KindSVG, false, true},
		{"svg behind an xml prologue", []byte("<?xml version=\"1.0\"?>\n<svg/>"), KindSVG, false, true},
		{"plain xml", []byte("<?xml version=\"1.0\"?><rss/>"), KindXML, false, true},
		{"png", []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\r"), KindImage, false, false},
		{"jpeg", []byte("\xff\xd8\xff\xe0\x00\x10"), KindImage, false, false},
		{"gif", []byte("GIF89a\x01\x00"), KindImage, false, false},
		{"webp", []byte("RIFF\x24\x00\x00\x00WEBPVP8 "), KindImage, false, false},
		{"wav", []byte("RIFF\x24\x00\x00\x00WAVEfmt "), KindAudio, false, false},
		{"mp3 with an id3 tag", []byte("ID3\x03\x00\x00\x00"), KindAudio, false, false},
		{"flac", []byte("fLaC\x00\x00\x00\x22"), KindAudio, false, false},
		{"mp4", []byte("\x00\x00\x00\x18ftypisom"), KindVideo, false, false},
		{"matroska", []byte("\x1a\x45\xdf\xa3\x01\x00\x00\x00"), KindVideo, false, false},
		{"zip", []byte("PK\x03\x04\x14\x00"), KindArchive, false, false},
		{"gzip", []byte("\x1f\x8b\x08\x00"), KindArchive, false, false},
		{"xz", []byte("\xfd7zXZ\x00\x00"), KindArchive, false, false},
		{"tar", tar, KindArchive, false, false},
		{"pdf", []byte("%PDF-1.7\n%\xe2\xe3"), KindDocument, false, false},
		{"legacy office", []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1"), KindDocument, false, false},
		{"utf-8 text", []byte("hello, 世界\n"), KindText, false, false},
		{"ascii text", []byte("zlib compressed data\n"), KindText, false, false},
		{"a rune cut in half by the window", []byte("hello \xe4\xb8"), KindText, false, false},
		{"binary with a nul", []byte("ab\x00cd"), KindBinary, false, false},
		{"invalid utf-8", []byte("ab\xff\xfecd"), KindBinary, false, false},
		{"empty", nil, KindBinary, false, false},
		{"a single byte", []byte("x"), KindText, false, false},
	}
	for _, tc := range cases {
		got := classifyBytes(tc.head)
		if got.Kind != tc.kind {
			t.Errorf("%s: kind = %q, want %q", tc.name, got.Kind, tc.kind)
		}
		if got.Executable != tc.executable {
			t.Errorf("%s: executable = %v, want %v", tc.name, got.Executable, tc.executable)
		}
		if got.Scriptable != tc.scriptable {
			t.Errorf("%s: scriptable = %v, want %v", tc.name, got.Scriptable, tc.scriptable)
		}
	}
}

// The hole this feature exists to close: an uploaded HTML file used to come
// back with `Content-Disposition: inline` whenever the URL said ?inline=1,
// which means a browser ran the uploader's script on this service's own origin.
func TestInlineRefusedForMarkup(t *testing.T) {
	e := newTestEnv(t, nil)
	html := []byte("<!DOCTYPE html><html><body><script>alert(document.domain)</script></body></html>")
	_, up := e.upload(html, "evil.html", withHeader("Content-Type", "text/html"))
	f := fileOf(t, up)
	id := f["id"].(string)

	if f["kind"] != KindHTML {
		t.Fatalf("kind = %v, want %s", f["kind"], KindHTML)
	}
	if f["scriptable"] != true || f["executable"] != false {
		t.Fatalf("flags: scriptable=%v executable=%v, want true/false", f["scriptable"], f["executable"])
	}

	for _, path := range []string{
		"/v1/files/" + id + "/content?inline=1",
		"/v1/files/" + id + "/content?view=1", // the alias
	} {
		resp := e.do(http.MethodGet, path, nil)
		if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
			t.Errorf("%s: Content-Disposition = %q, want an attachment", path, got)
		}
		if got := resp.Header.Get("X-Inline-Refused"); got != "scriptable" {
			t.Errorf("%s: X-Inline-Refused = %q, want scriptable", path, got)
		}
		if got := resp.Header.Get("Content-Security-Policy"); got != sandboxCSP {
			t.Errorf("%s: CSP = %q, want the download sandbox", path, got)
		}
		if got := resp.Header.Get("X-Content-Kind"); got != KindHTML {
			t.Errorf("%s: X-Content-Kind = %q, want %s", path, got, KindHTML)
		}
		if got := readBody(t, resp); got != string(html) {
			t.Errorf("%s: the bytes must still arrive intact", path)
		}
	}
}

// Refusal reasons, one case per way of getting it wrong. The declared type
// belongs to the uploader and the bytes belong to the uploader, so neither can
// decide this alone.
func TestInlineRefusalReasons(t *testing.T) {
	cases := []struct {
		name     string
		body     []byte
		declared string
		want     string
	}{
		{"markup declared as markup", []byte("<html><body>hi"), "text/html", "scriptable"},
		{"svg declared as an image", []byte(`<svg xmlns="http://www.w3.org/2000/svg"/>`), "image/svg+xml", "scriptable"},
		{"xhtml", []byte(`<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml"/>`), "application/xhtml+xml", "scriptable"},
		{"a pdf", []byte("%PDF-1.7\n"), "application/pdf", "type"},
		{"a program declared as an image", []byte("\x7fELF\x02\x01\x01\x00\x00\x00"), "image/png", "executable"},
		{"a script declared as text", []byte("#!/bin/sh\nrm -rf/\n"), "text/plain", "executable"},
		{"opaque bytes", []byte("\x01\x02\x03\xff\xfe\x00"), "application/octet-stream", "type"},
	}
	for _, tc := range cases {
		e := newTestEnv(t, nil)
		_, up := e.upload(tc.body, "thing", withHeader("Content-Type", tc.declared))
		id := fileOf(t, up)["id"].(string)

		resp := e.do(http.MethodGet, "/v1/files/"+id+"/content?inline=1", nil)
		if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
			t.Errorf("%s: Content-Disposition = %q, want an attachment", tc.name, got)
		}
		if got := resp.Header.Get("X-Inline-Refused"); got != tc.want {
			t.Errorf("%s: X-Inline-Refused = %q, want %q", tc.name, got, tc.want)
		}
		readBody(t, resp)
	}
}

// Rendering stays available where it is safe, and those are the responses that
// must NOT carry the download sandbox: they are the ones a sandbox would have
// to be reasoned about, and none of them can carry script.
func TestInlineAllowedForInertTypes(t *testing.T) {
	png := append([]byte("\x89PNG\r\n\x1a\n"), []byte("magic bytes are the whole test")...)
	e := newTestEnv(t, nil)
	_, up := e.upload(png, "shot.png", withHeader("Content-Type", "image/png"))
	f := fileOf(t, up)
	id := f["id"].(string)
	if f["kind"] != KindImage || f["executable"] != false {
		t.Fatalf("kind = %v executable = %v", f["kind"], f["executable"])
	}

	resp := e.do(http.MethodGet, "/v1/files/"+id+"/content?inline=1", nil)
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "inline") {
		t.Fatalf("Content-Disposition = %q, want inline", got)
	}
	if got := resp.Header.Get("X-Inline-Refused"); got != "" {
		t.Errorf("an image was refused: %q", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("a rendered image must not carry the download sandbox: %q", got)
	}

	// Without the flag it is a download, and a download carries the sandbox.
	resp = e.do(http.MethodGet, "/v1/files/"+id+"/content", nil)
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("Content-Disposition = %q, want an attachment", got)
	}
	if got := resp.Header.Get("Content-Security-Policy"); got != sandboxCSP {
		t.Errorf("CSP on a download = %q", got)
	}

	// The digest route decides the same way; it has no stored type to lean on
	// and sniffs the bytes instead.
	resp = e.do(http.MethodGet, "/v1/content/"+f["sha256"].(string)+"?inline=1", nil)
	if got := resp.Header.Get("Content-Security-Policy"); got != "" {
		t.Errorf("the digest route refused a png: csp=%q refused=%q",
			got, resp.Header.Get("X-Inline-Refused"))
	}
	readBody(t, resp)
}

// A program is labelled as one everywhere a caller might look: in the upload
// reply, in a listing, in a share's listing, and on the bytes themselves.
func TestExecutableIsLabelledEverywhere(t *testing.T) {
	e := newTestEnv(t, nil)
	elf := append([]byte("\x7fELF\x02\x01\x01\x00"), []byte("pretend payload")...)

	_, up := e.upload(elf, "payload.bin")
	f := fileOf(t, up)
	id := f["id"].(string)
	if f["kind"] != KindExecutable || f["executable"] != true || f["scriptable"] != false {
		t.Fatalf("upload reply: kind=%v executable=%v scriptable=%v",
			f["kind"], f["executable"], f["scriptable"])
	}
	if _, up := e.upload([]byte("just notes\n"), "notes.txt"); fileOf(t, up)["kind"] != KindText {
		t.Fatalf("plain text is not text: %v", fileOf(t, up)["kind"])
	}

	// On the bytes.
	resp := e.do(http.MethodGet, "/v1/files/"+id+"/content", nil)
	if got := resp.Header.Get("X-Content-Kind"); got != KindExecutable {
		t.Errorf("X-Content-Kind = %q, want %s", got, KindExecutable)
	}
	readBody(t, resp)

	// In a listing — the question "which of these would run?" should not cost
	// a download per file.
	var listing struct {
		Files []map[string]any `json:"files"`
	}
	decode(t, e.do(http.MethodGet, "/v1/files", nil), &listing)
	kinds := map[string]any{}
	for _, it := range listing.Files {
		kinds[it["name"].(string)] = it["kind"]
	}
	if kinds["payload.bin"] != KindExecutable || kinds["notes.txt"] != KindText {
		t.Fatalf("listing kinds = %v", kinds)
	}
}

// The index page is where a recipient decides what to fetch, so it says which
// members are programs before the download rather than after it — and it still
// escapes the names it prints.
func TestShareIndexFlagsExecutables(t *testing.T) {
	e := newTestEnv(t, nil)
	elf := append([]byte("\x7fELF\x02\x01\x01\x00"), []byte("pretend payload")...)
	_, up := e.upload(elf, "payload.bin")
	id := fileOf(t, up)["id"].(string)

	var created struct {
		Share map[string]any `json:"share"`
	}
	decode(t, e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_ids": []string{id}}),
		withHeader("Content-Type", "application/json")), &created)
	token := created.Share["id"].(string)

	body := readBody(t, e.do(http.MethodGet, "/s/"+token+"/", nil, withoutAuth()))
	if !strings.Contains(body, "payload.bin") {
		t.Fatal("the index does not name the file")
	}
	if !strings.Contains(body, `class="tag">`+KindExecutable) {
		t.Error("the row is not tagged as an executable")
	}
	if !strings.Contains(body, `class="warn"`) {
		t.Error("the page does not warn about the executable")
	}

	var info struct {
		Files []map[string]any `json:"files"`
	}
	decode(t, e.do(http.MethodGet, "/s/"+token+"/info", nil, withoutAuth()), &info)
	if len(info.Files) != 1 {
		t.Fatalf("share info lists %d files", len(info.Files))
	}
	if info.Files[0]["kind"] != KindExecutable || info.Files[0]["executable"] != true {
		t.Fatalf("share info: kind=%v executable=%v", info.Files[0]["kind"], info.Files[0]["executable"])
	}

	// Names inside the page are still escaped, tags or no tags.
	_, up = e.upload([]byte("x"), `<img src=x onerror=alert(1)>.bin`)
	evil := fileOf(t, up)["id"].(string)
	var second struct {
		Share map[string]any `json:"share"`
	}
	decode(t, e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_id": evil}),
		withHeader("Content-Type", "application/json")), &second)
	page := readBody(t, e.do(http.MethodGet, "/s/"+second.Share["id"].(string)+"/", nil, withoutAuth()))
	if strings.Contains(page, "<img src=x onerror") {
		t.Error("a file name reached the page unescaped")
	}
}

// A share of one file downloads those bytes through the same gate, so the
// capability URL is not a way around the rule.
func TestShareMemberObeysTheSameInlineRule(t *testing.T) {
	e := newTestEnv(t, nil)
	_, up := e.upload([]byte("<html><script>alert(1)</script>"), "evil.html",
		withHeader("Content-Type", "text/html"))
	id := fileOf(t, up)["id"].(string)

	var created struct {
		Share map[string]any `json:"share"`
	}
	decode(t, e.do(http.MethodPost, "/v1/shares", mustJSON(t, map[string]any{"file_id": id}),
		withHeader("Content-Type", "application/json")), &created)
	token := created.Share["id"].(string)

	resp := e.do(http.MethodGet, "/s/"+token+"/f/"+id+"?inline=1", nil, withoutAuth())
	if got := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Fatalf("Content-Disposition = %q, want an attachment", got)
	}
	if got := resp.Header.Get("X-Inline-Refused"); got != "scriptable" {
		t.Fatalf("X-Inline-Refused = %q, want scriptable", got)
	}
	readBody(t, resp)
}
