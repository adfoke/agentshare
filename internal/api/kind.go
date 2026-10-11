package api

import (
	"bytes"
	"io"
	"strings"
	"unicode/utf8"
)

// What a stored file actually is, decided from its first bytes.
//
// This service never executes, decodes or parses what it stores — that is the
// invariant that keeps uploaded programs from becoming running programs. But by
// the time a file has been through the store, this is the last place where
// "what is this, really" can still be answered cheaply, and it is exactly what a
// caller needs before running something it just downloaded. So the answer is
// published: a kind on every description, and an X-Content-Kind header on every
// response that carries bytes.
//
// The classification reads magic bytes and nothing else. There is no parser, no
// decoder, no subprocess and no library for any of the formats named below — a
// hostile file has nothing to exploit here beyond being compared to a few
// constants. That is a deliberate property, not an accident: recognising an
// image by decoding it would hand the attacker a decoder, and decoders are
// where the bugs are.
//
// A kind is a hint about the bytes, never a verdict about the sender. See
// inlineReason for how it is used to decide what a browser is allowed to render.

// kindSniffLen is how far into a file classification looks. Every magic number
// below lives well inside it, and 512 bytes is the same window
// http.DetectContentType uses.
const kindSniffLen = 512

// Kinds. The values are part of the API: a client may branch on them.
//
// The first four are the ones worth being careful about. Everything else is
// inert data that no amount of double-clicking will run.
const (
	// KindExecutable is a program image: ELF, PE, Mach-O, WebAssembly, or a
	// Java class.
	KindExecutable = "executable"
	// KindScript is a script with an interpreter line (#!/bin/sh, #!/usr/bin/env
	// python3, ...). It is data until something runs it.
	KindScript = "script"
	// KindHTML is markup a browser would treat as a document.
	KindHTML = "html"
	// KindSVG is SVG, which is both an image and a document that can carry
	// script.
	KindSVG = "svg"
	// KindXML is XML that is not SVG. XHTML lives here, so it stays on the
	// scriptable side of the line.
	KindXML = "xml"

	KindImage    = "image"
	KindAudio    = "audio"
	KindVideo    = "video"
	KindArchive  = "archive"
	KindDocument = "document"
	KindText     = "text"
	KindBinary   = "binary"
)

// contentKind is the result of reading the head of a file.
type contentKind struct {
	// Kind is the classification, or "" when the bytes could not be read at all
	// (a missing blob). An empty kind is reported as unknown, never guessed.
	Kind string
	// Executable reports that an operating system would run these bytes if it
	// were asked to, which is precisely the question a downloading agent needs
	// answered before it decides to ask.
	Executable bool
	// Scriptable reports that a browser could end up running code out of this
	// file if it were rendered: markup, SVG, XHTML.
	Scriptable bool
}

