package filemind

import (
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
}
type limiter struct {
	mu       sync.Mutex
	visitors map[string]*visitor
}

func newLimiter() *limiter { return &limiter{visitors: map[string]*visitor{}} }
func (l *limiter) visitor(ip string, now time.Time) *visitor {
	v := l.visitors[ip]
	if v != nil {
		v.seen = now
		return v
	}
	if len(l.visitors) >= 10000 {
		for k, v := range l.visitors {
			if v.downloads == 0 && now.Sub(v.seen) > 30*time.Minute {
				delete(l.visitors, k)
			}
		}
	}
	if len(l.visitors) >= 10000 {
		return nil
	}
	v = &visitor{seen: now}
	l.visitors[ip] = v
	return v
}
func trimTimes(values []time.Time, after time.Time) []time.Time {
	n := 0
	for n < len(values) && values[n].Before(after) {
		n++
	}
	return values[n:]
}
func (l *limiter) allow(ip string, public bool) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	v := l.visitor(ip, now)
	if v == nil {
		return false, 10
	}
	if public {
		v.requests = trimTimes(v.requests, now.Add(-10*time.Second))
		if len(v.requests) >= 60 {
			return false, 10
		}
		v.requests = append(v.requests, now)
	}
	return true, 0
}

// Limit password-check traffic without banning the source's unrelated requests.
func (l *limiter) passwordAttempt(ip string) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	v := l.visitor(ip, now)
	if v == nil {
		return false, 60
	}
	v.passwordAttempts = trimTimes(v.passwordAttempts, now.Add(-time.Minute))
	if len(v.passwordAttempts) >= 60 {
		return false, int(v.passwordAttempts[0].Add(time.Minute).Sub(now).Seconds()) + 1
	}
	v.passwordAttempts = append(v.passwordAttempts, now)
	return true, 0
}
func (l *limiter) download(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	v := l.visitor(ip, time.Now())
	if v == nil || v.downloads >= 4 {
		return false
	}
	v.downloads++
	return true
}
func (l *limiter) release(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if v := l.visitors[ip]; v != nil && v.downloads > 0 {
		v.downloads--
	}
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
