package filemind

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func subjectSessionCount(t *testing.T, a *App, kind, subject string) int {
	t.Helper()
	column := "user_id"
	if kind == "share" {
		column = "transfer_id"
	}
	var count int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE kind=? AND "+column+"=?", kind, subject).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func seedSubjectSessions(t *testing.T, a *App, kind string, user User, transfer Transfer, count int) []string {
	t.Helper()
	var userID any = user.ID
	transferID, version := "", user.AuthVersion
	expires := time.Now().Add(2 * time.Hour).Unix()
	if kind == "share" {
		userID, transferID, version = nil, transfer.ID, transfer.AuthVersion
		expires = time.Now().Add(30 * time.Minute).Unix()
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	var hashes []string
	for index := range count {
		hash := tokenHash(randomToken())
		_, err = tx.Exec("INSERT INTO sessions(token_hash,kind,user_id,transfer_id,auth_version,expires_at) VALUES(?,?,?,?,?,?)", hash, kind, userID, transferID, version, expires+int64(index))
		if err != nil {
			t.Fatal(err)
		}
		hashes = append(hashes, hash)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return hashes
}

func TestAccountSessionCapRetiresOnlyItsOldestSession(t *testing.T) {
	for _, kind := range []string{"owner", "admin"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			admin := adminBrowser(t, a)
			original, other := newBrowser(a, false), newBrowser(a, false)
			username := "test-owner"
			if kind == "admin" {
				username, original = "admin", admin
				// The same account's session on the other listener must survive.
				loginAs(t, other, "admin", "owner-password-for-validation")
			} else {
				original.login(t)
				another := addAccount(t, admin, "other-session-owner", 1024)
				loginAs(t, other, another.Username, accountTestPassword)
			}
			user := passwordTestUser(t, a, username)
			previous := original.cookies[a.cookieName(sessionCookie(kind))]
			if _, err := a.store.db.Exec("UPDATE sessions SET expires_at=? WHERE token_hash=?", time.Now().Add(time.Minute).Unix(), tokenHash(previous.Value)); err != nil {
				t.Fatal(err)
			}
			hashes := seedSubjectSessions(t, a, kind, user, Transfer{}, maxSessionsPerAccount-1)
			fresh := newBrowser(a, false)
			fresh.admin = kind == "admin"
			loginAs(t, fresh, username, "owner-password-for-validation")
			if subjectSessionCount(t, a, kind, user.ID) != maxSessionsPerAccount {
				t.Fatal("fresh account sign-in exceeded its session cap")
			}
			path := "/api/transfers"
			if kind == "admin" {
				path = "/admin/api/users"
			}
			checkStatus(t, original.request("GET", path, nil, nil), 401)
			checkStatus(t, fresh.request("GET", path, nil, nil), 200)
			checkStatus(t, other.request("GET", "/api/transfers", nil, nil), 200)
			// Rotating a browser's existing cookie frees its own slot; it must
			// not retire another device merely because the account is at its cap.
			before := fresh.cookies[a.cookieName(sessionCookie(kind))].Value
			checkStatus(t, fresh.json("POST", "/login", map[string]string{"username": username, "password": "owner-password-for-validation"}), 200)
			if fresh.cookies[a.cookieName(sessionCookie(kind))].Value == before || subjectSessionCount(t, a, kind, user.ID) != maxSessionsPerAccount {
				t.Fatal("account cookie rotation did not preserve the cap")
			}
			var kept int
			if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token_hash=?", hashes[0]).Scan(&kept); err != nil || kept != 1 {
				t.Fatal("cookie replacement retired an unrelated device session", err)
			}
		})
	}
}

