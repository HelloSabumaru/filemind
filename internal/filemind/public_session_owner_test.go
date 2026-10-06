package filemind

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func publicOwnerSessionCount(t *testing.T, a *App, ownerID string) int {
	t.Helper()
	var count int
	if err := a.store.db.QueryRow(`SELECT COUNT(*) FROM sessions s
 JOIN transfers t ON t.id=s.transfer_id WHERE s.kind='share' AND t.user_id=?`, ownerID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Metadata-only fixtures let us exercise aggregate capacity without uploads or
// thousands of HTTP requests. Their credentials match the published template.
func seedSessionTransfer(t *testing.T, a *App, template Transfer, ownerID string) Transfer {
	t.Helper()
	id := randomID()
	_, err := a.store.db.Exec(`INSERT INTO transfers
 (id,user_id,title,status,created_at,touched_at,expiry_seconds,download_limit,password_hash,auth_version,share_token)
 SELECT ?,?,'Public session fixture','published',created_at,touched_at,expiry_seconds,download_limit,password_hash,auth_version,?
 FROM transfers WHERE id=?`, id, ownerID, randomToken(), template.ID)
	if err != nil {
		t.Fatal(err)
	}
	transfer, err := a.store.transfer(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return transfer
}

func TestPublicOwnerSessionBudgetRetiresOnlyItsOwnOldestSession(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	admin := adminBrowser(t, a)
	user := passwordTestUser(t, a, "test-owner")
	shared := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	transfer, err := a.store.transfer(context.Background(), shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	first := newBrowser(a, true)
	first.page(t, "/s/"+shared.ShareToken)
	unlock := "/s/" + shared.ShareToken + "/unlock"
	checkStatus(t, first.json("POST", unlock, map[string]string{"password": "secret"}), 200)
	name := a.cookieName("share-" + transfer.ID)
	if _, err = a.store.db.Exec("UPDATE sessions SET expires_at=? WHERE token_hash=?", time.Now().Add(time.Minute).Unix(), tokenHash(first.cookies[name].Value)); err != nil {
		t.Fatal(err)
	}
	hashes := seedSubjectSessions(t, a, "share", User{}, transfer, maxSessionsPerTransfer-1)
	for remaining := maxPublicSessionsPerOwner - maxSessionsPerTransfer; remaining > 0; {
		fixture := seedSessionTransfer(t, a, transfer, user.ID)
		n := min(remaining, maxSessionsPerTransfer)
		seedSubjectSessions(t, a, "share", User{}, fixture, n)
		remaining -= n
	}

	// The other uploader's even older session must survive eviction, and a
	// fresh unlock for their link must still succeed despite the first budget.
	another := addAccount(t, admin, "other-public-owner", 1024)
	otherOwner := newBrowser(a, false)
	loginAs(t, otherOwner, another.Username, accountTestPassword)
	other := publish(t, otherOwner, draft(t, otherOwner, 0, "secret", "other"), "other")
	separate := newBrowser(a, true)
	separate.page(t, "/s/"+other.ShareToken)
	otherUnlock := "/s/" + other.ShareToken + "/unlock"
	checkStatus(t, separate.json("POST", otherUnlock, map[string]string{"password": "secret"}), 200)
	if _, err = a.store.db.Exec("UPDATE sessions SET expires_at=? WHERE token_hash=?", time.Now().Add(30*time.Second).Unix(), tokenHash(separate.cookies[a.cookieName("share-"+other.ID)].Value)); err != nil {
		t.Fatal(err)
	}
	cookie := first.cookies[name].Value
	checkStatus(t, first.json("POST", unlock, map[string]string{"password": "secret"}), 200)
	if first.cookies[name].Value != cookie || publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner {
		t.Fatal("valid cookie reuse changed the uploader's full budget")
	}
	checkStatus(t, first.request("GET", downloadPath(shared, 0), nil, nil), 200)

	// Unlock a different transfer that has no sessions: only the aggregate
	// budget, not the per-transfer cap, can retire the first link's session.
	target := publish(t, owner, draft(t, owner, 0, "secret", "target"), "target")
	fresh := newBrowser(a, true)
	fresh.page(t, "/s/"+target.ShareToken)
	targetUnlock := "/s/" + target.ShareToken + "/unlock"
	checkStatus(t, fresh.json("POST", targetUnlock, map[string]string{"password": "secret"}), 200)
	if publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner || subjectSessionCount(t, a, "share", target.ID) != 1 {
		t.Fatal("fresh unlock exceeded the uploader budget or failed to allocate")
	}
	checkStatus(t, first.request("GET", downloadPath(shared, 0), nil, nil), 401)
	checkStatus(t, separate.request("GET", downloadPath(other, 0), nil, nil), 200)
	otherFresh := newBrowser(a, true)
	otherFresh.page(t, "/s/"+other.ShareToken)
	checkStatus(t, otherFresh.json("POST", otherUnlock, map[string]string{"password": "secret"}), 200)
	if publicOwnerSessionCount(t, a, another.ID) != 2 {
		t.Fatal("another uploader's unlock was affected by the full budget")
	}
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)

	// Reauthorization replaces a stale cookie without evicting another device
	// merely because the uploader has reached its budget.
	checkStatus(t, owner.json("PATCH", "/api/transfers/"+target.ID, map[string]string{"password": "replacement"}), 200)
	checkStatus(t, fresh.json("POST", targetUnlock, map[string]string{"password": "replacement"}), 200)
	var kept int
	if err = a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token_hash=?", hashes[0]).Scan(&kept); err != nil || kept != 1 {
		t.Fatal("reauthorization evicted another device session", err)
	}
	if publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner {
		t.Fatal("reauthorization changed the full aggregate budget")
	}
}

func TestConcurrentPublicAllocationsPreserveOwnerBudget(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	user := passwordTestUser(t, a, "test-owner")
	shared := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	for remaining := maxPublicSessionsPerOwner - 1; remaining > 0; {
		fixture := seedSessionTransfer(t, a, shared, user.ID)
		n := min(remaining, maxSessionsPerTransfer)
		seedSubjectSessions(t, a, "share", User{}, fixture, n)
		remaining -= n
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for range 2 {
		transfer := seedSessionTransfer(t, a, shared, user.ID)
		// Allocation must use the persisted owner, not an unchecked caller's
		// snapshot, to prevent bypassing the aggregate budget.
		transfer.UserID = "incorrect-snapshot-owner"
		group.Go(func() {
			<-start
			r := httptest.NewRequest("POST", a.cfg.PublicURL+"/s/"+transfer.ShareToken+"/unlock", nil)
			results <- a.grantSession(httptest.NewRecorder(), r, "share", transfer, User{})
		})
	}
	close(start)
	group.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal("concurrent unlock was locked out", err)
		}
	}
	if publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner {
		t.Fatal("concurrent unlocks exceeded the uploader's aggregate budget")
	}
}

func TestPublicOwnerBudgetEvictionRollsBackOnAllocationFailure(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	user := passwordTestUser(t, a, "test-owner")
	shared := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	var oldest, next string
	for remaining := maxPublicSessionsPerOwner; remaining > 0; {
		fixture := seedSessionTransfer(t, a, shared, user.ID)
		n := min(remaining, maxSessionsPerTransfer)
		hashes := seedSubjectSessions(t, a, "share", User{}, fixture, n)
		if oldest == "" {
			oldest, next = hashes[0], hashes[1]
		}
		remaining -= n
	}
	if _, err := a.store.db.Exec("UPDATE sessions SET expires_at=? WHERE token_hash=?", time.Now().Add(time.Minute).Unix(), oldest); err != nil {
		t.Fatal(err)
	}
	// Closed transfers' live session records still occupy the shared pool.
	if _, err := a.store.db.Exec("UPDATE transfers SET status='revoked' WHERE user_id=? AND id<>?", user.ID, shared.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.db.Exec(`CREATE TRIGGER fail_public_session BEFORE INSERT ON sessions
 WHEN NEW.kind='share' BEGIN SELECT RAISE(ABORT,'fixture allocation failure'); END`); err != nil {
		t.Fatal(err)
	}
	target := seedSessionTransfer(t, a, shared, user.ID)
	r := httptest.NewRequest("POST", a.cfg.PublicURL+"/s/"+target.ShareToken+"/unlock", nil)
	w := httptest.NewRecorder()
	if err := a.grantSession(w, r, "share", target, User{}); err == nil {
		t.Fatal("fixture allocation failure did not abort the grant")
	}
	var kept int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token_hash=?", oldest).Scan(&kept); err != nil || kept != 1 {
		t.Fatal("failed allocation committed the aggregate eviction", err)
	}
	if publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner || len(w.Result().Cookies()) != 0 {
		t.Fatal("failed allocation changed occupancy or set a session cookie")
	}
	if _, err := a.store.db.Exec("DROP TRIGGER fail_public_session"); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	if err := a.grantSession(w, r, "share", target, User{}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token_hash=?", oldest).Scan(&kept); err != nil || kept != 0 {
		t.Fatal("successful allocation ignored retained closed transfers' sessions", err)
	}
	if publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner {
		t.Fatal("retained closed transfers allowed allocation over the owner budget")
	}
	// With the new session expired, allocation reclaims its slot before
	// considering eviction; the next device should remain authorized.
	if _, err := a.store.db.Exec("UPDATE sessions SET expires_at=0 WHERE token_hash=?", tokenHash(w.Result().Cookies()[0].Value)); err != nil {
		t.Fatal(err)
	}
	if err := a.grantSession(httptest.NewRecorder(), r, "share", target, User{}); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token_hash=?", next).Scan(&kept); err != nil || kept != 1 {
		t.Fatal("reclaiming an expired session unnecessarily evicted another device", err)
	}
	if publicOwnerSessionCount(t, a, user.ID) != maxPublicSessionsPerOwner {
		t.Fatal("expired session reclamation exceeded the aggregate budget")
	}
}

