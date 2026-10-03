// Package whatsapp wraps the small slice of the Meta Graph API that Embedded
// Signup needs: swapping a Facebook Login code for a token, discovering the
// WhatsApp Business Accounts and phone numbers it unlocks, and subscribing the
// app to their webhooks.
//
// The package holds no database or HTTP-framework code so it can be unit tested
// against an httptest server. Timeouts are mandatory: a signup click blocks a
// user-facing button, and Graph is an external dependency that can be slow.
package whatsapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// DefaultBaseURL is the Graph API root. Tests replace it with an httptest
	// server; nothing else should.
	DefaultBaseURL = "https://graph.facebook.com"

	// Timeout bounds a single Graph call. Embedded Signup makes four of them in
	// sequence, so this is the ceiling for the whole click, not per attempt.
	Timeout = 20 * time.Second

	// MaxErrorBytes caps how much of an unreadable body is quoted in an error,
	// so a proxy returning a huge HTML page cannot flood the logs.
	MaxErrorBytes = 4 << 10

	// MaxResponseBytes caps a successful body. Graph listings for one WABA are
	// far below this; anything larger is a bug or a hostile endpoint.
	MaxResponseBytes = 4 << 20 // 4 MiB
)

// Graph error codes worth naming. They are stable and appear in Meta's docs, so
// branching on them beats matching error message text.
const (
	// CodeInvalidToken is OAuth code 190: the token or code is invalid,
	// expired or revoked. For a signup that means the customer must retry.
	CodeInvalidToken = 190
	// CodeSessionExpired is 463: the session passed to the SDK has expired.
	CodeSessionExpired = 463
	// CodeSession is 102: the session is not valid.
	CodeSession = 102
)

// APIError is a Graph API error response, or a failed OAuth exchange.
//
// The Graph error envelope is nested under "error"; a failed OAuth exchange
// instead returns the same information as query parameters. Both are folded into
// this one type so callers have a single thing to inspect.
type APIError struct {
	// Message is Meta's human-readable explanation.
	Message string
	// Type and Reason refine the failure. Type is Meta's exception class
	// ("OAuthException") and Reason the machine-readable cause
	// ("invalid_verification_code"), which is the more useful of the two.
	Type    string
	Reason  string
	Code    int
	Subcode int
	// FBTraceID is Meta's support correlation id, worth surfacing in logs when
	// a signup fails so the failure can be looked up on Meta's side.
	FBTraceID string
	// StatusCode is the HTTP status, which for OAuth failures is 400.
	StatusCode int
}

func (e *APIError) Error() string {
	var b strings.Builder
	b.WriteString("whatsapp: graph api error")
	if e.Code != 0 {
		b.WriteString(" (code " + strconv.Itoa(e.Code))
		if e.Subcode != 0 {
			b.WriteString(", subcode " + strconv.Itoa(e.Subcode))
		}
		b.WriteString(")")
	}
	if e.Message != "" {
		b.WriteString(": " + e.Message)
	}
	if e.FBTraceID != "" {
		b.WriteString(" [fbtrace_id " + e.FBTraceID + "]")
	}
	return b.String()
}

// AuthorizationFailed reports whether the failure is about the customer's
// authorisation rather than the app being misconfigured or the network failing.
//
// It separates "the user should click again" from "something is broken", which
// the connections page turns into very different messages.
func (e *APIError) AuthorizationFailed() bool {
	if e.StatusCode == http.StatusUnauthorized {
		return true
	}
	switch {
	case e.Code == CodeInvalidToken, e.Code == CodeSessionExpired:
		return true
	// 102 is "session", and 200-299 are permission problems. Meta also reports
	// those as an error_subcode under a generic code, so both are checked.
	case e.Code == CodeSession:
		return true
	case inRange(e.Code, 200, 299), inRange(e.Subcode, 200, 299):
		return true
	default:
		return false
	}
}

func inRange(v, low, high int) bool {
	return v >= low && v <= high
}

// Client calls the Graph API as the WasaTN app.
type Client struct {
	http *http.Client
	// baseURL and version are separate so tests can point at httptest while
	// still exercising real URL construction.
	baseURL string
	version string

	appID     string
	appSecret string
}

// Config is everything the client needs from the environment.
type Config struct {
	AppID        string
	AppSecret    string
	GraphVersion string
	BaseURL      string
	HTTPClient   *http.Client
}

// NewClient builds a Graph client.
//
// It refuses to build without an app id or secret: without them every call
// fails with an opaque OAuth error, and a client that cannot work is better as a
// startup failure than as a broken signup button.
func NewClient(cfg Config) (*Client, error) {
	if strings.TrimSpace(cfg.AppID) == "" {
		return nil, errors.New("whatsapp: META_APP_ID is required")
	}
	if strings.TrimSpace(cfg.AppSecret) == "" {
		return nil, errors.New("whatsapp: META_APP_SECRET is required")
	}

	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	version := strings.TrimSpace(cfg.GraphVersion)
	if version == "" {
		return nil, errors.New("whatsapp: graph version is required")
	}
	if !strings.HasPrefix(version, "v") {
		version = "v" + version
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: Timeout}
	}

	return &Client{
		http:      httpClient,
		baseURL:   baseURL,
		version:   version,
		appID:     strings.TrimSpace(cfg.AppID),
		appSecret: strings.TrimSpace(cfg.AppSecret),
	}, nil
}

