package api

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"
)

// Stable error codes. An agent should branch on these, never on the message
// text, which is allowed to change.
const (
	codeBadRequest    = "bad_request"
	codeUnauthorized  = "unauthorized"
	codeForbidden     = "forbidden"
	codeNotFound      = "not_found"
	codeConflict      = "conflict"
	codeGone          = "gone"
	codeTooLarge      = "payload_too_large"
	codeUnsupported   = "unsupported_media_type"
	codeRateLimited   = "rate_limited"
	codeInternal      = "internal"
	codeUnavailable   = "storage_unavailable"
	codeChecksum      = "checksum_mismatch"
	codeNotAcceptable = "not_acceptable"
)

type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Hint    string `json:"hint,omitempty"`
}

type errorBody struct {
	Error     apiError `json:"error"`
	RequestID string   `json:"request_id,omitempty"`
}

// fail writes the one and only error shape agentshare produces:
//
//	{"error": {"code": "...", "message": "...", "hint": "..."}}
//
// The hint is written for whoever (or whatever) is reading the response and
// usually names the exact next call to make.
func fail(c *gin.Context, status int, code, message string, hint ...string) {
	body := errorBody{
		Error:     apiError{Code: code, Message: message},
		RequestID: requestID(c),
	}
	if len(hint) > 0 {
		body.Error.Hint = hint[0]
	}
	c.AbortWithStatusJSON(status, body)
}

func failInternal(c *gin.Context, log *slog.Logger, err error, message string) {
	if log != nil && err != nil {
		log.Error("request failed", "err", err.Error(), "path", c.Request.URL.Path,
			"request_id", requestID(c))
	}
	fail(c, http.StatusInternalServerError, codeInternal, message,
		"retry once; if it persists, check the server log with this request_id")
}
