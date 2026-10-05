package filemind

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

const accountTestPassword = "account-password-for-validation"

func TestAccountInvalidationStopsActiveUploads(t *testing.T) {
	for _, change := range []string{"disable", "reset"} {
		t.Run(change, func(t *testing.T) {
			a := testApp(t)
			admin := adminBrowser(t, a)
			user := addAccount(t, admin, "uploading-user", 2<<20)
			owner := newBrowser(a, false)
			loginAs(t, owner, user.Username, accountTestPassword)
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
			for key, value := range map[string]string{"Origin": a.cfg.OwnerURL, "X-CSRF-Token": owner.csrf, "Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"} {
				request.Header.Set(key, value)
			}
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
					t.Fatal("upload did not start")
				}
				time.Sleep(5 * time.Millisecond)
			}
			input := map[string]any{"disabled": true}
			if change == "reset" {
				input = map[string]any{"password": "replacement-password-for-validation"}
			}
			checkStatus(t, admin.json("PATCH", "/admin/api/users/"+user.ID, input), 200)
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("account invalidation did not stop active upload")
			}
			checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 401)
			stored, e := a.store.transfer(context.Background(), transfer.ID)
			if e != nil || stored.Files[0].Uploaded || stored.Status != "draft" {
				t.Fatal("invalidated upload became complete")
			}
		})
	}
}

func loginAs(t *testing.T, b *browser, name, password string) {
	t.Helper()
	b.page(t, "/login")
	checkStatus(t, b.json("POST", "/login", map[string]string{"username": name, "password": password}), 200)
	if b.admin {
		b.page(t, "/admin/users")
	} else {
		b.page(t, "/upload")
	}
}

func adminBrowser(t *testing.T, a *App) *browser {
	t.Helper()
	b := newBrowser(a, false)
	b.admin = true
	loginAs(t, b, "admin", "owner-password-for-validation")
	return b
}

func addAccount(t *testing.T, b *browser, name string, quota int64) User {
	t.Helper()
	w := b.json("POST", "/admin/api/users", map[string]any{"username": name, "password": accountTestPassword, "storageQuota": quota})
	checkStatus(t, w, 201)
	var user User
	if err := json.Unmarshal(w.Body.Bytes(), &user); err != nil {
		t.Fatal(err)
	}
	return user
}

func TestAdminSessionsAccountsAndTransferManagement(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	owner := newBrowser(a, false)
	owner.login(t)
	for _, path := range []string{"/admin/users", "/admin/api/users", "/admin/api/settings", "/admin/api/transfers", "/admin/preferences", "/admin/api/preferences"} {
		checkStatus(t, owner.request("GET", path, nil, nil), 404)
		checkStatus(t, newBrowser(a, true).request("GET", path, nil, nil), 404)
	}
	wrongSurface := newBrowser(a, false)
	wrongSurface.admin = true
	wrongSurface.cookies = owner.cookies
	checkStatus(t, wrongSurface.request("GET", "/admin/api/users", nil, nil), 401)
	user := addAccount(t, admin, "alice", 20)
	addAccount(t, admin, "bob", 20)
	alice, bob := newBrowser(a, false), newBrowser(a, false)
	loginAs(t, alice, "alice", accountTestPassword)
	loginAs(t, bob, "bob", accountTestPassword)
	transfer := publish(t, alice, draft(t, alice, 0, "", "private"), "private")
	checkStatus(t, bob.request("GET", "/api/transfers/"+transfer.ID, nil, nil), 404)
	list := bob.request("GET", "/api/transfers", nil, nil)
	checkStatus(t, list, 200)
	if strings.Contains(list.Body.String(), transfer.ID) {
		t.Fatal("another user's transfer appeared in list")
	}
	checkStatus(t, admin.request("GET", "/admin/api/transfers/"+transfer.ID, nil, nil), 200)
	all := admin.request("GET", "/admin/api/transfers", nil, nil)
	checkStatus(t, all, 200)
	if !strings.Contains(all.Body.String(), "alice") {
		t.Fatal("admin list missing owner")
	}
	checkStatus(t, admin.json("PATCH", "/admin/api/transfers/"+transfer.ID, map[string]any{"revision": transfer.Revision, "title": "Admin correction"}), 200)
	checkStatus(t, admin.json("PATCH", "/admin/api/users/"+user.ID, map[string]any{"password": "replacement-password-for-validation"}), 200)
	checkStatus(t, alice.request("GET", "/api/transfers", nil, nil), 401)
	checkStatus(t, newBrowser(a, true).request("GET", downloadPath(transfer, 0), nil, nil), 200)
	checkStatus(t, admin.json("POST", "/admin/api/transfers/"+transfer.ID+"/revoke", nil), 200)
	checkStatus(t, newBrowser(a, true).request("GET", downloadPath(transfer, 0), nil, nil), 404)
	checkStatus(t, admin.json("DELETE", "/admin/api/transfers/"+transfer.ID, nil), 200)
	if _, err := a.root.Stat(transfer.Files[0].ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("admin deletion left payload")
	}
	regularAdmin := newBrowser(a, false)
	regularAdmin.admin = true
	regularAdmin.page(t, "/login")
	checkStatus(t, regularAdmin.json("POST", "/login", map[string]string{"username": "bob", "password": accountTestPassword}), 401)
}

