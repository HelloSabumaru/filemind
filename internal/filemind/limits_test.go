package filemind

import (
	"fmt"
	"hash/maphash"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Populate bounded in-memory fixture state directly, without generating traffic.
func fillVisitorTable(l *limiter, now time.Time) {
	for index := 0; len(l.visitors) < maxVisitors; index++ {
		key := fmt.Sprintf("fixture-%d", index)
		v := &visitor{seen: now, requests: []time.Time{now}}
		v.entry = l.order.PushBack(key)
		l.visitors[key] = v
	}
}

func collidingVisitorIPs(t *testing.T, l *limiter) (string, string) {
	t.Helper()
	seen := make(map[uint64]string)
	for index := 1; index <= overflowVisitorBuckets+1; index++ {
		ip := fmt.Sprintf("2001:db8::%x", index)
		bucket := maphash.String(l.seed, ip) % overflowVisitorBuckets
		if previous, found := seen[bucket]; found {
			return previous, ip
		}
		seen[bucket] = ip
	}
	t.Fatal("bounded fixture did not contain a bucket collision")
	return "", ""
}

func TestVisitorOverflowAdmitsNewClientsAndRetainsRateWindows(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	fillVisitorTable(l, now)
	first, second := collidingVisitorIPs(t, l)
	if allowed, _ := l.allow(first, true); !allowed {
		t.Fatal("visitor-table saturation rejected an unseen client")
	}
	if allowed, _ := l.passwordAttempt(first); !allowed {
		t.Fatal("visitor-table saturation rejected an unseen password check")
	}
	bucket := &l.overflow[maphash.String(l.seed, first)%overflowVisitorBuckets]
	bucket.requests = make([]time.Time, 60)
	bucket.passwordAttempts = make([]time.Time, 60)
	for index := range 60 {
		bucket.requests[index], bucket.passwordAttempts[index] = now, now
	}
	if allowed, _ := l.allow(second, true); allowed {
		t.Fatal("overflow allocation reset the shared request counter")
	}
	if allowed, _ := l.passwordAttempt(second); allowed {
		t.Fatal("overflow allocation reset the shared password counter")
	}
	if allowed, _ := l.allow("unseen-owner-client", false); !allowed || len(l.visitors) != maxVisitors {
		t.Fatal("ordinary owner browsing depended on visitor capacity")
	}
	now = now.Add(10 * time.Second)
	if allowed, _ := l.allow(second, true); !allowed {
		t.Fatal("public rate window did not expire after ten seconds")
	}
	if allowed, _ := l.passwordAttempt(second); allowed {
		t.Fatal("password rate window expired before one minute")
	}
	now = now.Add(50 * time.Second)
	if allowed, _ := l.passwordAttempt(second); !allowed {
		t.Fatal("password rate window did not expire after one minute")
	}
	if len(l.visitors) != maxVisitors || l.order.Len() != maxVisitors {
		t.Fatal("overflow grew the exact visitor table")
	}
}

func TestVisitorEvictionKeepsRecentCountersAndReclaimsExpiredEntries(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	fillVisitorTable(l, now)
	recent := l.visitors["fixture-0"]
	recent.requests, recent.passwordAttempts = make([]time.Time, 60), make([]time.Time, 60)
	for index := range 60 {
		recent.requests[index], recent.passwordAttempts[index] = now, now
	}
	if allowed, _ := l.allow("new-client", true); !allowed {
		t.Fatal("new client could not use overflow capacity")
	}
	if allowed, _ := l.allow("fixture-0", true); allowed {
		t.Fatal("table pressure cleared an unexpired request counter")
	}
	if allowed, _ := l.passwordAttempt("fixture-0"); allowed {
		t.Fatal("table pressure cleared an unexpired password counter")
	}
	now = now.Add(time.Minute)
	if allowed, _ := l.allow("after-expiry", true); !allowed {
		t.Fatal("client was rejected after visitor windows expired")
	}
	if l.visitors["fixture-1"] != nil || l.visitors["after-expiry"] == nil {
		t.Fatal("expired visitor state was retained for the old thirty-minute window")
	}
	if allowed, _ := l.passwordAttempt("fixture-0"); !allowed {
		t.Fatal("expired password counter continued blocking its address")
	}
	if len(l.visitors) != maxVisitors || l.order.Len() != maxVisitors {
		t.Fatal("eviction did not keep visitor memory bounded")
	}
}

func TestVisitorDownloadLeasesSurviveOverflowAndEviction(t *testing.T) {
	l := newLimiter()
	now := time.Now()
	l.now = func() time.Time { return now }
	mainFirst, ok := l.download("192.0.2.1")
	if !ok {
		t.Fatal("download was rejected")
	}
	t.Cleanup(mainFirst)
	mainSecond, ok := l.download("192.0.2.1")
	if !ok {
		t.Fatal("second download was rejected")
	}
	t.Cleanup(mainSecond)
	pinned := l.visitors["192.0.2.1"]
	fillVisitorTable(l, now)
	first, second := collidingVisitorIPs(t, l)
	var releases []func()
	for range 2 {
		release, allowed := l.download(first)
		if !allowed {
			t.Fatal("overflow rejected an available download")
		}
		t.Cleanup(release)
		releases = append(releases, release)
	}
	bucket := &l.overflow[maphash.String(l.seed, first)%overflowVisitorBuckets]
	now = now.Add(2 * time.Minute)
	// Reclaim one expired exact entry while active exact and overflow downloads
	// remain pinned. Free exact capacity must not migrate the overflow counter.
	entry := l.visitors["fixture-0"].entry
	delete(l.visitors, "fixture-0")
	l.order.Remove(entry)
	for _, ip := range []string{second, first} {
		release, allowed := l.download(ip)
		if !allowed {
			t.Fatal("available overflow download was rejected")
		}
		t.Cleanup(release)
		releases = append(releases, release)
	}
	if l.visitors[first] != nil || l.visitors[second] != nil || bucket.downloads != 4 {
		t.Fatal("overflow downloads migrated or lost accounting when exact capacity opened")
	}
	if _, allowed := l.download(first); allowed {
		t.Fatal("overflow allowed more than four simultaneous downloads")
	}
	releases[0]()
	releases[0]()
	if bucket.downloads != 3 {
		t.Fatal("releasing one lease twice removed another download's reservation")
	}
	for _, release := range releases[1:] {
		release()
	}
	if bucket.downloads != 0 || pinned.downloads != 2 {
		t.Fatal("download releases modified a different counter")
	}
	now = now.Add(time.Minute)
	release, allowed := l.download(first)
	if !allowed || l.visitors[first] == nil {
		t.Fatal("idle overflow client could not return to available exact capacity")
	}
	t.Cleanup(release)
	if bucket.downloads != 0 || l.visitors[first].downloads != 1 {
		t.Fatal("returning to exact capacity reused an old download reservation")
	}
	mainFirst()
	mainFirst()
	if pinned.downloads != 1 {
		t.Fatal("exact-IP lease release decremented another active download")
	}
	mainSecond()
	release()
	if pinned.downloads != 0 || l.visitors[first].downloads != 0 || l.order.Len() != len(l.visitors) {
		t.Fatal("completed downloads retained counters or invalid visitor ordering")
	}
}

func TestConcurrentOverflowDownloadsKeepTheirCapacityBound(t *testing.T) {
	l := newLimiter()
	fillVisitorTable(l, time.Now())
	releases := make(chan func(), 12)
	var group sync.WaitGroup
	for range 12 {
		group.Go(func() {
			if release, ok := l.download("2001:db8::1"); ok {
				releases <- release
			}
		})
	}
	group.Wait()
	close(releases)
	if len(releases) != 4 {
		t.Fatal("concurrent overflow downloads exceeded or lost available capacity")
	}
	for release := range releases {
		group.Go(release)
	}
	group.Wait()
	for index := range l.overflow {
		if l.overflow[index].downloads != 0 {
			t.Fatal("concurrent releases left a download reservation behind")
		}
	}
	if len(l.visitors) != maxVisitors {
		t.Fatal("concurrent overflow requests grew exact visitor state")
	}
}

func TestSaturatedVisitorTablesKeepBrowsingSignInAndDownloadsAvailable(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "", "available"), "available")
	for _, l := range []*limiter{a.ownerLimiter, a.adminLimiter, a.limiter} {
		fillVisitorTable(l, time.Now())
	}
	owner.remoteAddr = "192.0.2.101:1000"
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	fresh := newBrowser(a, false)
	fresh.remoteAddr = "192.0.2.102:1000"
	loginAs(t, fresh, "test-owner", "owner-password-for-validation")
	private := newBrowser(a, false)
	private.admin, private.remoteAddr = true, "192.0.2.103:1000"
	loginAs(t, private, "admin", "owner-password-for-validation")
	checkStatus(t, private.request("GET", "/admin/api/users", nil, nil), 200)
	public := newBrowser(a, true)
	public.remoteAddr = "192.0.2.104:1000"
	public.page(t, "/s/"+transfer.ShareToken)
	response := public.request("GET", downloadPath(transfer, 0), nil, nil)
	checkStatus(t, response, 200)
	if response.Body.String() != "available" {
		t.Fatal("overflow download changed its contents")
	}
	for _, l := range []*limiter{a.ownerLimiter, a.adminLimiter, a.limiter} {
		if len(l.visitors) != maxVisitors {
			t.Fatal("new clients grew the saturated visitor table")
		}
		for index := range l.overflow {
			if l.overflow[index].downloads != 0 {
				t.Fatal("completed overflow download retained its reservation")
			}
		}
	}
}

