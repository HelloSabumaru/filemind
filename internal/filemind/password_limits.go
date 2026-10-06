package filemind

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	maxPasswordTargets    = 10000
	passwordFailureWindow = 15 * time.Minute
	maxPasswordCooldown   = 30 * time.Second
)

type passwordFailure struct {
	failures   int
	seen, next time.Time
	checking   bool
}

type passwordLimiter struct {
	mu         sync.Mutex
	targets    map[string]*passwordFailure
	now        func() time.Time
	pruneAfter time.Time
}

func newPasswordLimiter() *passwordLimiter {
	return &passwordLimiter{targets: make(map[string]*passwordFailure), now: time.Now}
}

func (l *passwordLimiter) reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.targets, key)
}

type passwordCheck struct {
	limiter  *passwordLimiter
	key      string
	state    *passwordFailure
	finished bool
}

func (l *passwordLimiter) begin(key string) (*passwordCheck, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	state := l.targets[key]
	if state == nil {
		if len(l.targets) >= maxPasswordTargets && !l.pruneAfter.After(now) {
			for key, old := range l.targets {
				if !old.checking && now.Sub(old.seen) >= passwordFailureWindow {
					delete(l.targets, key)
				}
			}
			l.pruneAfter = now.Add(time.Minute)
		}
		if len(l.targets) >= maxPasswordTargets {
			return nil, 30
		}
		state = &passwordFailure{}
		l.targets[key] = state
	}
	if state.checking {
		return nil, 1
	}
	if now.Sub(state.seen) >= passwordFailureWindow {
		state.failures, state.next = 0, time.Time{}
	}
	if state.next.After(now) {
		return nil, int((state.next.Sub(now) + time.Second - 1) / time.Second)
	}
	state.checking, state.seen = true, now
	return &passwordCheck{limiter: l, key: key, state: state}, 0
}

func (c *passwordCheck) finish(valid bool) {
	c.limiter.mu.Lock()
	defer c.limiter.mu.Unlock()
	if c.finished {
		return
	}
	c.finished, c.state.checking = true, false
	// Credential changes can retire a state while a previous verification is
	// finishing. Its result must not affect the replacement credential.
	if c.limiter.targets[c.key] != c.state {
		return
	}
	if valid {
		delete(c.limiter.targets, c.key)
		return
	}
	now := c.limiter.now()
	c.state.seen = now
	// Two mistakes can be corrected immediately. Later failures introduce
	// 1, 2, 4, 8, 16, then 30 seconds of cooldown, never a permanent lockout.
	if c.state.failures < 8 {
		c.state.failures++
	}
	if c.state.failures >= 3 {
		delay := min(time.Second<<(c.state.failures-3), maxPasswordCooldown)
		c.state.next = now.Add(delay)
	}
}

// A storage failure, busy hash pool or canceled request is not a bad password.
func (c *passwordCheck) cancel() {
	c.limiter.mu.Lock()
	defer c.limiter.mu.Unlock()
	if !c.finished {
		c.finished, c.state.checking = true, false
		if c.state.failures == 0 && c.limiter.targets[c.key] == c.state {
			delete(c.limiter.targets, c.key)
		}
	}
}

type passwordThrottleError struct{ retry int }

func (e *passwordThrottleError) Error() string { return "Too many password attempts. Try again later." }
func (e *passwordThrottleError) Unwrap() error {
	return &problem{http.StatusTooManyRequests, e.Error()}
}

func unknownPasswordKey(username string) string {
	return tokenHash(strings.ToLower(strings.TrimSpace(username)))
}

// All password entry points share target counters and hash-pool protection.
// Unknown usernames have an independent bounded pool, so they cannot evict
// real account counters or consume the capacity reserved for known accounts.
func (a *App) verifyCredential(r *http.Request, source *limiter, targets *passwordLimiter, key, hash, password string, eligible bool) (bool, error) {
	if allowed, retry := source.passwordAttempt(a.clientIP(r)); !allowed {
		return false, &passwordThrottleError{retry}
	}
	check, retry := targets.begin(key)
	if check == nil {
		return false, &passwordThrottleError{retry}
	}
	defer check.cancel()
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		return false, &problem{429, "Password verification busy. Try again shortly."}
	}
	if err := r.Context().Err(); err != nil {
		return false, err
	}
	valid := verifyPassword(hash, password)
	if err := r.Context().Err(); err != nil {
		return false, err
	}
	valid = valid && eligible
	check.finish(valid)
	return valid, nil
}
