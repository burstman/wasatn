package auth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/alexedwards/scs/v2"
	"github.com/google/uuid"
)

// Password policy.
const (
	// MinPasswordLength keeps brute-force cost high enough to matter with bcrypt.
	MinPasswordLength = 10
	// MaxPasswordBytes is bcrypt's hard input limit. Anything longer is rejected
	// rather than silently truncated, which would make two different passwords
	// equivalent.
	MaxPasswordBytes = 72
	// MaxEmailLength guards against absurd input reaching the database.
	MaxEmailLength = 254
)

// Session keys. Stored server-side in the session store; the cookie only holds
// an opaque token.
const (
	sessionUserIDKey = "user_id"
	sessionEmailKey  = "email"
	sessionPlanKey   = "plan"
)

// Session wraps the scs session manager with WasaTN's conventions.
type Session struct {
	manager *scs.SessionManager
}

// NewSession builds the session manager backed by store (see the pgxstore
// module for the Postgres implementation).
//
// secure should be true in production so cookies are only sent over HTTPS.
func NewSession(store scs.Store, cookieName string, lifetime, idleTimeout time.Duration, secure bool) *Session {
	manager := scs.New()
	manager.Store = store
	manager.Lifetime = lifetime
	manager.IdleTimeout = idleTimeout
	// Store only a hash of the token, so a database leak cannot be replayed.
	manager.HashTokenInStore = true
	manager.Cookie = scs.SessionCookie{
		Name:     cookieName,
		Domain:   "",
		HttpOnly: true,
		Path:     "/",
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		Persist:  true,
	}
	return &Session{manager: manager}
}

// Manager exposes the underlying scs manager for the CSRF middleware.
func (s *Session) Manager() *scs.SessionManager { return s.manager }

// Middleware loads and saves the session. It must wrap every route that reads
// session data.
func (s *Session) Middleware(next http.Handler) http.Handler {
	return s.manager.LoadAndSave(next)
}

// Login marks the session as authenticated for u.
//
// RenewToken is called first: without it an attacker who planted a known token
// in the victim's browser before sign-in would keep a valid session afterwards.
func (s *Session) Login(r *http.Request, u User) error {
	ctx := r.Context()
	if err := s.manager.RenewToken(ctx); err != nil {
		return fmt.Errorf("auth: renew session token: %w", err)
	}
	s.manager.Put(ctx, sessionUserIDKey, u.ID.String())
	s.manager.Put(ctx, sessionEmailKey, u.Email)
	s.manager.Put(ctx, sessionPlanKey, string(u.Plan))
	return nil
}

// Logout clears and destroys the session.
func (s *Session) Logout(r *http.Request) error {
	if err := s.manager.Destroy(r.Context()); err != nil {
		return fmt.Errorf("auth: destroy session: %w", err)
	}
	return nil
}

// getString reads a value from the session, returning "" when the session has
// not been loaded.
//
// scs panics when its context value is missing, which happens if a route reads
// session data without the session middleware in its chain. A routing mistake
// should read as "anonymous" rather than turn every request into a 500. Writes
// are left to panic, because a lost write is a real bug worth surfacing.
func getString(manager *scs.SessionManager, ctx context.Context, key string) (value string) {
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	return manager.GetString(ctx, key)
}

// popString reads and clears a value, behaving like getString on a missing
// session.
func popString(manager *scs.SessionManager, ctx context.Context, key string) (value string) {
	defer func() {
		if recover() != nil {
			value = ""
		}
	}()
	return manager.PopString(ctx, key)
}

// UserID returns the signed-in user id, or uuid.Nil when anonymous. It satisfies
// httpx.SessionReader.
func (s *Session) UserID(r *http.Request) uuid.UUID {
	raw := getString(s.manager, r.Context(), sessionUserIDKey)
	if raw == "" {
		return uuid.Nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// User returns the signed-in user, or false when anonymous.
func (s *Session) User(r *http.Request) (User, bool) {
	id := s.UserID(r)
	if id == uuid.Nil {
		return User{}, false
	}
	ctx := r.Context()
	u := User{
		ID:    id,
		Email: getString(s.manager, ctx, sessionEmailKey),
		Plan:  Plan(getString(s.manager, ctx, sessionPlanKey)),
	}
	return u, true
}

// Flash stores a one-shot message shown after the next redirect.
func (s *Session) Flash(r *http.Request, kind, message string) {
	s.manager.Put(r.Context(), "flash_kind", kind)
	s.manager.Put(r.Context(), "flash_message", message)
}

// flowKeyPrefix namespaces the short-lived values below, so they cannot collide
// with the session's own keys.
const flowKeyPrefix = "flow:"

// PutFlowValue stores a short-lived value tied to the session.
//
// Multi-step flows that leave the app and come back need somewhere to keep a
// value across the round trip: Embedded Signup sends the browser to Facebook and
// returns to a callback, and the state that proves the return is legitimate has
// to survive in the server-side session rather than in a query parameter a
// visitor could edit.
func (s *Session) PutFlowValue(r *http.Request, key, value string) {
	s.manager.Put(r.Context(), flowKeyPrefix+key, value)
}

// TakeFlowValue returns a value set by PutFlowValue and clears it, so a single
// value cannot be replayed.
func (s *Session) TakeFlowValue(r *http.Request, key string) string {
	return popString(s.manager, r.Context(), flowKeyPrefix+key)
}

// TakeFlash returns and clears the pending flash message.
func (s *Session) TakeFlash(r *http.Request) (kind, message string) {
	ctx := r.Context()
	kind = getString(s.manager, ctx, "flash_kind")
	message = popString(s.manager, ctx, "flash_message")
	s.manager.Remove(ctx, "flash_kind")
	return kind, message
}

// NormalizeEmail trims and lowercases an email address. The database enforces
// the same invariant with a CHECK constraint.
func NormalizeEmail(raw string) string {
	return strings.ToLower(strings.TrimSpace(raw))
}

// ValidateEmail checks the address is syntactically usable and not absurdly
// long. mail.ParseAddress accepts display names ("A <a@b.com>"), which must be
// rejected because the address is used as a login identifier.
func ValidateEmail(email string) error {
	if email == "" {
		return errors.New("email is required")
	}
	if len(email) > MaxEmailLength {
		return fmt.Errorf("email must be at most %d characters", MaxEmailLength)
	}
	addr, err := mail.ParseAddress(email)
	if err != nil {
		return errors.New("enter a valid email address")
	}
	if addr.Address != email {
		return errors.New("enter a valid email address without a display name")
	}
	at := strings.LastIndex(email, "@")
	if at <= 0 || at == len(email)-1 {
		return errors.New("enter a valid email address")
	}
	if !strings.Contains(email[at+1:], ".") {
		return errors.New("enter a valid email address")
	}
	return nil
}

// ValidatePassword enforces the password policy.
func ValidatePassword(password string) error {
	if utf8.RuneCountInString(password) < MinPasswordLength {
		return fmt.Errorf("password must be at least %d characters", MinPasswordLength)
	}
	if len(password) > MaxPasswordBytes {
		// bcrypt ignores bytes past 72, which would silently weaken the password.
		return fmt.Errorf("password must be at most %d bytes", MaxPasswordBytes)
	}
	return nil
}
