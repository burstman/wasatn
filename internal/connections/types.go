// Package connections owns the WhatsApp numbers a customer has connected to
// WasaTN: the Embedded Signup flow that produces them, and the rules for what
// state a connection may be in.
package connections

import (
	"errors"
	"time"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/phone"
	"github.com/burstman/wasatn/internal/whatsapp"
	"github.com/google/uuid"
)

// Status mirrors the connection_status enum in the schema.
type Status = sqlc.ConnectionStatus

// Re-exported so callers do not have to import sqlc to compare a status.
const (
	StatusActive       = sqlc.ConnectionStatusActive
	StatusDisconnected = sqlc.ConnectionStatusDisconnected
	StatusTokenExpired = sqlc.ConnectionStatusTokenExpired
	StatusPendingPhone = sqlc.ConnectionStatusPendingPhone
)

// Sentinel errors the handlers map onto HTTP responses.
var (
	// ErrNotFound is returned for a connection id that does not exist, or does
	// not belong to the signed-in user. The two are deliberately
	// indistinguishable: telling them apart would confirm that a connection id
	// exists in some other tenant.
	ErrNotFound = errors.New("connections: connection not found")

	// ErrNumberOwnedByAnotherAccount means the WhatsApp number is already
	// attached to a different WasaTN user. The schema enforces one owner per
	// number; this turns the violation into something the signup page can
	// explain instead of a 500.
	ErrNumberOwnedByAnotherAccount = errors.New("connections: phone number is already connected to another account")

	// ErrAlreadyDisconnected means the disconnect was a no-op because Meta or an
	// earlier click had already disconnected it.
	ErrAlreadyDisconnected = errors.New("connections: connection is already disconnected")

	// ErrNothingConnected means the signup succeeded but Meta returned no
	// WhatsApp Business Accounts, which points at a permissions problem rather
	// than a bug.
	ErrNothingConnected = errors.New("connections: no WhatsApp Business Account was shared")

	// ErrSignupNotConfigured means the server has no Meta app credentials, so
	// there is nothing to exchange the authorization code with. It is a
	// deployment problem rather than a customer mistake.
	ErrSignupNotConfigured = errors.New("connections: Meta Embedded Signup is not configured on this server")
)

// Result summarises one Embedded Signup run, so the handler can report what
// happened without re-reading the connections table.
type Result struct {
	// Connections are the rows written or updated.
	Connections []sqlc.Connection
	// Accounts is how many WhatsApp Business Accounts Meta reported. It is
	// larger than the number of rows when an account has no phone number yet.
	Accounts int
	// PendingPhone is true when at least one account was stored without a phone
	// number, which Embedded Signup v4 allows.
	PendingPhone bool
}

// DisplayName is what the customer recognises a connection by: the verified name
// Meta shows on messages, falling back to the account id.
func DisplayName(c sqlc.Connection) string {
	if c.DisplayName != "" {
		return c.DisplayName
	}
	return c.MessagingAccountID
}

// PhoneNumber returns the stored E.164 number, or "" when the connection is
// still waiting for one.
func PhoneNumber(c sqlc.Connection) string {
	if c.PhoneNumber == nil {
		return ""
	}
	return *c.PhoneNumber
}

// ExpiringSoon reports whether the token is close enough to expiry to warn
// about, while there is still time for the customer to reconnect.
func ExpiringSoon(c sqlc.Connection, now time.Time, window time.Duration) bool {
	if c.Status != StatusActive {
		return false
	}
	return !c.TokenExpiresAt.After(now.Add(window))
}

// upsertParams assembles the row for one phone number.
//
// The access token is filled in by the caller, since it must be encrypted fresh
// for every row.
//
// A nil number produces a row with no phone, which is only legal with the
// pending_phone status; the schema enforces that pairing.
func upsertParams(userID uuid.UUID, account whatsapp.BusinessAccount, tokenExpiresAt time.Time, number *whatsapp.PhoneNumber, status Status) (sqlc.UpsertConnectionParams, error) {
	params := sqlc.UpsertConnectionParams{
		UserID:             userID,
		MessagingAccountID: account.ID,
		TokenExpiresAt:     tokenExpiresAt,
		// The column is NOT NULL and Meta omits quality_rating for a number that
		// has never sent.
		QualityRating: "UNKNOWN",
		Status:        status,
	}

	if number == nil {
		return params, nil
	}

	normalized := phone.Normalize(number.DisplayPhoneNumber)
	if !phone.IsE164(normalized) {
		// The database CHECK would reject this and, worse, a guessed number could
		// message the wrong person. The caller records the account without the
		// number instead.
		return params, phone.ErrNotE164
	}

	id := number.ID
	params.PhoneNumberID = &id
	params.PhoneNumber = &normalized
	params.DisplayName = firstNonEmpty(number.VerifiedName, account.Name, account.ID)
	if number.QualityRating != "" {
		params.QualityRating = number.QualityRating
	}
	if number.MessagingLimit > 0 {
		params.MessagingLimit = int32(number.MessagingLimit)
	}
	return params, nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
