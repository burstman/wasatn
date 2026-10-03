package connections

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/anthdm/superkit/kit"
	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/burstman/wasatn/internal/httpx"
	"github.com/burstman/wasatn/internal/web"
	"github.com/burstman/wasatn/internal/whatsapp"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// fakeFlow is a session stub.
type fakeFlow struct {
	userID  uuid.UUID
	values  map[string]string
	flashes []string
	// takeCalls records how many times a flow value was consumed, which is how a
	// replay is detected.
	takeCalls int
}

func newFakeFlow(userID uuid.UUID) *fakeFlow {
	return &fakeFlow{userID: userID, values: map[string]string{}}
}

func (f *fakeFlow) UserID(*http.Request) uuid.UUID { return f.userID }

func (f *fakeFlow) PutFlowValue(_ *http.Request, key, value string) {
	f.values[key] = value
}

func (f *fakeFlow) TakeFlowValue(_ *http.Request, key string) string {
	f.takeCalls++
	value := f.values[key]
	delete(f.values, key)
	return value
}

func (f *fakeFlow) Flash(_ *http.Request, kind, message string) {
	f.flashes = append(f.flashes, kind+":"+message)
}

func testPageData(k *kit.Kit, title string) web.PageData {
	return web.PageData{Title: title, CSRFToken: "test-token"}
}

func testSignupConfig() SignupConfig {
	return SignupConfig{
		AppID:       "1234567890",
		ConfigID:    "1688820642776714",
		Version:     "v25.0",
		RedirectURI: "https://wasatn.test/connections",
	}
}

// newTestHandlers wires handlers with stub dependencies. The central error
// handler is not installed, because SuperKit keeps it in package-level state and
// tests must not race over it; the returned recorder therefore shows whatever the
// handler wrote before the error handler would have run.
func newTestHandlers(t *testing.T, flow Flow, service *Service, signup SignupConfig) *Handlers {
	t.Helper()
	return NewHandlers(service, flow, signup, testPageData, discardLogger())
}

