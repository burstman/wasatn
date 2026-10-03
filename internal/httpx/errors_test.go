package httpx

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/anthdm/superkit/kit"
)

func TestStatusOfAndMessageOf(t *testing.T) {
	t.Parallel()

	secretCause := errors.New("connection refused on 10.0.0.7:5432")
	badRequest := BadRequest("email %s is invalid", "nope")

	tests := []struct {
		name        string
		err         error
		wantStatus  int
		wantMessage string
	}{
		{
			name:        "bad request",
			err:         badRequest,
			wantStatus:  http.StatusBadRequest,
			wantMessage: "email nope is invalid",
		},
		{
			name:        "unauthorized gets default message",
			err:         Unauthorized(""),
			wantStatus:  http.StatusUnauthorized,
			wantMessage: "Please sign in to continue.",
		},
		{
			name:        "forbidden gets default message",
			err:         Forbidden(""),
			wantStatus:  http.StatusForbidden,
			wantMessage: "You do not have access to this resource.",
		},
		{
			name:        "not found gets default message",
			err:         NotFound(""),
			wantStatus:  http.StatusNotFound,
			wantMessage: "Not found.",
		},
		{
			name:        "conflict gets default message",
			err:         Conflict(""),
			wantStatus:  http.StatusConflict,
			wantMessage: "That conflicts with existing data.",
		},
		{
			name:        "too many requests",
			err:         TooManyRequests(""),
			wantStatus:  http.StatusTooManyRequests,
			wantMessage: "Too many requests. Please slow down.",
		},
		{
			name:        "internal error hides cause",
			err:         Internal(secretCause),
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "Something went wrong.",
		},
		{
			name:        "unknown error becomes 500 with generic message",
			err:         secretCause,
			wantStatus:  http.StatusInternalServerError,
			wantMessage: "Something went wrong.",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			if got := StatusOf(tc.err); got != tc.wantStatus {
				t.Errorf("StatusOf() = %d, want %d", got, tc.wantStatus)
			}
			if got := MessageOf(tc.err); got != tc.wantMessage {
				t.Errorf("MessageOf() = %q, want %q", got, tc.wantMessage)
			}
		})
	}
}

// The user-facing message must never contain the underlying cause, which can
// include hostnames and connection strings.
func TestInternalDoesNotLeakCauseToUser(t *testing.T) {
	t.Parallel()

	err := Internal(errors.New("dial tcp 10.0.0.7:5432: connect: connection refused"))
	if got := MessageOf(err); got != "Something went wrong." {
		t.Errorf("MessageOf() = %q, want a generic message", got)
	}
}

func TestErrorWrapping(t *testing.T) {
	t.Parallel()

	cause := errors.New("underlying")

	if !errors.Is(BadRequest("bad").WithCause(cause), cause) {
		t.Error("errors.Is() = false, want true; WithCause must be unwrappable")
	}
	if got := Internal(cause).WithMessage("Custom.").Message; got != "Custom." {
		t.Errorf("WithMessage() message = %q, want %q", got, "Custom.")
	}
	if got := Unauthorized("").Status; got != http.StatusUnauthorized {
		t.Errorf("Unauthorized().Status = %d, want 401", got)
	}
}

func TestJSONSetsContentType(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	k := &kit.Kit{Response: rec, Request: httptest.NewRequest(http.MethodGet, "/", nil)}

	if err := JSON(k, http.StatusCreated, map[string]string{"id": "abc"}); err != nil {
		t.Fatalf("JSON() error = %v", err)
	}

	// Regression guard: kit.Kit.JSON writes the status before setting the header,
	// which silently drops Content-Type. httpx.JSON must set headers first.
	if got := rec.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Errorf("Content-Type = %q, want application/json; charset=utf-8", got)
	}
	if rec.Code != http.StatusCreated {
		t.Errorf("status = %d, want 201", rec.Code)
	}

	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body["id"] != "abc" {
		t.Errorf("body id = %q, want %q", body["id"], "abc")
	}
}