// classifyBytes classifies a file from its head. A short or empty slice is
// fine: the caller passes whatever it could read.
func classifyBytes(head []byte) contentKind {
	if len(head) == 0 {
		return contentKind{Kind: KindBinary}
	}

	// Program images first: these are the bytes an operating system executes.
	switch {
	case bytes.HasPrefix(head, []byte("\x7fELF")):
		return contentKind{Kind: KindExecutable, Executable: true}
	case len(head) >= 2 && head[0] == 'M' && head[1] == 'Z':
		// DOS/Windows PE. Every PE starts with the DOS stub, so two bytes are
		// as much as is needed to say "this is a program".
		return contentKind{Kind: KindExecutable, Executable: true}
	case hasAnyPrefix(head,
		"\xfe\xed\xfa\xce", // Mach-O 32-bit, big endian
		"\xce\xfa\xed\xfe", // Mach-O 32-bit, little endian
		"\xfe\xed\xfa\xcf", // Mach-O 64-bit, big endian
		"\xcf\xfa\xed\xfe", // Mach-O 64-bit, little endian
		"\xca\xfe\xba\xbe", // Mach-O universal binary, or a Java class
	):
		return contentKind{Kind: KindExecutable, Executable: true}
	case bytes.HasPrefix(head, []byte("\x00asm")):
		return contentKind{Kind: KindExecutable, Executable: true}
	case bytes.HasPrefix(head, []byte("#!")):
		// The interpreter line is the definition of an executable text file.
		return contentKind{Kind: KindScript, Executable: true}
	}

	// Markup. Whitespace and a byte-order mark may precede the first tag, and an
	// XML prologue may precede everything else.
	trimmed := trimLeadingSpace(head)
	if k, ok := classifyMarkup(trimmed); ok {
		return k
	}

	// Formats that are containers.
	switch {
	case bytes.HasPrefix(head, []byte("%PDF-")):
		return contentKind{Kind: KindDocument}
	case bytes.HasPrefix(head, []byte("\xd0\xcf\x11\xe0\xa1\xb1\x1a\xe1")):
		// OLE2 compound file: legacy .doc/.xls/.ppt.
		return contentKind{Kind: KindDocument}
	case hasAnyPrefix(head,
		"PK\x03\x04", "PK\x05\x06", "PK\x07\x08", // zip and its empty/spanning forms
		"\x1f\x8b",         // gzip
		"BZh",              // bzip2
		"\xfd7zXZ\x00",     // xz
		"\x28\xb5\x2f\xfd", // zstd
		"7z\xbc\xaf\x27\x1c",
		"Rar!\x1a\x07",
	):
		return contentKind{Kind: KindArchive}
	case len(head) > 262 && bytes.Equal(head[257:262], []byte("ustar")):
		return contentKind{Kind: KindArchive}
	}

	// Images, audio and video, by container signature.
	switch {
	case hasAnyPrefix(head,
		"\x89PNG\r\n\x1a\n", "\xff\xd8\xff", "GIF87a", "GIF89a",
		"II*\x00", "MM\x00*", "\x00\x00\x01\x00"):
		return contentKind{Kind: KindImage}
	case bytes.HasPrefix(head, []byte("BM")) && len(head) > 2:
		// Two bytes is all BMP offers; a text file starting with "BM" is the
		// cost of that, and it costs nothing here — image is inert.
		return contentKind{Kind: KindImage}
	case riffKind(head) == "WEBP":
		return contentKind{Kind: KindImage}
	case riffKind(head) == "WAVE":
		return contentKind{Kind: KindAudio}
	case isftyp(head, "heic", "heix", "hevc", "hevx", "mif1", "msf1", "avif", "avis"):
		return contentKind{Kind: KindImage}
	case isftyp(head, "isom", "iso2", "mp41", "mp42", "avc1", "M4V ", "mp4", "qt  ", "3gp4"):
		return contentKind{Kind: KindVideo}
	case hasAnyPrefix(head, "ID3", "\xff\xfb", "\xff\xf3", "\xff\xf2", "OggS", "fLaC"):
		return contentKind{Kind: KindAudio}
	case bytes.HasPrefix(head, []byte("\x1a\x45\xdf\xa3")):
		// Matroska / WebM. Both are video containers.
		return contentKind{Kind: KindVideo}
	}

	if looksLikeText(head) {
		return contentKind{Kind: KindText}
	}
	return contentKind{Kind: KindBinary}
}

// classifyMarkup recognises the formats a browser would render as a document.
func classifyMarkup(b []byte) (contentKind, bool) {
	lower := bytes.ToLower(b)
	// An XML prologue can be followed by SVG, which is the case worth catching:
	// plain XML is inert, SVG is not.
	if bytes.HasPrefix(lower, []byte("<?xml")) {
		if bytes.Contains(lower, []byte("<svg")) {
			return contentKind{Kind: KindSVG, Scriptable: true}, true
		}
		return contentKind{Kind: KindXML, Scriptable: true}, true
	}
	if hasAnyPrefix(lower, "<!doctype html", "<html", "<head", "<body", "<script",
		"<iframe", "<meta", "<title", "<div", "<p>", "<p ", "<a ", "<!--") {
		return contentKind{Kind: KindHTML, Scriptable: true}, true
	}
	if bytes.HasPrefix(lower, []byte("<svg")) {
		return contentKind{Kind: KindSVG, Scriptable: true}, true
	}
	return contentKind{}, false
}

// inlineReason reports why a browser must not be allowed to render this file,
// or "" when rendering it is safe.
//
// Two independent conditions have to hold, and both are computed here rather
// than taken from the client. The declared content type is the one the uploader
// chose, so it cannot be trusted on its own — a file whose bytes are a program
// but whose type says image/png still must not render. The bytes are the
// uploader's too, so they cannot be trusted on their own either — a text/html
// body with PNG bytes rendered as text/html would be script in a document.
// Together they are enough: the browser is told a type from the allowlist, and
// the bytes are known not to be markup or a program.
//
// The check is written as a refusal rather than an approval on purpose: a
// format nobody thought of stays out until somebody adds it here.
func (k contentKind) inlineReason(mediaType string) string {
	switch {
	case k.Executable:
		return "executable"
	case k.Scriptable:
		return "scriptable"
	case !inlineAllowedType(mediaType):
		return "type"
	}
	return ""
}