// endpoint builds a versioned Graph URL with query parameters attached.
func (c *Client) endpoint(path string, query url.Values) string {
	clean := strings.Trim(path, "/")
	if clean == "" {
		clean = "me"
	}
	// The oauth namespace is not versioned.
	if strings.HasPrefix(clean, "oauth/") {
		return withQuery(c.baseURL+"/"+clean, query)
	}
	return withQuery(c.baseURL+"/"+c.version+"/"+clean, query)
}

func withQuery(url string, query url.Values) string {
	if len(query) == 0 {
		return url
	}
	return url + "?" + query.Encode()
}

// graphCode is an error code from Graph, which is a JSON number on most
// endpoints and a quoted string on a few older ones. Decoding both avoids
// failing to parse an error the user needs to see.
type graphCode int

func (c *graphCode) UnmarshalJSON(b []byte) error {
	text := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if text == "" || text == "null" {
		*c = 0
		return nil
	}
	n, err := strconv.Atoi(text)
	if err != nil {
		// An unparseable code is not worth failing the decode over: the message
		// is what gets shown.
		*c = 0
		return nil
	}
	*c = graphCode(n)
	return nil
}

// graphError is the "error" object Graph nests its failures in.
//
// Meta's documentation is inconsistent about whether error_reason,
// error_user_msg and fbtrace_id sit inside it or beside it, so both positions are
// decoded and the first non-empty value wins.
type graphError struct {
	Message   string    `json:"message"`
	Type      string    `json:"type"`
	Code      graphCode `json:"code"`
	Subcode   graphCode `json:"error_subcode"`
	FBTraceID string    `json:"fbtrace_id"`

	// Meta documents these as members of the nested object, but some responses
	// place them beside it.
	ErrorReason      string `json:"error_reason"`
	ErrorUserTitle   string `json:"error_user_title"`
	ErrorUserMessage string `json:"error_user_msg"`
}

// errorEnvelope is a Graph failure body: the nested error plus whatever Meta put
// beside it.
type errorEnvelope struct {
	Error graphError `json:"error"`

	ErrorReason    string `json:"error_reason"`
	ErrorUserTitle string `json:"error_user_title"`
	ErrorUserMsg   string `json:"error_user_msg"`
	FBTraceID      string `json:"fbtrace_id"`
}

// apiError converts a decoded envelope into the exported error type.
func (e errorEnvelope) apiError(status int) *APIError {
	// error_user_msg is written for a customer and explains a declined
	// permission far better than the generic text.
	message := firstNonEmpty(e.Error.Message, e.Error.ErrorUserMessage, e.ErrorUserMsg)
	if message == "" {
		message = "The Meta Graph API rejected the request."
	}
	return &APIError{
		Message: message,
		Type:    e.Error.Type,
		Reason:  firstNonEmpty(e.Error.ErrorReason, e.ErrorReason),
		Code:    int(e.Error.Code),
		Subcode: int(e.Error.Subcode),
		// The trace id is what makes a signup failure searchable on Meta's side.
		FBTraceID:  firstNonEmpty(e.Error.FBTraceID, e.FBTraceID),
		StatusCode: status,
	}
}

// asAPIError reports whether body is a Graph failure and returns it decoded.
//
// An empty message is not enough to prove failure: a successful
// subscribed_apps response is {"success":true}, and decoding that into the
// envelope must not invent an error.
func asAPIError(body []byte, status int) *APIError {
	var envelope errorEnvelope
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	empty := envelope.Error.Message == "" &&
		envelope.Error.Code == 0 &&
		envelope.Error.ErrorUserMessage == "" &&
		envelope.ErrorUserMsg == ""
	if empty {
		return nil
	}
	return envelope.apiError(status)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// get performs a GET and decodes the response into out.
func (c *Client) get(ctx context.Context, path string, query url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint(path, query), nil)
	if err != nil {
		return fmt.Errorf("whatsapp: build request: %w", err)
	}
	return c.send(req, out)
}

// post performs a form POST. Graph takes parameters in the body for writes, not
// the query string.
func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint(path, nil), strings.NewReader(form.Encode()))
	if err != nil {
		return fmt.Errorf("whatsapp: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.send(req, out)
}

// send executes req and decodes a JSON body into out.
//
// Meta answers failures with a JSON error envelope even when the status is 200
// on some endpoints, so the envelope is checked first and a non-2xx status with
// no envelope still produces a usable error.
func (c *Client) send(req *http.Request, out any) error {
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("whatsapp: call graph api: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxResponseBytes))
	if err != nil {
		return fmt.Errorf("whatsapp: read response: %w", err)
	}

	if apiErr := asAPIError(body, resp.StatusCode); apiErr != nil {
		return apiErr
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &APIError{
			Message:    strings.TrimSpace(truncate(string(body), MaxErrorBytes)),
			StatusCode: resp.StatusCode,
		}
	}

	// An empty body is the documented success for subscribed_apps.
	if out == nil || len(bytesTrimSpace(body)) == 0 {
		return nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("whatsapp: decode response: %w", err)
	}
	return nil
}

// bytesTrimSpace reports whether b holds only whitespace.
func bytesTrimSpace(b []byte) []byte {
	return []byte(strings.TrimSpace(string(b)))
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}
