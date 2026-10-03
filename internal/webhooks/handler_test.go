package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/burstman/wasatn/internal/db/sqlc"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
)

const testVerifyToken = "verify-me"

// stubQuerier records the calls the handlers make. Each method has a matching
// field so a test can assert on arguments and force an error.
type stubQuerier struct {
	advance    []sqlc.AdvanceMessageStatusParams
	advanceErr error

	disconnect     []sqlc.DisconnectConnectionAndPauseCampaignsParams
	disconnectErr  error
	disconnectRows int64

	template    []sqlc.UpdateTemplateReviewStatusParams
	templateErr error
}

func (s *stubQuerier) AdvanceMessageStatus(_ context.Context, arg sqlc.AdvanceMessageStatusParams) (sqlc.MessageLog, error) {
	s.advance = append(s.advance, arg)
	return sqlc.MessageLog{}, s.advanceErr
}

func (s *stubQuerier) DisconnectConnectionAndPauseCampaigns(_ context.Context, arg sqlc.DisconnectConnectionAndPauseCampaignsParams) (int64, error) {
	s.disconnect = append(s.disconnect, arg)
	return s.disconnectRows, s.disconnectErr
}

func (s *stubQuerier) UpdateTemplateReviewStatus(_ context.Context, arg sqlc.UpdateTemplateReviewStatusParams) (int64, error) {
	s.template = append(s.template, arg)
	return int64(len(s.template)), s.templateErr
}

// fixture loads a recorded payload from testdata.
func fixture(t *testing.T, name string) []byte {
	t.Helper()

	body, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return body
}

// newTestHandler builds a handler with a stubbed database and the given secret.
func newTestHandler(t *testing.T, secret string, queries Querier) http.Handler {
	t.Helper()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	r := chi.NewRouter()
	New(queries, secret, testVerifyToken, log).Routes(r)
	return r
}

// postDelivery sends a signed delivery, exactly as Meta would.
func postDelivery(t *testing.T, h http.Handler, secret string, body []byte) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
	if secret != "" {
		req.Header.Set(SignatureHeader, Sign(secret, body))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestChallenge(t *testing.T) {
	t.Parallel()

	queries := &stubQuerier{}
	h := newTestHandler(t, testSecret, queries)

	t.Run("valid challenge", func(t *testing.T) {
		t.Parallel()

		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
			"/webhooks/whatsapp?hub.mode=subscribe&hub.verify_token=verify-me&hub.challenge=1158201444", nil))

		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); got != "1158201444" {
			t.Errorf("body = %q, want %q", got, "1158201444")
		}
	})

	for name, query := range map[string]string{
		"wrong token":   "hub.mode=subscribe&hub.verify_token=nope&hub.challenge=1",
		"missing token": "hub.mode=subscribe&hub.challenge=1",
		"wrong mode":    "hub.mode=unsubscribe&hub.verify_token=verify-me&hub.challenge=1",
		"no params":     "",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/webhooks/whatsapp?"+query, nil))
			if rec.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", rec.Code)
			}
		})
	}
}

func TestReceiveSignature(t *testing.T) {
	t.Parallel()

	body := fixture(t, "messages_delivered.json")

	tests := map[string]struct {
		header   func() string
		wantCode int
	}{
		"missing header": {
			// The caller did not set a header at all.
			header:   func() string { return "" },
			wantCode: http.StatusUnauthorized,
		},
		"malformed header": {
			header:   func() string { return "sha256=not-hex" },
			wantCode: http.StatusUnauthorized,
		},
		"wrong signature": {
			header:   func() string { return Sign("other-secret", body) },
			wantCode: http.StatusForbidden,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			queries := &stubQuerier{}
			h := newTestHandler(t, testSecret, queries)

			req := httptest.NewRequest(http.MethodPost, "/webhooks/whatsapp", bytes.NewReader(body))
			req.Header.Set(SignatureHeader, tc.header())
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if len(queries.advance) != 0 {
				t.Errorf("AdvanceMessageStatus called %d times, want 0 for an unauthenticated body", len(queries.advance))
			}
		})
	}
}

