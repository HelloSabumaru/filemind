package filemind

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

func passwordTestClock(l *passwordLimiter) func(time.Duration) {
	now := time.Now()
	l.now = func() time.Time { return now }
	return func(delay time.Duration) { now = now.Add(delay) }
}

func passwordTestUser(t *testing.T, a *App, name string) User {
	t.Helper()
	u, err := scanUser(a.store.db.QueryRow("SELECT "+userColumns+" FROM users u WHERE username=?", name))
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func TestPasswordCooldownProgressionAndRecovery(t *testing.T) {
	l := newPasswordLimiter()
	advance := passwordTestClock(l)
	for _, delay := range []time.Duration{0, 0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		check, retry := l.begin("account")
		if check == nil || retry != 0 {
			t.Fatal("check did not resume after cooldown")
		}
		check.finish(false)
		if delay > 0 {
			if blocked, retry := l.begin("account"); blocked != nil || retry < int(delay.Seconds()) || retry > int(delay.Seconds())+1 {
				t.Fatal("unexpected cooldown")
			}
		}
		other, _ := l.begin("other")
		if other == nil {
			t.Fatal("unrelated account was throttled")
		}
		other.finish(true)
		advance(delay)
	}
	good, _ := l.begin("account")
	good.finish(true)
	for range 2 {
		check, retry := l.begin("account")
		if check == nil || retry != 0 {
			t.Fatal("success did not clear failures")
		}
		check.finish(false)
	}
	advance(passwordFailureWindow)
	check, retry := l.begin("account")
	if check == nil || retry != 0 {
		t.Fatal("idle account remained locked")
	}
	check.finish(false)
	if next, _ := l.begin("account"); next == nil {
		t.Fatal("idle failures did not expire")
	} else {
		next.cancel()
	}
}

func TestPasswordChecksSerializeAndCredentialResetRetiresOldWork(t *testing.T) {
	l := newPasswordLimiter()
	first, _ := l.begin("account")
	if second, retry := l.begin("account"); second != nil || retry != 1 {
		t.Fatal("concurrent credential verification admitted")
	}
	first.cancel()
	old, _ := l.begin("account")
	l.reset("account")
	fresh, _ := l.begin("account")
	if fresh == nil {
		t.Fatal("new credential blocked by old work")
	}
	old.finish(false)
	fresh.finish(false)
	second, _ := l.begin("account")
	second.finish(false)
	third, _ := l.begin("account")
	if third == nil {
		t.Fatal("old credential failure affected new credential")
	}
	third.cancel()
}

func TestAccountThrottlingCombinesAddressesAndPasswordEntryPoints(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	u := passwordTestUser(t, a, "admin")
	advance := passwordTestClock(a.accountPasswords)
	admin.remoteAddr = "192.0.2.1:1000"
	checkStatus(t, admin.json("PATCH", "/admin/api/users/"+u.ID, map[string]string{"password": "replacement"}), 403)
	admin.remoteAddr = "192.0.2.2:1000"
	checkStatus(t, admin.json("POST", "/admin/api/password", map[string]string{"currentPassword": "wrong", "newPassword": "replacement"}), 403)
	third := newBrowser(a, false)
	third.admin, third.remoteAddr = true, "192.0.2.3:1000"
	third.page(t, "/login")
	checkStatus(t, third.json("POST", "/login", map[string]string{"username": "AdMiN", "password": "wrong"}), 401)
	admin.remoteAddr = "192.0.2.4:1000"
	response := admin.json("PATCH", "/admin/api/users/"+u.ID, map[string]string{"password": "replacement", "currentPassword": "owner-password-for-validation"})
	checkStatus(t, response, 429)
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("missing cooldown retry delay")
	}
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)
	checkStatus(t, admin.request("GET", "/admin/upload", nil, nil), 200)
	if stored := passwordTestUser(t, a, "admin"); stored.AuthVersion != u.AuthVersion || stored.PasswordHash != u.PasswordHash {
		t.Fatal("unverified password change modified credentials")
	}
	owner := newBrowser(a, false)
	owner.login(t)
	advance(time.Second)
	checkStatus(t, admin.json("PATCH", "/admin/api/users/"+u.ID, map[string]string{"password": "replacement", "currentPassword": "owner-password-for-validation"}), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 401)
	loginAs(t, third, "admin", "replacement")
	checkStatus(t, third.json("POST", "/admin/api/password", map[string]string{"currentPassword": "wrong", "newPassword": "another"}), 403)
	checkStatus(t, third.json("POST", "/admin/api/password", map[string]string{"currentPassword": "replacement", "newPassword": "another"}), 200)
	checkStatus(t, third.request("GET", "/admin/api/users", nil, nil), 401)
}