func TestEnabledRequiresEveryBrowserValue(t *testing.T) {
	full := testSignupConfig()

	tests := []struct {
		name   string
		signup SignupConfig
		want   bool
	}{
		{name: "complete", signup: full, want: true},
		{name: "no app id", signup: SignupConfig{ConfigID: "c", RedirectURI: "r"}},
		{name: "no config id", signup: SignupConfig{AppID: "a", RedirectURI: "r"}},
		// Without a redirect URI the code cannot be exchanged, so offering the
		// button would fail after the customer clicked it.
		{name: "no redirect uri", signup: SignupConfig{AppID: "a", ConfigID: "c"}},
		{name: "nothing configured", signup: SignupConfig{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newTestHandlers(t, newFakeFlow(testUser), nil, tc.signup)
			if got := h.Enabled(); got != tc.want {
				t.Fatalf("Enabled() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSignupStateStoresANonce(t *testing.T) {
	flow := newFakeFlow(testUser)
	h := newTestHandlers(t, flow, nil, testSignupConfig())

	rec := serve(t, h.SignupState, httptest.NewRequest(http.MethodPost, "/connections/signup-state", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
	state := flow.values[FlowStateKey]
	if state == "" {
		t.Fatal("no state was stored in the session")
	}
	if !strings.Contains(rec.Body.String(), state) {
		t.Error("the response did not carry the state it stored")
	}
}

func TestSignupStateRefusesWhenNotConfigured(t *testing.T) {
	flow := newFakeFlow(testUser)
	h := newTestHandlers(t, flow, nil, SignupConfig{})

	rec := serve(t, h.SignupState, httptest.NewRequest(http.MethodPost, "/connections/signup-state", nil))

	if len(flow.values) != 0 {
		t.Fatal("a state was issued while signup is unconfigured")
	}
	if rec.Code == http.StatusOK {
		t.Fatal("expected a failure status")
	}
}

// The state is what proves the round trip came from Meta, so a callback without
// the matching value must be refused before any code is exchanged.
func TestCallbackRejectsAMismatchedState(t *testing.T) {
	tests := []struct {
		name     string
		issued   string
		returned string
		reason   string
	}{
		{name: "no state issued", issued: "", returned: "attacker-state", reason: "no pending signup"},
		{name: "wrong state", issued: "expected", returned: "attacker-state", reason: "mismatch"},
		{name: "empty state returned", issued: "expected", returned: "", reason: "missing state"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			flow := newFakeFlow(testUser)
			if tc.issued != "" {
				flow.values[FlowStateKey] = tc.issued
			}
			graph := defaultGraph()
			queries := newFakeQueries()
			h := newTestHandlers(t, flow, newService(t, queries, graph), testSignupConfig())

			rec := serve(t, h.Index, signupReturn("auth-code", tc.returned))

			if rec.Code != http.StatusSeeOther {
				t.Fatalf("status = %d, want a redirect for %s", rec.Code, tc.reason)
			}
			if len(flow.flashes) != 1 || !strings.HasPrefix(flow.flashes[0], "error:") {
				t.Fatalf("flashes = %v, want one error for %s", flow.flashes, tc.reason)
			}
			if len(graph.calls) != 0 {
				t.Fatalf("the code was exchanged anyway: %v", graph.calls)
			}
			if len(queries.upserts) != 0 {
				t.Fatal("a connection was stored anyway")
			}
		})
	}
}

// The state must be consumed on every attempt, so a leaked authorization code
// cannot be replayed with a fresh state.
func TestCallbackConsumesTheStateSoItCannotBeReplayed(t *testing.T) {
	flow := newFakeFlow(testUser)
	flow.values[FlowStateKey] = "expected"

	queries := newFakeQueries()
	h := newTestHandlers(t, flow, newService(t, queries, defaultGraph()), testSignupConfig())

	rec := serve(t, h.Index, signupReturn("auth-code", "expected"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if len(flow.values) != 0 {
		t.Fatal("the state survived the callback")
	}

	// Replaying the same code must now fail, because there is no state left.
	rec = serve(t, h.Index, signupReturn("auth-code", "expected"))
	if !strings.HasPrefix(flow.flashes[len(flow.flashes)-1], "error:") {
		t.Fatalf("a replayed return was accepted: %v", flow.flashes)
	}
	if len(queries.upserts) != 1 {
		t.Fatalf("the replay stored another connection: %d rows", len(queries.upserts))
	}
}

func TestCallbackRejectsAnEmptyCode(t *testing.T) {
	flow := newFakeFlow(testUser)
	flow.values[FlowStateKey] = "expected"

	graph := defaultGraph()
	h := newTestHandlers(t, flow, newService(t, newFakeQueries(), graph), testSignupConfig())

	rec := serve(t, h.Index, signupReturn("  ", "expected"))

	// A blank code is not a return at all, so the page renders and Meta is never
	// called.
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the page: %s", rec.Code, rec.Body.String())
	}
	if len(flow.flashes) != 0 {
		t.Errorf("a blank code produced a flash: %v", flow.flashes)
	}
	if len(graph.calls) != 0 {
		t.Fatalf("an empty code reached Meta: %v", graph.calls)
	}
}

// A completed signup redirects without leaving the code in the address bar,
// which matters because the popup that lands here is the one holding it.
func TestCallbackStoresTheSignupAndRedirects(t *testing.T) {
	flow := newFakeFlow(testUser)
	flow.values[FlowStateKey] = "expected"

	queries := newFakeQueries()
	h := newTestHandlers(t, flow, newService(t, queries, defaultGraph()), testSignupConfig())

	rec := serve(t, h.Index, signupReturn("auth-code", "expected"))
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Location"); got != "/connections" {
		t.Errorf("Location = %q", got)
	}
	if len(flow.flashes) != 1 || !strings.HasPrefix(flow.flashes[0], "success:") {
		t.Errorf("flashes = %v, want one success", flow.flashes)
	}
}

// Facebook reports a refusal in the query string rather than as a code, and the
// customer is sitting on the page that opened the dialog, so it becomes a flash.
func TestCallbackExplainsADenial(t *testing.T) {
	flow := newFakeFlow(testUser)
	flow.values[FlowStateKey] = "expected"

	graph := defaultGraph()
	h := newTestHandlers(t, flow, newService(t, newFakeQueries(), graph), testSignupConfig())

	req := httptest.NewRequest(http.MethodGet, "/connections?error=access_denied&error_description=User+denied", nil)
	rec := serve(t, h.Index, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status = %d, want 303: %s", rec.Code, rec.Body.String())
	}
	if len(flow.flashes) != 1 || !strings.Contains(flow.flashes[0], "User denied") {
		t.Errorf("flashes = %v, want Meta's own reason", flow.flashes)
	}
	if len(graph.calls) != 0 {
		t.Fatalf("a denial reached Meta: %v", graph.calls)
	}
}

// A code arriving on a server with no Meta configuration is just a stray query
// parameter, so the page renders as usual and nothing is exchanged.
func TestIndexIgnoresASignupReturnWhenSignupIsNotConfigured(t *testing.T) {
	flow := newFakeFlow(testUser)
	graph := defaultGraph()
	h := newTestHandlers(t, flow, newService(t, newFakeQueries(), graph), SignupConfig{})

	rec := serve(t, h.Index, signupReturn("c", "s"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want the page: %s", rec.Code, rec.Body.String())
	}
	if flow.takeCalls != 0 {
		t.Fatal("the state was consumed by an ignored return")
	}
	if len(graph.calls) != 0 {
		t.Fatalf("an ignored return still reached Meta: %v", graph.calls)
	}
}

func TestDisconnectRejectsAMalformedID(t *testing.T) {
	queries := newFakeQueries()
	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, queries, defaultGraph()), testSignupConfig())

	rec := serveRoute(t, "/connections/{id}/disconnect", h.Disconnect,
		httptest.NewRequest(http.MethodPost, "/connections/not-a-uuid/disconnect", nil))

	if rec.Code == http.StatusOK {
		t.Fatal("a malformed connection id was accepted")
	}
}

func TestDisconnectRendersTheUpdatedRow(t *testing.T) {
	connectionID := uuid.New()
	queries := newFakeQueries()
	queries.disconnected = sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow{Changed: true, PausedCampaigns: 2}
	queries.byID = sqlc.Connection{
		ID:                 connectionID,
		UserID:             testUser,
		MessagingAccountID: "waba-1",
		PhoneNumberID:      ptr("pn-1"),
		PhoneNumber:        ptr("+15550109999"),
		DisplayName:        "Acme Support",
		Status:             sqlc.ConnectionStatusDisconnected,
		TokenExpiresAt:     time.Now().UTC().Add(30 * 24 * time.Hour),
	}

	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, queries, defaultGraph()), testSignupConfig())
	path := "/connections/" + connectionID.String() + "/disconnect"

	rec := serveRoute(t, "/connections/{id}/disconnect", h.Disconnect, httptest.NewRequest(http.MethodPost, path, nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Disconnected") {
		t.Errorf("the row does not show the new status:\n%s", body)
	}
	// The swap target has to be present for htmx to replace the right element.
	if !strings.Contains(body, `id="connection-`+connectionID.String()+`"`) {
		t.Errorf("the row id is missing:\n%s", body)
	}
	// A disconnected connection offers reconnection, not another disconnect.
	if !strings.Contains(body, "Use Connect WhatsApp to reconnect") {
		t.Errorf("the reconnect hint is missing:\n%s", body)
	}
}

// Re-running a disconnect is a no-op, so it must still render rather than fail.
func TestDisconnectIsIdempotentFromTheHandlersPointOfView(t *testing.T) {
	connectionID := uuid.New()
	queries := newFakeQueries()
	queries.disconnected = sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow{Changed: false}
	queries.byID = sqlc.Connection{
		ID:                 connectionID,
		UserID:             testUser,
		MessagingAccountID: "waba-1",
		PhoneNumberID:      ptr("pn-1"),
		PhoneNumber:        ptr("+15550109999"),
		Status:             sqlc.ConnectionStatusDisconnected,
		TokenExpiresAt:     time.Now().UTC().Add(time.Hour),
	}

	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, queries, defaultGraph()), testSignupConfig())
	path := "/connections/" + connectionID.String() + "/disconnect"

	rec := serveRoute(t, "/connections/{id}/disconnect", h.Disconnect, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDisconnectHidesAVanishedRow(t *testing.T) {
	connectionID := uuid.New()
	queries := newFakeQueries()
	queries.disconnected = sqlc.DisconnectOwnedConnectionAndPauseCampaignsRow{Changed: true}
	queries.byIDErr = pgx.ErrNoRows

	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, queries, defaultGraph()), testSignupConfig())
	path := "/connections/" + connectionID.String() + "/disconnect"

	rec := serveRoute(t, "/connections/{id}/disconnect", h.Disconnect, httptest.NewRequest(http.MethodPost, path, nil))
	if rec.Code == http.StatusOK {
		t.Fatalf("an empty fragment was served instead of a 404: %s", rec.Body.String())
	}
}

func TestIndexRendersRows(t *testing.T) {
	queries := newFakeQueries()
	queries.list = []sqlc.Connection{{
		ID:                 uuid.New(),
		UserID:             testUser,
		MessagingAccountID: "waba-1",
		PhoneNumberID:      ptr("pn-1"),
		PhoneNumber:        ptr("+15550109999"),
		DisplayName:        "Acme Support",
		Status:             sqlc.ConnectionStatusActive,
		QualityRating:      "GREEN",
		MessagingLimit:     1000,
		TokenExpiresAt:     time.Now().UTC().Add(60 * 24 * time.Hour),
	}}

	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, queries, defaultGraph()), testSignupConfig())
	rec := serve(t, h.Index, httptest.NewRequest(http.MethodGet, "/connections", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"Acme Support", "+15550109999", "Active", "High", "1000", "Connect WhatsApp"} {
		if !strings.Contains(body, want) {
			t.Errorf("the page is missing %q:\n%s", want, body)
		}
	}
}

func TestIndexExplainsAMissingConfiguration(t *testing.T) {
	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, newFakeQueries(), defaultGraph()), SignupConfig{})
	rec := serve(t, h.Index, httptest.NewRequest(http.MethodGet, "/connections", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "META_FB_CONFIG_ID") {
		t.Errorf("the setup notice does not name the missing variable:\n%s", body)
	}
	// A button that cannot work must not be rendered.
	if strings.Contains(body, `id="connect-whatsapp"`) {
		t.Error("the connect button was rendered while signup is unconfigured")
	}
}

