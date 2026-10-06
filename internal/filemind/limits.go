package filemind

import (
	"container/list"
	"hash/maphash"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"
)

type visitor struct {
	requests, passwordAttempts []time.Time
	seen                       time.Time
	downloads                  int
	entry                      *list.Element
}
type limiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
	order    list.List
	overflow [overflowVisitorBuckets]visitor
	seed     maphash.Seed
	now      func() time.Time
}

const (
	maxVisitors            = 10000
	overflowVisitorBuckets = 256
	visitorIdleWindow      = time.Minute
	visitorEvictionScan    = 32
)

func newLimiter() *limiter {
	return &limiter{visitors: map[string]*visitor{}, seed: maphash.MakeSeed(), now: time.Now}
}

// Called with mu held. Exact-IP entries are evicted only after all rate windows
// expire and all their downloads finish. A bounded overflow bucket retains
// accounting for new addresses while every exact entry is still in use.
func (l *limiter) visitor(ip string, now time.Time) *visitor {
	v := l.visitors[ip]
	if v != nil {
		v.seen = now
		l.order.MoveToBack(v.entry)
		return v
	}
	bucket := &l.overflow[maphash.String(l.seed, ip)%overflowVisitorBuckets]
	// Keep overflow users in the same bucket until its counters expire. Moving
	// them into a fresh exact entry sooner would reset throttles/download counts.
	if bucket.downloads > 0 || (!bucket.seen.IsZero() && now.Sub(bucket.seen) < visitorIdleWindow) {
		bucket.seen = now
		return bucket
	}
	if len(l.visitors) >= maxVisitors {
		for entry, scanned := l.order.Front(), 0; entry != nil && scanned < visitorEvictionScan; scanned++ {
			next := entry.Next()
			key := entry.Value.(string)
			candidate := l.visitors[key]
			if now.Sub(candidate.seen) < visitorIdleWindow {
				break
			}
			if candidate.downloads == 0 {
				delete(l.visitors, key)
				l.order.Remove(entry)
				break
			}
			entry = next
		}
	}
	if len(l.visitors) >= maxVisitors {
		*bucket = visitor{seen: now}
		return bucket
	}
	v = &visitor{seen: now}
	v.entry = l.order.PushBack(ip)
	l.visitors[ip] = v
	return v
}
func trimTimes(values []time.Time, after time.Time) []time.Time {
	n := 0
	for n < len(values) && !values[n].After(after) {
		n++
	}
	return values[n:]
}
func (l *limiter) allow(ip string, public bool) (bool, int) {
	// Authenticated interfaces only limit password checks, so ordinary browsing
	// neither consumes visitor entries nor depends on visitor-table capacity.
	if !public {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	v := l.visitor(ip, now)
	v.requests = trimTimes(v.requests, now.Add(-10*time.Second))
	if len(v.requests) >= 60 {
		return false, 10
	}
	v.requests = append(v.requests, now)
	return true, 0
}

// Limit password-check traffic without banning the source's unrelated requests.
func (l *limiter) passwordAttempt(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	v := l.visitor(ip, now)
	v.passwordAttempts = trimTimes(v.passwordAttempts, now.Add(-time.Minute))
	if len(v.passwordAttempts) >= 60 {
		return false, int(v.passwordAttempts[0].Add(time.Minute).Sub(now).Seconds()) + 1
	}
	v.passwordAttempts = append(v.passwordAttempts, now)
	return true, 0
}
func (l *limiter) download(ip string) (func(), bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.visitor(ip, l.now())
	if v.downloads >= 4 {
		return nil, false
	}
	v.downloads++
	// Capture the actual counter, rather than looking the IP up again after
	// eviction or overflow allocation. Each reservation is released once.
	return sync.OnceFunc(func() {
		l.mu.Lock()
		defer l.mu.Unlock()
		v.downloads--
		v.seen = l.now()
		if v.entry != nil {
			l.order.MoveToBack(v.entry)
		}
	}), true
}
func (a *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return "unknown"
	}
	ip = ip.Unmap()
	for _, prefix := range a.cfg.TrustedProxies {
		if prefix.Contains(ip) {
			header := r.Header.Get("X-Forwarded-For")
			if len(header) > 128 {
				break
			}
			parts := strings.Split(header, ",")
			if forwarded, e := netip.ParseAddr(strings.TrimSpace(parts[len(parts)-1])); e == nil {
				return forwarded.Unmap().String()
			}
			break
		}
	}
	return ip.String()
}