func TestTransferThrottlingCombinesSourcesAndPreservesOtherLinks(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	protected := publish(t, owner, draft(t, owner, 0, "secret", "protected"), "protected")
	open := publish(t, owner, draft(t, owner, 0, "", "open"), "open")
	authorized := newBrowser(a, true)
	authorized.page(t, "/s/"+protected.ShareToken)
	checkStatus(t, authorized.json("POST", "/s/"+protected.ShareToken+"/unlock", map[string]string{"password": "secret"}), 200)
	for index := range 3 {
		client := newBrowser(a, true)
		client.remoteAddr = "192.0.2." + strconv.Itoa(index+1) + ":1000"
		client.page(t, "/s/"+protected.ShareToken)
		checkStatus(t, client.json("POST", "/s/"+protected.ShareToken+"/unlock", map[string]string{"password": "wrong"}), 403)
	}
	client := newBrowser(a, true)
	client.remoteAddr = "192.0.2.4:1000"
	client.page(t, "/s/"+protected.ShareToken)
	checkStatus(t, client.json("POST", "/s/"+protected.ShareToken+"/unlock", map[string]string{"password": "secret"}), 429)
	checkStatus(t, authorized.request("GET", downloadPath(protected, 0), nil, nil), 200)
	checkStatus(t, client.request("GET", downloadPath(open, 0), nil, nil), 200)
	checkStatus(t, owner.json("PATCH", "/api/transfers/"+protected.ID, map[string]string{"password": "replacement"}), 200)
	checkStatus(t, client.json("POST", "/s/"+protected.ShareToken+"/unlock", map[string]string{"password": "replacement"}), 200)
}

func TestUnknownUsernamesHaveGenericResponsesAndSeparateBoundedCapacity(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	for _, name := range []string{"test-owner", "does-not-exist"} {
		client := newBrowser(a, false)
		client.page(t, "/login")
		for range 3 {
			response := client.json("POST", "/login", map[string]string{"username": name, "password": "wrong"})
			checkStatus(t, response, 401)
			if !strings.Contains(response.Body.String(), "Incorrect username or password.") {
				t.Fatal("account existence disclosed")
			}
		}
		response := client.json("POST", "/login", map[string]string{"username": strings.ToUpper(name), "password": "wrong"})
		checkStatus(t, response, 429)
		if response.Header().Get("Retry-After") == "" {
			t.Fatal("missing retry delay")
		}
	}
	a.accountPasswords.reset(passwordTestUser(t, a, "test-owner").ID)
	// Fill only bounded in-memory fixture state, without generating traffic.
	a.unknownPasswords.targets = make(map[string]*passwordFailure)
	for index := range maxPasswordTargets {
		a.unknownPasswords.targets[strconv.Itoa(index)] = &passwordFailure{seen: time.Now()}
	}
	client := newBrowser(a, false)
	client.page(t, "/login")
	checkStatus(t, client.json("POST", "/login", map[string]string{"username": "another-unknown", "password": "wrong"}), 429)
	loginAs(t, client, "test-owner", "owner-password-for-validation")
	if len(a.unknownPasswords.targets) != maxPasswordTargets {
		t.Fatal("unknown username pool exceeded bounds")
	}
}

