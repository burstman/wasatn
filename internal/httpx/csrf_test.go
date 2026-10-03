package httpx

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
)

// newCSRFSession returns a session manager backed by the default in-memory
// store, which is enough for CSRF tests.
func newCSRFSession() *scs.SessionManager {
	m := scs.New()
	m.Lifetime = time.Hour
	m.Cookie = scs.SessionCookie{Name: "test_session", HttpOnly: true, Path: "/"}
	return m
}

func TestCSRFMiddleware(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		method     string
		useToken   bool
		override   string
		wantStatus int
		wantNext   bool
	}{
		{name: "safe GET allowed without token", method: http.MethodGet, useToken: false, wantStatus: http.StatusNoContent, wantNext: true},
		{name: "HEAD allowed without token", method: http.MethodHead, useToken: false, wantStatus: http.StatusNoContent, wantNext: true},
		{name: "POST with valid header", method: http.MethodPost, useToken: true, wantStatus: http.StatusNoContent, wantNext: true},
		{name: "POST without token rejected", method: http.MethodPost, useToken: false, wantStatus: http.StatusForbidden, wantNext: false},
		{name: "POST with wrong token rejected", method: http.MethodPost, useToken: true, override: "deadbeef", wantStatus: http.StatusForbidden, wantNext: false},
		{name: "PUT with valid header", method: http.MethodPut, useToken: true, wantStatus: http.StatusNoContent, wantNext: true},
		{name: "DELETE without token rejected", method: http.MethodDelete, useToken: false, wantStatus: http.StatusForbidden, wantNext: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sessions := newCSRFSession()
			csrf := NewCSRF(sessions, false)

			reached := false
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				w.WriteHeader(http.StatusNoContent)
			})

			handler := csrf.Middleware(next)

			// First request a token so the session cookie exists.
			seed := httptest.NewRecorder()
			handler.ServeHTTP(seed, httptest.NewRequest(http.MethodGet, "/", nil))
			reached = false

			req := httptest.NewRequest(tc.method, "/", strings.NewReader(""))
			for _, c := range seed.Result().Cookies() {
				if c.Name == sessions.Cookie.Name {
					req.AddCookie(c)
				}
			}
			if tc.useToken {
				token := cookieValue(seed.Result().Cookies(), CSRFCookieName)
				if tc.override != "" {
					token = tc.override
				}
				req.Header.Set(CSRFHeader, token)
			}

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d (body %q)", rec.Code, tc.wantStatus, rec.Body.String())
			}
			if reached != tc.wantNext {
				t.Errorf("next handler reached = %v, want %v", reached, tc.wantNext)
			}
		})
	}
}

func TestCSRFTokenIsStableWithinSession(t *testing.T) {
	t.Parallel()

	sessions := newCSRFSession()
	csrf := NewCSRF(sessions, false)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	csrf.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		first := csrf.Token(r)
		second := csrf.Token(r)
		if first != second {
			t.Errorf("Token() returned %q then %q within one request", first, second)
		}
		if len(first) != 64 {
			t.Errorf("Token() length = %d, want 64 hex characters", len(first))
		}
	})).ServeHTTP(rec, req)
}

func TestCSRFFormFieldFallback(t *testing.T) {
	t.Parallel()

	sessions := newCSRFSession()
	csrf := NewCSRF(sessions, false)

	collect := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := csrf.Middleware(collect)

	seed := httptest.NewRecorder()
	handler.ServeHTTP(seed, httptest.NewRequest(http.MethodGet, "/", nil))

	// A plain HTML form posts the token as a field, not a header.
	form := url.Values{"csrf_token": {cookieValue(seed.Result().Cookies(), CSRFCookieName)}}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for _, c := range seed.Result().Cookies() {
		if c.Name == sessions.Cookie.Name {
			req.AddCookie(c)
		}
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want %d; form field token should be accepted", rec.Code, http.StatusNoContent)
	}
}

func TestCSRFTokenFromRequest(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		header string
		cookie string
		want   string
	}{
		{name: "header wins", header: "from-header", cookie: "from-cookie", want: "from-header"},
		{name: "falls back to cookie", cookie: "from-cookie", want: "from-cookie"},
		{name: "neither", want: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			if tc.header != "" {
				req.Header.Set(CSRFHeader, tc.header)
			}
			if tc.cookie != "" {
				req.AddCookie(&http.Cookie{Name: CSRFCookieName, Value: tc.cookie})
			}
			if got := TokenFromRequest(req); got != tc.want {
				t.Errorf("TokenFromRequest() = %q, want %q", got, tc.want)
			}
		})
	}
}

func cookieValue(cookies []*http.Cookie, name string) string {
	for _, c := range cookies {
		if c.Name == name {
			return c.Value
		}
	}
	return ""
}
