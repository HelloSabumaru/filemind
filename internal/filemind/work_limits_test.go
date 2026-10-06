package filemind

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

func TestAdminHashCapacitySurvivesOrdinaryWork(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	u := passwordTestUser(t, a, "test-owner")
	for range cap(a.hashSlots) {
		release, err := a.acquireHash(context.Background(), false, newUserID())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	client := newBrowser(a, false)
	client.page(t, "/login")
	busy := client.json("POST", "/login", map[string]string{"username": u.Username, "password": "owner-password-for-validation"})
	checkStatus(t, busy, 429)
	if busy.Header().Get("Retry-After") != "1" {
		t.Fatal("busy password verification omitted retry delay")
	}
	a.accountPasswords.mu.Lock()
	_, counted := a.accountPasswords.targets[u.ID]
	a.accountPasswords.mu.Unlock()
	if counted {
		t.Fatal("work capacity rejection counted as a password failure")
	}
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	admin := adminBrowser(t, a)
	added := addAccount(t, admin, "reserved-capacity", 1024)
	checkStatus(t, admin.json("PATCH", "/admin/api/users/"+added.ID, map[string]string{"password": "changed"}), 200)
	checkStatus(t, admin.json("POST", "/api/transfers", map[string]any{
		"password": "bulk-password", "files": []map[string]any{{"name": "empty", "size": 0, "sha256": testDigest("")}},
	}), 429)
}

func TestWorkLimitsPreserveOtherUsersAndReleaseState(t *testing.T) {
	ctx := context.Background()
	users := newUserWorkLimit(1)
	slots := make(chan struct{}, 2)
	first, err := acquireWork(ctx, slots, users, "first")
	if err != nil {
		t.Fatal(err)
	}
	first = sync.OnceFunc(first)
	t.Cleanup(first)
	if _, err = acquireWork(ctx, slots, users, "first"); err == nil {
		t.Fatal("one user occupied multiple work slots")
	}
	second, err := acquireWork(ctx, slots, users, "second")
	if err != nil {
		t.Fatal("another user could not use remaining capacity", err)
	}
	second = sync.OnceFunc(second)
	t.Cleanup(second)
	if _, err = acquireWork(ctx, slots, users, "third"); err == nil {
		t.Fatal("global capacity exceeded")
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = acquireWork(canceled, slots, users, "canceled"); !errors.Is(err, context.Canceled) {
		t.Fatal("canceled work acquired capacity", err)
	}
	first()
	second()
	if len(users.active) != 0 || len(slots) != 0 {
		t.Fatal("completed or rejected work retained capacity or user state")
	}
	// A user's password work is bounded across the ordinary and reserved pools.
	a := testApp(t)
	release, err := a.acquireHash(ctx, false, "same-user")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	if _, err = a.acquireHash(ctx, true, "same-user"); err == nil {
		t.Fatal("switching interfaces bypassed per-user password capacity")
	}
	reserved, err := a.acquireHash(ctx, true, "other-user")
	if err != nil {
		t.Fatal("ordinary work occupied reserved capacity", err)
	}
	reserved()
}

func TestUploadWorkLimitLeavesReadAccessAvailable(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	u := passwordTestUser(t, a, "test-owner")
	transfer := draft(t, owner, 0, "", "payload")
	path := startFile(t, owner, transfer.Files[0])
	for range 2 {
		release, err := a.uploadUsers.acquire(u.ID)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(release)
	}
	response := owner.request("PATCH", path, nil, map[string]string{"Tus-Resumable": "1.0.0", "Upload-Offset": "0", "Content-Type": "application/offset+octet-stream"})
	checkStatus(t, response, 429)
	if response.Header().Get("Retry-After") != "1" {
		t.Fatal("busy upload omitted retry delay")
	}
	checkStatus(t, owner.request("HEAD", path, nil, map[string]string{"Tus-Resumable": "1.0.0"}), 200)
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
}

func pauseFileSync(t *testing.T, a *App, id string) (<-chan struct{}, func()) {
	t.Helper()
	entered, resume := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := sync.OnceFunc(func() { close(resume) })
	a.syncFile = func(f *os.File) error {
		if filepath.Base(f.Name()) == id {
			once.Do(func() { close(entered) })
			<-resume
		}
		return f.Sync()
	}
	t.Cleanup(unblock)
	return entered, unblock
}

func waitForFileSync(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("file operation did not reach the paused synchronization")
	}
}

func responsiveAdminAction(t *testing.T, admin *browser, method, path string, input any) {
	t.Helper()
	result := make(chan *httptest.ResponseRecorder, 1)
	go func() { result <- admin.json(method, path, input) }()
	select {
	case response := <-result:
		checkStatus(t, response, 200)
	case <-time.After(5 * time.Second):
		t.Fatal("admin action waited for unrelated file work")
	}
}

func TestAdminActionsDoNotWaitForPublicationVerification(t *testing.T) {
	for _, action := range []string{"disable", "reset-password", "delete", "edit", "revoke-other"} {
		t.Run(action, func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			admin := adminBrowser(t, a)
			u := passwordTestUser(t, a, "test-owner")
			var other Transfer
			if action == "revoke-other" {
				other = publish(t, owner, draft(t, owner, 0, "", "other"), "other")
			}
			transfer := draft(t, owner, 0, "", "payload")
			path := startFile(t, owner, transfer.Files[0])
			patchFile(t, owner, path, 0, "payload")
			// Simulate a durable complete payload awaiting final verification.
			if _, err := a.store.db.Exec("UPDATE files SET uploaded=0 WHERE id=?", transfer.Files[0].ID); err != nil {
				t.Fatal(err)
			}
			entered, unblock := pauseFileSync(t, a, transfer.Files[0].ID)
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := a.publishTransfer(context.Background(), u, transfer.ID)
				result <- err
			}()
			t.Cleanup(func() { unblock(); <-done })
			waitForFileSync(t, entered)
			switch action {
			case "disable":
				responsiveAdminAction(t, admin, "PATCH", "/admin/api/users/"+u.ID, map[string]bool{"disabled": true})
			case "reset-password":
				responsiveAdminAction(t, admin, "PATCH", "/admin/api/users/"+u.ID, map[string]string{"password": "changed"})
			case "delete":
				responsiveAdminAction(t, admin, "DELETE", "/admin/api/transfers/"+transfer.ID, nil)
			case "edit":
				responsiveAdminAction(t, admin, "PATCH", "/admin/api/transfers/"+transfer.ID, map[string]any{"revision": transfer.Revision, "title": "Changed during verification"})
			case "revoke-other":
				responsiveAdminAction(t, admin, "POST", "/admin/api/transfers/"+other.ID+"/revoke", nil)
			}
			unblock()
			err := <-result
			stored, readErr := a.store.transfer(context.Background(), transfer.ID)
			if action == "delete" {
				if err == nil || !errors.Is(readErr, sql.ErrNoRows) {
					t.Fatal("stale verification recreated the deleted transfer", err, readErr)
				}
				assertTransferRemoved(t, a, transfer)
				return
			}
			if readErr != nil {
				t.Fatal(readErr)
			}
			if action == "revoke-other" {
				if err != nil || stored.Status != "published" {
					t.Fatal("unrelated intervention prevented publication", err)
				}
				return
			}
			if err == nil || stored.Status == "published" || stored.Files[0].Uploaded {
				t.Fatal("stale verification committed after admin intervention")
			}
			if action == "edit" {
				if stored.Title != "Changed during verification" {
					t.Fatal("publication overwrote the concurrent transfer edit")
				}
				if _, err = a.publishTransfer(context.Background(), u, transfer.ID); err != nil {
					t.Fatal("publication could not retry with the updated revision", err)
				}
			}
		})
	}
}