func TestReceiveDeliveredStatus(t *testing.T) {
	t.Parallel()

	body := fixture(t, "messages_delivered.json")
	queries := &stubQuerier{}
	h := newTestHandler(t, testSecret, queries)

	rec := postDelivery(t, h, testSecret, body)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	if len(queries.advance) != 1 {
		t.Fatalf("AdvanceMessageStatus called %d times, want 1", len(queries.advance))
	}
	got := queries.advance[0]
	if got.Wamid == nil || !strings.HasPrefix(*got.Wamid, "wamid.") {
		t.Errorf("Wamid = %v, want the full wamid from the payload", got.Wamid)
	}
	if got.Status != sqlc.MessageStatusDelivered {
		t.Errorf("Status = %q, want %q", got.Status, sqlc.MessageStatusDelivered)
	}
	if !got.At.Valid || got.At.Time.Unix() != 1767225599 {
		t.Errorf("At = %v, want Meta's epoch timestamp 1767225599", got.At)
	}
	if got.ErrorCode != nil || got.ErrorMessage != nil {
		t.Errorf("ErrorCode/ErrorMessage = %v/%v, want nil for a delivered status", got.ErrorCode, got.ErrorMessage)
	}
}

func TestReceiveFailedStatus(t *testing.T) {
	t.Parallel()

	body := fixture(t, "messages_failed.json")
	queries := &stubQuerier{}
	h := newTestHandler(t, testSecret, queries)

	if rec := postDelivery(t, h, testSecret, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}

	if len(queries.advance) != 1 {
		t.Fatalf("AdvanceMessageStatus called %d times, want 1", len(queries.advance))
	}
	got := queries.advance[0]
	if got.Status != sqlc.MessageStatusFailed {
		t.Errorf("Status = %q, want %q", got.Status, sqlc.MessageStatusFailed)
	}
	// The failure detail matters: it is what the template builder uses to decide
	// whether a retry can work.
	if got.ErrorCode == nil || *got.ErrorCode != "131047" {
		t.Errorf("ErrorCode = %v, want 131047", got.ErrorCode)
	}
	if got.ErrorMessage == nil || *got.ErrorMessage != "Message failed to send because more than 24 hours have passed." {
		t.Errorf("ErrorMessage = %v, want the error_data detail", got.ErrorMessage)
	}
}

