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
