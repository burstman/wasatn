package auth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/anthdm/superkit/kit"
	"github.com/burstman/wasatn/internal/httpx"
	"golang.org/x/crypto/bcrypt"
)

// harness wires real handlers around the fake data layer and drives them the
// way a browser does: load a page to obtain the session cookie and CSRF token,
// then submit the form with both.
type harness struct {
	handlers *Handlers
	service  *Service
	queries  *fakeQuerier
	session  *Session
	csrf     *httpx.CSRF

	sessionCookie *http.Cookie
	csrfToken     string
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	q := newFakeQuerier()
	session := NewSession(scs.New().Store, "test_session", time.Hour, 30*time.Minute, false)
	csrf := httpx.NewCSRF(session.Manager(), false)
	service := NewService(q, bcrypt.MinCost, quiet)

	return &harness{
		handlers: NewHandlers(service, session, csrf, quiet),
		service:  service,
		queries:  q,
		session:  session,
		csrf:     csrf,
	}
}

// render serves a page with the session loaded but no CSRF check, which is what
// the router does for safe methods.
func (h *harness) render(handler kit.HandlerFunc, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.session.Middleware(kit.Handler(handler)).ServeHTTP(rec, req)
	return rec
}

// captureSession loads the sign-in page and keeps whatever cookies it sets, so
// later submissions look like they come from a real browser session.
func (h *harness) captureSession(t *testing.T) {
	t.Helper()

	// The CSRF cookie is written by the CSRF middleware, so the page load must go
	// through it, exactly as it does in the router.
	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(h.handlers.LoginPage())).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))
	h.absorbCookies(rec)
	if h.sessionCookie == nil {
		t.Fatal("loading the sign-in page issued no session cookie")
	}
	if h.csrfToken == "" {
		t.Fatal("loading the sign-in page issued no CSRF token")
	}
}

// absorbCookies remembers the newest session cookie and CSRF token. Signing in
// renews the session token, so the harness must track the latest one.
func (h *harness) absorbCookies(rec *httptest.ResponseRecorder) {
	for _, c := range rec.Result().Cookies() {
		switch c.Name {
		case h.session.Manager().Cookie.Name:
			// A negative MaxAge deletes the cookie; do not keep it.
			if c.Value != "" && c.MaxAge >= 0 {
				h.sessionCookie = c
			}
		case httpx.CSRFCookieName:
			if c.Value != "" {
				h.csrfToken = c.Value
			}
		}
	}
}

// request builds a request carrying the current cookies.
func (h *harness) request(method, target string, body url.Values) *http.Request {
	var req *http.Request
	if body == nil {
		req = httptest.NewRequest(method, target, nil)
	} else {
		req = httptest.NewRequest(method, target, strings.NewReader(body.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range []*http.Cookie{h.sessionCookie, {Name: httpx.CSRFCookieName, Value: h.csrfToken}} {
		if c != nil {
			req.AddCookie(c)
		}
	}
	return req
}

// submit performs a CSRF-checked form submission and keeps the resulting
// cookies.
func (h *harness) submit(t *testing.T, handler kit.HandlerFunc, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if h.sessionCookie == nil {
		h.captureSession(t)
	}

	values = cloneValues(values)
	values.Set("csrf_token", h.csrfToken)

	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(handler)).ServeHTTP(rec, h.request(http.MethodPost, path, values))
	h.absorbCookies(rec)
	return rec
}

// submitWithoutToken submits with no CSRF token, to prove the request is
// stopped before the handler runs.
func (h *harness) submitWithoutToken(t *testing.T, handler kit.HandlerFunc, path string, values url.Values) *httptest.ResponseRecorder {
	t.Helper()
	if h.sessionCookie == nil {
		h.captureSession(t)
	}

	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(handler)).ServeHTTP(rec, h.request(http.MethodPost, path, cloneValues(values)))
	return rec
}

// formRequest builds an urlencoded form submission.
func formRequest(t *testing.T, path string, values url.Values) *http.Request {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return req
}

// runLoaded serves handler with the session loaded and returns the error the
// handler produced, the same value the router hands to the central error
// handler.
func (h *harness) runLoaded(handler kit.HandlerFunc, req *http.Request) error {
	var captured error
	probe := kit.HandlerFunc(func(k *kit.Kit) error {
		captured = handler(k)
		// Swallow it so kit does not invoke its global error handler, which is
		// shared process-wide state.
		return nil
	})
	h.session.Middleware(kit.Handler(probe)).ServeHTTP(httptest.NewRecorder(), req)
	return captured
}

func cloneValues(values url.Values) url.Values {
	out := make(url.Values, len(values))
	for k, v := range values {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// register creates an account through the real handler.
func (h *harness) register(t *testing.T, email, password string) *httptest.ResponseRecorder {
	t.Helper()
	return h.submit(t, h.handlers.Register(), "/register", url.Values{
		"email":    {email},
		"password": {password},
	})
}

const testPassword = "correct horse battery"

func TestRegisterPage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rec := h.render(h.handlers.RegisterPage(), httptest.NewRequest(http.MethodGet, "/register", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{"Create account", `name="email"`, `name="password"`, "csrf_token"} {
		if !strings.Contains(body, want) {
			t.Errorf("page does not contain %q", want)
		}
	}
}

func TestLoginPage(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rec := h.render(h.handlers.LoginPage(), httptest.NewRequest(http.MethodGet, "/login", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Sign in") {
		t.Error("page does not contain the sign-in heading")
	}
}

func TestRegisterSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rec := h.register(t, "user@example.com", testPassword)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want /dashboard", got)
	}
	if h.sessionCookie == nil {
		t.Error("no session cookie issued, want the user signed in")
	}
	if len(h.queries.users) != 1 {
		t.Errorf("stored %d users, want 1", len(h.queries.users))
	}

	// The session must now identify the new user.
	req := h.request(http.MethodGet, "/dashboard", nil)
	rec = httptest.NewRecorder()
	h.session.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, ok := h.session.User(r)
		if !ok {
			t.Error("session does not identify a user after registration")
			return
		}
		if user.Email != "user@example.com" {
			t.Errorf("session email = %q, want user@example.com", user.Email)
		}
		if user.Plan != PlanFree {
			t.Errorf("session plan = %q, want %q", user.Plan, PlanFree)
		}
	})).ServeHTTP(rec, req)
}

func TestRegisterValidationErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		values   url.Values
		wantBody string
	}{
		{
			name:     "invalid email",
			values:   url.Values{"email": {"not-an-email"}, "password": {testPassword}},
			wantBody: "valid email",
		},
		{
			name:     "short password",
			values:   url.Values{"email": {"user@example.com"}, "password": {"short"}},
			wantBody: "at least 10 characters",
		},
		{
			name:     "missing password",
			values:   url.Values{"email": {"user@example.com"}},
			wantBody: "at least 10 characters",
		},
		{
			name:     "password over the bcrypt limit",
			values:   url.Values{"email": {"user@example.com"}, "password": {strings.Repeat("a", 100)}},
			wantBody: "at most 72 bytes",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			rec := h.submit(t, h.handlers.Register(), "/register", tc.values)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
			if len(h.queries.users) != 0 {
				t.Error("a user was stored despite a validation failure")
			}
			if !strings.Contains(rec.Body.String(), tc.wantBody) {
				t.Errorf("body does not contain %q", tc.wantBody)
			}
		})
	}
}

func TestRegisterDuplicateEmail(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
		t.Fatalf("first registration status = %d, want 303", rec.Code)
	}

	// Sign out first: the second attempt needs a session that is not already
	// authenticated for the redirect-to-dashboard path.
	req := h.request(http.MethodPost, "/logout", nil)
	req.Header.Set("CSRF-Token", h.csrfToken)
	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(h.handlers.Logout())).ServeHTTP(rec, req)
	h.absorbCookies(rec)

	rec = h.submit(t, h.handlers.Register(), "/register", url.Values{
		"email": {"user@example.com"}, "password": {testPassword},
	})

	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Error("body does not explain the duplicate email")
	}
	if len(h.queries.users) != 1 {
		t.Errorf("stored %d users, want 1", len(h.queries.users))
	}
}

// A database failure must not be shown to the user.
func TestRegisterDatabaseFailureIsInternal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.queries.createErr = errors.New("connection refused")

	err := h.runLoaded(h.handlers.Register(), formRequest(t, "/register", url.Values{
		"email": {"user@example.com"}, "password": {testPassword},
	}))

	if err == nil {
		t.Fatal("handler error = nil, want an error")
	}
	if httpx.StatusOf(err) != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", httpx.StatusOf(err))
	}
	if strings.Contains(httpx.MessageOf(err), "connection refused") {
		t.Error("the database error leaked into the user-facing message")
	}
}

func TestLoginSuccess(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
		t.Fatalf("registration status = %d, want 303", rec.Code)
	}

	// Sign out, then sign back in with the stored credentials.
	logout(t, h)

	rec := h.submit(t, h.handlers.Login(), "/login", url.Values{
		"email": {"user@example.com"}, "password": {testPassword},
	})

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303; body %q", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/dashboard" {
		t.Errorf("Location = %q, want /dashboard", got)
	}

	req := h.request(http.MethodGet, "/dashboard", nil)
	h.session.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.session.User(r); !ok {
			t.Error("session does not identify a user after sign-in")
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
}