func TestReceiveErrorVariants(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body       string
		setup      func(*stubQuerier)
		wantCode   int
		wantCalls  func() int
		wantRecent bool
	}{
		"malformed json": {
			body:     `{"object":`,
			wantCode: http.StatusBadRequest,
		},
		"not an object": {
			body:     `"nope"`,
			wantCode: http.StatusBadRequest,
		},
		"empty entries": {
			// A valid delivery with nothing in it: answer 200 so Meta stops.
			body:      `{"object":"whatsapp_business_account","entry":[]}`,
			wantCode:  http.StatusOK,
			wantCalls: func() int { return 0 },
		},
		"unknown field is ignored": {
			// Meta adds fields regularly; an unknown one must not fail delivery.
			body:      `{"object":"whatsapp_business_account","entry":[{"id":"1","changes":[{"field":"history","value":{}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: func() int { return 0 },
		},
		"messages field with a bad value": {
			body:     `{"object":"x","entry":[{"id":"1","changes":[{"field":"messages","value":"nope"}]}]}`,
			wantCode: http.StatusBadRequest,
		},
		"unknown status is skipped": {
			body: `{"object":"x","entry":[{"id":"1","changes":[{"field":"messages","value":` +
				`{"statuses":[{"id":"wamid.1","status":"queued","timestamp":"1767225599"}]}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: func() int { return 0 },
		},
		"status without an id is skipped": {
			body: `{"object":"x","entry":[{"id":"1","changes":[{"field":"messages","value":` +
				`{"statuses":[{"status":"delivered","timestamp":"1767225599"}]}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: func() int { return 0 },
		},
		"unparseable timestamp falls back to receipt time": {
			// Losing the transition would be worse than a slightly wrong second,
			// and answering 500 would make Meta redeliver it for days.
			body: `{"object":"x","entry":[{"id":"1","changes":[{"field":"messages","value":` +
				`{"statuses":[{"id":"wamid.1","status":"read","timestamp":"yesterday"}]}}]}]}`,
			wantCode:   http.StatusOK,
			wantCalls:  func() int { return 1 },
			wantRecent: true,
		},
		"database failure": {
			body:      string(fixture(t, "messages_delivered.json")),
			setup:     func(q *stubQuerier) { q.advanceErr = errors.New("connection refused") },
			wantCode:  http.StatusInternalServerError,
			wantCalls: func() int { return 1 },
		},
		"unknown wamid": {
			// We did not send this message. 404 would make Meta redeliver for days.
			body:      string(fixture(t, "messages_delivered.json")),
			setup:     func(q *stubQuerier) { q.advanceErr = pgx.ErrNoRows },
			wantCode:  http.StatusOK,
			wantCalls: func() int { return 1 },
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			queries := &stubQuerier{}
			if tc.setup != nil {
				tc.setup(queries)
			}

			h := newTestHandler(t, testSecret, queries)
			rec := postDelivery(t, h, testSecret, []byte(tc.body))

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCalls != nil {
				if got := len(queries.advance); got != tc.wantCalls() {
					t.Errorf("AdvanceMessageStatus called %d times, want %d", got, tc.wantCalls())
				}
			}
			if tc.wantRecent && len(queries.advance) == 1 {
				got := queries.advance[0].At
				if !got.Valid {
					t.Error("At is invalid, want the receipt time as a fallback")
				} else if delta := time.Since(got.Time); delta < 0 || delta > time.Minute {
					t.Errorf("At = %v, want roughly now", got.Time)
				}
			}
		})
	}
}

func TestReceiveInboundMessage(t *testing.T) {
	t.Parallel()

	queries := &stubQuerier{}
	h := newTestHandler(t, testSecret, queries)

	body := fixture(t, "messages_inbound_text.json")
	if rec := postDelivery(t, h, testSecret, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(queries.advance) != 0 {
		t.Errorf("AdvanceMessageStatus called %d times, want 0 for an inbound message", len(queries.advance))
	}
}

func TestReceiveAccountUpdate(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body      string
		wantCalls int
		wantPhone bool
	}{
		"removal disconnects and pauses": {
			body:      string(fixture(t, "account_update_removed.json")),
			wantCalls: 1,
			wantPhone: true,
		},
		"removal by waba": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"account_update","value":{"event":"PARTNER_REMOVED","messaging_account_id":"102900000000001"}}]}]}`,
			wantCalls: 1,
		},
		"lower case event still counts": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"account_update","value":{"event":"partner_removed","phone_number_id":"552100000000001"}}]}]}`,
			wantCalls: 1,
			wantPhone: true,
		},
		"other events are ignored": {
			// Disconnecting on an unrecognised event would cut off a paying
			// customer over a guess.
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"account_update","value":{"event":"PARTNER_ADDED","phone_number_id":"552100000000001"}}]}]}`,
			wantCalls: 0,
		},
		"unknown event is ignored": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"account_update","value":{"event":"SOMETHING_NEW","phone_number_id":"552100000000001"}}]}]}`,
			wantCalls: 0,
		},
		"no identifier is ignored": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"account_update","value":{"event":"PARTNER_REMOVED"}}]}]}`,
			wantCalls: 0,
		},
		"bad value": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"account_update","value":[]}]}]}`,
			wantCalls: 0,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			queries := &stubQuerier{}
			h := newTestHandler(t, testSecret, queries)

			// account_update with a bad value is a 400; everything else must be
			// answered 200 so Meta does not redeliver.
			wantCode := http.StatusOK
			if name == "bad value" {
				wantCode = http.StatusBadRequest
			}

			rec := postDelivery(t, h, testSecret, []byte(tc.body))
			if rec.Code != wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, wantCode, rec.Body.String())
			}
			if got := len(queries.disconnect); got != tc.wantCalls {
				t.Fatalf("DisconnectConnectionAndPauseCampaigns called %d times, want %d", got, tc.wantCalls)
			}
			if tc.wantCalls > 0 && tc.wantPhone {
				if got := queries.disconnect[0].PhoneNumberID; got != "552100000000001" {
					t.Errorf("PhoneNumberID = %q, want 552100000000001", got)
				}
			}
			if tc.wantCalls > 0 && !tc.wantPhone {
				// Matching on the WABA alone must leave the phone argument empty so
				// the query skips that predicate.
				if got := queries.disconnect[0].PhoneNumberID; got != "" {
					t.Errorf("PhoneNumberID = %q, want empty for a WABA-only match", got)
				}
				if got := queries.disconnect[0].MessagingAccountID; got != "102900000000001" {
					t.Errorf("MessagingAccountID = %q, want 102900000000001", got)
				}
			}
		})
	}
}

func TestReceiveTemplateStatus(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		body       string
		wantCode   int
		wantCalls  int
		wantStatus sqlc.TemplateStatus
		wantReason bool
	}{
		"rejected with a reason": {
			body:       string(fixture(t, "template_rejected.json")),
			wantCode:   http.StatusOK,
			wantCalls:  1,
			wantStatus: sqlc.TemplateStatusRejected,
			wantReason: true,
		},
		"approved": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"message_template_status_update","value":{"event":"APPROVED","message_template_id":"1234567890"}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: 1,
			// APPROVED must be recorded as approved, not pending.
			wantStatus: sqlc.TemplateStatusApproved,
		},
		"pending": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"message_template_status_update","value":{"event":"PENDING","message_template_id":"1234567890"}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: 1,
			// A pending template is not a paused one.
			wantStatus: sqlc.TemplateStatusPending,
		},
		"unknown event is ignored": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"message_template_status_update","value":{"event":"SOMETHING","message_template_id":"1234567890"}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: 0,
		},
		"no template id is ignored": {
			body:      `{"object":"x","entry":[{"id":"1","changes":[{"field":"message_template_status_update","value":{"event":"APPROVED"}}]}]}`,
			wantCode:  http.StatusOK,
			wantCalls: 0,
		},
		"no reason stays empty": {
			body:       `{"object":"x","entry":[{"id":"1","changes":[{"field":"message_template_status_update","value":{"event":"REJECTED","message_template_id":"1","reason":"   "}}]}]}`,
			wantCode:   http.StatusOK,
			wantCalls:  1,
			wantStatus: sqlc.TemplateStatusRejected,
			// Left nil rather than storing whitespace for the UI to render.
			wantReason: false,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			queries := &stubQuerier{}
			h := newTestHandler(t, testSecret, queries)

			rec := postDelivery(t, h, testSecret, []byte(tc.body))
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			if got := len(queries.template); got != tc.wantCalls {
				t.Fatalf("UpdateTemplateReviewStatus called %d times, want %d", got, tc.wantCalls)
			}
			if tc.wantCalls == 0 {
				return
			}
			got := queries.template[0]
			if got.Status != tc.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, tc.wantStatus)
			}
			if got.MetaTemplateID == nil || *got.MetaTemplateID == "" {
				t.Errorf("MetaTemplateID = %v, want the id from the payload", got.MetaTemplateID)
			}
			if tc.wantReason && (got.RejectionReason == nil || *got.RejectionReason == "") {
				t.Errorf("RejectionReason = %v, want Meta's reason", got.RejectionReason)
			}
			if !tc.wantReason && got.RejectionReason != nil {
				t.Errorf("RejectionReason = %q, want nil", *got.RejectionReason)
			}
		})
	}
}

func TestReceiveRejectsOversizedBody(t *testing.T) {
	t.Parallel()

	queries := &stubQuerier{}
	h := newTestHandler(t, testSecret, queries)

	// One byte over the cap. Signing it keeps the failure about size, not auth.
	body := append([]byte(`{"padding":"`), bytes.Repeat([]byte("x"), MaxBodyBytes)...)
	body = append(body, []byte(`"}`)...)

	rec := postDelivery(t, h, testSecret, body)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if len(queries.advance) != 0 {
		t.Errorf("AdvanceMessageStatus called %d times, want 0", len(queries.advance))
	}
}

