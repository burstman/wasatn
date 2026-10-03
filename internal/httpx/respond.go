package httpx

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/anthdm/superkit/kit"
)

// JSON writes v as a JSON response.
//
// This exists because kit.Kit.JSON calls WriteHeader before Header().Set, which
// silently drops Content-Type. Always set headers first.
func JSON(k *kit.Kit, status int, v any) error {
	body, err := json.Marshal(v)
	if err != nil {
		return Internal(fmt.Errorf("httpx: marshal json response: %w", err))
	}
	k.Response.Header().Set("Content-Type", "application/json; charset=utf-8")
	k.Response.Header().Set("X-Content-Type-Options", "nosniff")
	k.Response.WriteHeader(status)
	_, err = k.Response.Write(body)
	return err
}

// Problem writes an RFC 9457 problem document, used by HTMX fragments that need
// structured errors (for example campaign validation summaries).
func Problem(k *kit.Kit, status int, title, detail string) error {
	return JSON(k, status, map[string]any{
		"type":   "about:blank",
		"title":  title,
		"detail": detail,
		"status": status,
	})
}

// Method reports the request method.
func Method(k *kit.Kit) string { return k.Request.Method }

// FormValue returns a POST form value.
func FormValue(k *kit.Kit, name string) string { return k.Request.PostFormValue(name) }

// FormInt returns a POST form value parsed as an int, falling back to def.
func FormInt(k *kit.Kit, name string, def int) int {
	raw := FormValue(k, name)
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// IsHTMX reports whether the request came from HTMX. HTMX swaps need different
// responses: HX-Redirect instead of 302, and bare fragments instead of a full
// page.
func IsHTMX(k *kit.Kit) bool { return k.Request.Header.Get("HX-Request") == "true" }

// WantsJSON reports whether the caller expects JSON rather than HTML.
func WantsJSON(k *kit.Kit) bool {
	if IsHTMX(k) {
		// HTMX partials are HTML unless the caller explicitly asks for JSON.
		return k.Request.Header.Get("Accept") == "application/json"
	}
	accept := k.Request.Header.Get("Accept")
	return accept == "application/json" || accept == "application/problem+json"
}

// Redirect sends a redirect, using HX-Redirect for HTMX requests so the client
// performs a full-page navigation. Status is ignored for HTMX requests, which
// always get 204 so the browser does not render an empty body.
func Redirect(k *kit.Kit, status int, url string) error {
	if IsHTMX(k) {
		k.Response.Header().Set("HX-Redirect", url)
		k.Response.WriteHeader(http.StatusNoContent)
		return nil
	}
	http.Redirect(k.Response, k.Request, url, status)
	return nil
}

// NoContent writes a bare 204.
func NoContent(k *kit.Kit) error {
	k.Response.WriteHeader(http.StatusNoContent)
	return nil
}