func TestCountDelegatesToTheService(t *testing.T) {
	queries := newFakeQueries()
	queries.count = 4

	h := newTestHandlers(t, newFakeFlow(testUser), newService(t, queries, defaultGraph()), testSignupConfig())
	got, err := h.Count(t.Context(), testUser)
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if got != 4 {
		t.Fatalf("Count = %d, want 4", got)
	}
}

func TestSignupErrorMapping(t *testing.T) {
	tests := []struct {
		name       string
		err        error
		wantStatus int
		wantText   string
	}{
		{
			name:       "phone number owned elsewhere",
			err:        ErrNumberOwnedByAnotherAccount,
			wantStatus: http.StatusConflict,
			wantText:   "another WasaTN account",
		},
		{
			name:       "nothing shared",
			err:        ErrNothingConnected,
			wantStatus: http.StatusBadRequest,
			wantText:   "No WhatsApp Business Account",
		},
		{
			name:       "signup not configured",
			err:        ErrSignupNotConfigured,
			wantStatus: http.StatusBadRequest,
			wantText:   "not configured",
		},
		{
			name:       "meta refused the authorization",
			err:        &whatsapp.APIError{Message: "Invalid OAuth token.", Code: whatsapp.CodeInvalidToken, StatusCode: 400},
			wantStatus: http.StatusBadRequest,
			wantText:   "Invalid OAuth token.",
		},
		{
			name:       "meta permissions error",
			err:        &whatsapp.APIError{Message: "Permissions error", Code: 200, StatusCode: 400},
			wantStatus: http.StatusBadRequest,
			wantText:   "Permissions error",
		},
		{
			// An internal failure must not leak Meta's wording or the app's state.
			name:       "unexpected failure stays a 500",
			err:        errors.New("dial tcp 10.0.0.1:443: connection refused"),
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			httpErr := signupError(tc.err)
			if got := httpx.StatusOf(httpErr); got != tc.wantStatus {
				t.Errorf("status = %d, want %d", got, tc.wantStatus)
			}
			if tc.wantText == "" {
				return
			}
			if got := httpx.MessageOf(httpErr); !strings.Contains(got, tc.wantText) {
				t.Errorf("message = %q, want it to contain %q", got, tc.wantText)
			}
		})
	}
}