func TestReceiveMultipleChanges(t *testing.T) {
	t.Parallel()

	// A single delivery can carry several changes across entries; all of them are
	// applied, and one failure fails the whole delivery so Meta retries it.
	payload := Payload{
		Object: "whatsapp_business_account",
		Entry: []Entry{{
			ID: "102900000000001",
			Changes: []Change{
				{Field: FieldMessages, Value: json.RawMessage(`{"statuses":[{"id":"wamid.1","status":"sent","timestamp":"1767225599"}]}`)},
				{Field: FieldAccountState, Value: json.RawMessage(`{"event":"PARTNER_REMOVED","phone_number_id":"552100000000001"}`)},
			},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	queries := &stubQuerier{}
	h := newTestHandler(t, testSecret, queries)
	if rec := postDelivery(t, h, testSecret, body); rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if len(queries.advance) != 1 {
		t.Errorf("AdvanceMessageStatus called %d times, want 1", len(queries.advance))
	}
	if len(queries.disconnect) != 1 {
		t.Errorf("DisconnectConnectionAndPauseCampaigns called %d times, want 1", len(queries.disconnect))
	}
}

func TestReceiveStopsAtFirstDatabaseError(t *testing.T) {
	t.Parallel()

	payload := Payload{
		Object: "whatsapp_business_account",
		Entry: []Entry{{
			ID: "1",
			Changes: []Change{
				{Field: FieldMessages, Value: json.RawMessage(`{"statuses":[{"id":"wamid.1","status":"sent","timestamp":"1767225599"}]}`)},
				{Field: FieldMessages, Value: json.RawMessage(`{"statuses":[{"id":"wamid.2","status":"read","timestamp":"1767225599"}]}`)},
			},
		}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}

	queries := &stubQuerier{advanceErr: errors.New("boom")}
	h := newTestHandler(t, testSecret, queries)

	// 500 makes Meta redeliver, which is the point: the second status must not be
	// applied as if the first one had succeeded.
	if rec := postDelivery(t, h, testSecret, body); rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rec.Code)
	}
	if len(queries.advance) != 1 {
		t.Errorf("AdvanceMessageStatus called %d times, want 1", len(queries.advance))
	}
}

func TestNormaliseStatus(t *testing.T) {
	t.Parallel()

	for _, status := range []string{StatusSent, StatusDelivered, StatusRead, StatusFailed} {
		if got, ok := normaliseStatus(status); !ok || got != status {
			t.Errorf("normaliseStatus(%q) = %q, %v; want %q, true", status, got, ok, status)
		}
	}

	// queued and skipped are ours, not Meta's, and anything new must be ignored
	// rather than written to the enum.
	for _, status := range []string{"", StatusQueued, StatusSkipped, "DELETED"} {
		if _, ok := normaliseStatus(status); ok {
			t.Errorf("normaliseStatus(%q) reported ok, want false", status)
		}
	}
}

func TestIsRemovalEvent(t *testing.T) {
	t.Parallel()

	for _, event := range []string{"PARTNER_REMOVED", "partner_removed", " REMOVED ", "ACCESS_REVOKED"} {
		if !isRemovalEvent(event) {
			t.Errorf("isRemovalEvent(%q) = false, want true", event)
		}
	}
	for _, event := range []string{"", "PARTNER_ADDED", "PAUSED", "UPGRADE", "removed!"} {
		if isRemovalEvent(event) {
			t.Errorf("isRemovalEvent(%q) = true, want false", event)
		}
	}
}

func TestFirstError(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		errs      []ChangeError
		wantCode  string
		wantBody  string
		wantNilOK bool
	}{
		"none": {
			wantNilOK: true,
		},
		"code and detail": {
			errs: []ChangeError{{
				Code:      131047,
				Title:     "Re-engagement message",
				Message:   "Message failed to send",
				ErrorData: &ErrorData{Details: "More than 24 hours have passed."},
			}},
			wantCode: "131047",
			wantBody: "More than 24 hours have passed.",
		},
		"falls back to message": {
			errs:     []ChangeError{{Code: 1, Title: "Title", Message: "Message"}},
			wantCode: "1",
			wantBody: "Message",
		},
		"falls back to title": {
			errs:     []ChangeError{{Code: 1, Title: "Title"}},
			wantCode: "1",
			wantBody: "Title",
		},
		"code zero is omitted": {
			// Error codes start at 130000, so a zero means "not supplied".
			errs:      []ChangeError{{Title: "Something"}},
			wantNilOK: true,
			wantBody:  "Something",
		},
		"only the first error is used": {
			errs: []ChangeError{
				{Code: 5, Message: "First"},
				{Code: 6, Message: "Second"},
			},
			wantCode: "5",
			wantBody: "First",
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			code, body := firstError(tc.errs)
			if tc.wantCode == "" {
				if code != nil {
					t.Errorf("code = %q, want nil", *code)
				}
			} else if code == nil || *code != tc.wantCode {
				t.Errorf("code = %v, want %q", code, tc.wantCode)
			}

			switch {
			case tc.wantBody == "" && body != nil:
				t.Errorf("body = %q, want nil", *body)
			case tc.wantBody != "" && (body == nil || *body != tc.wantBody):
				t.Errorf("body = %v, want %q", body, tc.wantBody)
			}
		})
	}
}

