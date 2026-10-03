package httpx

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ClientIP extracts the caller's address for rate limiting and audit logs.
//
// X-Forwarded-For is only honoured when trustProxy is true, because the header
// is attacker-controlled: a client could otherwise spoof its address and bypass
// per-IP limits.
func ClientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			// Left-most entry is the original client.
			if first, _, found := strings.Cut(xff, ","); found {
				xff = first
			}
			if ip := strings.TrimSpace(xff); ip != "" {
				return ip
			}
		}
		if real := strings.TrimSpace(r.Header.Get("X-Real-IP")); real != "" {
			return real
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// RequestIDHeader carries a correlation id across the app and into Meta.
const RequestIDHeader = "X-Request-ID"

type requestIDContextKey struct{}

// RequestID returns the correlation id for a request, generating one when the
// caller did not supply it, and stores it on the request so that every later
// call agrees. Without the context, the id echoed in the response header and
// the id written to the log would be two different random values.
func RequestID(r *http.Request) string {
	if id, ok := r.Context().Value(requestIDContextKey{}).(string); ok && id != "" {
		return id
	}

	id := strings.TrimSpace(r.Header.Get(RequestIDHeader))
	if id == "" || len(id) > 64 {
		id = NewRequestID()
	}

	*r = *r.WithContext(context.WithValue(r.Context(), requestIDContextKey{}, id))
	return id
}

// NewRequestID generates a random correlation id.
func NewRequestID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "req-fallback"
	}
	return hex.EncodeToString(buf)
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush lets templ and streaming responses work through the recorder.
func (s *statusRecorder) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Logger records one structured line per request.
type Logger struct {
	log        *slog.Logger
	trustProxy bool
}

// NewLogger builds request logging middleware.
func NewLogger(log *slog.Logger, trustProxy bool) *Logger {
	return &Logger{log: log, trustProxy: trustProxy}
}

// Middleware logs method, path, status, duration and the correlation id, and
// echoes the id back to the caller.
func (l *Logger) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		id := RequestID(r)
		w.Header().Set(RequestIDHeader, id)

		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)

		l.log.InfoContext(r.Context(), "http request",
			"request_id", id,
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration_ms", time.Since(start).Milliseconds(),
			"ip", ClientIP(r, l.trustProxy),
		)
	})
}

// Recoverer turns a panic into a 500 so one bad request cannot kill the process.
func Recoverer(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.ErrorContext(r.Context(), "panic recovered",
						"request_id", RequestID(r),
						"method", r.Method,
						"path", r.URL.Path,
						"panic", rec,
					)
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// Timeout bounds handler execution.
func Timeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.TimeoutHandler(next, d, "request timed out")
	}
}
