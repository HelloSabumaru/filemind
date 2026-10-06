package filemind

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func sessionCount(t *testing.T, a *App, kind string) int {
	t.Helper()
	var count int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE kind=?", kind).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// Seed occupancy directly in the temporary test database without generating traffic.
func seedSessionOccupancy(t *testing.T, a *App, kind string, count int, transfer Transfer) {
	t.Helper()
	var userID any
	if kind != "share" {
		var id string
		name := "test-owner"
		if kind == "admin" {
			name = "admin"
		}
		if err := a.store.db.QueryRow("SELECT id FROM users WHERE username=?", name).Scan(&id); err != nil {
			t.Fatal(err)
		}
		userID = id
	}
	_, err := a.store.db.Exec(`WITH RECURSIVE occupancy(n) AS (
 SELECT 1 WHERE ?>0 UNION ALL SELECT n+1 FROM occupancy WHERE n<?
 ) INSERT INTO sessions(token_hash,kind,user_id,transfer_id,auth_version,expires_at)
 SELECT 'test-occupancy-' || ? || '-' || n,?,?,?,?,? FROM occupancy`, count, count, kind, kind, userID, transfer.ID, transfer.AuthVersion, time.Now().Add(time.Hour).Unix())
	if err != nil {
		t.Fatal(err)
	}
}

func TestPasswordlessUnlockDoesNotCreateSession(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "", "open"), "open")
	public := newBrowser(a, true)
	public.page(t, "/s/"+transfer.ShareToken)
	for range 2 {
		response := public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": ""})
		checkStatus(t, response, http.StatusOK)
		if len(response.Result().Cookies()) != 0 || sessionCount(t, a, "share") != 0 {
			t.Fatal("passwordless unlock created a session or session cookie")
		}
	}
	public.csrf = "invalid"
	checkStatus(t, public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": ""}), http.StatusForbidden)
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), http.StatusOK)
}

func TestShareSessionReuseAndReplacement(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	public := newBrowser(a, true)
	public.page(t, "/s/"+transfer.ShareToken)
	unlock := "/s/" + transfer.ShareToken + "/unlock"
	checkStatus(t, public.json("POST", unlock, map[string]string{"password": "secret"}), http.StatusOK)
	name := a.cookieName("share-" + transfer.ID)
	first := public.cookies[name]
	var expires int64
	if err := a.store.db.QueryRow("SELECT expires_at FROM sessions WHERE token_hash=?", tokenHash(first.Value)).Scan(&expires); err != nil {
		t.Fatal(err)
	}
	response := public.json("POST", unlock, map[string]string{"password": "secret"})
	checkStatus(t, response, http.StatusOK)
	if len(response.Result().Cookies()) != 0 || public.cookies[name].Value != first.Value || sessionCount(t, a, "share") != 1 {
		t.Fatal("unlock replaced or duplicated an existing valid session")
	}
	var reusedExpiry int64
	if err := a.store.db.QueryRow("SELECT expires_at FROM sessions WHERE token_hash=?", tokenHash(first.Value)).Scan(&reusedExpiry); err != nil || reusedExpiry != expires {
		t.Fatal("session reuse extended its expiry", err)
	}
	checkStatus(t, owner.json("PATCH", "/api/transfers/"+transfer.ID, map[string]string{"password": "replacement"}), http.StatusOK)
	checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), http.StatusUnauthorized)
	checkStatus(t, public.json("POST", unlock, map[string]string{"password": "replacement"}), http.StatusOK)
	if public.cookies[name].Value == first.Value || sessionCount(t, a, "share") != 1 {
		t.Fatal("new authorization did not retire the stale session")
	}
	old := newBrowser(a, true)
	old.cookies[name] = first
	checkStatus(t, old.request("GET", downloadPath(transfer, 0), nil, nil), http.StatusUnauthorized)
	if _, err := a.store.db.Exec("UPDATE sessions SET expires_at=0 WHERE token_hash=?", tokenHash(public.cookies[name].Value)); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, public.json("POST", unlock, map[string]string{"password": "replacement"}), http.StatusOK)
	if sessionCount(t, a, "share") != 1 {
		t.Fatal("expired session was not reclaimed")
	}
}

