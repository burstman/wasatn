// Package web also owns the central error handler, which turns the errors
// returned by kit handlers into HTML pages or JSON problem documents.
package web

import (
	"log/slog"
	"net/http"

	"github.com/anthdm/superkit/kit"
	"github.com/burstman/wasatn/internal/httpx"
)

// ErrorHandler renders err as a response.
//
// It is installed once at start-up with kit.UseErrorHandler. Because SuperKit
// keeps it in package-level state, it must be set once during boot rather than
// per request or per test in parallel.
func ErrorHandler(k *kit.Kit, err error) {
	status := httpx.StatusOf(err)
	message := httpx.MessageOf(err)

	if status >= 500 {
		// Only unexpected failures are logged with their cause; client mistakes
		// would just be noise.
		slog.ErrorContext(k.Request.Context(), "request failed",
			"request_id", httpx.RequestID(k.Request),
			"method", k.Request.Method,
			"path", k.Request.URL.Path,
			"status", status,
			"err", err.Error(),
		)
	}

	if httpx.WantsJSON(k) {
		_ = httpx.Problem(k, status, ErrorTitle(status), message)
		return
	}

	k.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	k.Response.WriteHeader(status)
	if !bodyAllowedForStatus(status) {
		return
	}

	page := ErrorPage(
		PageData{
			Title:     ErrorTitle(status),
			CSRFToken: httpx.TokenFromRequest(k.Request),
		},
		ErrorView{
			Status:    status,
			Title:     ErrorTitle(status),
			Detail:    message,
			RequestID: httpx.RequestID(k.Request),
		},
	)
	_ = page.Render(k.Request.Context(), k.Response)
}

// bodyAllowedForStatus reports whether a response of this status may carry a
// body.
func bodyAllowedForStatus(status int) bool {
	switch {
	case status >= 100 && status <= 199:
		return false
	case status == http.StatusNoContent:
		return false
	case status == http.StatusNotModified:
		return false
	default:
		return true
	}
}
