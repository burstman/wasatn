package connections

import (
	"testing"
	"time"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/google/uuid"
)

func TestStatusBadgeCoversEveryStatus(t *testing.T) {
	tests := []struct {
		status    sqlc.ConnectionStatus
		wantLabel string
		wantKind  string
	}{
		{status: sqlc.ConnectionStatusActive, wantLabel: "Active", wantKind: "badge-success"},
		{status: sqlc.ConnectionStatusPendingPhone, wantLabel: "Needs a number", wantKind: "badge-warning"},
		{status: sqlc.ConnectionStatusTokenExpired, wantLabel: "Token expired", wantKind: "badge-error"},
		{status: sqlc.ConnectionStatusDisconnected, wantLabel: "Disconnected", wantKind: "badge-ghost"},
		// An operator may have to read this page, so an unknown status is shown
		// as it is rather than dressed up as healthy.
		{status: sqlc.ConnectionStatus("paused_by_operator"), wantLabel: "paused_by_operator", wantKind: "badge-ghost"},
	}

	for _, tc := range tests {
		label, kind := statusBadge(tc.status)
		if label != tc.wantLabel || kind != tc.wantKind {
			t.Errorf("statusBadge(%q) = %q/%q, want %q/%q", tc.status, label, kind, tc.wantLabel, tc.wantKind)
		}
	}
}

func TestQualityLabelExplainsMetaRatings(t *testing.T) {
	tests := []struct {
		rating string
		want   string
	}{
		{rating: "GREEN", want: "High"},
		{rating: "YELLOW", want: "Medium"},
		{rating: "RED", want: "Low"},
		{rating: "UNKNOWN", want: "Unknown"},
		{rating: "", want: "Unknown"},
		// A rating WasaTN has not heard of is passed through untouched.
		{rating: "NA", want: "NA"},
	}

	for _, tc := range tests {
		if got := qualityLabel(tc.rating); got != tc.want {
			t.Errorf("qualityLabel(%q) = %q, want %q", tc.rating, got, tc.want)
		}
	}
}