func TestSessionCapacityIsIndependentForEachKind(t *testing.T) {
	for _, saturated := range []string{"share", "owner", "admin"} {
		t.Run(saturated, func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			transfer := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
			stored, err := a.store.transfer(context.Background(), transfer.ID)
			if err != nil {
				t.Fatal(err)
			}
			seedSessionOccupancy(t, a, saturated, maxSessionsPerKind-sessionCount(t, a, saturated), stored)
			for _, kind := range []string{"owner", "admin"} {
				account := newBrowser(a, false)
				account.admin = kind == "admin"
				account.page(t, "/login")
				want := http.StatusOK
				if kind == saturated {
					want = http.StatusServiceUnavailable
				}
				name := "test-owner"
				if account.admin {
					name = "admin"
				}
				checkStatus(t, account.json("POST", "/login", map[string]string{"username": name, "password": "owner-password-for-validation"}), want)
				if want == http.StatusOK {
					path := "/api/transfers"
					if account.admin {
						path = "/admin/api/users"
					}
					checkStatus(t, account.request("GET", path, nil, nil), http.StatusOK)
				}
			}
			public := newBrowser(a, true)
			public.page(t, "/s/"+transfer.ShareToken)
			want := http.StatusOK
			if saturated == "share" {
				want = http.StatusServiceUnavailable
			}
			checkStatus(t, public.json("POST", "/s/"+transfer.ShareToken+"/unlock", map[string]string{"password": "secret"}), want)
			if sessionCount(t, a, saturated) != maxSessionsPerKind {
				t.Fatal("saturated pool grew past its limit")
			}
			// Existing account sessions continue working and reauthentication frees its own slot.
			checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), http.StatusOK)
			previous := owner.cookies[a.cookieName(sessionCookie("owner"))]
			count := sessionCount(t, a, "owner")
			checkStatus(t, owner.json("POST", "/login", map[string]string{"username": "test-owner", "password": "owner-password-for-validation"}), http.StatusOK)
			if sessionCount(t, a, "owner") != count || owner.cookies[previous.Name].Value == previous.Value {
				t.Fatal("sign-in did not rotate and replace the browser's previous session")
			}
			old := newBrowser(a, false)
			old.cookies[previous.Name] = previous
			checkStatus(t, old.request("GET", "/api/transfers", nil, nil), http.StatusUnauthorized)
		})
	}
}

func TestExistingUnlockAndPasswordlessLinksWorkAtPublicSessionLimit(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	protected := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	open := publish(t, owner, draft(t, owner, 0, "", "open"), "open")
	public := newBrowser(a, true)
	public.page(t, "/s/"+protected.ShareToken)
	unlock := "/s/" + protected.ShareToken + "/unlock"
	checkStatus(t, public.json("POST", unlock, map[string]string{"password": "secret"}), http.StatusOK)
	stored, err := a.store.transfer(context.Background(), protected.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedSessionOccupancy(t, a, "share", maxSessionsPerKind-1, stored)
	checkStatus(t, public.json("POST", unlock, map[string]string{"password": "secret"}), http.StatusOK)
	public.page(t, "/s/"+open.ShareToken)
	checkStatus(t, public.json("POST", "/s/"+open.ShareToken+"/unlock", map[string]string{"password": ""}), http.StatusOK)
	checkStatus(t, public.request("GET", downloadPath(open, 0), nil, nil), http.StatusOK)
	if sessionCount(t, a, "share") != maxSessionsPerKind {
		t.Fatal("session reuse or open link increased public occupancy")
	}
}

func TestConcurrentSessionAllocationKeepsCapacityBound(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "secret", "private"), "private")
	stored, err := a.store.transfer(context.Background(), transfer.ID)
	if err != nil {
		t.Fatal(err)
	}
	seedSessionOccupancy(t, a, "share", maxSessionsPerKind-1, stored)
	var group sync.WaitGroup
	statuses := make(chan int, 2)
	// Exercise the allocation transaction independently of password-check
	// serialization, which now rejects simultaneous checks of one credential.
	for range 2 {
		group.Go(func() {
			request := httptest.NewRequest("POST", a.cfg.PublicURL+"/s/"+transfer.ShareToken+"/unlock", nil)
			response := httptest.NewRecorder()
			err := a.grantSession(response, request, "share", stored, User{})
			status := http.StatusOK
			if err != nil {
				var p *problem
				if errors.As(err, &p) {
					status = p.status
				} else {
					status = http.StatusInternalServerError
				}
			}
			statuses <- status
		})
	}
	group.Wait()
	close(statuses)
	successes, capped := 0, 0
	for status := range statuses {
		switch status {
		case http.StatusOK:
			successes++
		case http.StatusServiceUnavailable:
			capped++
		default:
			t.Fatalf("unexpected allocation status: %d", status)
		}
	}
	if successes != 1 || capped != 1 || sessionCount(t, a, "share") != maxSessionsPerKind {
		t.Fatal("concurrent allocation exceeded the public session limit")
	}
}
