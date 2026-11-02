package auth

import (
	"sync"
	"time"
)

const (
	// keys longer than this are cut, so that a long name cannot fill memory
	maxKeyLength = 128
	// more distinct keys than this within a window means an attack; new keys are
	// refused then rather than letting the map grow
	maxKeys = 50000
)

// Limiter counts login attempts per key, such as an address or a user name, and
// blocks a key after too many within the window.
type Limiter struct {
	mu       sync.Mutex
	attempts map[string][]time.Time
	limits   func() (int, time.Duration)
	window   time.Duration
	now      func() time.Time
}

// NewLimiter allows a number of failures per key within a window. limits is asked each
// time, so that a change in the settings applies at once; zero failures turn it off.
func NewLimiter(limits func() (int, time.Duration), now func() time.Time) *Limiter {
	return &Limiter{attempts: map[string][]time.Time{}, limits: limits, now: now}
}

func cut(key string) string {
	if len(key) > maxKeyLength {
		return key[:maxKeyLength]
	}
	return key
}

func (l *Limiter) recent(key string, now time.Time) []time.Time {
	kept := l.attempts[key][:0]
	for _, t := range l.attempts[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.attempts, key)
		return nil
	}
	l.attempts[key] = kept
	return kept
}

// Attempt checks and counts an attempt in one step, so that parallel requests cannot
// all pass before the first failure is recorded. A correct login is taken back with
// Succeeded.
func (l *Limiter) Attempt(keys ...string) (time.Duration, bool) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()
	limit, window := l.limits()
	if limit <= 0 {
		return 0, true
	}
	l.window = window
	if len(l.attempts) >= maxKeys {
		for key := range l.attempts {
			l.recent(key, now)
		}
	}
	var wait time.Duration
	for _, key := range keys {
		key = cut(key)
		if _, known := l.attempts[key]; !known && len(l.attempts) >= maxKeys {
			// a flood of keys refuses new ones, but known addresses and users still pass
			wait = max(wait, l.window)
			continue
		}
		if recent := l.recent(key, now); len(recent) >= limit {
			wait = max(wait, l.window-now.Sub(recent[0]))
		}
	}
	if wait > 0 {
		return wait, false
	}
	for _, key := range keys {
		key = cut(key)
		l.attempts[key] = append(l.attempts[key], now)
	}
	return 0, true
}

// Succeeded takes back the attempt of a correct login. It forgets the user name, but
// only one attempt of the address: a valid account of its own must not let an attacker
// reset the count of an address between guesses.
func (l *Limiter) Succeeded(user, address string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.attempts, cut(user))
	switch list := l.attempts[cut(address)]; len(list) {
	case 0:
	case 1:
		delete(l.attempts, cut(address))
	default:
		l.attempts[cut(address)] = list[:len(list)-1]
	}
}
