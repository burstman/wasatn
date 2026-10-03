package httpx

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRequestIDIsStableAcrossCalls(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	first := RequestID(req)
	second := RequestID(req)
	if first != second {
		t.Errorf("RequestID() = %q then %q, want the same id", first, second)
	}
	if len(first) != 32 {
		t.Errorf("RequestID() = %q, want 32 hex characters", first)
	}
}

func TestRequestIDAdoptsCallerSuppliedID(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, "trace-abc-123")
	if got := RequestID(req); got != "trace-abc-123" {
		t.Errorf("RequestID() = %q, want the caller-supplied id", got)
	}
}

func TestRequestIDIgnoresOversizedCallerID(t *testing.T) {
	t.Parallel()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set(RequestIDHeader, strings.Repeat("x", 200))
	got := RequestID(req)
	if got == strings.Repeat("x", 200) {
		t.Error("RequestID() echoed an oversized caller id, want a generated one")
	}
	if len(got) != 32 {
		t.Errorf("RequestID() = %q, want a 32 character generated id", got)
	}
}

// The id echoed to the caller must be the same one written to the log.
func TestLoggerEchoesTheIDItLogs(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	mw := NewLogger(log, false).Middleware

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	})).ServeHTTP(rec, req)

	echoed := rec.Header().Get(RequestIDHeader)
	if echoed == "" {
		t.Fatal("response is missing the request id header")
	}

	var entry struct {
		RequestID string `json:"request_id"`
		Status    int    `json:"status"`
	}
	if err := json.Unmarshal(buf.Bytes(), &entry); err != nil {
		t.Fatalf("log line is not valid JSON: %v", err)
	}
	if entry.RequestID != echoed {
		t.Errorf("logged request_id %q does not match the echoed header %q", entry.RequestID, echoed)
	}
	if entry.Status != http.StatusTeapot {
		t.Errorf("logged status = %d, want 418", entry.Status)
	}
}

func TestLoggerRecordsClientIP(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "203.0.113.9:5555"

	NewLogger(log, false).Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(httptest.NewRecorder(), req)

	if !strings.Contains(buf.String(), `"ip":"203.0.113.9"`) {
		t.Errorf("log line = %q, want the client address", buf.String())
	}
}

func TestRecovererTurnsPanicInto500(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	log := slog.New(slog.NewJSONHandler(&buf, nil))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	h := Recoverer(log)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(buf.String(), "boom") {
		t.Errorf("log = %q, want the panic value recorded", buf.String())
	}
}