func TestPasswordIPLimitDoesNotBlockExistingSessionsOrOtherSurfaces(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	a.ownerLimiter.mu.Lock()
	v := a.ownerLimiter.visitors["127.0.0.1"]
	v.passwordAttempts = make([]time.Time, 60)
	for index := range v.passwordAttempts {
		v.passwordAttempts[index] = time.Now()
	}
	a.ownerLimiter.mu.Unlock()
	client := newBrowser(a, false)
	client.page(t, "/login")
	checkStatus(t, client.json("POST", "/login", map[string]string{"username": "test-owner", "password": "owner-password-for-validation"}), 429)
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)), 200)
	adminBrowser(t, a)
}

func TestAdminCredentialsAndLegacyOwnerSessionsArePrivate(t *testing.T) {
	a := testApp(t)
	if verifyPassword(a.loginDummyHash, "owner-password-for-validation") {
		t.Fatal("public dummy verification uses the administrator's credential")
	}
	owner := newBrowser(a, false)
	owner.page(t, "/login")
	for _, password := range []string{"wrong", "owner-password-for-validation", "wrong"} {
		checkStatus(t, owner.json("POST", "/login", map[string]string{"username": "admin", "password": password}), 401)
	}
	if sessionCount(t, a, "owner") != 0 || len(a.accountPasswords.targets) != 0 {
		t.Fatal("public listener authorized or throttled the real admin credential")
	}
	adminBrowser(t, a)
	u := passwordTestUser(t, a, "admin")
	token := randomToken()
	if _, err := a.store.db.Exec("INSERT INTO sessions(token_hash,kind,user_id,auth_version,expires_at) VALUES(?,'owner',?,?,?)", tokenHash(token), u.ID, u.AuthVersion, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	owner.cookies[a.cookieName(sessionCookie("owner"))] = &http.Cookie{Name: a.cookieName(sessionCookie("owner")), Value: token, Path: "/"}
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 401)
	checkStatus(t, owner.request("GET", "/upload", nil, nil), 303)
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if sessionCount(t, restarted, "owner") != 0 {
		t.Fatal("obsolete administrator owner sessions were not retired")
	}
}

func TestAdminPersonalUploadAndOwnershipStayOnPrivateListener(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	owner := newBrowser(a, false)
	owner.login(t)
	other := publish(t, owner, draft(t, owner, 0, "", "ordinary"), "ordinary")
	checkStatus(t, admin.request("GET", "/admin/api/personal/transfers/"+other.ID, nil, nil), 404)
	checkStatus(t, admin.json("DELETE", "/admin/api/personal/transfers/"+other.ID, nil), 404)
	payload := strings.Repeat("a", 128*1024+13)
	transfer := draft(t, admin, 0, "", payload)
	path := startFile(t, admin, transfer.Files[0])
	patchFile(t, admin, path, 0, payload[:32768])
	checkStatus(t, admin.request("HEAD", path, nil, map[string]string{"Tus-Resumable": "1.0.0"}), 200)
	patchFile(t, admin, path, 32768, payload[32768:])
	shared := readTransfer(t, admin.json("POST", "/admin/api/personal/transfers/"+transfer.ID+"/publish", nil))
	shared.ShareToken = strings.TrimPrefix(shared.ShareURL, a.cfg.PublicURL+"/s/")
	response := newBrowser(a, true).request("GET", downloadPath(shared, 0), nil, nil)
	checkStatus(t, response, 200)
	if response.Body.String() != payload {
		t.Fatal("private admin upload changed file contents")
	}
	for path, count := range map[string]int{"/admin/api/personal/transfers": 1, "/admin/api/transfers": 2} {
		response := admin.request("GET", path, nil, nil)
		checkStatus(t, response, 200)
		var transfers []Transfer
		if err := json.Unmarshal(response.Body.Bytes(), &transfers); err != nil || len(transfers) != count {
			t.Fatal("personal and all-transfer scopes overlap", err)
		}
	}
	if stored, err := a.store.transfer(context.Background(), other.ID); err != nil || stored.Status != "published" {
		t.Fatal("admin personal action changed another account's transfer", err)
	}
}
