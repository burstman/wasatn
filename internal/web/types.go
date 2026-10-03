// Package web contains the templ components and the view models they render.
// It has no dependency on the domain packages: handlers translate domain types
// into the view models below.
package web

import (
	"fmt"
	"strings"
)

// PageData is passed to every page component.
type PageData struct {
	// Title is the page heading shown in the browser title and navbar.
	Title string
	// CSRFToken is embedded in a meta tag so app.js can attach it to HTMX
	// requests.
	CSRFToken string
	// User is the signed-in account, or nil when anonymous.
	User *UserView
	// Flash is a one-shot message from the previous action, or nil.
	Flash *FlashView
}

// UserView is the navbar representation of an account.
type UserView struct {
	Email string
	Plan  string
}

// FlashView is a dismissible message.
type FlashView struct {
	// Kind is a daisyUI alert class modifier: success, error, info, warning.
	Kind    string
	Message string
}

// AuthForm is the view model for the login and register forms.
type AuthForm struct {
	Email     string
	Errors    map[string]string
	Action    string
	Submit    string
	Alternate string
	// AlternateHref points at the other auth page.
	AlternateHref string
	// PasswordAutocomplete tells the browser whether the field is a new
	// password or an existing one, so it offers the right suggestions.
	PasswordAutocomplete string
}

// ErrorView renders a failure page.
type ErrorView struct {
	Status    int
	Title     string
	Detail    string
	RequestID string
}

// EmptyView is the placeholder shown for features that land in later
// milestones.
type EmptyView struct {
	Title       string
	Description string
	Milestone   string
}

// SignupView carries the Facebook Login configuration the browser needs to open
// the Embedded Signup dialog.
//
// None of these are secrets. The app id and config id are public identifiers
// that Meta expects the browser to have, the version selects the Graph API, and
// the redirect URI must match the app's registered OAuth redirect URI exactly.
type SignupView struct {
	AppID       string
	ConfigID    string
	Version     string
	RedirectURI string
	// Enabled is false when the app is missing Meta configuration, so the page
	// can explain the problem instead of loading the SDK for a dialog that
	// cannot open.
	Enabled bool
}

// ConnectionsView is the connections page: the signup controls and the rows.
type ConnectionsView struct {
	Signup SignupView
	Rows   []ConnectionView
}

// ConnectionView is one row on the connections page.
type ConnectionView struct {
	ID          string
	AccountID   string
	PhoneNumber string
	DisplayName string
	// Status is the raw connection_status value. The page compares it directly
	// to decide whether a disconnect button makes sense.
	Status string
	// StatusLabel and StatusKind turn the status into a readable badge.
	StatusLabel    string
	StatusKind     string
	QualityRating  string
	MessagingLimit int
	// TokenExpires is formatted for display, never a raw timestamp.
	TokenExpires string
	// TokenState is one of "ok", "expiring" or "expired".
	TokenState string
}

// PhoneNumberOrPlaceholder renders the number, or a note that onboarding has not
// produced one yet.
func (c ConnectionView) PhoneNumberOrPlaceholder() string {
	if c.PhoneNumber == "" {
		return "Waiting for a phone number"
	}
	return c.PhoneNumber
}

// TokenClass styles the expiry cell, so an expiring or expired token is not
// something a customer has to notice by reading a date.
func (c ConnectionView) TokenClass() string {
	switch c.TokenState {
	case "ok":
		return "text-sm opacity-70"
	case "expired":
		return "text-sm font-semibold text-error"
	default:
		return "text-sm font-medium text-warning"
	}
}

// PageTitle composes the browser title.
func (p PageData) PageTitle() string {
	if p.Title == "" {
		return "WasaTN"
	}
	return p.Title + " · WasaTN"
}

// AlertKind normalises a flash kind to a daisyUI alert modifier, defaulting to
// info so an unexpected value still renders.
func (f FlashView) AlertKind() string {
	switch strings.ToLower(f.Kind) {
	case "success", "error", "info", "warning":
		return strings.ToLower(f.Kind)
	default:
		return "info"
	}
}

// ErrorTitle maps a status code to a plain-language heading.
func ErrorTitle(status int) string {
	switch status {
	case 400:
		return "Bad request"
	case 401:
		return "Please sign in"
	case 403:
		return "Not allowed"
	case 404:
		return "Page not found"
	case 409:
		return "Conflict"
	case 422:
		return "Check the form"
	case 429:
		return "Too many requests"
	default:
		return "Something went wrong"
	}
}

// StatusText renders a status code with its reason phrase.
func StatusText(status int) string {
	return fmt.Sprintf("%d %s", status, strings.ReplaceAll(ErrorTitle(status), " ", "-"))
}

// ClassIf appends extra classes to base when cond is true, for building class
// attributes conditionally in templ components.
func ClassIf(base string, cond bool, extra ...string) string {
	if !cond {
		return base
	}
	classes := []string{base}
	classes = append(classes, extra...)
	return strings.Join(classes, " ")
}