func TestGlobalDownloadRejectionReleasesOverflowReservation(t *testing.T) {
	a := testApp(t)
	fillVisitorTable(a.limiter, time.Now())
	for range cap(a.downloadSlots) {
		a.downloadSlots <- struct{}{}
	}
	t.Cleanup(func() {
		for len(a.downloadSlots) > 0 {
			<-a.downloadSlots
		}
	})
	request := httptest.NewRequest("GET", a.cfg.PublicURL+"/", nil)
	request.RemoteAddr = "192.0.2.105:1000"
	response := httptest.NewRecorder()
	if _, allowed := a.downloadCapacity(response, request); allowed || response.Code != 429 {
		t.Fatal("full global download capacity accepted another reservation")
	}
	ip := a.clientIP(request)
	bucket := &a.limiter.overflow[maphash.String(a.limiter.seed, ip)%overflowVisitorBuckets]
	if bucket.downloads != 0 {
		t.Fatal("global rejection leaked an overflow download reservation")
	}
	<-a.downloadSlots
	release, allowed := a.downloadCapacity(httptest.NewRecorder(), request)
	if !allowed {
		t.Fatal("overflow could not use newly available global download capacity")
	}
	release()
	a.downloadSlots <- struct{}{}
	if bucket.downloads != 0 {
		t.Fatal("successful download did not release its overflow counter")
	}
}