// Login must not reveal whether the address exists.
func TestLoginRejectsBadCredentialsWithoutLeaking(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		values url.Values
	}{
		{name: "wrong password", values: url.Values{"email": {"user@example.com"}, "password": {"wrong password"}}},
		{name: "unknown email", values: url.Values{"email": {"nobody@example.com"}, "password": {testPassword}}},
		{name: "empty email", values: url.Values{"email": {""}, "password": {testPassword}}},
		{name: "empty password", values: url.Values{"email": {"user@example.com"}, "password": {""}}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
				t.Fatalf("registration status = %d, want 303", rec.Code)
			}
			logout(t, h)

			rec := h.submit(t, h.handlers.Login(), "/login", tc.values)

			if rec.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422", rec.Code)
			}
			body := rec.Body.String()
			if !strings.Contains(body, "Invalid email or password") {
				t.Errorf("body does not show the generic message: %q", body)
			}
			if strings.Contains(body, "no account") || strings.Contains(body, "does not exist") {
				t.Error("the response reveals whether the account exists")
			}

			// The session must stay anonymous.
			req := h.request(http.MethodGet, "/dashboard", nil)
			h.session.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, ok := h.session.User(r); ok {
					t.Error("a session was created for failed sign-in")
				}
			})).ServeHTTP(httptest.NewRecorder(), req)
		})
	}
}

// The sign-in pages send an already-authenticated visitor to the dashboard.
func TestAuthPagesRedirectSignedInUsers(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
		t.Fatalf("registration status = %d, want 303", rec.Code)
	}

	for _, tc := range []struct {
		name    string
		handler kit.HandlerFunc
	}{
		{name: "login", handler: h.handlers.LoginPage()},
		{name: "register", handler: h.handlers.RegisterPage()},
	} {
		rec := h.render(tc.handler, h.request(http.MethodGet, "/"+tc.name, nil))

		if rec.Code != http.StatusSeeOther {
			t.Errorf("%s page status = %d, want 303", tc.name, rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/dashboard" {
			t.Errorf("%s page Location = %q, want /dashboard", tc.name, got)
		}
	}
}

func TestLogout(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
		t.Fatalf("registration status = %d, want 303", rec.Code)
	}

	// Keep the cookie that was in use during sign-out so it can be replayed.
	stale := h.sessionCookie

	rec := logout(t, h)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303", rec.Code)
	}
	if got := rec.Header().Get("Location"); got != "/login" {
		t.Errorf("Location = %q, want /login", got)
	}

	// Replaying the pre-logout cookie must not authenticate.
	replay := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	replay.AddCookie(stale)
	h.session.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := h.session.User(r); ok {
			t.Error("the pre-logout session cookie still authenticates")
		}
	})).ServeHTTP(httptest.NewRecorder(), replay)
}

// logout signs the current harness session out.
func logout(t *testing.T, h *harness) *httptest.ResponseRecorder {
	t.Helper()

	req := h.request(http.MethodPost, "/logout", url.Values{"csrf_token": {h.csrfToken}})
	req.Header.Set(httpx.CSRFHeader, h.csrfToken)

	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(h.handlers.Logout())).ServeHTTP(rec, req)
	h.absorbCookies(rec)

	// Logout destroys the session, so the next request must start a fresh one,
	// exactly as a browser would when it lands back on /login.
	h.captureSession(t)
	return rec
}

func TestRequireAuth(t *testing.T) {
	t.Parallel()

	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	t.Run("anonymous redirects", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		rec := httptest.NewRecorder()
		h.handlers.RequireAuth(ok).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/dashboard", nil))

		if rec.Code != http.StatusSeeOther {
			t.Fatalf("status = %d, want 303", rec.Code)
		}
		if got := rec.Header().Get("Location"); got != "/login" {
			t.Errorf("Location = %q, want /login", got)
		}
	})

	t.Run("anonymous htmx request is told to redirect", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.Header.Set("HX-Request", "true")

		rec := httptest.NewRecorder()
		h.handlers.RequireAuth(ok).ServeHTTP(rec, req)

		if rec.Code != http.StatusNoContent {
			t.Errorf("status = %d, want 204", rec.Code)
		}
		if got := rec.Header().Get("HX-Redirect"); got != "/login" {
			t.Errorf("HX-Redirect = %q, want /login", got)
		}
	})

	t.Run("signed-in user passes through", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
			t.Fatalf("registration status = %d, want 303", rec.Code)
		}

		rec := httptest.NewRecorder()
		h.session.Middleware(h.handlers.RequireAuth(ok)).ServeHTTP(rec, h.request(http.MethodGet, "/dashboard", nil))

		if rec.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", rec.Code)
		}
	})

	t.Run("forged session token does not authenticate", func(t *testing.T) {
		t.Parallel()

		h := newHarness(t)
		req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
		req.AddCookie(&http.Cookie{Name: "test_session", Value: "forged-token-value"})

		rec := httptest.NewRecorder()
		h.session.Middleware(h.handlers.RequireAuth(ok)).ServeHTTP(rec, req)

		if rec.Code != http.StatusSeeOther {
			t.Errorf("status = %d, want 303; a forged token must not authenticate", rec.Code)
		}
	})
}

