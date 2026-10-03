package auth

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
)

func TestNormalizeEmail(t *testing.T) {
	t.Parallel()

	tests := []struct{ in, want string }{
		{in: "user@example.com", want: "user@example.com"},
		{in: "  User@Example.COM  ", want: "user@example.com"},
		{in: "", want: ""},
	}

	for _, tc := range tests {
		if got := NormalizeEmail(tc.in); got != tc.want {
			t.Errorf("NormalizeEmail(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateEmail(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		email   string
		wantErr bool
	}{
		{name: "simple", email: "user@example.com"},
		{name: "subdomain", email: "user@mail.example.co.uk"},
		{name: "plus tag", email: "user+tag@example.com"},
		{name: "dotted local part", email: "first.last@example.com"},
		{name: "empty", email: "", wantErr: true},
		{name: "no at sign", email: "userexample.com", wantErr: true},
		{name: "no local part", email: "@example.com", wantErr: true},
		{name: "no domain", email: "user@", wantErr: true},
		{name: "no dot in domain", email: "user@localhost", wantErr: true},
		{name: "display name", email: "User <user@example.com>", wantErr: true},
		{name: "spaces", email: "user @example.com", wantErr: true},
		{name: "too long", email: strings.Repeat("a", 250) + "@example.com", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidateEmail(tc.email)
			if tc.wantErr && err == nil {
				t.Errorf("ValidateEmail(%q) error = nil, want error", tc.email)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidateEmail(%q) error = %v, want nil", tc.email, err)
			}
		})
	}
}

func TestValidatePassword(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		password string
		wantErr  bool
	}{
		{name: "long enough", password: "correct horse"},
		{name: "exactly minimum", password: strings.Repeat("a", MinPasswordLength)},
		{name: "one under minimum", password: strings.Repeat("a", MinPasswordLength-1), wantErr: true},
		{name: "empty", password: "", wantErr: true},
		{name: "multibyte characters count as runes", password: "éééééééééé"},
		{name: "too many bytes", password: strings.Repeat("a", MaxPasswordBytes+1), wantErr: true},
		{name: "at byte limit", password: strings.Repeat("a", MaxPasswordBytes)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			err := ValidatePassword(tc.password)
			if tc.wantErr && err == nil {
				t.Errorf("ValidatePassword() error = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("ValidatePassword() error = %v, want nil", err)
			}
		})
	}
}

// newTestSession builds a session manager backed by scs's in-memory store.
func newTestSession(t *testing.T) *Session {
	t.Helper()

	return NewSession(scs.New().Store, "test_session", time.Hour, 30*time.Minute, false)
}

func TestSessionLoginAndUser(t *testing.T) {
	t.Parallel()

	s := newTestSession(t)
	want := User{ID: uuid.New(), Email: "user@example.com", Plan: PlanFree}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/login", nil)

	handler := s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := s.UserID(r); got != uuid.Nil {
			t.Errorf("UserID() before login = %s, want uuid.Nil", got)
		}
		if _, ok := s.User(r); ok {
			t.Error("User() before login reported a signed-in user")
		}
		if err := s.Login(r, want); err != nil {
			t.Fatalf("Login() error = %v", err)
		}
		got, ok := s.User(r)
		if !ok {
			t.Fatal("User() after login reported anonymous")
		}
		if got != want {
			t.Errorf("User() = %+v, want %+v", got, want)
		}
	}))
	handler.ServeHTTP(rec, req)

	// An anonymous request with no cookie must not be signed in.
	if _, ok := s.User(httptest.NewRequest("GET", "/dashboard", nil)); ok {
		t.Error("a request without the session cookie reported a signed-in user")
	}
}

func TestSessionLogout(t *testing.T) {
	t.Parallel()

	s := newTestSession(t)
	user := User{ID: uuid.New(), Email: "user@example.com", Plan: PlanPro}

	req := httptest.NewRequest("POST", "/login", nil)
	s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Login(r, user); err != nil {
			t.Fatalf("Login() error = %v", err)
		}
		if err := s.Logout(r); err != nil {
			t.Fatalf("Logout() error = %v", err)
		}
		if _, ok := s.User(r); ok {
			t.Error("User() after Logout reported a signed-in user")
		}
	})).ServeHTTP(httptest.NewRecorder(), req)
}

// Sign-in must invalidate a token the attacker could have planted beforehand,
// otherwise a session fixation attack succeeds.
func TestSessionLoginRenewsToken(t *testing.T) {
	t.Parallel()

	s := newTestSession(t)
	user := User{ID: uuid.New(), Email: "user@example.com", Plan: PlanFree}

	// A visitor arrives with a session cookie already set by an attacker.
	planted := httptest.NewRecorder()
	plantedReq := httptest.NewRequest("GET", "/login", nil)
	s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Manager().Put(r.Context(), "attacker", "planted")
	})).ServeHTTP(planted, plantedReq)

	var plantedCookie string
	for _, c := range planted.Result().Cookies() {
		if c.Name == s.Manager().Cookie.Name {
			plantedCookie = c.Value
		}
	}
	if plantedCookie == "" {
		t.Fatal("no session cookie was issued, cannot test renewal")
	}

	// The victim signs in.
	signedIn := httptest.NewRecorder()
	signInReq := httptest.NewRequest("POST", "/login", nil)
	signInReq.AddCookie(&http.Cookie{Name: s.Manager().Cookie.Name, Value: plantedCookie})
	s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := s.Login(r, user); err != nil {
			t.Fatalf("Login() error = %v", err)
		}
	})).ServeHTTP(signedIn, signInReq)

	var newCookie string
	for _, c := range signedIn.Result().Cookies() {
		if c.Name == s.Manager().Cookie.Name {
			newCookie = c.Value
		}
	}
	if newCookie == "" {
		t.Fatal("sign-in issued no session cookie")
	}
	if newCookie == plantedCookie {
		t.Error("sign-in reused the pre-existing token; a planted token would stay valid")
	}
}

func TestSessionFlash(t *testing.T) {
	t.Parallel()

	s := newTestSession(t)

	s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Flash(r, "success", "Connection added.")

		kind, message := s.TakeFlash(r)
		if kind != "success" || message != "Connection added." {
			t.Errorf("TakeFlash() = %q, %q, want success, Connection added.", kind, message)
		}

		// Flash messages are one-shot.
		if _, again := s.TakeFlash(r); again != "" {
			t.Errorf("TakeFlash() returned %q on the second call, want empty", again)
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/dashboard", nil))
}

func TestSessionUserIgnoresCorruptValue(t *testing.T) {
	t.Parallel()

	s := newTestSession(t)

	s.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.Manager().Put(r.Context(), sessionUserIDKey, "not-a-uuid")
		if got := s.UserID(r); got != uuid.Nil {
			t.Errorf("UserID() = %s, want uuid.Nil for a corrupt value", got)
		}
		if _, ok := s.User(r); ok {
			t.Error("User() reported a signed-in user for a corrupt session")
		}
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/dashboard", nil))
}