func TestAdminActionsDoNotWaitForUploadCreation(t *testing.T) {
	for _, action := range []string{"disable", "reset-password", "delete", "edit"} {
		t.Run(action, func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			admin := adminBrowser(t, a)
			u := passwordTestUser(t, a, "test-owner")
			transfer := draft(t, owner, 0, "", "payload")
			f := transfer.Files[0]
			entered, unblock := pauseFileSync(t, a, f.ID)
			result := make(chan *httptest.ResponseRecorder, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				result <- owner.request("POST", "/uploads/", nil, map[string]string{"Tus-Resumable": "1.0.0", "Upload-Length": strconv.FormatInt(f.Size, 10), "Upload-Metadata": "file_id " + base64.StdEncoding.EncodeToString([]byte(f.ID))})
			}()
			t.Cleanup(func() { unblock(); <-done })
			waitForFileSync(t, entered)
			switch action {
			case "disable":
				responsiveAdminAction(t, admin, "PATCH", "/admin/api/users/"+u.ID, map[string]bool{"disabled": true})
			case "reset-password":
				responsiveAdminAction(t, admin, "PATCH", "/admin/api/users/"+u.ID, map[string]string{"password": "changed"})
			case "delete":
				responsiveAdminAction(t, admin, "DELETE", "/admin/api/transfers/"+transfer.ID, nil)
			case "edit":
				responsiveAdminAction(t, admin, "PATCH", "/admin/api/transfers/"+transfer.ID, map[string]any{"revision": transfer.Revision, "title": "Changed while creating"})
			}
			unblock()
			response := <-result
			if response.Code >= 200 && response.Code < 300 {
				t.Fatal("stale upload creation succeeded after intervention")
			}
			stored, err := a.store.transfer(context.Background(), transfer.ID)
			if action == "delete" {
				if !errors.Is(err, sql.ErrNoRows) {
					t.Fatal("stale upload creation recreated the deleted transfer", err)
				}
				assertTransferRemoved(t, a, transfer)
				return
			}
			if err != nil || stored.Files[0].Started {
				t.Fatal("stale upload creation marked the file started", err)
			}
			for _, name := range []string{f.ID, f.ID + ".info"} {
				if _, err = a.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("canceled upload creation left payload or metadata behind", err)
				}
			}
			if action == "edit" {
				startFile(t, owner, f)
			}
		})
	}
}

