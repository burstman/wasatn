package connections

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/anthdm/superkit/kit"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/httpx"
	"github.com/burstman/wasatn/internal/web"
	"github.com/burstman/wasatn/internal/whatsapp"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

// FlowStateKey names the session value holding the state that proves a signup
// round trip came back from Meta rather than being forged.
const FlowStateKey = "whatsapp_signup"

// MaxCallbackBytes caps the callback body. It carries two short strings, so
// anything larger is not a browser bug worth serving.
const MaxCallbackBytes = 8 << 10

// TokenWarningWindow is how far ahead of expiry the connections page warns.
const TokenWarningWindow = whatsapp.TokenExpiryWarningWindow

// Flow is the session surface the signup round trip needs.
//
// It is an interface rather than *auth.Session so this package stays
// independent of the auth package, and so tests can supply a fake.
type Flow interface {
	UserID(r *http.Request) uuid.UUID
	PutFlowValue(r *http.Request, key, value string)
	TakeFlowValue(r *http.Request, key string) string
	Flash(r *http.Request, kind, message string)
}

// SignupConfig is the browser-facing half of Embedded Signup.
type SignupConfig struct {
	// AppID, ConfigID and Version are handed to the Facebook SDK.
	AppID    string
	ConfigID string
	Version  string
	// RedirectURI must match the app's registered OAuth redirect URI exactly.
	RedirectURI string
}

// Handlers serves the connections page and the Embedded Signup endpoints.
type Handlers struct {
	service  *Service
	flow     Flow
	signup   SignupConfig
	pageData func(k *kit.Kit, title string) web.PageData
	log      *slog.Logger
}

// NewHandlers wires the connection handlers.
//
// pageData is injected rather than rebuilt here so the connections page picks up
// the same navbar and flash handling as every other page, and so the session is
// only read for it once.
func NewHandlers(service *Service, flow Flow, signup SignupConfig, pageData func(k *kit.Kit, title string) web.PageData, log *slog.Logger) *Handlers {
	return &Handlers{
		service:  service,
		flow:     flow,
		signup:   signup,
		pageData: pageData,
		log:      log.With("component", "connections"),
	}
}

// Enabled reports whether Embedded Signup is configured.
func (h *Handlers) Enabled() bool {
	return h.signup.AppID != "" && h.signup.ConfigID != "" && h.signup.RedirectURI != ""
}

// SignupView is the browser-facing configuration for the page.
func (h *Handlers) SignupView() web.SignupView {
	return web.SignupView{
		AppID:       h.signup.AppID,
		ConfigID:    h.signup.ConfigID,
		Version:     h.signup.Version,
		RedirectURI: h.signup.RedirectURI,
		Enabled:     h.Enabled(),
	}
}

// Index renders the connections page.
func (h *Handlers) Index(k *kit.Kit) error {
	rows, err := h.service.List(k.Request.Context(), h.flow.UserID(k.Request))
	if err != nil {
		return httpx.Internal(err)
	}
	return k.Render(web.ConnectionsPage(h.pageData(k, "Connections"), web.ConnectionsView{
		Signup: h.SignupView(),
		Rows:   viewRows(rows, time.Now().UTC()),
	}))
}

// SignupState issues the state value that proves the signup round trip.
//
// The browser asks for it immediately before opening the dialog and sends it
// back with the authorization code. Without it, an attacker could load
// Facebook's SDK on their own page, complete a signup there, and trick a
// victim's browser into posting the resulting code: the victim's own WhatsApp
// number would then be attached to the attacker's WasaTN account.
func (h *Handlers) SignupState(k *kit.Kit) error {
	if !h.Enabled() {
		return httpx.BadRequest("Meta Embedded Signup is not configured on this server.")
	}
	state, err := NewState()
	if err != nil {
		return httpx.Internal(err)
	}
	h.flow.PutFlowValue(k.Request, FlowStateKey, state)

	k.Response.Header().Set("Cache-Control", "no-store")
	return httpx.JSON(k, http.StatusOK, map[string]string{"state": state})
}

// Callback completes a signup.
//
// It accepts both a JSON body, which is what the page's own JavaScript sends, and
// a form post, so the flow still works if the script has to fall back.
func (h *Handlers) Callback(k *kit.Kit) error {
	if !h.Enabled() {
		return httpx.BadRequest("Meta Embedded Signup is not configured on this server.")
	}

	userID := h.flow.UserID(k.Request)
	code, state := signupParams(k)

	// The state is consumed whatever happens next, so a leaked authorization code
	// cannot be replayed with a fresh state.
	expected := h.flow.TakeFlowValue(k.Request, FlowStateKey)
	if !ValidState(expected, state) {
		return httpx.BadRequest("That sign-up attempt has expired. Start again from the Connect WhatsApp button.")
	}
	if code == "" {
		return httpx.BadRequest("Facebook did not return an authorization code. Start again from the Connect WhatsApp button.")
	}

	result, err := h.service.Connect(k.Request.Context(), userID, code, h.signup.RedirectURI)
	if err != nil {
		h.logSignupFailure(k, userID, err)
		return signupError(err)
	}

	h.log.InfoContext(k.Request.Context(), "embedded signup completed",
		"user_id", userID,
		"request_id", httpx.RequestID(k.Request),
		"accounts", result.Accounts,
		"connections", len(result.Connections),
		"pending_phone", result.PendingPhone,
	)
	h.flow.Flash(k.Request, "success", signupSummary(result))
	return httpx.Redirect(k, http.StatusSeeOther, "/connections")
}

