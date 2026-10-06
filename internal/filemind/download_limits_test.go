package filemind

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"
)

func downloadCapacityRequest(a *App, ip string) *http.Request {
	r := httptest.NewRequest("GET", a.cfg.PublicURL+"/", nil)
	r.RemoteAddr = ip + ":12345"
	return r
}

func assertDownloadCapacityReleased(t *testing.T, a *App) {
	t.Helper()
	if len(a.downloadSlots) != 0 {
		t.Fatal("download retained global capacity")
	}
	for _, limit := range []*userWorkLimit{a.downloadTransfers, a.downloadUsers} {
		limit.mu.Lock()
		entries := len(limit.active)
		limit.mu.Unlock()
		if entries != 0 {
			t.Fatal("download retained transfer/uploader counters")
		}
	}
	a.limiter.mu.Lock()
	defer a.limiter.mu.Unlock()
	for _, visitor := range a.limiter.visitors {
		if visitor.downloads != 0 {
			t.Fatal("download retained an IP reservation")
		}
	}
	for _, visitor := range a.limiter.overflow {
		if visitor.downloads != 0 {
			t.Fatal("download retained an overflow IP reservation")
		}
	}
}

func TestDownloadBudgetsPreserveOtherTransfersAndOwners(t *testing.T) {
	a := testApp(t)
	var releases []func()
	acquire := func(transferID, ownerID string) {
		t.Helper()
		request := downloadCapacityRequest(a, fmt.Sprintf("192.0.2.%d", len(releases)+1))
		release, ok := a.downloadCapacity(httptest.NewRecorder(), request, Transfer{ID: transferID, UserID: ownerID})
		if !ok {
			t.Fatal("another transfer or uploader could not use available capacity")
		}
		releases = append(releases, release)
		t.Cleanup(release)
	}
	reject := func(transferID, ownerID string) {
		t.Helper()
		response := httptest.NewRecorder()
		if _, ok := a.downloadCapacity(response, downloadCapacityRequest(a, "192.0.2.100"), Transfer{ID: transferID, UserID: ownerID}); ok {
			t.Fatal("exhausted budget admitted a download")
		}
		checkStatus(t, response, 429)
		if response.Header().Get("Retry-After") == "" {
			t.Fatal("capacity rejection omitted retry delay")
		}
	}
	for range maxDownloadsPerTransfer {
		acquire("first", "alice")
	}
	reject("first", "alice")
	for range maxDownloadsPerTransfer {
		acquire("second", "alice")
	}
	reject("third", "alice")
	// Repeated rejected transfers cannot retain uploader or transfer entries.
	for range 3 {
		reject("rejected", "alice")
	}
	for range maxDownloadsPerTransfer {
		acquire("fourth", "bob")
		acquire("fifth", "bob")
	}
	reject("sixth", "charlie") // Global capacity is now full.
	if len(a.downloadSlots) != maxConcurrentDownloads {
		t.Fatal("subject budgets changed the global ceiling")
	}
	for _, release := range releases {
		release()
		release() // A second release must not take another download's slot.
	}
	assertDownloadCapacityReleased(t, a)
	acquire("sixth", "charlie")
	releases[len(releases)-1]()
	assertDownloadCapacityReleased(t, a)
}

