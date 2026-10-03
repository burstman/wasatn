// Package auth implements email + password authentication with server-side
// sessions.
//
// Domain types live here rather than being passed around as sqlc rows so the
// rest of the application never depends on generated database structs
// (PROMPT.md: "define domain types in internal/<pkg>/types.go").
package auth

import (
	"time"

	"github.com/google/uuid"
)

// Plan is the subscription tier a user is on. It mirrors the user_plan enum in
// the database.
type Plan string

const (
	PlanFree     Plan = "free"
	PlanStarter  Plan = "starter"
	PlanPro      Plan = "pro"
	PlanBusiness Plan = "business"
)

// Valid reports whether p is a known plan.
func (p Plan) Valid() bool {
	switch p {
	case PlanFree, PlanStarter, PlanPro, PlanBusiness:
		return true
	default:
		return false
	}
}

// User is an authenticated account.
type User struct {
	ID        uuid.UUID
	Email     string
	Plan      Plan
	CreatedAt time.Time
}

// Sentinel errors returned by the service.
var (
	// ErrInvalidCredentials is returned for both an unknown email and a wrong
	// password so the endpoint cannot be used to enumerate accounts.
	ErrInvalidCredentials = errInvalidCredentials{}
	ErrEmailTaken         = errEmailTaken{}
)

// errInvalidCredentials and errEmailTaken are distinct types so callers can
// errors.Is them without exporting sentinel values that could be compared
// against a zero value.
type errInvalidCredentials struct{}

func (errInvalidCredentials) Error() string { return "invalid email or password" }

type errEmailTaken struct{}

func (errEmailTaken) Error() string { return "an account with that email already exists" }
