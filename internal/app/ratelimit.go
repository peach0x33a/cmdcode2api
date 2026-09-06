package app

import (
	"sync"
	"time"
)

// adminAuthRateLimit caps consecutive failed admin-password attempts per IP
// before the source is locked out (brute-force protection).
const (
	adminFailLimit   = 5
	adminFailWindow  = 10 * time.Minute
	adminLockoutTime = 15 * time.Minute
)

// ipRateLimiter tracks failed attempts per client IP. After adminFailLimit
// failures inside adminFailWindow, the IP is locked out for
// adminLockoutTime. A successful authentication clears the record.
type ipRateLimiter struct {
	mu    sync.Mutex
	fails map[string]*failRecord
}

type failRecord struct {
	count       int
	windowStart time.Time
	lockedUntil time.Time
}

func newIPRateLimiter() *ipRateLimiter {
	return &ipRateLimiter{fails: make(map[string]*failRecord)}
}

// Allow reports whether the IP may attempt authentication now, and how long
// until the lockout expires when it may not.
func (l *ipRateLimiter) Allow(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	rec, ok := l.fails[ip]
	if !ok {
		return true, 0
	}
	now := time.Now()
	if !rec.lockedUntil.IsZero() {
		if now.Before(rec.lockedUntil) {
			return false, time.Until(rec.lockedUntil)
		}
		delete(l.fails, ip)
		return true, 0
	}
	if now.Sub(rec.windowStart) > adminFailWindow {
		delete(l.fails, ip)
	}
	return true, 0
}

// Fail records a failed attempt, arming the lockout once the limit is hit.
func (l *ipRateLimiter) Fail(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	rec, ok := l.fails[ip]
	if !ok || now.Sub(rec.windowStart) > adminFailWindow {
		rec = &failRecord{windowStart: now}
		l.fails[ip] = rec
	}
	rec.count++
	if rec.count >= adminFailLimit {
		rec.lockedUntil = now.Add(adminLockoutTime)
	}
}

// Reset clears the IP's record after a successful authentication.
func (l *ipRateLimiter) Reset(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, ip)
}

func (l *ipRateLimiter) Len() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.fails)
}