func TestStartupPrunesPublicOwnerBudgetsAcrossRetainedTransfers(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	admin := adminBrowser(t, a)
	user := passwordTestUser(t, a, "test-owner")
	administrator := passwordTestUser(t, a, "admin")
	shared := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	var oldest, newest string
	var transfers []Transfer
	// Every transfer stays within its own cap. Identical expiries exercise
	// the rowid tie-breaker across transfers, keeping the latest allocations.
	for remaining := maxPublicSessionsPerOwner + 3; remaining > 0; {
		transfer := seedSessionTransfer(t, a, shared, user.ID)
		transfers = append(transfers, transfer)
		n := min(remaining, maxSessionsPerTransfer)
		hashes := seedSubjectSessions(t, a, "share", User{}, transfer, n)
		if oldest == "" {
			oldest = hashes[0]
		}
		newest = hashes[len(hashes)-1]
		remaining -= n
	}
	if _, err := a.store.db.Exec("UPDATE sessions SET expires_at=? WHERE kind='share'", time.Now().Add(30*time.Minute).Unix()); err != nil {
		t.Fatal(err)
	}
	// Closed metadata continues to occupy the session pool and must remain
	// attributable to its uploader until its sessions or history are removed.
	if _, err := a.store.db.Exec("UPDATE transfers SET status='revoked' WHERE id=?", transfers[0].ID); err != nil {
		t.Fatal(err)
	}
	other := seedSessionTransfer(t, a, shared, administrator.ID)
	hashes := seedSubjectSessions(t, a, "share", User{}, other, 1)
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if publicOwnerSessionCount(t, restarted, user.ID) != maxPublicSessionsPerOwner || publicOwnerSessionCount(t, restarted, administrator.ID) != 1 {
		t.Fatal("startup did not isolate and prune each uploader's budget")
	}
	for _, transfer := range transfers {
		if subjectSessionCount(t, restarted, "share", transfer.ID) > maxSessionsPerTransfer {
			t.Fatal("startup aggregate pruning broke the per-transfer cap")
		}
	}
	for _, expected := range []struct {
		hash  string
		count int
	}{{oldest, 0}, {newest, 1}, {hashes[0], 1}} {
		var count int
		if err := restarted.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE token_hash=?", expected.hash).Scan(&count); err != nil || count != expected.count {
			t.Fatal("startup failed to retain the newest sessions within each owner's budget", err)
		}
	}
	owner.app, admin.app = restarted, restarted
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)
}