// Regression guard: the auth forms must carry the CSRF token themselves.
// The header hook in static/js/app.js only covers HTMX requests, so without a
// hidden field a browser with JavaScript disabled could never sign in.
func TestAuthFormsCarryTheCSRFToken(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(h.handlers.LoginPage())).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/login", nil))

	body := rec.Body.String()
	if !strings.Contains(body, `name="csrf_token"`) {
		t.Fatal("the sign-in form has no csrf_token field")
	}

	// A plain form POST with no HTMX headers must still be accepted.
	if rec := h.submit(t, h.handlers.Login(), "/login", url.Values{
		"email": {"user@example.com"}, "password": {testPassword},
	}); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("plain form POST status = %d, want 422 (reached the handler, not 403)", rec.Code)
	}
}

// A POST without a valid CSRF token must be stopped before the handler runs.
func TestCSRFBlocksSubmissionsWithoutToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		handler func(h *harness) kit.HandlerFunc
		path    string
	}{
		{name: "login", handler: func(h *harness) kit.HandlerFunc { return h.handlers.Login() }, path: "/login"},
		{name: "register", handler: func(h *harness) kit.HandlerFunc { return h.handlers.Register() }, path: "/register"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			h := newHarness(t)
			rec := h.submitWithoutToken(t, tc.handler(h), tc.path, url.Values{
				"email": {"user@example.com"}, "password": {testPassword},
			})

			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
			if len(h.queries.users) != 0 {
				t.Error("the handler ran despite the missing CSRF token")
			}
		})
	}
}

// A wrong CSRF token must be rejected too.
func TestCSRFBlocksSubmissionsWithWrongToken(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.captureSession(t)

	req := h.request(http.MethodPost, "/register", url.Values{
		"email": {"user@example.com"}, "password": {testPassword}, "csrf_token": {"wrong"},
	})

	rec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(h.handlers.Register())).ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403", rec.Code)
	}
	if len(h.queries.users) != 0 {
		t.Error("the handler ran despite an invalid CSRF token")
	}
}

// HTMX form submissions get the bare form fragment back, not a whole document.
func TestAuthErrorReturnsFragmentForHTMX(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	if rec := h.register(t, "user@example.com", testPassword); rec.Code != http.StatusSeeOther {
		t.Fatalf("registration status = %d, want 303", rec.Code)
	}
	logout(t, h)

	values := url.Values{"email": {"user@example.com"}, "password": {"wrong password"}, "csrf_token": {h.csrfToken}}
	htmxReq := h.request(http.MethodPost, "/login", values)
	htmxReq.Header.Set("HX-Request", "true")
	htmxReq.Header.Set(httpx.CSRFHeader, h.csrfToken)

	htmxRec := httptest.NewRecorder()
	h.csrf.Middleware(kit.Handler(h.handlers.Login())).ServeHTTP(htmxRec, htmxReq)

	if htmxRec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", htmxRec.Code)
	}
	body := htmxRec.Body.String()
	if strings.Contains(body, "<html") {
		t.Error("HTMX request received a full HTML document, want the form fragment")
	}
	if !strings.Contains(body, "Invalid email or password") {
		t.Error("fragment does not contain the error message")
	}
}

// Service errors must never be reported to the browser as-is.
func TestLoginDatabaseFailureIsInternal(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.queries.getErr = errors.New("dial tcp 10.0.0.7:5432: connect: connection refused")

	err := h.runLoaded(h.handlers.Login(), formRequest(t, "/login", url.Values{
		"email": {"user@example.com"}, "password": {testPassword},
	}))

	if err == nil {
		t.Fatal("handler error = nil, want an error")
	}
	if httpx.StatusOf(err) != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", httpx.StatusOf(err))
	}
	if strings.Contains(httpx.MessageOf(err), "10.0.0.7") {
		t.Error("the database host leaked into the user-facing message")
	}
}

// A handler must not lose the request context, which carries cancellation and
// request-scoped values.
func TestHandlerKeepsRequestContext(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	type ctxKey struct{}

	req := httptest.NewRequest(http.MethodGet, "/register", nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKey{}, "carried"))

	var seen any
	rec := httptest.NewRecorder()
	h.session.Middleware(kit.Handler(func(k *kit.Kit) error {
		seen = k.Request.Context().Value(ctxKey{})
		return nil
	})).ServeHTTP(rec, req)

	if seen != "carried" {
		t.Error("the handler request context was replaced")
	}
}