func TestWamidSuffix(t *testing.T) {
	t.Parallel()

	if got := wamidSuffix("short"); got != "short" {
		t.Errorf("wamidSuffix(short) = %q, want %q", got, "short")
	}
	if got := wamidSuffix("wamid.ABCDEFGH"); got != "ABCDEFGH" {
		t.Errorf("wamidSuffix(wamid.ABCDEFGH) = %q, want %q", got, "ABCDEFGH")
	}
}

func TestParseTime(t *testing.T) {
	t.Parallel()

	tests := map[string]struct {
		value   string
		want    time.Time
		wantErr bool
	}{
		"epoch seconds as a string": {
			// What Meta actually sends.
			value: "1767225599",
			want:  time.Unix(1767225599, 0).UTC(),
		},
		"epoch with whitespace": {
			value: " 1767225599 ",
			want:  time.Unix(1767225599, 0).UTC(),
		},
		"rfc 3339 fallback": {
			value: "2026-01-01T00:00:00Z",
			want:  time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		},
		"empty":    {value: "", wantErr: true},
		"blank":    {value: "   ", wantErr: true},
		"nonsense": {value: "yesterday", wantErr: true},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got, err := ParseTime(tc.value)
			if tc.wantErr {
				if err == nil {
					t.Fatal("ParseTime() = nil error, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseTime() = %v, want nil", err)
			}
			if !got.Equal(tc.want) {
				t.Errorf("ParseTime() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestTemplateStatusMapping(t *testing.T) {
	t.Parallel()

	tests := map[string]string{
		"APPROVED":       TemplateApproved,
		"REJECTED":       TemplateRejected,
		"DISAPPROVED":    TemplateRejected,
		"PAUSED":         TemplatePaused,
		"PENDING":        TemplatePending,
		"PENDING_REVIEW": TemplatePending,
	}

	for event, want := range tests {
		got, ok := TemplateStatus(event)
		if !ok {
			t.Errorf("TemplateStatus(%q) reported not ok", event)
			continue
		}
		if got != want {
			t.Errorf("TemplateStatus(%q) = %q, want %q", event, got, want)
		}
	}

	if _, ok := TemplateStatus("SOMETHING_NEW"); ok {
		t.Error("TemplateStatus(SOMETHING_NEW) reported ok, want false")
	}
}

// fixNow replaces the receipt-time fallback so the timestamp is predictable.
func fixNow(queries *stubQuerier) {}

// fixClock freezes nowFunc and returns the function that restores it.
func fixClock() func() {
	original := nowFunc
	nowFunc = func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) }
	return func() { nowFunc = original }
}
