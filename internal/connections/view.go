package connections

import (
	"time"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/web"
)

// expiryFormat is how a token expiry is shown. The year matters: a token lives
// 60 days, so a customer looking at this page needs to know which year the date
// belongs to.
const expiryFormat = "2 Jan 2006"

// viewRows maps stored connections onto the connections page.
func viewRows(rows []sqlc.Connection, now time.Time) []web.ConnectionView {
	views := make([]web.ConnectionView, 0, len(rows))
	for _, row := range rows {
		views = append(views, viewRow(row, now))
	}
	return views
}

// viewRow maps one connection.
//
// Expiry is computed here rather than in SQL because it is a comparison against
// the current time, and because the warning window is a product decision that
// belongs with the presentation.
func viewRow(row sqlc.Connection, now time.Time) web.ConnectionView {
	label, kind := statusBadge(row.Status)

	return web.ConnectionView{
		ID:             row.ID.String(),
		AccountID:      row.MessagingAccountID,
		PhoneNumber:    PhoneNumber(row),
		DisplayName:    DisplayName(row),
		Status:         string(row.Status),
		StatusLabel:    label,
		StatusKind:     kind,
		QualityRating:  qualityLabel(row.QualityRating),
		MessagingLimit: int(row.MessagingLimit),
		TokenExpires:   row.TokenExpiresAt.UTC().Format(expiryFormat),
		TokenState:     tokenState(row, now),
	}
}

// statusBadge turns a connection status into a readable label and a daisyUI badge
// modifier.
func statusBadge(status sqlc.ConnectionStatus) (label, kind string) {
	switch status {
	case sqlc.ConnectionStatusActive:
		return "Active", "badge-success"
	case sqlc.ConnectionStatusPendingPhone:
		return "Needs a number", "badge-warning"
	case sqlc.ConnectionStatusTokenExpired:
		return "Token expired", "badge-error"
	case sqlc.ConnectionStatusDisconnected:
		return "Disconnected", "badge-ghost"
	default:
		// An unexpected status is shown verbatim rather than claimed to be
		// healthy: this is a page an operator may have to read.
		return string(status), "badge-ghost"
	}
}

// qualityLabel explains Meta's rating, which is otherwise four bare words.
func qualityLabel(rating string) string {
	switch rating {
	case "GREEN":
		return "High"
	case "YELLOW":
		return "Medium"
	case "RED":
		return "Low"
	// A number that has never sent has no rating, and signup records that as
	// UNKNOWN because the column is NOT NULL. Both spellings mean the same thing
	// to the customer.
	case "", "UNKNOWN":
		return "Unknown"
	default:
		return rating
	}
}

// tokenState collapses expiry into what the customer needs to see.
//
// An expired token on an active connection is the case worth calling out: it is
// what happens 60 days after a signup, and Meta would otherwise fail every send
// with an error that says nothing useful.
func tokenState(row sqlc.Connection, now time.Time) string {
	if !row.TokenExpiresAt.After(now) {
		return "expired"
	}
	if ExpiringSoon(row, now, TokenWarningWindow) {
		return "expiring"
	}
	return "ok"
}