func TestConcurrentDownloadAllocationPreservesBudgets(t *testing.T) {
	for _, scope := range []string{"transfer", "uploader", "global"} {
		t.Run(scope, func(t *testing.T) {
			a := testApp(t)
			start := make(chan struct{})
			var group sync.WaitGroup
			results := make(chan func(), 16)
			for index := range 16 {
				group.Go(func() {
					<-start
					transfer := Transfer{ID: "one-transfer", UserID: "one-owner"}
					if scope != "transfer" {
						transfer.ID = fmt.Sprintf("transfer-%d", index/2)
					}
					if scope == "global" {
						transfer.UserID = fmt.Sprintf("owner-%d", index/4)
					}
					response := httptest.NewRecorder()
					release, ok := a.downloadCapacity(response, downloadCapacityRequest(a, fmt.Sprintf("192.0.2.%d", index+1)), transfer)
					if ok {
						results <- release
					} else if response.Code != 429 {
						t.Error("concurrent capacity rejection did not return 429")
					}
				})
			}
			close(start)
			group.Wait()
			close(results)
			maximum := maxDownloadsPerTransfer
			if scope == "uploader" {
				maximum = maxDownloadsPerOwner
			} else if scope == "global" {
				maximum = maxConcurrentDownloads
			}
			if len(results) != maximum || len(a.downloadSlots) != maximum {
				t.Errorf("concurrent reservations = %d, expected %d", len(results), maximum)
			}
			for _, limit := range []*userWorkLimit{a.downloadTransfers, a.downloadUsers} {
				limit.mu.Lock()
				for _, count := range limit.active {
					if count > limit.limit {
						t.Error("concurrent allocation exceeded a subject's budget")
					}
				}
				limit.mu.Unlock()
			}
			for release := range results {
				release()
			}
			assertDownloadCapacityReleased(t, a)
		})
	}
}

func TestDownloadBudgetsCoverFilesRangesAndArchives(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	var transfers []Transfer
	for range 3 {
		shared := publish(t, owner, draft(t, owner, 0, "", "payload"), "payload")
		stored, err := a.store.transfer(context.Background(), shared.ID)
		if err != nil {
			t.Fatal(err)
		}
		transfers = append(transfers, stored)
	}
	admin := adminBrowser(t, a)
	otherUser := addAccount(t, admin, "other-download-owner", 1024)
	otherOwner := newBrowser(a, false)
	loginAs(t, otherOwner, otherUser.Username, accountTestPassword)
	other := publish(t, otherOwner, draft(t, otherOwner, 0, "", "other"), "other")
	var releases []func()
	for _, transfer := range transfers[:2] {
		for range maxDownloadsPerTransfer {
			release, ok := a.downloadCapacity(httptest.NewRecorder(), downloadCapacityRequest(a, fmt.Sprintf("192.0.2.%d", len(releases)+1)), transfer)
			if !ok {
				t.Fatal("fixture could not reserve download capacity")
			}
			releases = append(releases, release)
			t.Cleanup(release)
		}
	}
	public := newBrowser(a, true)
	for _, transfer := range []Transfer{transfers[0], transfers[2]} {
		for _, request := range []struct {
			path    string
			headers map[string]string
		}{
			{downloadPath(transfer, 0), nil},
			{downloadPath(transfer, 0), map[string]string{"Range": "bytes=0-1"}},
			{"/s/" + transfer.ShareToken + "/archive", nil},
		} {
			response := public.request("GET", request.path, nil, request.headers)
			checkStatus(t, response, 429)
			if response.Header().Get("Retry-After") == "" {
				t.Fatal("download budget response omitted retry delay")
			}
		}
		checkStatus(t, public.request("HEAD", downloadPath(transfer, 0), nil, nil), 200)
		checkStatus(t, public.request("HEAD", "/s/"+transfer.ShareToken+"/archive", nil, nil), 200)
		checkStatus(t, public.request("GET", "/s/"+transfer.ShareToken, nil, nil), 200)
	}
	checkStatus(t, public.request("GET", downloadPath(other, 0), nil, nil), 200)
	var reservations int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM reservations").Scan(&reservations); err != nil || reservations != 0 {
		t.Fatal("capacity rejection leaked a database reservation", err)
	}
	for _, release := range releases {
		release()
	}
	assertDownloadCapacityReleased(t, a)
	checkStatus(t, public.request("GET", downloadPath(transfers[0], 0), nil, nil), 200)
	assertDownloadCapacityReleased(t, a)
}

type observedDeadlineWriter struct {
	*httptest.ResponseRecorder
	deadlines  []time.Time
	onDeadline func()
	onWrite    func([]byte) (int, error)
}

