package httpx

import (
	"net/http"

	"github.com/google/uuid"
)

// UserID is the session-bound user identifier.
type UserID = uuid.UUID

// SessionReader provides the signed-in user id for a request. It is satisfied by
// auth.Session; declaring it here keeps this package free of a dependency on
// auth, and Go's structural typing means no import cycle is needed.
type SessionReader interface {
	UserID(r *http.Request) uuid.UUID
}