// Disconnect ends a connection and pauses its campaigns.
//
// It answers with the re-rendered table row, which is what the button's hx-swap
// replaces, so the page updates in place without a reload.
func (h *Handlers) Disconnect(k *kit.Kit) error {
	connectionID, err := uuid.Parse(strings.TrimSpace(chi.URLParam(k.Request, "id")))
	if err != nil {
		return httpx.NotFound("That connection does not exist.")
	}

	userID := h.flow.UserID(k.Request)
	ctx := k.Request.Context()

	switch err := h.service.Disconnect(ctx, userID, connectionID); {
	case err == nil:
	case errors.Is(err, ErrAlreadyDisconnected):
		// Already disconnected: idempotent, so fall through and re-render.
	case errors.Is(err, ErrNotFound):
		return httpx.NotFound("That connection does not exist.")
	default:
		return httpx.Internal(err)
	}

	row, err := h.service.Get(ctx, userID, connectionID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return httpx.NotFound("That connection does not exist.")
		}
		return httpx.Internal(err)
	}

	views := viewRows([]sqlc.Connection{row}, time.Now().UTC())
	if len(views) == 0 {
		return httpx.Internal(errors.New("connections: expected one row to re-render"))
	}

	k.Response.Header().Set("Content-Type", "text/html; charset=utf-8")
	return web.ConnectionRow(views[0]).Render(ctx, k.Response)
}

// Count is the dashboard's number of connected WhatsApp numbers.
func (h *Handlers) Count(ctx context.Context, userID uuid.UUID) (int, error) {
	return h.service.Count(ctx, userID)
}

// signupParams reads the code and state from either a JSON body or a form post.
func signupParams(k *kit.Kit) (code, state string) {
	if isJSON(k.Request) {
		var body struct {
			Code  string `json:"code"`
			State string `json:"state"`
		}
		if err := json.NewDecoder(io.LimitReader(k.Request.Body, MaxCallbackBytes)).Decode(&body); err != nil {
			// The body is consumed, so a form fallback is not possible. An empty
			// code produces the "start again" message, which is the right outcome
			// for a truncated or non-JSON body.
			return "", ""
		}
		return strings.TrimSpace(body.Code), strings.TrimSpace(body.State)
	}
	return strings.TrimSpace(k.Request.PostFormValue("code")), strings.TrimSpace(k.Request.PostFormValue("state"))
}

func isJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Type"), "application/json")
}

// signupError maps a signup failure onto a message the customer can act on.
//
// A Meta authorisation failure is the customer's to fix, so it becomes a 400
// carrying Meta's own wording; a collision with another tenant's number is a 409
// they cannot resolve alone. Anything else is ours, so it stays a 500.
func signupError(err error) error {
	var apiErr *whatsapp.APIError
	switch {
	case errors.Is(err, ErrNumberOwnedByAnotherAccount):
		return httpx.Conflict("That WhatsApp number is already connected to another WasaTN account. Disconnect it there first, or contact support if you believe that is wrong.")
	case errors.Is(err, ErrNothingConnected):
		return httpx.BadRequest("No WhatsApp Business Account was shared. Pick your business account in the Facebook dialog and try again.")
	case errors.Is(err, ErrSignupNotConfigured):
		return httpx.BadRequest("Meta Embedded Signup is not configured on this server.")
	case errors.As(err, &apiErr) && apiErr.AuthorizationFailed():
		return httpx.BadRequest("Meta would not complete the sign-up: %s If you declined a permission, accept it and try again.", apiErr.Message)
	default:
		return httpx.Internal(err)
	}
}

// signupSummary describes what the signup saved, so the flash after the redirect
// is specific rather than a bare "done".
func signupSummary(r Result) string {
	if r.PendingPhone {
		return "Connected. Finish verifying a phone number in Meta to make it ready to send from."
	}
	noun := " number"
	if len(r.Connections) != 1 {
		noun += "s"
	}
	accounts := " business account"
	if r.Accounts != 1 {
		accounts += "s"
	}
	return "Connected " + strconv.Itoa(len(r.Connections)) + noun + " across " + strconv.Itoa(r.Accounts) + accounts + "."
}

// logSignupFailure records a failed signup.
//
// Neither the authorization code nor the token is logged; both would let anyone
// act as the customer. Phone numbers are never part of the message.
func (h *Handlers) logSignupFailure(k *kit.Kit, userID uuid.UUID, err error) {
	attrs := []any{
		"user_id", userID,
		"request_id", httpx.RequestID(k.Request),
		"err", err.Error(),
	}
	var apiErr *whatsapp.APIError
	if errors.As(err, &apiErr) {
		// The trace id is what makes a signup failure searchable on Meta's side.
		attrs = append(attrs, "graph_code", apiErr.Code, "fbtrace_id", apiErr.FBTraceID)
	}
	h.log.WarnContext(k.Request.Context(), "whatsapp signup failed", attrs...)
}