func (w *observedDeadlineWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadlines = append(w.deadlines, deadline)
	if w.onDeadline != nil {
		w.onDeadline()
	}
	return nil
}

func (w *observedDeadlineWriter) Write(data []byte) (int, error) {
	if w.onWrite != nil {
		return w.onWrite(data)
	}
	return w.ResponseRecorder.Write(data)
}

func TestDownloadProgressDeadlineRejectsTrickleWrites(t *testing.T) {
	output := &observedDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	policy := defaultDownloadStreamPolicy()
	writer := newStreamWriter(output, context.Background(), policy)
	start := time.Now()
	now := start
	writer.now = func() time.Time { return now }
	writer.progressDeadline = start.Add(policy.progressWindow)
	for _, elapsed := range []time.Duration{0, 20 * time.Second, 40 * time.Second, 59 * time.Second} {
		now = start.Add(elapsed)
		if _, err := writer.Write(make([]byte, 16*1024)); err != nil {
			t.Fatal("stream aborted before its initial progress window", err)
		}
	}
	if !output.deadlines[len(output.deadlines)-1].Equal(start.Add(time.Minute)) {
		t.Fatal("tiny writes renewed the fixed progress deadline")
	}
	now = start.Add(time.Minute)
	if n, err := writer.Write([]byte{0}); n != 0 || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("slow trickle was allowed past its progress window")
	}
	if output.Body.Len() != 4*16*1024 {
		t.Fatal("timed out stream wrote additional bytes")
	}
}

func TestDownloadProgressRenewsOnlyForBytesAndBoundsFinalFlush(t *testing.T) {
	output := &observedDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
	policy := defaultDownloadStreamPolicy()
	start := time.Now()
	ctx, cancel := context.WithDeadline(context.Background(), start.Add(95*time.Second))
	defer cancel()
	writer := newStreamWriter(output, ctx, policy)
	now := start
	writer.now = func() time.Time { return now }
	writer.progressDeadline = start.Add(policy.progressWindow)
	for _, elapsed := range []time.Duration{20 * time.Second, 79 * time.Second} {
		now = start.Add(elapsed)
		if _, err := writer.Write(make([]byte, policy.minimumWindowBytes)); err != nil {
			t.Fatal("healthy long stream was interrupted", err)
		}
	}
	if err := writer.flush(); err != nil {
		t.Fatal(err)
	}
	if !output.deadlines[len(output.deadlines)-1].Equal(start.Add(95 * time.Second)) {
		t.Fatal("final flush extended the transfer/request deadline")
	}
	before := writer.progressDeadline
	if err := writer.flush(); err != nil || !writer.progressDeadline.Equal(before) {
		t.Fatal("flushing without additional bytes renewed the progress window", err)
	}
}

func TestDownloadWriteCannotOverrideCancellationOrExpiredProgress(t *testing.T) {
	t.Run("cancellation races deadline update", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		output := &observedDeadlineWriter{ResponseRecorder: httptest.NewRecorder(), onDeadline: cancel}
		writer := newStreamWriter(output, ctx, defaultDownloadStreamPolicy())
		if n, err := writer.Write([]byte("data")); n != 0 || !errors.Is(err, context.Canceled) {
			t.Fatal("deadline update overrode canceled stream")
		}
		if output.Body.Len() != 0 || output.deadlines[len(output.deadlines)-1].After(time.Now()) {
			t.Fatal("canceled stream wrote bytes or left a future write deadline")
		}
	})
	t.Run("write returns after progress deadline", func(t *testing.T) {
		now := time.Now()
		output := &observedDeadlineWriter{ResponseRecorder: httptest.NewRecorder()}
		output.onWrite = func(data []byte) (int, error) {
			now = now.Add(2 * time.Minute)
			return len(data), nil
		}
		writer := newStreamWriter(output, context.Background(), defaultDownloadStreamPolicy())
		writer.now = func() time.Time { return now }
		if _, err := writer.Write(make([]byte, writer.policy.minimumWindowBytes)); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatal("late write renewed an already expired progress window")
		}
	})
}

