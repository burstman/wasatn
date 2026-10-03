package httpx

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"strings"
	"time"

	"github.com/alexedwards/scs/v2"
)

// CSRFHeader is the header HTMX sends the token in. app.js installs a
// htmx:configRequest listener that copies the token from a meta tag, so every
// non-GET HTMX request is covered without per-form wiring.
const CSRFHeader = "X-CSRF-Token"

// CSRFCookieName is the double-submit cookie holding the token.
const CSRFCookieName = "csrf_token"

// CSRFFormField is the fallback for plain HTML form posts.
const CSRFFormField = "csrf_token"

// CSRF protects state-changing requests using a session-bound double-submit
// token: the value is stored in the scs session and mirrored in a readable
// cookie, and a mutating request must present the same value in either the
// CSRFHeader or CSRFFormField.
//
// Binding to the session (rather than only comparing cookie to parameter) means
// a token stolen from one visitor's page cannot be replayed by another.
type CSRF struct {
	sessions *scs.SessionManager
	secure   bool
}

// NewCSRF builds the CSRF middleware. secure should be true in production so the
// cookie is only sent over HTTPS.
func NewCSRF(sessions *scs.SessionManager, secure bool) *CSRF {
	return &CSRF{sessions: sessions, secure: secure}
}

// Token returns the session's CSRF token, generating and storing one on first
// use. It never returns an error: a fresh random token is always available.
//
// It returns "" when the session has not been loaded, which fails verification
// rather than panicking. scs panics on a missing context value, so a route that
// reads the token outside the middleware chain must not take the process down.
func (c *CSRF) Token(r *http.Request) string {
	ctx := r.Context()
	if existing := sessionString(c.sessions, ctx, CSRFCookieName); existing != "" {
		return existing
	}
	token, err := newCSRFToken()
	if err != nil {
		// crypto/rand failing is fatal for correctness; fall back to a value that
		// will simply fail verification rather than silently disabling the check.
		return ""
	}
	c.sessions.Put(ctx, CSRFCookieName, token)
	return token
}

// sessionString reads a session string, treating an unloaded session as empty.
func sessionString(manager *scs.SessionManager, ctx context.Context, key string) (value string) {
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	return manager.GetString(ctx, key)
}

// Middleware returns the CSRF-protected handler.
//
// It applies the session middleware itself, because reading the session token
// requires a loaded session. Owning that ordering here removes the footgun of
// registering the two middlewares in the wrong order, which would panic at
// request time.
func (c *CSRF) Middleware(next http.Handler) http.Handler {
	return c.sessions.LoadAndSave(c.check(next))
}

// check verifies the token on unsafe methods and refreshes the cookie on safe
// ones.
func (c *CSRF) check(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if csrfExempt(r.URL.Path) {
			// These endpoints authenticate themselves and have no session, so
			// there is no token to check and no cookie to hand out.
			next.ServeHTTP(w, r)
			return
		}

		token := c.Token(r)

		if !isSafeMethod(r.Method) {
			presented := r.Header.Get(CSRFHeader)
			if presented == "" {
				presented = r.FormValue(CSRFFormField)
			}
			expected := sessionString(c.sessions, r.Context(), CSRFCookieName)
			if expected == "" || subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return
			}
		}

		// Refresh on safe requests so the cookie is present for the first POST.
		if isSafeMethod(r.Method) && token != "" {
			http.SetCookie(w, c.cookie(token))
		}
		next.ServeHTTP(w, r)
	})
}

// csrfExemptPaths are endpoints that cannot present a CSRF token.
//
// Meta's webhook delivery is authenticated by an HMAC signature over the raw
// body instead, and its subscription challenge by a shared verify token. Listing
// exact paths rather than a prefix keeps this from silently exempting a future
// state-changing route.
var csrfExemptPaths = map[string]bool{
	"/webhooks/whatsapp": true,
}

func csrfExempt(path string) bool {
	return csrfExemptPaths[path]
}

func (c *CSRF) cookie(token string) *http.Cookie {
	return &http.Cookie{
		Name:     CSRFCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: false, // read by app.js to populate the header
		Secure:   c.secure,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	}
}

func isSafeMethod(method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodOptions, http.MethodTrace:
		return true
	default:
		return false
	}
}

func newCSRFToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

// TokenFromRequest returns the token embedded in a rendered page, if any.
func TokenFromRequest(r *http.Request) string {
	if v := r.Header.Get(CSRFHeader); v != "" {
		return strings.TrimSpace(v)
	}
	if c, err := r.Cookie(CSRFCookieName); err == nil {
		return c.Value
	}
	return ""
}
