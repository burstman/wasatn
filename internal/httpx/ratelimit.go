package httpx

import (
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Limiter is a fixed-window rate limiter keyed by an arbitrary string (usually
// a user id or client IP).
//
// A fixed window allows a burst of 2x the limit across a window boundary, which
// is acceptable here: the limits guard credential stuffing and accidental
// hammering, not a billing quota.
type Limiter struct {
	mu      sync.Mutex
	limit   int
	window  time.Duration
	buckets map[string]bucket
	// now is injectable so tests do not sleep.
	now func() time.Time
}

type bucket struct {
	count int
	start time.Time
}

// NewLimiter allows limit events per window. A limit of zero or less disables
// limiting.
func NewLimiter(limit int, window time.Duration) *Limiter {
	return &Limiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]bucket),
		now:     time.Now,
	}
}

// Allow records an event for key and reports whether it is within the limit.
func (l *Limiter) Allow(key string) bool {
	if l == nil || l.limit <= 0 {
		return true
	}

	now := l.now()

	l.mu.Lock()
	defer l.mu.Unlock()

	// Opportunistically drop expired buckets so the map cannot grow unbounded
	// under a spray of distinct keys.
	if len(l.buckets) > 1024 {
		l.evictLocked(now)
	}

	b, ok := l.buckets[key]
	if !ok || now.Sub(b.start) >= l.window {
		l.buckets[key] = bucket{count: 1, start: now}
		return true
	}

	b.count++
	l.buckets[key] = b
	return b.count <= l.limit
}

// Remaining reports how many events are left in the current window for key.
func (l *Limiter) Remaining(key string) int {
	if l == nil || l.limit <= 0 {
		return -1
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok || l.now().Sub(b.start) >= l.window {
		return l.limit
	}
	if used := l.limit - b.count; used > 0 {
		return used
	}
	return 0
}

// Reset forgets the bucket for key.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.buckets, key)
}

func (l *Limiter) evictLocked(now time.Time) {
	for k, b := range l.buckets {
		if now.Sub(b.start) >= 2*l.window {
			delete(l.buckets, k)
		}
	}
}

// KeyFunc derives a rate limit key from a request.
type KeyFunc func(r *http.Request) string

// ByIP keys on the client address.
func ByIP(trustProxy bool) KeyFunc {
	return func(r *http.Request) string { return ClientIP(r, trustProxy) }
}

// BySessionUser keys on the signed-in user, falling back to the client address
// for anonymous requests.
func BySessionUser(sessions SessionReader, trustProxy bool) KeyFunc {
	return func(r *http.Request) string {
		if sessions != nil {
			if id := sessions.UserID(r); id != uuid.Nil {
				return "user:" + id.String()
			}
		}
		return "ip:" + ClientIP(r, trustProxy)
	}
}

// Middleware rejects requests over the limit with 429.
func (l *Limiter) Middleware(keyFunc KeyFunc) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(keyFunc(r)) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "too many requests", http.StatusTooManyRequests)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
