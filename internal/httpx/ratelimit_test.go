package httpx

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestLimiterAllowsUpToLimit(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(3, time.Minute)

	for i := 1; i <= 3; i++ {
		if !limiter.Allow("client") {
			t.Fatalf("Allow() = false on attempt %d, want true", i)
		}
	}
	if limiter.Allow("client") {
		t.Error("Allow() = true on attempt 4, want false (over limit)")
	}
}

func TestLimiterIsolatesKeys(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(1, time.Minute)

	if !limiter.Allow("a") {
		t.Fatal("Allow(a) = false, want true")
	}
	if !limiter.Allow("b") {
		t.Error("Allow(b) = false, want true; keys must not share a budget")
	}
	if limiter.Allow("a") {
		t.Error("Allow(a) = true on second call, want false")
	}
}

func TestLimiterResetsAfterWindow(t *testing.T) {
	t.Parallel()

	now := time.Unix(1_700_000_000, 0)
	limiter := NewLimiter(2, time.Minute)
	limiter.now = func() time.Time { return now }

	limiter.Allow("k")
	limiter.Allow("k")
	if limiter.Allow("k") {
		t.Fatal("Allow() = true past the limit, want false")
	}

	// Advance past the window.
	now = now.Add(time.Minute + time.Second)

	if !limiter.Allow("k") {
		t.Error("Allow() = false after the window elapsed, want true")
	}
}

func TestLimiterRemaining(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(3, time.Minute)

	if got := limiter.Remaining("k"); got != 3 {
		t.Errorf("Remaining() before use = %d, want 3", got)
	}
	limiter.Allow("k")
	if got := limiter.Remaining("k"); got != 2 {
		t.Errorf("Remaining() after one = %d, want 2", got)
	}
	limiter.Reset("k")
	if got := limiter.Remaining("k"); got != 3 {
		t.Errorf("Remaining() after reset = %d, want 3", got)
	}
}

func TestLimiterDisabledWhenLimitNotPositive(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{0, -1} {
		limiter := NewLimiter(limit, time.Minute)
		for i := 0; i < 100; i++ {
			if !limiter.Allow("k") {
				t.Fatalf("Allow() = false with limit %d, want always true", limit)
			}
		}
	}
}

func TestLimiterMiddlewareReturns429(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(1, time.Minute)
	middleware := limiter.Middleware(ByIP(false))

	reached := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached++
		w.WriteHeader(http.StatusOK)
	})

	// Both requests come from the same address, so they share one budget.
	send := func() int {
		req := httptest.NewRequest(http.MethodPost, "/", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		rec := httptest.NewRecorder()
		middleware(next).ServeHTTP(rec, req)
		return rec.Code
	}

	if got := send(); got != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", got)
	}
	if got := send(); got != http.StatusTooManyRequests {
		t.Errorf("second request status = %d, want 429", got)
	}
	if reached != 1 {
		t.Errorf("next reached %d times, want 1 (blocked request must not pass through)", reached)
	}
}

func TestLimiterMiddlewareAllowsUnderLimit(t *testing.T) {
	t.Parallel()

	limiter := NewLimiter(5, time.Minute)
	middleware := limiter.Middleware(ByIP(false))

	called := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	})

	rec := httptest.NewRecorder()
	middleware(next).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !called {
		t.Error("next handler was not reached")
	}
}

func TestLimiterConcurrentAccessIsSafe(t *testing.T) {
	t.Parallel()

	const limit = 1000
	limiter := NewLimiter(limit, time.Minute)

	const goroutines = 50
	const perGoroutine = 20

	var wg sync.WaitGroup
	var mu sync.Mutex
	allowed := 0

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			local := 0
			for j := 0; j < perGoroutine; j++ {
				if limiter.Allow("shared") {
					local++
				}
			}
			mu.Lock()
			allowed += local
			mu.Unlock()
		}()
	}
	wg.Wait()

	if allowed != limit {
		t.Errorf("allowed %d requests, want exactly %d", allowed, limit)
	}
}

func TestClientIP(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		remoteAddr   string
		forwardedFor string
		realIP       string
		trustProxy   bool
		want         string
	}{
		{name: "remote addr only", remoteAddr: "203.0.113.5:1234", want: "203.0.113.5"},
		{name: "ipv6 remote addr", remoteAddr: "[2001:db8::1]:443", want: "2001:db8::1"},
		{name: "remote addr without port", remoteAddr: "203.0.113.5", want: "203.0.113.5"},
		{name: "ignores forwarded when untrusted", remoteAddr: "203.0.113.5:1234", forwardedFor: "1.2.3.4", want: "203.0.113.5"},
		{name: "uses forwarded when trusted", remoteAddr: "203.0.113.5:1234", forwardedFor: "1.2.3.4", trustProxy: true, want: "1.2.3.4"},
		{name: "left-most forwarded entry", remoteAddr: "10.0.0.1:1", forwardedFor: "9.9.9.9, 8.8.8.8", trustProxy: true, want: "9.9.9.9"},
		{name: "falls back to real ip", remoteAddr: "10.0.0.1:1", realIP: "5.5.5.5", trustProxy: true, want: "5.5.5.5"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req.RemoteAddr = tc.remoteAddr
			if tc.forwardedFor != "" {
				req.Header.Set("X-Forwarded-For", tc.forwardedFor)
			}
			if tc.realIP != "" {
				req.Header.Set("X-Real-IP", tc.realIP)
			}

			if got := ClientIP(req, tc.trustProxy); got != tc.want {
				t.Errorf("ClientIP() = %q, want %q", got, tc.want)
			}
		})
	}
}
