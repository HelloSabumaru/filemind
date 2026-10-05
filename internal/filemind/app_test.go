package filemind

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func testApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "password")
	if err := os.WriteFile(secret, []byte("owner-password-for-validation"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{DataDir: filepath.Join(dir, "data"), OwnerListen: ":8080", PublicListen: ":8081", OwnerURL: "http://localhost:9080", PublicURL: "http://localhost:9081", OwnerUsername: "admin", OwnerPasswordFile: secret, DefaultExpiry: 24 * time.Hour, MaxFileSize: 2 << 30, MaxTransferSize: 2 << 30, StorageQuota: 20 << 30, Development: true}
	a, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	return a
}

type browser struct {
	app     *App
	public  bool
	cookies map[string]*http.Cookie
	csrf    string
}

func newBrowser(a *App, public bool) *browser {
	return &browser{app: a, public: public, cookies: map[string]*http.Cookie{}}
}

func (b *browser) request(method, path string, body io.Reader, headers map[string]string) *httptest.ResponseRecorder {
	origin := b.app.cfg.OwnerURL
	h := b.app.OwnerHandler()
	if b.public {
		origin = b.app.cfg.PublicURL
		h = b.app.PublicHandler()
	}
	r := httptest.NewRequest(method, origin+path, body)
	r.RemoteAddr = "127.0.0.1:12345"
	for _, cookie := range b.cookies {
		if strings.HasPrefix(path, cookie.Path) {
			r.AddCookie(cookie)
		}
	}
	if method != "GET" && method != "HEAD" {
		r.Header.Set("Origin", origin)
		r.Header.Set("X-CSRF-Token", b.csrf)
	}
	for key, value := range headers {
		r.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	for _, cookie := range w.Result().Cookies() {
		if cookie.MaxAge < 0 {
			delete(b.cookies, cookie.Name)
		} else {
			b.cookies[cookie.Name] = cookie
		}
	}
	return w
}

func (b *browser) json(method, path string, input any) *httptest.ResponseRecorder {
	data, _ := json.Marshal(input)
	return b.request(method, path, bytes.NewReader(data), map[string]string{"Content-Type": "application/json"})
}

var csrfPattern = regexp.MustCompile(`name="csrf-token" content="([^"]+)"`)

func (b *browser) page(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	w := b.request("GET", path, nil, nil)
	checkStatus(t, w, 200)
	match := csrfPattern.FindStringSubmatch(w.Body.String())
	if len(match) != 2 {
		t.Fatal("missing CSRF token")
	}
	b.csrf = match[1]
	return w
}
func (b *browser) login(t *testing.T) {
	t.Helper()
	b.page(t, "/admin/login")
	checkStatus(t, b.json("POST", "/admin/login", map[string]string{"username": "admin", "password": "owner-password-for-validation"}), 200)
	b.page(t, "/admin")
}
func checkStatus(t *testing.T, w *httptest.ResponseRecorder, want int) {
	t.Helper()
	if w.Code != want {
		t.Fatalf("status %d, want %d: %s", w.Code, want, w.Body.String())
	}
}
func readTransfer(t *testing.T, w *httptest.ResponseRecorder) Transfer {
	t.Helper()
	checkStatus(t, w, 200)
	var transfer Transfer
	if err := json.Unmarshal(w.Body.Bytes(), &transfer); err != nil {
		t.Fatal(err)
	}
	return transfer
}

func draft(t *testing.T, b *browser, limit int64, password string, payloads ...string) Transfer {
	t.Helper()
	files := []map[string]any{}
	for i, data := range payloads {
		files = append(files, map[string]any{"name": "file-" + strconv.Itoa(i) + ".txt", "size": len(data)})
	}
	return readTransfer(t, b.json("POST", "/admin/api/transfers", map[string]any{"title": "Validation transfer", "expirySeconds": 86400, "downloadLimit": limit, "password": password, "files": files}))
}

func startFile(t *testing.T, b *browser, file File) string {
	t.Helper()
	w := b.request("POST", "/admin/uploads/", nil, map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": strconv.FormatInt(file.Size, 10), "Upload-Metadata": "file_id " + base64.StdEncoding.EncodeToString([]byte(file.ID))})
	checkStatus(t, w, 201)
	want := b.app.cfg.OwnerURL + "/admin/uploads/" + file.ID
	if w.Header().Get("Location") != want {
		t.Fatalf("noncanonical upload location %q", w.Header().Get("Location"))
	}
	return "/admin/uploads/" + file.ID
}
func patchFile(t *testing.T, b *browser, path string, offset int, data string) {
	t.Helper()
	checkStatus(t, b.request("PATCH", path, strings.NewReader(data), map[string]string{"Tus-Resumable": "1.0.0", "Upload-Offset": strconv.Itoa(offset), "Content-Type": "application/offset+octet-stream"}), 204)
}
func publish(t *testing.T, b *browser, transfer Transfer, payloads ...string) Transfer {
	t.Helper()
	for i, f := range transfer.Files {
		path := startFile(t, b, f)
		if f.Size > 0 {
			patchFile(t, b, path, 0, payloads[i])
		}
	}
	transfer = readTransfer(t, b.json("POST", "/admin/api/transfers/"+transfer.ID+"/publish", map[string]any{}))
	transfer.ShareToken = strings.TrimPrefix(transfer.ShareURL, b.app.cfg.PublicURL+"/s/")
	return transfer
}
func downloadPath(transfer Transfer, index int) string {
	return "/s/" + transfer.ShareToken + "/files/" + transfer.Files[index].ID
}

func TestPrivatePublicBoundaryAndCSRF(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	public := newBrowser(a, true)
	for _, path := range []string{"/admin", "/admin/login", "/admin/api/transfers", "/admin/uploads/"} {
		checkStatus(t, public.request("GET", path, nil, map[string]string{"Tailscale-Funnel-Request": "?0"}), 404)
	}
	checkStatus(t, owner.json("POST", "/admin/api/transfers", map[string]any{}), 401)
	owner.page(t, "/admin/login")
	checkStatus(t, owner.request("POST", "/admin/login", strings.NewReader(`{"username":"admin","password":"owner-password-for-validation"}`), map[string]string{"Content-Type": "application/json", "Origin": "https://attacker.invalid"}), 403)
	owner.login(t)
	checkStatus(t, owner.request("POST", "/admin/api/transfers", strings.NewReader(`{}`), map[string]string{"Content-Type": "application/json", "X-CSRF-Token": "invalid"}), 403)
	transfer := publish(t, owner, draft(t, owner, 1, "", "hello"), "hello")
	checkStatus(t, public.request("GET", "/s/"+transfer.ShareToken, nil, nil), 200)
	checkStatus(t, public.request("POST", "/admin/uploads/", nil, nil), 404)
	qr := owner.request("GET", "/admin/api/transfers/"+transfer.ID+"/qr.png", nil, nil)
	checkStatus(t, qr, 200)
	if _, err := png.Decode(bytes.NewReader(qr.Body.Bytes())); err != nil {
		t.Fatal(err)
	}
	current, _ := a.store.transfer(context.Background(), transfer.ID)
	if current.Files[0].Downloads != 0 || current.Files[0].Deleted {
		t.Fatal("page or QR consumed download")
	}
	checkStatus(t, public.request("GET", "/s/"+randomToken(), nil, nil), 404)
	checkStatus(t, public.request("GET", "/s/not-a-token", nil, nil), 404)
	checkStatus(t, owner.json("POST", "/admin/logout", nil), 200)
	checkStatus(t, owner.request("GET", "/admin/api/transfers", nil, nil), 401)
}

func TestResumableUploadAndRestart(t *testing.T) {
	a := testApp(t)
	b := newBrowser(a, false)
	b.login(t)
	transfer := draft(t, b, 0, "", "abcdef")
	path := startFile(t, b, transfer.Files[0])
	patchFile(t, b, path, 0, "abc")
	checkStatus(t, b.json("POST", "/admin/api/transfers/"+transfer.ID+"/publish", nil), 409)
	checkStatus(t, b.request("PATCH", path, strings.NewReader("bad"), map[string]string{"Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"}), 409)
	a.Close()
	restarted, err := New(a.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { restarted.Close() })
	b.app = restarted
	w := b.request("HEAD", path, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	checkStatus(t, w, 200)
	if w.Header().Get("Upload-Offset") != "3" {
		t.Fatal("offset did not survive restart")
	}
	patchFile(t, b, path, 3, "def")
	shared := readTransfer(t, b.json("POST", "/admin/api/transfers/"+transfer.ID+"/publish", nil))
	shared.ShareToken = strings.TrimPrefix(shared.ShareURL, a.cfg.PublicURL+"/s/")
	public := newBrowser(restarted, true)
	data := public.request("GET", downloadPath(shared, 0), nil, nil)
	checkStatus(t, data, 200)
	if data.Body.String() != "abcdef" {
		t.Fatal("resumed contents differ")
	}
}

func TestUploadValidationAndQuota(t *testing.T) {
	a := testApp(t)
	a.cfg.MaxFileSize = 10
	a.cfg.MaxTransferSize = 10
	a.cfg.StorageQuota = 12
	b := newBrowser(a, false)
	b.login(t)
	for _, name := range []string{"../outside", "/tmp/file", "bad\\path", "bad\x00name"} {
		checkStatus(t, b.json("POST", "/admin/api/transfers", map[string]any{"files": []map[string]any{{"name": name, "size": 1}}}), 400)
	}
	transfer := draft(t, b, 0, "", "12345678")
	checkStatus(t, b.json("POST", "/admin/api/transfers", map[string]any{"files": []map[string]any{{"name": "too-big", "size": 11}}}), 400)
	checkStatus(t, b.json("POST", "/admin/api/transfers", map[string]any{"files": []map[string]any{{"name": "quota", "size": 5}}}), 400)
	checkStatus(t, b.request("POST", "/admin/uploads/", nil, map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": "9", "Upload-Metadata": "file_id " + base64.StdEncoding.EncodeToString([]byte(transfer.Files[0].ID))}), 409)
	checkStatus(t, b.json("DELETE", "/admin/api/transfers/"+transfer.ID, nil), 200)
	draft(t, b, 0, "", "12345")
	a.cfg.MinFreeSpace = 1 << 60
	checkStatus(t, b.json("POST", "/admin/api/transfers", map[string]any{"files": []map[string]any{{"name": "disk-full", "size": 1}}}), 507)
}

func TestActiveUploadDoesNotBlockOtherTransfersAndCanBeDeleted(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := draft(t, owner, 0, "", strings.Repeat("x", 1<<20))
	path := startFile(t, owner, transfer.Files[0])
	server := httptest.NewServer(a.OwnerHandler())
	defer server.Close()
	reader, writer := io.Pipe()
	defer writer.Close()
	request, err := http.NewRequest("PATCH", server.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Origin", a.cfg.OwnerURL)
	request.Header.Set("X-CSRF-Token", owner.csrf)
	request.Header.Set("Tus-Resumable", "1.0.0")
	request.Header.Set("Upload-Offset", "0")
	request.Header.Set("Content-Type", "application/offset+octet-stream")
	for _, cookie := range owner.cookies {
		request.AddCookie(cookie)
	}
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		response, e := server.Client().Do(request)
		if e == nil {
			response.Body.Close()
		}
	}()
	if _, err = writer.Write(bytes.Repeat([]byte{'x'}, 32768)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		info, e := a.root.Stat(transfer.Files[0].ID)
		if e == nil && info.Size() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("upload did not begin")
		}
		time.Sleep(5 * time.Millisecond)
	}
	other := publish(t, owner, draft(t, owner, 1, "", "other"), "other")
	public := newBrowser(a, true)
	checkStatus(t, public.request("GET", downloadPath(other, 0), nil, nil), 200)
	checkStatus(t, owner.json("DELETE", "/admin/api/transfers/"+transfer.ID, nil), 200)
	writer.Close()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("deletion did not interrupt upload")
	}
	if _, err = a.root.Stat(transfer.Files[0].ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("deleted upload remains")
	}
}

func TestLimitedDownloadsHEADRangeAndDeletion(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	public := newBrowser(a, true)
	transfer := publish(t, owner, draft(t, owner, 1, "", "first", "second"), "first", "second")
	head := public.request("HEAD", downloadPath(transfer, 0), nil, nil)
	checkStatus(t, head, 200)
	if head.Header().Get("Content-Length") != "5" {
		t.Fatal("incorrect HEAD metadata")
	}
	current, _ := a.store.transfer(context.Background(), transfer.ID)
	if current.Files[0].Downloads != 0 {
		t.Fatal("HEAD consumed allowance")
	}
	w := public.request("GET", downloadPath(transfer, 0), nil, map[string]string{"Range": "bytes=0-1"})
	checkStatus(t, w, 200)
	if w.Body.String() != "first" || w.Header().Get("Accept-Ranges") != "none" {
		t.Fatal("limited file allowed partial download")
	}
	current, _ = a.store.transfer(context.Background(), transfer.ID)
	if !current.Files[0].Deleted || current.Files[0].Downloads != 1 {
		t.Fatal("exhausted file not counted and deleted")
	}
	if _, err := a.root.Open(current.Files[0].ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("exhausted payload remains")
	}
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 404)
	archive := public.request("GET", "/s/"+transfer.ShareToken+"/archive", nil, nil)
	checkStatus(t, archive, 200)
	z, err := zip.NewReader(bytes.NewReader(archive.Body.Bytes()), int64(archive.Body.Len()))
	if err != nil || len(z.File) != 1 {
		t.Fatalf("remaining ZIP: %v", err)
	}
	current, _ = a.store.transfer(context.Background(), transfer.ID)
	if current.Status != "exhausted" || !current.Files[1].Deleted {
		t.Fatal("ZIP exhaustion not applied")
	}
	checkStatus(t, public.request("GET", "/s/"+transfer.ShareToken, nil, nil), 404)
}

func TestUnlimitedRangeAndZIP(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	public := newBrowser(a, true)
	transfer := publish(t, owner, draft(t, owner, 0, "", "abcdef", ""), "abcdef", "")
	w := public.request("GET", downloadPath(transfer, 0), nil, map[string]string{"Range": "bytes=2-4"})
	checkStatus(t, w, 206)
	if w.Body.String() != "cde" {
		t.Fatal("incorrect range")
	}
	current, _ := a.store.transfer(context.Background(), transfer.ID)
	if current.Files[0].Downloads != 0 {
		t.Fatal("partial range counted as full download")
	}
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 200)
	archive := public.request("GET", "/s/"+transfer.ShareToken+"/archive", nil, nil)
	checkStatus(t, archive, 200)
	z, err := zip.NewReader(bytes.NewReader(archive.Body.Bytes()), int64(archive.Body.Len()))
	if err != nil {
		t.Fatal(err)
	}
	if len(z.File) != 2 {
		t.Fatal("ZIP missing empty or regular file")
	}
	for i, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		want := "abcdef"
		if i == 1 {
			want = ""
		}
		if string(data) != want {
			t.Fatal("incorrect ZIP entry")
		}
	}
	current, _ = a.store.transfer(context.Background(), transfer.ID)
	if current.Files[0].Downloads != 2 || current.Files[1].Downloads != 1 || current.Files[0].Deleted {
		t.Fatal("incorrect unlimited accounting")
	}
}

func TestPasswordSessionsRotationAndBan(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	public := newBrowser(a, true)
	transfer := publish(t, owner, draft(t, owner, 0, "transfer-password", "private"), "private")
	page := public.page(t, "/s/"+transfer.ShareToken)
	if strings.Contains(page.Body.String(), "file-0.txt") {
		t.Fatal("locked page disclosed filename")
	}
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 401)
	checkStatus(t, public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": "transfer-password"}), 200)
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 200)
	checkStatus(t, owner.json("PATCH", "/admin/api/transfers/"+transfer.ID, map[string]string{"password": "new-transfer-password"}), 200)
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 401)
	for i := 0; i < 5; i++ {
		checkStatus(t, public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": "wrong"}), 403)
	}
	checkStatus(t, public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": "new-transfer-password"}), 429)
	checkStatus(t, owner.request("GET", "/admin/api/transfers", nil, nil), 200)
}

func TestReservationsAtomicConcurrentAndCrashRecovery(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 1, "", "one", "two"), "one", "two")
	stored, err := a.store.transfer(context.Background(), transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wins := make(chan string, 10)
	for range 10 {
		wg.Go(func() {
			lease, err := a.store.reserve(context.Background(), stored, []File{stored.Files[0]})
			if err == nil {
				wins <- lease
			}
		})
	}
	wg.Wait()
	close(wins)
	leases := []string{}
	for lease := range wins {
		leases = append(leases, lease)
	}
	if len(leases) != 1 {
		t.Fatalf("%d concurrent winners", len(leases))
	}
	if _, err = a.store.reserve(context.Background(), stored, []File{stored.Files[1], stored.Files[0]}); err == nil {
		t.Fatal("ZIP reserved busy file")
	}
	files, _ := a.store.files(context.Background(), stored.ID)
	if files[1].Reserved != 0 {
		t.Fatal("partial ZIP reservation leaked")
	}
	checkStatus(t, owner.json("PATCH", "/admin/api/transfers/"+stored.ID, map[string]any{"downloadLimit": 2}), 409)
	if err = a.recover(); err != nil {
		t.Fatal(err)
	}
	files, _ = a.store.files(context.Background(), stored.ID)
	if files[0].Reserved != 0 || files[0].Downloads != 0 {
		t.Fatal("crash-uncertain allowance not released")
	}
	lease, err := a.store.reserve(context.Background(), stored, stored.Files)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.finishReservation(lease, false); err != nil {
		t.Fatal(err)
	}
	files, _ = a.store.files(context.Background(), stored.ID)
	if files[0].Downloads != 0 || files[1].Downloads != 0 {
		t.Fatal("failed stream consumed allowance")
	}
}

