package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// testKey is a 32-byte key, matching what production uses.
var testKey = []byte("0123456789abcdef0123456789abcdef")

// newTestClient points a client at server and returns it with the version fixed.
func newTestClient(t *testing.T, server *httptest.Server) *Client {
	t.Helper()
	client, err := NewClient(Config{
		AppID:        "1234567890",
		AppSecret:    "app-secret",
		GraphVersion: DefaultVersion,
		BaseURL:      server.URL,
		HTTPClient:   server.Client(),
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}

// DefaultVersion keeps the tests readable; production reads META_GRAPH_VERSION.
const DefaultVersion = "v25.0"

// recorder is a Graph stub that records requests and replays canned bodies.
type recorder struct {
	server *httptest.Server
	paths  []string
	forms  []map[string][]string
}

func newRecorder(t *testing.T, handler func(w http.ResponseWriter, r *http.Request, rec *recorder)) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.paths = append(rec.paths, r.URL.Path)
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				t.Errorf("parse form: %v", err)
			}
			rec.forms = append(rec.forms, r.PostForm)
		}
		handler(w, r, rec)
	}))
	t.Cleanup(rec.server.Close)
	return rec
}

func writeJSON(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

func TestNewClientRequiresCredentials(t *testing.T) {
	tests := []struct {
		name    string
		cfg     Config
		wantErr string
	}{
		{
			name:    "missing app id",
			cfg:     Config{AppSecret: "secret", GraphVersion: DefaultVersion},
			wantErr: "META_APP_ID is required",
		},
		{
			name:    "missing app secret",
			cfg:     Config{AppID: "123", GraphVersion: DefaultVersion},
			wantErr: "META_APP_SECRET is required",
		},
		{
			name:    "missing version",
			cfg:     Config{AppID: "123", AppSecret: "secret"},
			wantErr: "graph version is required",
		},
		{
			name:    "blank app id is not an app id",
			cfg:     Config{AppID: "   ", AppSecret: "secret", GraphVersion: DefaultVersion},
			wantErr: "META_APP_ID is required",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewClient(tc.cfg)
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestNewClientNormalisesVersionAndBaseURL(t *testing.T) {
	client, err := NewClient(Config{AppID: "123", AppSecret: "s", GraphVersion: "25.0", BaseURL: "https://example.test/"})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if got, want := client.endpoint("me/accounts", nil), "https://example.test/v25.0/me/accounts"; got != want {
		t.Fatalf("endpoint = %q, want %q", got, want)
	}
	// The oauth namespace is unversioned, so it must not pick up the version.
	if got, want := client.endpoint("oauth/access_token", nil), "https://example.test/oauth/access_token"; got != want {
		t.Fatalf("oauth endpoint = %q, want %q", got, want)
	}
}

// TestExchangeCodeExchangesTwice is the core of Embedded Signup: the code is
// traded for a short-lived token and then again for the long-lived one. A single
// exchange would produce a connection that dies after 24 hours.
func TestExchangeCodeExchangesTwice(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		if got := r.URL.Query().Get("code"); got == "auth-code" {
			writeJSON(w, http.StatusOK, `{"access_token":"short-lived","token_type":"bearer"}`)
			return
		}
		if got := r.URL.Query().Get("fb_exchange_token"); got == "short-lived" {
			writeJSON(w, http.StatusOK, `{"access_token":"long-lived","token_type":"bearer","expires_in":5184000}`)
			return
		}
		t.Errorf("unexpected exchange: %v", r.URL.Query())
		writeJSON(w, http.StatusBadRequest, `{"error":{"message":"nope","code":100}}`)
	})

	client := newTestClient(t, rec.server)
	token, err := client.ExchangeCode(context.Background(), "auth-code", "https://wasatn.test/connections")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if token.AccessToken != "long-lived" {
		t.Fatalf("AccessToken = %q, want %q", token.AccessToken, "long-lived")
	}
	// 5184000 seconds is 60 days.
	if want := 60 * 24 * time.Hour; token.ExpiresAt.Sub(time.Now().UTC()) < want-time.Minute {
		t.Fatalf("ExpiresAt is not ~60 days out: %v", token.ExpiresAt)
	}

	if len(rec.paths) != 2 {
		t.Fatalf("expected 2 calls, got %d: %v", len(rec.paths), rec.paths)
	}
	for _, path := range rec.paths {
		if path != "/oauth/access_token" {
			t.Fatalf("expected the unversioned oauth path, got %q", path)
		}
	}
}

func TestExchangeCodeRequiresCodeAndRedirectURI(t *testing.T) {
	client := &Client{appID: "1", appSecret: "s", baseURL: "https://example.test", version: DefaultVersion, http: http.DefaultClient}

	tests := []struct {
		name        string
		code        string
		redirectURI string
	}{
		{name: "empty code", code: "", redirectURI: "https://x.test/cb"},
		{name: "blank code", code: "   ", redirectURI: "https://x.test/cb"},
		{name: "missing redirect uri", code: "abc", redirectURI: ""},
		{name: "blank redirect uri", code: "abc", redirectURI: "  "},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := client.ExchangeCode(context.Background(), tc.code, tc.redirectURI); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

// A token with no expiry cannot be stored, because token_expires_at is NOT NULL.
// Failing here is better than inventing an expiry that would hide the problem.
func TestExchangeCodeRejectsMissingExpiry(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		if r.URL.Query().Get("code") != "" {
			writeJSON(w, http.StatusOK, `{"access_token":"short"}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"access_token":"long"}`)
	})

	client := newTestClient(t, rec.server)
	_, err := client.ExchangeCode(context.Background(), "code", "https://x.test/cb")
	if err == nil || !strings.Contains(err.Error(), "expires_in") {
		t.Fatalf("expected an expires_in error, got %v", err)
	}
}

func TestExchangeCodeSurfacesOauthFailure(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusBadRequest, `{"error":{"message":"Invalid verification code format.","type":"OAuthException","code":100,"error_subcode":33},"error_reason":"invalid_verification_code"}`)
	})

	client := newTestClient(t, rec.server)
	_, err := client.ExchangeCode(context.Background(), "code", "https://x.test/cb")
	if err == nil {
		t.Fatal("expected an error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Code != 100 {
		t.Fatalf("Code = %d, want 100", apiErr.Code)
	}
	if apiErr.Subcode != 33 {
		t.Fatalf("Subcode = %d, want 33", apiErr.Subcode)
	}
	if apiErr.Type != "OAuthException" {
		t.Fatalf("Type = %q, want OAuthException", apiErr.Type)
	}
	if apiErr.Reason != "invalid_verification_code" {
		t.Fatalf("Reason = %q, want the error_reason", apiErr.Reason)
	}
	if !strings.Contains(apiErr.Error(), "Invalid verification code format.") {
		t.Fatalf("message missing from %q", apiErr.Error())
	}
}

func TestExchangeCodeRejectsEmptyToken(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusOK, `{"token_type":"bearer"}`)
	})

	client := newTestClient(t, rec.server)
	_, err := client.ExchangeCode(context.Background(), "code", "https://x.test/cb")
	if err == nil || !strings.Contains(err.Error(), "no access token") {
		t.Fatalf("expected a missing-token error, got %v", err)
	}
}