// The token is the 60 day customer credential, so the page has to say whether it
// still works before a campaign fails against Meta.
func TestTokenState(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name string
		row  sqlc.Connection
		want string
	}{
		{
			name: "plenty of life left",
			row:  sqlc.Connection{Status: sqlc.ConnectionStatusActive, TokenExpiresAt: now.Add(30 * 24 * time.Hour)},
			want: "ok",
		},
		{
			name: "inside the warning window",
			row:  sqlc.Connection{Status: sqlc.ConnectionStatusActive, TokenExpiresAt: now.Add(3 * 24 * time.Hour)},
			want: "expiring",
		},
		{
			// Expiry is the exact boundary: a token expiring now is no longer
			// usable.
			name: "already expired",
			row:  sqlc.Connection{Status: sqlc.ConnectionStatusActive, TokenExpiresAt: now.Add(-time.Minute)},
			want: "expired",
		},
		{
			// A disconnected connection is not being warned about, even though its
			// token has run out.
			name: "disconnected is not warned about",
			row:  sqlc.Connection{Status: sqlc.ConnectionStatusDisconnected, TokenExpiresAt: now.Add(-time.Hour)},
			want: "expired",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tokenState(tc.row, now); got != tc.want {
				t.Errorf("tokenState = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestExpiringSoon(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	window := 7 * 24 * time.Hour

	tests := []struct {
		name string
		row  sqlc.Connection
		want bool
	}{
		{name: "far away", row: sqlc.Connection{Status: sqlc.ConnectionStatusActive, TokenExpiresAt: now.Add(30 * 24 * time.Hour)}},
		{name: "inside the window", row: sqlc.Connection{Status: sqlc.ConnectionStatusActive, TokenExpiresAt: now.Add(time.Hour)}, want: true},
		{name: "expired counts as expiring", row: sqlc.Connection{Status: sqlc.ConnectionStatusActive, TokenExpiresAt: now.Add(-time.Hour)}, want: true},
		{name: "only active connections are warned about", row: sqlc.Connection{Status: sqlc.ConnectionStatusPendingPhone, TokenExpiresAt: now.Add(time.Hour)}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExpiringSoon(tc.row, now, window); got != tc.want {
				t.Errorf("ExpiringSoon = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestViewRowMapsAConnection(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	id := uuid.New()

	view := viewRow(sqlc.Connection{
		ID:                 id,
		MessagingAccountID: "waba-1",
		PhoneNumber:        ptr("+15550109999"),
		DisplayName:        "Acme Support",
		Status:             sqlc.ConnectionStatusActive,
		QualityRating:      "GREEN",
		MessagingLimit:     1000,
		TokenExpiresAt:     now.Add(60 * 24 * time.Hour),
	}, now)

	if view.ID != id.String() {
		t.Errorf("ID = %q", view.ID)
	}
	if view.AccountID != "waba-1" {
		t.Errorf("AccountID = %q", view.AccountID)
	}
	if view.PhoneNumber != "+15550109999" {
		t.Errorf("PhoneNumber = %q", view.PhoneNumber)
	}
	if view.DisplayName != "Acme Support" {
		t.Errorf("DisplayName = %q", view.DisplayName)
	}
	if view.Status != "active" || view.StatusLabel != "Active" || view.StatusKind != "badge-success" {
		t.Errorf("status = %q/%q/%q", view.Status, view.StatusLabel, view.StatusKind)
	}
	if view.QualityRating != "High" {
		t.Errorf("QualityRating = %q", view.QualityRating)
	}
	if view.MessagingLimit != 1000 {
		t.Errorf("MessagingLimit = %d", view.MessagingLimit)
	}
	if view.TokenState != "ok" {
		t.Errorf("TokenState = %q", view.TokenState)
	}
	// The year is shown because the token lives 60 days and may cross new year.
	if view.TokenExpires != "30 Apr 2026" {
		t.Errorf("TokenExpires = %q, want 30 Apr 2026", view.TokenExpires)
	}
}

// A connection still waiting for a number has no number to show, and its name
// falls back to the account id so the row is not blank.
func TestViewRowHandlesAPendingConnection(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	view := viewRow(sqlc.Connection{
		ID:                 uuid.New(),
		MessagingAccountID: "waba-1",
		Status:             sqlc.ConnectionStatusPendingPhone,
		TokenExpiresAt:     now.Add(time.Hour),
	}, now)

	if view.PhoneNumber != "" {
		t.Errorf("PhoneNumber = %q, want empty", view.PhoneNumber)
	}
	if view.DisplayName != "waba-1" {
		t.Errorf("DisplayName = %q, want the account id", view.DisplayName)
	}
	if view.StatusLabel != "Needs a number" {
		t.Errorf("StatusLabel = %q", view.StatusLabel)
	}
}

func TestViewRowsIsNeverNil(t *testing.T) {
	// A templ range over a nil slice is fine, but returning an allocated slice
	// keeps the page's "no connections yet" branch the single place that decides
	// what an empty list means.
	if got := viewRows(nil, time.Now()); got == nil || len(got) != 0 {
		t.Fatalf("viewRows(nil) = %#v", got)
	}
	if got := viewRows([]sqlc.Connection{{ID: uuid.New()}}, time.Now()); len(got) != 1 {
		t.Fatalf("viewRows returned %d rows, want 1", len(got))
	}
}

func TestDisplayNameAndPhoneNumberHelpers(t *testing.T) {
	row := sqlc.Connection{MessagingAccountID: "waba-1"}
	if got := DisplayName(row); got != "waba-1" {
		t.Errorf("DisplayName fallback = %q", got)
	}
	if got := PhoneNumber(row); got != "" {
		t.Errorf("PhoneNumber for a pending connection = %q, want empty", got)
	}

	row.DisplayName = "Acme"
	row.PhoneNumber = ptr("+15550109999")
	if got := DisplayName(row); got != "Acme" {
		t.Errorf("DisplayName = %q", got)
	}
	if got := PhoneNumber(row); got != "+15550109999" {
		t.Errorf("PhoneNumber = %q", got)
	}
}
