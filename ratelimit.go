package main

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// failureLimiter locks a key out for a while after too many failures. It is
// used to make guessing the password of a share impractical: unlocking has no
// other cost, so without it a script could try thousands of passwords a second.
type failureLimiter struct {
	maxFailures int
	lockFor     time.Duration

	mu      sync.Mutex
	entries map[string]*failureEntry
}

type failureEntry struct {
	failures    int
	lockedUntil time.Time
	lastFailure time.Time
}

// failureMemory is how long failures count towards a lockout.
const failureMemory = 10 * time.Minute

func newFailureLimiter(maxFailures int, lockFor time.Duration) *failureLimiter {
	return &failureLimiter{
		maxFailures: maxFailures,
		lockFor:     lockFor,
		entries:     make(map[string]*failureEntry),
	}
}

// retryAfter returns how long the key is still locked out, or 0.
func (l *failureLimiter) retryAfter(key string, now time.Time) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e, ok := l.entries[key]; ok && e.lockedUntil.After(now) {
		return e.lockedUntil.Sub(now)
	}
	return 0
}

func (l *failureLimiter) recordFailure(key string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	e, ok := l.entries[key]
	if !ok {
		e = &failureEntry{}
		l.entries[key] = e
	}
	if now.Sub(e.lastFailure) > failureMemory {
		e.failures = 0
	}
	e.lastFailure = now
	e.failures++
	if e.failures >= l.maxFailures {
		e.failures = 0
		e.lockedUntil = now.Add(l.lockFor)
	}
}

func (l *failureLimiter) reset(key string) {
	l.mu.Lock()
	delete(l.entries, key)
	l.mu.Unlock()
}

// prune forgets keys that are neither locked nor recently failed, so the map
// cannot grow without bound.
func (l *failureLimiter) prune(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for key, e := range l.entries {
		if !e.lockedUntil.After(now) && now.Sub(e.lastFailure) > failureMemory {
			delete(l.entries, key)
		}
	}
}

// clientAddr identifies the peer of a request. Forwarding headers are ignored
// on purpose: they are attacker-controlled unless a trusted proxy sets them.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