func TestBusinessAccountsUsesPerAccountToken(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		if got := r.URL.Query().Get("fields"); got != BusinessAccountFields {
			t.Errorf("fields = %q, want %q", got, BusinessAccountFields)
		}
		if got := r.URL.Query().Get("access_token"); got != "user-token" {
			t.Errorf("access_token = %q", got)
		}
		writeJSON(w, http.StatusOK, `{"data":[
			{"id":"111","name":"Acme","access_token":"waba-token"},
			{"id":"222","name":"Acme EU"}
		]}`)
	})

	client := newTestClient(t, rec.server)
	accounts, err := client.BusinessAccounts(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("BusinessAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2", len(accounts))
	}
	if accounts[0].AccessToken != "waba-token" {
		t.Errorf("first account token = %q", accounts[0].AccessToken)
	}
	// An account with no token of its own falls back to the user token, or every
	// later call for that account would fail.
	if accounts[1].AccessToken != "user-token" {
		t.Errorf("second account token = %q, want the fallback", accounts[1].AccessToken)
	}
}

func TestBusinessAccountsSkipsRecordsWithoutAnID(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusOK, `{"data":[{"id":"","name":"Ghost"},{"id":"111","name":"Acme"}]}`)
	})

	client := newTestClient(t, rec.server)
	accounts, err := client.BusinessAccounts(context.Background(), "user-token")
	if err != nil {
		t.Fatalf("BusinessAccounts: %v", err)
	}
	if len(accounts) != 1 || accounts[0].ID != "111" {
		t.Fatalf("accounts = %+v, want only the identified one", accounts)
	}
}