// TestMain installs an error handler for this package.
//
// SuperKit keeps the handler in package-level state with no way to read it back,
// and it decides the status code a handler's error turns into. Without this, a
// deliberate httpx.BadRequest would be recorded as a 500 and every status
// assertion below would be meaningless. Process-wide state is safe here because
// each package's tests are their own binary and none of these tests are parallel.
func TestMain(m *testing.M) {
	kit.UseErrorHandler(func(k *kit.Kit, err error) {
		k.Response.Header().Set("Content-Type", "text/plain; charset=utf-8")
		k.Response.WriteHeader(httpx.StatusOf(err))
		io.WriteString(k.Response, httpx.MessageOf(err))
	})
	os.Exit(m.Run())
}

// serve invokes a handler the way the router does: kit.Handler builds the Kit
// and turns a returned error into a response.
func serve(t *testing.T, handler func(*kit.Kit) error, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	kit.Handler(handler).ServeHTTP(rec, req)
	return rec
}

// serveRoute serves a handler mounted at its real path, so chi URL params
// resolve the way they do in production.
func serveRoute(t *testing.T, pattern string, handler func(*kit.Kit) error, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()

	rec := httptest.NewRecorder()
	router := chi.NewRouter()
	router.Post(pattern, kit.Handler(handler))
	router.ServeHTTP(rec, req)
	return rec
}

// signupReturn is the request Facebook makes when it hands a signup back: a GET
// of the connections page carrying the authorization code and the state.
func signupReturn(code, state string) *http.Request {
	query := url.Values{}
	if code != "" {
		query.Set("code", code)
	}
	query.Set("state", state)
	return httptest.NewRequest(http.MethodGet, "/connections?"+query.Encode(), nil)
}

func quote(s string) string {
	return `"` + s + `"`
}
