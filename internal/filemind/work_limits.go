package filemind

import (
	"context"
	"sync"
)

// Entries exist only while work is running; rejected requests never queue or
// retain a user entry. This bounds both concurrency and limiter memory.
type userWorkLimit struct {
	mu     sync.Mutex
	active map[string]int
	limit  int
}

type workBusyError struct{ message string }

func (e *workBusyError) Error() string { return e.message }
func (e *workBusyError) Unwrap() error { return &problem{429, e.message} }

func newUserWorkLimit(limit int) *userWorkLimit {
	return &userWorkLimit{active: make(map[string]int), limit: limit}
}

func (l *userWorkLimit) acquire(key string) (func(), error) {
	if key == "" {
		return func() {}, nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.active[key] >= l.limit {
		return nil, &workBusyError{"Your account already has work in progress. Try again shortly."}
	}
	l.active[key]++
	return func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.active[key]--
		if l.active[key] == 0 {
			delete(l.active, key)
		}
	}, nil
}

func acquireWork(ctx context.Context, slots chan struct{}, users *userWorkLimit, userID string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releaseUser, err := users.acquire(userID)
	if err != nil {
		return nil, err
	}
	select {
	case slots <- struct{}{}:
		return func() { <-slots; releaseUser() }, nil
	default:
		releaseUser()
		return nil, &workBusyError{"Server work capacity is busy. Try again shortly."}
	}
}

func (a *App) acquireHash(ctx context.Context, admin bool, userID string) (func(), error) {
	slots := a.hashSlots
	if admin {
		slots = a.adminHashSlots
	}
	return acquireWork(ctx, slots, a.hashUsers, userID)
}

// The lease ends inside this helper, before callers acquire locks or start a
// database transaction. Bulk transfer passwords use the ordinary hash pool.
func (a *App) hashNewPassword(ctx context.Context, user User, admin bool, password string) (string, error) {
	release, err := a.acquireHash(ctx, admin, user.ID)
	if err != nil {
		return "", err
	}
	defer release()
	hash, err := hashPassword(password)
	if err == nil {
		err = ctx.Err()
	}
	return hash, err
}

func (a *App) acquireStorageWork(ctx context.Context, userID string) (func(), error) {
	return acquireWork(ctx, a.integritySlots, a.integrityUsers, userID)
}