func TestBusinessAccountsFollowsPaging(t *testing.T) {
	var rec *recorder
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		if r.URL.Path != "/v25.0/me/accounts" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.URL.Query().Get("after") == "abc" {
			writeJSON(w, http.StatusOK, `{"data":[{"id":"2"}]}`)
			return
		}
		writeJSON(w, http.StatusOK, `{"data":[{"id":"1"}],"paging":{"next":"`+rec.server.URL+`/v25.0/me/accounts?after=abc"}}`)
	})

	client := newTestClient(t, rec.server)
	accounts, err := client.BusinessAccounts(context.Background(), "t")
	if err != nil {
		t.Fatalf("BusinessAccounts: %v", err)
	}
	if len(accounts) != 2 {
		t.Fatalf("got %d accounts, want 2 across both pages", len(accounts))
	}
	if len(rec.paths) != 2 {
		t.Fatalf("expected 2 requests, got %v", rec.paths)
	}
}

func TestPhoneNumbersDecodesTheFieldsWasaTNStores(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		if r.URL.Path != "/v25.0/111/phone_numbers" {
			t.Errorf("path = %q", r.URL.Path)
		}
		writeJSON(w, http.StatusOK, `{"data":[{
			"id":"pn-1",
			"display_phone_number":"+1 555-010-9999",
			"verified_name":"Acme Support",
			"quality_rating":"GREEN",
			"status":"CONNECTED",
			"is_official_business_account":true,
			"name_status":"APPROVED",
			"account_mode":"LIVE",
			"current_messaging_limit":1000,
			"ignored_field":"whatever"
		}]}`)
	})

	client := newTestClient(t, rec.server)
	numbers, err := client.PhoneNumbers(context.Background(), "111", "t")
	if err != nil {
		t.Fatalf("PhoneNumbers: %v", err)
	}
	if len(numbers) != 1 {
		t.Fatalf("got %d numbers, want 1", len(numbers))
	}

	got := numbers[0]
	want := PhoneNumber{
		ID:                        "pn-1",
		DisplayPhoneNumber:        "+1 555-010-9999",
		VerifiedName:              "Acme Support",
		QualityRating:             "GREEN",
		Status:                    "CONNECTED",
		IsOfficialBusinessAccount: true,
		NameStatus:                "APPROVED",
		AccountMode:               "LIVE",
		MessagingLimit:            1000,
	}
	if got != want {
		t.Fatalf("PhoneNumber =\n %+v\nwant\n %+v", got, want)
	}
}

// A number that has never sent has no quality_rating, and the column is NOT NULL.
func TestPhoneNumbersDefaultsQualityRating(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusOK, `{"data":[{"id":"pn-1","display_phone_number":"+15550109999"}]}`)
	})

	client := newTestClient(t, rec.server)
	numbers, err := client.PhoneNumbers(context.Background(), "111", "t")
	if err != nil {
		t.Fatalf("PhoneNumbers: %v", err)
	}
	if numbers[0].QualityRating != "UNKNOWN" {
		t.Fatalf("QualityRating = %q, want UNKNOWN", numbers[0].QualityRating)
	}
}