func TestProblem(t *testing.T) {
	t.Parallel()

	rec := httptest.NewRecorder()
	k := &kit.Kit{Response: rec, Request: httptest.NewRequest(http.MethodGet, "/", nil)}

	if err := Problem(k, http.StatusUnprocessableEntity, "Check the form", "password is too short"); err != nil {
		t.Fatalf("Problem() error = %v", err)
	}
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", rec.Code)
	}

	var body struct {
		Title  string `json:"title"`
		Detail string `json:"detail"`
		Status int    `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("body is not valid JSON: %v", err)
	}
	if body.Title != "Check the form" || body.Detail != "password is too short" || body.Status != 422 {
		t.Errorf("body = %+v, want title/detail/status populated", body)
	}
}

func TestRedirectUsesHXRedirectForHTMX(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		htmx           bool
		wantStatus     int
		wantHXRedirect string
		wantLocation   string
	}{
		{name: "normal request", htmx: false, wantStatus: http.StatusSeeOther, wantLocation: "/dashboard"},
		{name: "htmx request", htmx: true, wantStatus: http.StatusNoContent, wantHXRedirect: "/dashboard"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodPost, "/login", nil)
			if tc.htmx {
				req.Header.Set("HX-Request", "true")
			}
			rec := httptest.NewRecorder()
			k := &kit.Kit{Response: rec, Request: req}

			if err := Redirect(k, http.StatusSeeOther, "/dashboard"); err != nil {
				t.Fatalf("Redirect() error = %v", err)
			}
			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if got := rec.Header().Get("HX-Redirect"); got != tc.wantHXRedirect {
				t.Errorf("HX-Redirect = %q, want %q", got, tc.wantHXRedirect)
			}
			if got := rec.Header().Get("Location"); got != tc.wantLocation {
				t.Errorf("Location = %q, want %q", got, tc.wantLocation)
			}
		})
	}
}

func TestIsHTMXAndWantsJSON(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		headers  map[string]string
		wantHTMX bool
		wantJSON bool
	}{
		{name: "plain html", headers: map[string]string{"Accept": "text/html"}, wantHTMX: false, wantJSON: false},
		{name: "json api", headers: map[string]string{"Accept": "application/json"}, wantHTMX: false, wantJSON: true},
		{name: "htmx html swap", headers: map[string]string{"HX-Request": "true", "Accept": "text/html"}, wantHTMX: true, wantJSON: false},
		{name: "htmx asking for json", headers: map[string]string{"HX-Request": "true", "Accept": "application/json"}, wantHTMX: true, wantJSON: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			for k, v := range tc.headers {
				req.Header.Set(k, v)
			}
			k := &kit.Kit{Response: httptest.NewRecorder(), Request: req}

			if got := IsHTMX(k); got != tc.wantHTMX {
				t.Errorf("IsHTMX() = %v, want %v", got, tc.wantHTMX)
			}
			if got := WantsJSON(k); got != tc.wantJSON {
				t.Errorf("WantsJSON() = %v, want %v", got, tc.wantJSON)
			}
		})
	}
}

func TestFormIntFallsBackOnBadInput(t *testing.T) {
	t.Parallel()

	form := "email=a%40b.com&count=notanumber&ok=7"
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	k := &kit.Kit{Response: httptest.NewRecorder(), Request: req}

	if got := FormValue(k, "email"); got != "a@b.com" {
		t.Errorf("FormValue(email) = %q, want a@b.com", got)
	}
	if got := FormInt(k, "count", 42); got != 42 {
		t.Errorf("FormInt(count) = %d, want 42 (fallback on unparsable input)", got)
	}
	if got := FormInt(k, "ok", 42); got != 7 {
		t.Errorf("FormInt(ok) = %d, want 7", got)
	}
	if got := FormInt(k, "missing", 42); got != 42 {
		t.Errorf("FormInt(missing) = %d, want 42", got)
	}
}