func TestSettingsDefaultsQuotaAndPersistence(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	settings := a.settings()
	settings.DefaultUserQuota = 12
	settings.ExpirySeconds = 7200
	settings.DownloadLimit = 2
	settings.MaxFileSize = 10
	settings.MaxTransferSize = 10
	settings.StorageQuota = 30
	checkStatus(t, admin.json("PUT", "/admin/api/settings", settings), 200)
	w := admin.json("POST", "/admin/api/users", map[string]any{"username": "defaults", "password": accountTestPassword})
	checkStatus(t, w, 201)
	var user User
	json.Unmarshal(w.Body.Bytes(), &user)
	if user.StorageQuota != 12 {
		t.Fatal("user default quota not applied")
	}
	owner := newBrowser(a, false)
	loginAs(t, owner, "defaults", accountTestPassword)
	transfer := readTransfer(t, owner.json("POST", "/api/transfers", map[string]any{"files": []map[string]any{{"name": "defaults.txt", "size": 8, "sha256": testDigest("12345678")}}}))
	if transfer.ExpirySeconds != 7200 || transfer.DownloadLimit != 2 {
		t.Fatal("transfer defaults not applied")
	}
	checkStatus(t, owner.json("POST", "/api/transfers", map[string]any{"files": []map[string]any{{"name": "quota.txt", "size": 5, "sha256": testDigest("12345")}}}), 409)
	bad := settings
	bad.StorageQuota = 7
	bad.MaxFileSize = 7
	bad.MaxTransferSize = 7
	bad.DefaultUserQuota = 7
	checkStatus(t, admin.json("PUT", "/admin/api/settings", bad), 409)
	checkStatus(t, owner.request("GET", "/api/config", nil, nil), 200)
	a.Close()
	restarted, err := New(a.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if restarted.settings() != settings {
		t.Fatal("settings did not persist")
	}
	admin.app = restarted
	response := admin.request("GET", "/admin/api/settings", nil, nil)
	checkStatus(t, response, 200)
	if !strings.Contains(response.Body.String(), "maintenance") {
		t.Fatal("maintenance visibility missing")
	}
}

func TestTransferRevisionsPreserveExpiryAndSerializePublication(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := draft(t, owner, 0, "", "abcdef")
	path := startFile(t, owner, transfer.Files[0])
	patchFile(t, owner, path, 0, "abcdef")
	storedUser, err := scanUser(a.store.db.QueryRow("SELECT " + userColumns + " FROM users u WHERE username='admin'"))
	if err != nil {
		t.Fatal(err)
	}
	title := "Concurrent edit"
	expiry := int64(7200)
	var wg sync.WaitGroup
	wg.Add(2)
	var publishErr, editErr error
	go func() {
		defer wg.Done()
		_, publishErr = a.publishTransfer(context.Background(), storedUser, transfer.ID)
	}()
	go func() {
		defer wg.Done()
		_, editErr = a.updateTransfer(context.Background(), storedUser, transfer.ID, transferPatch{Revision: transfer.Revision, Title: &title, ExpirySeconds: &expiry}, false)
	}()
	wg.Wait()
	if publishErr != nil {
		t.Fatal(publishErr)
	}
	if editErr != nil {
		var p *problem
		if !errors.As(editErr, &p) || p.status != 409 {
			t.Fatal(editErr)
		}
	}
	current, err := a.store.transfer(context.Background(), transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Status != "published" || current.ExpiresAt != current.PublishedAt+current.ExpirySeconds || current.ExpiresAt == 0 {
		t.Fatal("publication and edit left inconsistent expiry")
	}
	checkStatus(t, owner.json("PATCH", "/api/transfers/"+transfer.ID, map[string]any{"revision": transfer.Revision, "title": "stale"}), 409)
	updated := readTransfer(t, owner.json("PATCH", "/api/transfers/"+transfer.ID, map[string]any{"revision": current.Revision, "title": "Title only"}))
	if updated.ExpiresAt != current.ExpiresAt || updated.Revision <= current.Revision {
		t.Fatal("unrelated edit reset expiry or revision")
	}
	checkStatus(t, owner.json("PATCH", "/api/transfers/"+transfer.ID, map[string]any{"revision": current.Revision, "title": "stale again"}), 409)
}

func TestUploadChecksumAndSyncFailures(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := draft(t, owner, 0, "", "abcdef")
	path := startFile(t, owner, transfer.Files[0])
	a.syncFile = func(*os.File) error { return syscall.EIO }
	response := owner.request("PATCH", path, strings.NewReader("abcdef"), map[string]string{"Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
	checkStatus(t, response, 503)
	current, _ := a.store.transfer(context.Background(), transfer.ID)
	if current.Files[0].Uploaded {
		t.Fatal("failed sync promoted upload")
	}
	checkStatus(t, owner.json("POST", "/api/transfers/"+transfer.ID+"/publish", nil), 503)
	a.syncFile = nil
	shared := readTransfer(t, owner.json("POST", "/api/transfers/"+transfer.ID+"/publish", nil))
	if shared.Status != "published" {
		t.Fatal("retry did not finalize durable payload")
	}
	second := draft(t, owner, 0, "", "correct")
	secondPath := startFile(t, owner, second.Files[0])
	response = owner.request("PATCH", secondPath, strings.NewReader("changed"), map[string]string{"Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
	checkStatus(t, response, 409)
	current, _ = a.store.transfer(context.Background(), second.ID)
	if current.Files[0].Uploaded || current.Files[0].IntegrityError == "" {
		t.Fatal("checksum mismatch not recorded")
	}
	checkStatus(t, owner.json("POST", "/api/transfers/"+second.ID+"/publish", nil), 409)
	checkStatus(t, owner.json("POST", "/api/transfers/"+second.ID+"/files/"+second.Files[0].ID+"/restart", nil), 200)
	path = startFile(t, owner, second.Files[0])
	patchFile(t, owner, path, 0, "correct")
	a.syncFile = func(*os.File) error { return syscall.EIO }
	checkStatus(t, owner.json("POST", "/api/transfers/"+second.ID+"/publish", nil), 503)
	a.syncFile = nil
	readTransfer(t, owner.json("POST", "/api/transfers/"+second.ID+"/publish", nil))
}

func TestArchiveReclaimsPayloadsUsernameAndCapacity(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	account := addAccount(t, admin, "archive-me", 0)
	owner := newBrowser(a, false)
	loginAs(t, owner, "archive-me", accountTestPassword)
	transfer := publish(t, owner, draft(t, owner, 0, "", "archive"), "archive")
	stored, _ := a.store.transfer(context.Background(), transfer.ID)
	lease, err := a.store.reserve(context.Background(), stored, stored.Files)
	if err != nil {
		t.Fatal(err)
	}
	ctx, end := a.activate(context.Background(), transfer.ID, lease)
	defer end()
	checkStatus(t, admin.json("DELETE", "/admin/api/users/"+account.ID, nil), 200)
	if ctx.Err() != context.Canceled {
		t.Fatal("account deletion did not cancel active stream")
	}
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 401)
	checkStatus(t, newBrowser(a, true).request("GET", downloadPath(transfer, 0), nil, nil), 404)
	a.complete(lease, false)
	if _, err = a.root.Stat(transfer.Files[0].ID); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("archived account payload remains")
	}
	replacement := addAccount(t, admin, "archive-me", 0)
	if replacement.ID == account.ID {
		t.Fatal("archived account was reused")
	}
	var active int
	a.store.db.QueryRow("SELECT COUNT(*) FROM users WHERE archived=0").Scan(&active)
	if active != 2 {
		t.Fatal("archived account consumes active capacity")
	}
	var adminID string
	a.store.db.QueryRow("SELECT id FROM users WHERE is_admin=1").Scan(&adminID)
	checkStatus(t, admin.json("DELETE", "/admin/api/users/"+adminID, nil), 400)
}

func TestAccountPasswordChangesAcceptShortAndLongPasswords(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	checkStatus(t, admin.json("POST", "/admin/api/users", map[string]string{"username": "short-password", "password": "x"}), 201)
	checkStatus(t, admin.json("POST", "/admin/api/users", map[string]string{"username": "empty-password", "password": ""}), 400)
	account := addAccount(t, admin, "self-service", 0)
	checkStatus(t, admin.json("PATCH", "/admin/api/users/"+account.ID, map[string]string{"password": "x"}), 200)
	owner, second := newBrowser(a, false), newBrowser(a, false)
	loginAs(t, owner, "self-service", "x")
	loginAs(t, second, "self-service", "x")
	checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": "wrong", "newPassword": "new-account-password-for-validation"}), 403)
	checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": "x", "newPassword": ""}), 400)
	checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": "x", "newPassword": "y"}), 200)
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 401)
	checkStatus(t, second.request("GET", "/api/transfers", nil, nil), 401)
	loginAs(t, owner, "self-service", "y")
	longPassword := strings.Repeat("p", 1024)
	checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": "y", "newPassword": longPassword}), 200)
	loginAs(t, newBrowser(a, false), "self-service", longPassword)
}

func TestBootstrapAndTransferPasswordsAcceptShortAndLongPasswords(t *testing.T) {
	for _, password := range []string{"x", strings.Repeat("p", 1024)} {
		t.Run(fmt.Sprint(len(password)), func(t *testing.T) {
			a := testApp(t)
			cfg := a.cfg
			a.Close()
			cfg.Development = false
			cfg.OwnerURL, cfg.PublicURL, cfg.AdminURL = "https://owner.example.test", "https://public.example.test", "https://admin.example.test"
			if err := os.WriteFile(cfg.OwnerPasswordFile, []byte(password), 0600); err != nil {
				t.Fatal(err)
			}
			restarted, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restarted.Close)
			owner := newBrowser(restarted, false)
			loginAs(t, owner, "admin", password)
			transfer := publish(t, owner, draft(t, owner, 0, password, "private"), "private")
			public := newBrowser(restarted, true)
			public.page(t, "/s/"+transfer.ShareToken)
			checkStatus(t, public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": password}), 200)
			checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 200)
		})
	}
}

func TestTargetedPurgeAndBrowserDownloadErrors(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	public := newBrowser(a, true)
	other := publish(t, owner, draft(t, owner, 0, "", "retained"), "retained")
	if _, err := a.store.db.Exec("UPDATE transfers SET expires_at=? WHERE id=?", time.Now().Unix()-1, other.ID); err != nil {
		t.Fatal(err)
	}
	transfer := publish(t, owner, draft(t, owner, 1, "", "one-use"), "one-use")
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 200)
	if _, err := a.root.Stat(other.Files[0].ID); err != nil {
		t.Fatal("download ran unrelated global cleanup")
	}
	if err := a.cleanup(); err != nil {
		t.Fatal(err)
	}
	var purged bool
	a.store.db.QueryRow("SELECT purged FROM files WHERE id=?", transfer.Files[0].ID).Scan(&purged)
	if !purged {
		t.Fatal("physical purge marker missing")
	}
	page := public.request("GET", downloadPath(transfer, 0), nil, map[string]string{"Accept": "text/html"})
	checkStatus(t, page, 404)
	if !strings.HasPrefix(page.Header().Get("Content-Type"), "text/html") || !strings.Contains(page.Body.String(), "Return to transfer") || page.Header().Get("X-Request-ID") == "" {
		t.Fatal("browser download error lacks recovery or correlation")
	}
	api := public.request("GET", downloadPath(transfer, 0), nil, map[string]string{"Accept": "application/json"})
	checkStatus(t, api, 404)
	if !strings.Contains(api.Body.String(), "error") {
		t.Fatal("API error must remain JSON")
	}
}

func TestRecoveryRepairsMetadataAndQuarantinesCorruption(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	partial := draft(t, owner, 0, "", "abcdef")
	path := startFile(t, owner, partial.Files[0])
	patchFile(t, owner, path, 0, "abc")
	if err := a.root.Remove(partial.Files[0].ID + ".info"); err != nil {
		t.Fatal(err)
	}
	full := publish(t, owner, draft(t, owner, 0, "", "valid"), "valid")
	if err := os.WriteFile(filepath.Join(a.cfg.DataDir, "uploads", full.Files[0].ID), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	oversized := draft(t, owner, 0, "", "small")
	startFile(t, owner, oversized.Files[0])
	os.WriteFile(filepath.Join(a.cfg.DataDir, "uploads", oversized.Files[0].ID), []byte("oversized"), 0600)
	a.Close()
	restarted, err := New(a.cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	owner.app = restarted
	head := owner.request("HEAD", path, nil, map[string]string{"Tus-Resumable": "1.0.0"})
	checkStatus(t, head, 200)
	if head.Header().Get("Upload-Offset") != "3" {
		t.Fatal("repaired metadata lost offset")
	}
	patchFile(t, owner, path, 3, "def")
	readTransfer(t, owner.json("POST", "/api/transfers/"+partial.ID+"/publish", nil))
	for _, transfer := range []Transfer{full, oversized} {
		current, _ := restarted.store.transfer(context.Background(), transfer.ID)
		if current.Files[0].Uploaded || current.Files[0].IntegrityError == "" {
			t.Fatal("corrupt file not quarantined")
		}
	}
	checkStatus(t, newBrowser(restarted, true).request("GET", downloadPath(full, 0), nil, nil), 404)
}

func TestReadinessDetectsReadOnlyPersistenceAndMaintenanceFailure(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	checkStatus(t, owner.request("GET", "/healthz", nil, nil), 200)
	a.healthMu.Lock()
	a.healthChecked = time.Time{}
	a.healthMu.Unlock()
	if _, err := a.store.db.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, owner.request("GET", "/healthz", nil, nil), 503)
	a.store.db.Exec("PRAGMA query_only=OFF")
	a.healthMu.Lock()
	a.healthChecked = time.Time{}
	a.healthMu.Unlock()
	checkStatus(t, owner.request("GET", "/healthz", nil, nil), 200)
	a.cleanupFailed.Store(true)
	checkStatus(t, owner.request("GET", "/healthz", nil, nil), 503)
	if err := a.cleanup(); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, owner.request("GET", "/healthz", nil, nil), 200)
}

func TestTypedStorageErrorsAndPrivateDiagnostics(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	shared := publish(t, owner, draft(t, owner, 0, "", "download"), "download")
	admin := adminBrowser(t, a)
	var logs bytes.Buffer
	a.logger = slog.New(slog.NewTextHandler(&logs, nil))
	a.store.db.Exec("PRAGMA query_only=ON")
	response := owner.json("POST", "/api/transfers", map[string]any{"title": "private-name-do-not-log", "files": []map[string]any{{"name": "private-filename-do-not-log", "size": 1, "sha256": testDigest("x")}}})
	checkStatus(t, response, 503)
	checkStatus(t, newBrowser(a, true).request("GET", downloadPath(shared, 0), nil, nil), 503)
	checkStatus(t, admin.json("PUT", "/admin/api/settings", a.settings()), 503)
	checkStatus(t, admin.json("POST", "/admin/api/users", map[string]any{"username": "readonly-user", "password": accountTestPassword}), 503)
	for _, secret := range []string{"private-name-do-not-log", "private-filename-do-not-log", a.cfg.DataDir, "owner-password-for-validation"} {
		if strings.Contains(logs.String(), secret) || strings.Contains(response.Body.String(), secret) {
			t.Fatal("private value exposed in diagnostic")
		}
	}
	if !strings.Contains(logs.String(), "database_read_only") || !strings.Contains(logs.String(), "request_id") {
		t.Fatal("diagnostic category/correlation missing")
	}
	a.store.db.Exec("PRAGMA query_only=OFF")
}

func TestMaintenanceBoundsPurgeWorkAndSkipsCompletedFiles(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := draft(t, owner, 0, "", "")
	tx, err := a.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 1; i < 600; i++ {
		if _, err = tx.Exec("INSERT INTO files(id,transfer_id,name,size,sha256) VALUES(?,?,'pending',0,?)", randomID(), transfer.ID, testDigest("")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = tx.Exec("UPDATE transfers SET status='deleted',closed_at=? WHERE id=?", time.Now().Unix(), transfer.ID); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	syncs := 0
	a.syncFile = func(file *os.File) error { syncs++; return file.Sync() }
	for _, expected := range []int{cleanupBatch, 600, 600} {
		if err = a.cleanup(); err != nil {
			t.Fatal(err)
		}
		var purged int
		if err = a.store.db.QueryRow("SELECT COUNT(*) FROM files WHERE purged=1").Scan(&purged); err != nil {
			t.Fatal(err)
		}
		if purged != expected {
			t.Fatalf("purged %d files, expected %d", purged, expected)
		}
	}
	if syncs != 2 {
		t.Fatal("maintenance repeated physical purges of completed files")
	}
}

func TestRecoveryQuarantinesOneFileWithoutBlockingHealthyDownloads(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "", "broken", "healthy"), "broken", "healthy")
	// Same length corruption must be detected by checksum, not just stat.
	if err := os.WriteFile(filepath.Join(a.cfg.DataDir, "uploads", transfer.Files[0].ID), []byte("damage"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := a.recover(); err != nil {
		t.Fatal(err)
	}
	public := newBrowser(a, true)
	checkStatus(t, public.request("HEAD", downloadPath(transfer, 0), nil, nil), 404)
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 404)
	response := public.request("GET", downloadPath(transfer, 1), nil, nil)
	checkStatus(t, response, 200)
	if response.Body.String() != "healthy" {
		t.Fatal("healthy file changed")
	}
	response = public.request("GET", "/s/"+transfer.ShareToken+"/archive", nil, nil)
	checkStatus(t, response, 200)
	archive, err := zip.NewReader(bytes.NewReader(response.Body.Bytes()), int64(response.Body.Len()))
	if err != nil || len(archive.File) != 1 {
		t.Fatalf("archive included quarantined file: %v", err)
	}
	entry, err := archive.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer entry.Close()
	body, err := io.ReadAll(entry)
	if err != nil || string(body) != "healthy" {
		t.Fatal("archive lost healthy contents")
	}
}