// inlineTypes are the content types the service is willing to render in a
// browser. Membership means "a browser given these bytes under this type cannot
// be made to run code": raster images, plain text, audio and video.
//
// Deliberately absent, each for a reason:
//
//	text/html, application/xhtml+xml, */*+xml   script, under the right type
//	image/svg+xml                               an image that is also a document
//	application/pdf                             an enormous parser in the
//	                                            browser, and the reason "just
//	                                            view the PDF" is a risky sentence
//	text/javascript and friends                 script, spelled out
type inlineTypes = map[string]bool

var renderableTypes = inlineTypes{
	"image/png":                true,
	"image/jpeg":               true,
	"image/gif":                true,
	"image/webp":               true,
	"image/bmp":                true,
	"image/tiff":               true,
	"image/avif":               true,
	"image/heic":               true,
	"image/x-icon":             true,
	"image/vnd.microsoft.icon": true,

	"text/plain": true,

	"audio/mpeg":  true,
	"audio/mp4":   true,
	"audio/aac":   true,
	"audio/ogg":   true,
	"audio/opus":  true,
	"audio/flac":  true,
	"audio/wav":   true,
	"audio/x-wav": true,
	"audio/webm":  true,

	"video/mp4":       true,
	"video/webm":      true,
	"video/ogg":       true,
	"video/quicktime": true,
}

// inlineAllowedType reports whether a stored content type may render. The
// comparison is on the bare media type: parameters such as charset travel with
// the header but say nothing about whether the type is safe.
func inlineAllowedType(mediaType string) bool {
	return renderableTypes[baseMediaType(mediaType)]
}

// sandboxCSP is sent with every response that is not rendered inline.
//
// It is belt and braces, and it is cheap: the Content-Disposition on those
// responses already says "download this", so for a browser this header changes
// nothing at all. It exists for the case where that stops being true — a rule
// relaxed here, a proxy rewriting headers there — and it is the difference
// between a mistake being a mistake and a mistake being an execution. A
// sandboxed document gets an opaque origin and no script, whatever it contains.
//
// It is deliberately not sent on responses that do render (images, text,
// audio, video): those are the ones where it would have to be reasoned about,
// and none of them can carry script in the first place.
const sandboxCSP = "sandbox; default-src 'none'; frame-ancestors 'none'"

// baseMediaType lowercases a content type and drops its parameters.
func baseMediaType(v string) string {
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}

// blobKind classifies a stored blob. The blob is immutable, so this answer is
// stable for the life of the content and needs no column to remember it in.
func (s *Server) blobKind(sha string) contentKind {
	f, err := s.blobs.Open(sha)
	if err != nil {
		return contentKind{} // unknown; callers omit rather than guess
	}
	defer f.Close()
	head := make([]byte, kindSniffLen)
	n, _ := io.ReadFull(f, head)
	return classifyBytes(head[:n])
}

// ---------------------------------------------------------------- helpers

func hasPrefix(b []byte, p string) bool { return bytes.HasPrefix(b, []byte(p)) }

func hasAnyPrefix(b []byte, prefixes ...string) bool {
	for _, p := range prefixes {
		if hasPrefix(b, p) {
			return true
		}
	}
	return false
}

// trimLeadingSpace drops leading ASCII whitespace and a UTF-8 BOM, both of
// which may legally precede the first byte of markup.
func trimLeadingSpace(b []byte) []byte {
	b = bytes.TrimPrefix(b, []byte("\xef\xbb\xbf"))
	for len(b) > 0 {
		switch b[0] {
		case ' ', '\t', '\r', '\n', '\f', '\v':
			b = b[1:]
		default:
			return b
		}
	}
	return b
}

// riffKind reports the four-character form type of a RIFF container ("WEBP",
// "WAVE", "AVI "), or "" when the bytes are not RIFF.
func riffKind(b []byte) string {
	if len(b) < 12 || !hasPrefix(b, "RIFF") {
		return ""
	}
	return string(b[8:12])
}

// isftyp reports whether b is an ISO base media file (MP4 and friends) whose
// brand is one of the given ones.
func isftyp(b []byte, brands ...string) bool {
	if len(b) < 12 || string(b[4:8]) != "ftyp" {
		return false
	}
	brand := string(b[8:12])
	for _, want := range brands {
		if brand == want {
			return true
		}
	}
	return false
}

// looksLikeText is the fallback: bytes with no NUL and valid UTF-8 read as text,
// everything else is opaque. A rune cut in half by the sniff window is expected
// at the very end and is not evidence of binary.
func looksLikeText(b []byte) bool {
	if bytes.IndexByte(b, 0) >= 0 {
		return false
	}
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			return len(b)-i < utf8.UTFMax
		}
		i += size
	}
	return true
}