// Embedded Signup v4 lets an account finish with no verified number, so an empty
// list is a normal answer rather than an error.
func TestPhoneNumbersAllowsAnEmptyList(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusOK, `{"data":[]}`)
	})

	client := newTestClient(t, rec.server)
	numbers, err := client.PhoneNumbers(context.Background(), "111", "t")
	if err != nil {
		t.Fatalf("PhoneNumbers: %v", err)
	}
	if len(numbers) != 0 {
		t.Fatalf("got %d numbers, want none", len(numbers))
	}
}

func TestSubscribeAppSucceedsOn204(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/v25.0/111/subscribed_apps" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})

	client := newTestClient(t, rec.server)
	if err := client.SubscribeApp(context.Background(), "111", "waba-token"); err != nil {
		t.Fatalf("SubscribeApp: %v", err)
	}
}

// Without a webhook subscription a connection sends messages it can never track,
// so a failure here must not be swallowed.
func TestSubscribeAppSurfacesFailure(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusBadRequest, `{"error":{"message":"(#200) Permissions error","type":"OAuthException","code":200}}`)
	})

	client := newTestClient(t, rec.server)
	err := client.SubscribeApp(context.Background(), "111", "t")
	if err == nil {
		t.Fatal("expected an error")
	}

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T", err)
	}
	if apiErr.Code != 200 {
		t.Fatalf("Code = %d, want 200", apiErr.Code)
	}
}

func TestCallsRequireAToken(t *testing.T) {
	client := &Client{appID: "1", appSecret: "s", baseURL: "https://example.test", version: DefaultVersion, http: http.DefaultClient}

	if _, err := client.BusinessAccounts(context.Background(), "  "); err == nil {
		t.Error("BusinessAccounts accepted an empty token")
	}
	if _, err := client.PhoneNumbers(context.Background(), "111", ""); err == nil {
		t.Error("PhoneNumbers accepted an empty token")
	}
	if err := client.SubscribeApp(context.Background(), "111", ""); err == nil {
		t.Error("SubscribeApp accepted an empty token")
	}
	if _, err := client.PhoneNumbers(context.Background(), "", "t"); err == nil {
		t.Error("PhoneNumbers accepted an empty account id")
	}
	if err := client.SubscribeApp(context.Background(), "", "t"); err == nil {
		t.Error("SubscribeApp accepted an empty account id")
	}
}

func TestAsAPIErrorHandlesMetaVariations(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		status     int
		wantCode   int
		wantSub    int
		wantMsg    string
		wantTrace  string
		wantReason string
	}{
		{
			name:     "numeric code",
			body:     `{"error":{"message":"Invalid OAuth token.","type":"OAuthException","code":190}}`,
			status:   http.StatusBadRequest,
			wantCode: 190,
			wantMsg:  "Invalid OAuth token.",
		},
		{
			name:     "quoted code",
			body:     `{"error":{"message":"Invalid OAuth token.","code":"190"}}`,
			status:   http.StatusBadRequest,
			wantCode: 190,
			wantMsg:  "Invalid OAuth token.",
		},
		{
			name:     "error_subcode",
			body:     `{"error":{"message":"nope","code":100,"error_subcode":463}}`,
			status:   http.StatusBadRequest,
			wantCode: 100,
			wantSub:  463,
			wantMsg:  "nope",
		},
		{
			name:      "nested fbtrace_id",
			body:      `{"error":{"message":"nope","code":1,"fbtrace_id":"AbCdEf"}}`,
			status:    http.StatusBadRequest,
			wantCode:  1,
			wantMsg:   "nope",
			wantTrace: "AbCdEf",
		},
		{
			name:      "top-level fbtrace_id",
			body:      `{"error":{"message":"nope","code":1},"fbtrace_id":"TopLevel"}`,
			status:    http.StatusBadRequest,
			wantCode:  1,
			wantMsg:   "nope",
			wantTrace: "TopLevel",
		},
		{
			name:     "error_user_msg used when message is absent",
			body:     `{"error":{"code":1,"error_user_msg":"Please accept the permission."}}`,
			status:   http.StatusBadRequest,
			wantCode: 1,
			wantMsg:  "Please accept the permission.",
		},
		{
			name:       "error_reason is captured",
			body:       `{"error":{"message":"nope","code":1},"error_reason":"user_denied"}`,
			status:     http.StatusBadRequest,
			wantCode:   1,
			wantMsg:    "nope",
			wantReason: "user_denied",
		},
		{
			name:       "nested error_reason is captured",
			body:       `{"error":{"message":"nope","code":1,"error_reason":"invalid_verification_code"}}`,
			status:     http.StatusBadRequest,
			wantCode:   1,
			wantMsg:    "nope",
			wantReason: "invalid_verification_code",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := asAPIError([]byte(tc.body), tc.status)
			if err == nil {
				t.Fatal("expected an APIError")
			}
			if err.Code != tc.wantCode {
				t.Errorf("Code = %d, want %d", err.Code, tc.wantCode)
			}
			if err.Subcode != tc.wantSub {
				t.Errorf("Subcode = %d, want %d", err.Subcode, tc.wantSub)
			}
			if err.Message != tc.wantMsg {
				t.Errorf("Message = %q, want %q", err.Message, tc.wantMsg)
			}
			if err.FBTraceID != tc.wantTrace {
				t.Errorf("FBTraceID = %q, want %q", err.FBTraceID, tc.wantTrace)
			}
			if err.Reason != tc.wantReason && tc.wantReason != "" {
				t.Errorf("Reason = %q, want %q", err.Reason, tc.wantReason)
			}
		})
	}
}