// Model the transport deadline contract, including a write or final flush that
// blocks until its deadline. Cancellation can update that deadline concurrently.
type stalledDownloadWriter struct {
	*httptest.ResponseRecorder
	mu         sync.Mutex
	deadline   time.Time
	changed    chan struct{}
	entered    chan struct{}
	once       sync.Once
	stallFlush bool
}

func (w *stalledDownloadWriter) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	w.deadline = deadline
	w.mu.Unlock()
	select {
	case w.changed <- struct{}{}:
	default:
	}
	return nil
}

func (w *stalledDownloadWriter) stall() error {
	w.once.Do(func() { close(w.entered) })
	for {
		w.mu.Lock()
		deadline := w.deadline
		w.mu.Unlock()
		if deadline.IsZero() {
			<-w.changed
			continue
		}
		timer := time.NewTimer(time.Until(deadline))
		select {
		case <-timer.C:
			return os.ErrDeadlineExceeded
		case <-w.changed:
			timer.Stop()
		}
	}
}

func (w *stalledDownloadWriter) Write(data []byte) (int, error) {
	if w.stallFlush {
		return w.ResponseRecorder.Write(data)
	}
	return 0, w.stall()
}

func (w *stalledDownloadWriter) FlushError() error {
	if w.stallFlush {
		return w.stall()
	}
	w.ResponseRecorder.Flush()
	return nil
}

func TestStalledFileRangeAndZIPReleaseCapacityAndAllowances(t *testing.T) {
	for _, endpoint := range []string{"limited file", "range", "ZIP"} {
		for _, phase := range []string{"write", "flush"} {
			t.Run(endpoint+"/"+phase, func(t *testing.T) {
				a := testApp(t)
				a.downloadPolicy.idleTimeout = 500 * time.Millisecond
				a.downloadPolicy.progressWindow = time.Second
				owner := newBrowser(a, false)
				owner.login(t)
				limit := int64(1)
				if endpoint == "range" {
					limit = 0
				}
				transfer := publish(t, owner, draft(t, owner, limit, "", "payload"), "payload")
				path := downloadPath(transfer, 0)
				if endpoint == "ZIP" {
					path = "/s/" + transfer.ShareToken + "/archive"
				}
				request := httptest.NewRequest("GET", a.cfg.PublicURL+path, nil)
				if endpoint == "range" {
					request.Header.Set("Range", "bytes=0-1")
				}
				output := &stalledDownloadWriter{ResponseRecorder: httptest.NewRecorder(), changed: make(chan struct{}, 1), entered: make(chan struct{}), stallFlush: phase == "flush"}
				done := make(chan any, 1)
				go func() {
					defer func() { done <- recover() }()
					a.PublicHandler().ServeHTTP(output, request)
				}()
				t.Cleanup(func() { output.SetWriteDeadline(time.Now()) })
				select {
				case <-output.entered:
				case <-time.After(2 * time.Second):
					t.Fatal("stream never entered the stalled transport")
				}
				if len(a.downloadSlots) != 1 || !a.transferActive(transfer.ID) {
					t.Fatal("stalled stream did not hold its capacity/cancellation lease")
				}
				checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
				select {
				case recovered := <-done:
					if recovered != http.ErrAbortHandler {
						t.Fatalf("timed out stream did not abort the transport: %v", recovered)
					}
				case <-time.After(2 * time.Second):
					t.Fatal("stalled stream ignored the write/flush deadline")
				}
				assertDownloadCapacityReleased(t, a)
				stored, err := a.store.transfer(context.Background(), transfer.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, file := range stored.Files {
					if file.Downloads != 0 || file.Reserved != 0 || file.Deleted {
						t.Fatal("timed out response consumed an allowance or retained a reservation")
					}
				}
				if a.transferActive(transfer.ID) {
					t.Fatal("timed out response retained its cancellation lease")
				}
				public := newBrowser(a, true)
				checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 200)
				assertDownloadCapacityReleased(t, a)
			})
		}
	}
}