func TestPublicationRejectsPayloadReplacementDuringFileWork(t *testing.T) {
	for _, verified := range []bool{false, true} {
		t.Run(strconv.FormatBool(verified), func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			u := passwordTestUser(t, a, "test-owner")
			transfer := draft(t, owner, 0, "", "payload")
			f := transfer.Files[0]
			path := startFile(t, owner, f)
			patchFile(t, owner, path, 0, "payload")
			if _, err := a.store.db.Exec("UPDATE files SET uploaded=? WHERE id=?", verified, f.ID); err != nil {
				t.Fatal(err)
			}
			entered, unblock := pauseFileSync(t, a, f.ID)
			result := make(chan error, 1)
			done := make(chan struct{})
			go func() {
				defer close(done)
				_, err := a.publishTransfer(context.Background(), u, transfer.ID)
				result <- err
			}()
			t.Cleanup(func() { unblock(); <-done })
			waitForFileSync(t, entered)
			// Replace the path with different, same-sized fixture bytes while the
			// original open descriptor still contains the declared checksum.
			if err := os.WriteFile(filepath.Join(a.cfg.DataDir, "uploads", "replacement"), []byte("changed"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := a.root.Rename("replacement", f.ID); err != nil {
				t.Fatal(err)
			}
			unblock()
			if err := <-result; err == nil {
				t.Fatal("publication accepted a payload replaced during file work")
			}
			stored, err := a.store.transfer(context.Background(), transfer.ID)
			if err != nil || stored.Status != "draft" || (!verified && stored.Files[0].Uploaded) {
				t.Fatal("stale file verification promoted the replacement payload", err)
			}
		})
	}
}