// A successful response must not be mistaken for a failure just because it lacks
// an "error" object.
func TestAsAPIErrorIgnoresSuccessBodies(t *testing.T) {
	for _, body := range []string{
		`{"success":true}`,
		`{"data":[{"id":"1"}]}`,
		`{"access_token":"abc"}`,
		`[]`,
		`not json at all`,
	} {
		if err := asAPIError([]byte(body), http.StatusOK); err != nil {
			t.Errorf("asAPIError(%s) = %v, want nil", body, err)
		}
	}
}

func TestNonJSONFailureBecomesAnAPIError(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body>Bad Gateway</body></html>"))
	})

	client := newTestClient(t, rec.server)
	_, err := client.BusinessAccounts(context.Background(), "t")

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *APIError, got %T (%v)", err, err)
	}
	if apiErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("StatusCode = %d", apiErr.StatusCode)
	}
	if !strings.Contains(apiErr.Message, "Bad Gateway") {
		t.Fatalf("body not carried into the message: %q", apiErr.Message)
	}
}

func TestAPIErrorAuthorizationFailed(t *testing.T) {
	tests := []struct {
		name string
		err  APIError
		want bool
	}{
		{name: "invalid token", err: APIError{Code: CodeInvalidToken}, want: true},
		{name: "session expired", err: APIError{Code: CodeSessionExpired}, want: true},
		{name: "permission", err: APIError{Code: 200}, want: true},
		{name: "permission subcode", err: APIError{Code: 10, Subcode: 200}, want: true},
		{name: "unauthorised status", err: APIError{StatusCode: http.StatusUnauthorized}, want: true},
		{name: "transient server error", err: APIError{Code: 1, StatusCode: http.StatusInternalServerError}},
		{name: "missing permission", err: APIError{Code: 803, StatusCode: http.StatusNotFound}},
		{name: "invalid argument", err: APIError{Code: 100, StatusCode: http.StatusBadRequest}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.err.AuthorizationFailed(); got != tc.want {
				t.Fatalf("AuthorizationFailed() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestNextPage(t *testing.T) {
	base := url.Values{
		"fields":       {"id,name"},
		"access_token": {"secret-token"},
	}

	tests := []struct {
		name      string
		next      string
		wantPath  string
		wantQuery map[string]string
	}{
		{
			name:     "empty stops paging",
			next:     "",
			wantPath: "",
		},
		{
			name:     "absolute url loses host and version but keeps the cursor",
			next:     "https://graph.facebook.com/v25.0/123/phone_numbers?after=abc&limit=25",
			wantPath: "123/phone_numbers",
			wantQuery: map[string]string{
				"fields":       "id,name",
				"access_token": "secret-token",
				"after":        "abc",
				"limit":        "25",
			},
		},
		{
			name:     "url without a version segment",
			next:     "https://graph.facebook.com/123/phone_numbers?after=abc",
			wantPath: "123/phone_numbers",
			wantQuery: map[string]string{
				"after": "abc",
			},
		},
		{
			name:     "a non numeric version is kept, not treated as a version",
			next:     "https://graph.facebook.com/vNext/123/phone_numbers",
			wantPath: "vNext/123/phone_numbers",
		},
		{
			name:     "bare path",
			next:     "/123/phone_numbers?after=abc",
			wantPath: "123/phone_numbers",
			wantQuery: map[string]string{
				"after": "abc",
			},
		},
		{
			name:     "a paging link cannot replace the access token",
			next:     "https://graph.facebook.com/v25.0/me/accounts?access_token=stolen",
			wantPath: "me/accounts",
			wantQuery: map[string]string{
				"access_token": "secret-token",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path, query := nextPage(tc.next, base)
			if path != tc.wantPath {
				t.Errorf("path = %q, want %q", path, tc.wantPath)
			}
			for key, want := range tc.wantQuery {
				if got := query.Get(key); got != want {
					t.Errorf("query %q = %q, want %q", key, got, want)
				}
			}
			if len(tc.wantQuery) == 0 && len(query) == 0 {
				t.Error("expected the base parameters to be carried over")
			}
		})
	}
}

func TestIsDigits(t *testing.T) {
	tests := map[string]bool{
		"":     false,
		"25":   true,
		"2a":   false,
		"0":    true,
		"100":  true,
		"-1":   false,
		"2.5":  false,
		"  25": false,
	}

	for in, want := range tests {
		if got := isDigits(in); got != want {
			t.Errorf("isDigits(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestTokenValid(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

	tests := []struct {
		name  string
		token Token
		want  bool
	}{
		{
			name:  "empty token",
			token: Token{ExpiresAt: now.Add(90 * 24 * time.Hour)},
			want:  false,
		},
		{
			name:  "fresh token",
			token: Token{AccessToken: "t", ExpiresAt: now.Add(60 * 24 * time.Hour)},
			want:  true,
		},
		{
			name:  "inside the warning window",
			token: Token{AccessToken: "t", ExpiresAt: now.Add(24 * time.Hour)},
			want:  false,
		},
		{
			name:  "expired",
			token: Token{AccessToken: "t", ExpiresAt: now.Add(-time.Hour)},
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.token.Valid(now); got != tc.want {
				t.Fatalf("Valid() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A cancelled dialog has no code, and the flow must not attempt an exchange.
func TestExchangeCodeNeverLeaksTheAppSecret(t *testing.T) {
	rec := newRecorder(t, func(w http.ResponseWriter, _ *http.Request, _ *recorder) {
		writeJSON(w, http.StatusBadRequest, `{"error":{"message":"Invalid verification code format.","code":100}}`)
	})

	client := newTestClient(t, rec.server)
	_, err := client.ExchangeCode(context.Background(), "code", "https://x.test/cb")
	if err == nil {
		t.Fatal("expected an error")
	}

	// The app secret is in the query string of the request, so the error text and
	// the wrapped cause are the risk; neither may repeat it.
	if strings.Contains(err.Error(), "app-secret") {
		t.Fatalf("error leaks the app secret: %v", err)
	}
	if strings.Contains(err.Error(), client.appSecret) {
		t.Fatalf("error leaks the app secret: %v", err)
	}
}

// The request itself must carry the documented parameters, or the exchange cannot
// work at all; this guards the wire format against a refactor.
//
// The redirect URI belongs only on the code exchange. The long-lived exchange
// passes the short token instead, and sending redirect_uri there is not part of
// the documented flow.
func TestExchangeCodeSendsTheDocumentedParameters(t *testing.T) {
	var rec *recorder
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		q := r.URL.Query()
		if q.Get("client_id") != "1234567890" || q.Get("client_secret") != "app-secret" {
			t.Errorf("credentials not sent: %v", q)
		}

		if q.Get("code") != "" {
			if q.Get("redirect_uri") != "https://wasatn.test/connections" {
				t.Errorf("redirect_uri = %q", q.Get("redirect_uri"))
			}
			writeJSON(w, http.StatusOK, `{"access_token":"short-lived"}`)
			return
		}

		if q.Get("grant_type") != "fb_exchange_token" {
			t.Errorf("grant_type = %q", q.Get("grant_type"))
		}
		if q.Get("fb_exchange_token") != "short-lived" {
			t.Errorf("fb_exchange_token = %q, want the short-lived token", q.Get("fb_exchange_token"))
		}
		if q.Get("redirect_uri") != "" {
			t.Errorf("redirect_uri must not be sent on the long-lived exchange: %q", q.Get("redirect_uri"))
		}
		writeJSON(w, http.StatusOK, `{"access_token":"long-lived","expires_in":5184000}`)
	})

	client := newTestClient(t, rec.server)
	token, err := client.ExchangeCode(context.Background(), "code", "https://wasatn.test/connections")
	if err != nil {
		t.Fatalf("ExchangeCode: %v", err)
	}
	if token.AccessToken != "long-lived" {
		t.Fatalf("AccessToken = %q", token.AccessToken)
	}
	if len(rec.paths) != 2 {
		t.Fatalf("expected 2 exchanges, got %v", rec.paths)
	}
}

func TestJSONBodiesAreDecodedIntoTypedStructs(t *testing.T) {
	// Guards the response types against a rename that would silently decode to
	// zero values.
	var accounts accountsResponse
	if err := json.Unmarshal([]byte(`{"data":[{"id":"1","name":"n","access_token":"t"}]}`), &accounts); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(accounts.Data) != 1 || accounts.Data[0].ID != "1" {
		t.Fatalf("accounts = %+v", accounts)
	}

	var numbers phoneNumbersResponse
	if err := json.Unmarshal([]byte(`{"data":[{"id":"p","current_messaging_limit":50}]}`), &numbers); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if numbers.Data[0].CurrentMessagingLimit != 50 {
		t.Fatalf("limit = %d", numbers.Data[0].CurrentMessagingLimit)
	}
}

// Three or more pages must each advance the cursor: carrying page one's cursor
// forward would loop on the same page forever.
func TestBusinessAccountsReplacesTheCursorOnEveryPage(t *testing.T) {
	var seen []string
	var rec *recorder
	rec = newRecorder(t, func(w http.ResponseWriter, r *http.Request, _ *recorder) {
		after := r.URL.Query().Get("after")
		seen = append(seen, after)

		switch after {
		case "":
			writeJSON(w, http.StatusOK, `{"data":[{"id":"1"}],"paging":{"next":"`+rec.server.URL+`/v25.0/me/accounts?after=page2"}}`)
		case "page2":
			writeJSON(w, http.StatusOK, `{"data":[{"id":"2"}],"paging":{"next":"`+rec.server.URL+`/v25.0/me/accounts?after=page3"}}`)
		case "page3":
			writeJSON(w, http.StatusOK, `{"data":[{"id":"3"}]}`)
		default:
			t.Errorf("unexpected cursor %q", after)
			writeJSON(w, http.StatusOK, `{"data":[]}`)
		}
	})

	client := newTestClient(t, rec.server)
	accounts, err := client.BusinessAccounts(context.Background(), "t")
	if err != nil {
		t.Fatalf("BusinessAccounts: %v", err)
	}
	if len(accounts) != 3 {
		t.Fatalf("got %d accounts, want 3: %v", len(accounts), seen)
	}
	want := []string{"", "page2", "page3"}
	if len(seen) != len(want) {
		t.Fatalf("cursors seen = %v, want %v", seen, want)
	}
	for i, cursor := range want {
		if seen[i] != cursor {
			t.Fatalf("cursors seen = %v, want %v", seen, want)
		}
	}
}