type failingWriter struct {
	*httptest.ResponseRecorder
	remaining int
}

func (w *failingWriter) Write(p []byte) (int, error) {
	if len(p) > w.remaining {
		n := w.remaining
		w.remaining = 0
		if n > 0 {
			w.ResponseRecorder.Write(p[:n])
		}
		return n, io.ErrClosedPipe
	}
	w.remaining -= len(p)
	return w.ResponseRecorder.Write(p)
}

func TestInterruptedFileAndZIPReleaseAllowances(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 1, "", strings.Repeat("x", 100000), "second"), strings.Repeat("x", 100000), "second")
	for _, path := range []string{downloadPath(transfer, 0), "/s/" + transfer.ShareToken + "/archive"} {
		r := httptest.NewRequest("GET", a.cfg.PublicURL+path, nil)
		w := &failingWriter{httptest.NewRecorder(), 200}
		a.PublicHandler().ServeHTTP(w, r)
		files, _ := a.store.files(context.Background(), transfer.ID)
		for _, f := range files {
			if f.Downloads != 0 || f.Reserved != 0 || f.Deleted {
				t.Fatal("interrupted stream consumed allowance or retained lease")
			}
		}
	}
}

func TestExpiryRevocationCleanupAndHistory(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	public := newBrowser(a, true)
	transfer := publish(t, owner, draft(t, owner, 0, "", "data"), "data")
	checkStatus(t, owner.json("POST", "/admin/api/transfers/"+transfer.ID+"/revoke", nil), 200)
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 404)
	_, err := a.store.db.Exec("UPDATE transfers SET expires_at=? WHERE id=?", time.Now().Unix()-1, transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.cleanup(); err != nil {
		t.Fatal(err)
	}
	current, _ := a.store.transfer(context.Background(), transfer.ID)
	if current.Status != "expired" || !current.Files[0].Deleted {
		t.Fatal("revoked transfer did not expire and purge")
	}
	unfinished := draft(t, owner, 0, "", "unfinished")
	_, err = a.store.db.Exec("UPDATE transfers SET touched_at=? WHERE id=?", time.Now().Unix()-86401, unfinished.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.cleanup(); err != nil {
		t.Fatal(err)
	}
	current, _ = a.store.transfer(context.Background(), unfinished.ID)
	if current.Status != "expired" || !current.Files[0].Deleted {
		t.Fatal("stale upload not cleaned")
	}
	_, err = a.store.db.Exec("UPDATE transfers SET closed_at=?", time.Now().Unix()-31*86400)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.cleanup(); err != nil {
		t.Fatal(err)
	}
	transfers, err := a.store.list(context.Background(), "", 0)
	if err != nil || len(transfers) != 0 {
		t.Fatal("history retained past limit")
	}
}

func TestPathContainmentAndProxyTrust(t *testing.T) {
	a := testApp(t)
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, []byte("private"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(a.cfg.DataDir, "uploads", randomID())); err != nil {
		t.Fatal(err)
	}
	dir, err := a.root.Open(".")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.root.Open(entries[0].Name()); err == nil {
		t.Fatal("root followed outside symlink")
	}
	r := httptest.NewRequest("GET", a.cfg.PublicURL+"/", nil)
	r.RemoteAddr = "192.0.2.10:1234"
	r.Header.Set("X-Forwarded-For", "198.51.100.1")
	if a.clientIP(r) != "192.0.2.10" {
		t.Fatal("untrusted proxy spoofed IP")
	}
	a.cfg.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("192.0.2.10/32")}
	if a.clientIP(r) != "198.51.100.1" {
		t.Fatal("trusted proxy not applied")
	}
}

func TestProductionConfiguration(t *testing.T) {
	a := testApp(t)
	cfg := a.cfg
	cfg.Development = false
	if cfg.Validate() == nil {
		t.Fatal("production allowed HTTP")
	}
	cfg.OwnerURL = "https://owner.example"
	cfg.PublicURL = "https://files.example"
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	cfg.PublicURL = cfg.OwnerURL
	if cfg.Validate() == nil {
		t.Fatal("shared origin allowed")
	}
	cfg.PublicURL = "https://files.example/path"
	if cfg.Validate() == nil {
		t.Fatal("path origin allowed")
	}
}