func TestTransferSessionCapReuseAndCredentialReplacement(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	other := publish(t, owner, draft(t, owner, 0, "secret", "other"), "other")
	stored, err := a.store.transfer(context.Background(), transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	first, separate := newBrowser(a, true), newBrowser(a, true)
	first.page(t, "/s/"+transfer.ShareToken)
	unlock := "/s/" + transfer.ShareToken + "/unlock"
	checkStatus(t, first.json("POST", unlock, map[string]string{"password": "secret"}), 200)
	separate.page(t, "/s/"+other.ShareToken)
	checkStatus(t, separate.json("POST", "/s/"+other.ShareToken+"/unlock", map[string]string{"password": "secret"}), 200)
	name := a.cookieName("share-" + transfer.ID)
	if _, err := a.store.db.Exec("UPDATE sessions SET expires_at=? WHERE token_hash=?", time.Now().Add(time.Minute).Unix(), tokenHash(first.cookies[name].Value)); err != nil {
		t.Fatal(err)
	}
	seedSubjectSessions(t, a, "share", User{}, stored, maxSessionsPerTransfer-1)
	fresh := newBrowser(a, true)
	fresh.page(t, "/s/"+transfer.ShareToken)
	checkStatus(t, fresh.json("POST", unlock, map[string]string{"password": "secret"}), 200)
	if subjectSessionCount(t, a, "share", transfer.ID) != maxSessionsPerTransfer {
		t.Fatal("unlock exceeded the protected transfer's session cap")
	}
	checkStatus(t, first.request("GET", downloadPath(transfer, 0), nil, nil), 401)
	checkStatus(t, separate.request("GET", downloadPath(other, 0), nil, nil), 200)
	cookie := fresh.cookies[name].Value
	checkStatus(t, fresh.json("POST", unlock, map[string]string{"password": "secret"}), 200)
	if fresh.cookies[name].Value != cookie || subjectSessionCount(t, a, "share", transfer.ID) != maxSessionsPerTransfer {
		t.Fatal("reusing authorization retired or allocated a transfer session")
	}
	checkStatus(t, owner.json("PATCH", "/api/transfers/"+transfer.ID, map[string]string{"password": "replacement"}), 200)
	checkStatus(t, fresh.json("POST", unlock, map[string]string{"password": "replacement"}), 200)
	if subjectSessionCount(t, a, "share", transfer.ID) != 1 || subjectSessionCount(t, a, "share", other.ID) != 1 {
		t.Fatal("credential replacement retained stale sessions or changed another link")
	}
	checkStatus(t, separate.request("GET", downloadPath(other, 0), nil, nil), 200)
}

func TestConcurrentAllocationPreservesSubjectSessionCaps(t *testing.T) {
	for _, kind := range []string{"owner", "admin", "share"} {
		t.Run(kind, func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			user := passwordTestUser(t, a, "test-owner")
			transfer, maximum, subject := Transfer{}, maxSessionsPerAccount, user.ID
			if kind == "admin" {
				adminBrowser(t, a)
				user = passwordTestUser(t, a, "admin")
				subject = user.ID
			}
			if kind == "share" {
				shared := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
				var err error
				transfer, err = a.store.transfer(context.Background(), shared.ID)
				if err != nil {
					t.Fatal(err)
				}
				maximum, subject = maxSessionsPerTransfer, transfer.ID
			}
			seedSubjectSessions(t, a, kind, user, transfer, maximum-1-subjectSessionCount(t, a, kind, subject))
			var group sync.WaitGroup
			results := make(chan error, 2)
			for range 2 {
				group.Go(func() {
					r := httptest.NewRequest("POST", a.cfg.OwnerURL+"/login", nil)
					results <- a.grantSession(httptest.NewRecorder(), r, kind, transfer, user)
				})
			}
			group.Wait()
			close(results)
			for err := range results {
				if err != nil {
					t.Fatal("valid concurrent allocation was locked out", err)
				}
			}
			if subjectSessionCount(t, a, kind, subject) != maximum {
				t.Fatal("concurrent allocation exceeded its subject session cap")
			}
		})
	}
}

func TestStartupReconcilesPersistedSubjectSessionCaps(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	admin := adminBrowser(t, a)
	user, administrator := passwordTestUser(t, a, "test-owner"), passwordTestUser(t, a, "admin")
	transfer := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	stored, err := a.store.transfer(context.Background(), transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedSubjectSessions(t, a, "owner", user, Transfer{}, maxSessionsPerAccount+3)
	seedSubjectSessions(t, a, "admin", administrator, Transfer{}, maxSessionsPerAccount+3)
	seedSubjectSessions(t, a, "share", User{}, stored, maxSessionsPerTransfer+3)
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if subjectSessionCount(t, restarted, "owner", user.ID) != maxSessionsPerAccount || subjectSessionCount(t, restarted, "admin", administrator.ID) != maxSessionsPerAccount || subjectSessionCount(t, restarted, "share", transfer.ID) != maxSessionsPerTransfer {
		t.Fatal("startup left persisted sessions above a subject limit")
	}
	// The newest real account sessions outlive the older fixture sessions.
	owner.app, admin.app = restarted, restarted
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)
}
