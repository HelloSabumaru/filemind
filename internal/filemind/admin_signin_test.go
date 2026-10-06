package filemind

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAdminMainSignInIsDefaultAndPersists(t *testing.T) {
	a := testApp(t)
	if a.cfg.RequirePrivateAdminSignIn {
		t.Fatal("private-only sign-in should be optional")
	}
	owner := newBrowser(a, false)
	loginAs(t, owner, "admin", "owner-password-for-validation")
	page := owner.request("GET", "/upload", nil, nil)
	checkStatus(t, page, 200)
	if !strings.Contains(page.Body.String(), `href="/admin/users"`) {
		t.Fatal("main navigation should keep Admin on the same listener")
	}
	token := owner.cookies[a.cookieName(sessionCookie("owner"))].Value
	for _, path := range []string{"/admin/users", "/admin/settings", "/admin/transfers", "/upload", "/transfers", "/settings"} {
		checkStatus(t, owner.request("GET", path, nil, nil), 200)
	}
	if owner.cookies[a.cookieName(sessionCookie("owner"))].Value != token || sessionCount(t, a, "admin") != 0 {
		t.Fatal("switching to Admin should reuse the existing session")
	}
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, owner.request("GET", "/admin/api/users", nil, nil), 200)
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	owner.app = restarted
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, owner.request("GET", "/admin/api/users", nil, nil), 200)
}

func TestMainAdminRoutesEnforceRoleOriginAndSession(t *testing.T) {
	a := testApp(t)
	admin := newBrowser(a, false)
	loginAs(t, admin, "admin", "owner-password-for-validation")
	admin.page(t, "/admin/users")
	user := addAccount(t, admin, "main-admin-user", 1024)
	ordinary := newBrowser(a, false)
	loginAs(t, ordinary, user.Username, accountTestPassword)
	transfer := publish(t, ordinary, draft(t, ordinary, 0, "", "regular file"), "regular file")
	checkStatus(t, admin.request("GET", "/api/transfers/"+transfer.ID, nil, nil), 404)
	checkStatus(t, admin.request("GET", "/admin/api/transfers/"+transfer.ID, nil, nil), 200)
	all := admin.request("GET", "/admin/api/transfers", nil, nil)
	checkStatus(t, all, 200)
	if !strings.Contains(all.Body.String(), `"ownerId":"`+user.ID+`"`) {
		t.Fatal("all transfers should include the owner UUID on the main listener")
	}
	for _, path := range []string{"/admin/users", "/admin/settings", "/admin/transfers", "/admin/api/users", "/admin/api/settings", "/admin/api/transfers", "/admin/api/transfers/" + transfer.ID} {
		checkStatus(t, ordinary.request("GET", path, nil, nil), 404)
		checkStatus(t, newBrowser(a, true).request("GET", path, nil, nil), 404)
	}
	for _, operation := range []struct{ method, path string }{
		{"POST", "/admin/api/users"},
		{"PATCH", "/admin/api/users/" + user.ID},
		{"DELETE", "/admin/api/users/" + user.ID},
		{"PUT", "/admin/api/settings"},
		{"PATCH", "/admin/api/transfers/" + transfer.ID},
		{"POST", "/admin/api/transfers/" + transfer.ID + "/revoke"},
		{"DELETE", "/admin/api/transfers/" + transfer.ID},
	} {
		checkStatus(t, ordinary.json(operation.method, operation.path, map[string]any{}), 404)
	}
	private := adminBrowser(t, a)
	privateOnlyCookie := newBrowser(a, false)
	privateOnlyCookie.cookies = private.cookies
	checkStatus(t, privateOnlyCookie.request("GET", "/admin/api/users", nil, nil), 401)
	mainOnlyCookie := newBrowser(a, false)
	mainOnlyCookie.admin = true
	mainOnlyCookie.cookies = admin.cookies
	checkStatus(t, mainOnlyCookie.request("GET", "/admin/api/users", nil, nil), 401)

	mainCSRF := admin.csrf
	admin.csrf = private.csrf
	checkStatus(t, admin.json("PUT", "/admin/api/settings", a.settings()), 403)
	admin.csrf = mainCSRF
	data, _ := json.Marshal(a.settings())
	checkStatus(t, admin.request("PUT", "/admin/api/settings", strings.NewReader(string(data)), map[string]string{"Content-Type": "application/json", "Origin": a.cfg.AdminURL}), 403)
	checkStatus(t, admin.json("PUT", "/admin/api/settings", a.settings()), 200)
	checkStatus(t, admin.json("PATCH", "/admin/api/users/"+passwordTestUser(t, a, "admin").ID, map[string]string{"password": "replacement-password"}), 403)
	checkStatus(t, admin.json("POST", "/admin/api/transfers/"+transfer.ID+"/revoke", nil), 200)
	checkStatus(t, newBrowser(a, true).request("GET", downloadPath(transfer, 0), nil, nil), 404)
	checkStatus(t, admin.json("POST", "/logout", nil), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 401)
	checkStatus(t, admin.request("GET", "/api/transfers", nil, nil), 401)
}

func restartWithAdminSignInPolicy(t *testing.T, a *App, privateOnly bool) *App {
	t.Helper()
	cfg := a.cfg
	cfg.RequirePrivateAdminSignIn = privateOnly
	a.Close()
	restarted, err := New(cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	return restarted
}

func TestPrivateAdminSignInDeploymentRetiresOnlyMainAdminSessions(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	ownerAdmin := newBrowser(a, false)
	loginAs(t, ownerAdmin, "admin", "owner-password-for-validation")
	ordinary := newBrowser(a, false)
	ordinary.login(t)
	u := passwordTestUser(t, a, "admin")
	a = restartWithAdminSignInPolicy(t, a, true)
	admin.app, ownerAdmin.app, ordinary.app = a, a, a
	checkStatus(t, ownerAdmin.request("GET", "/api/transfers", nil, nil), 401)
	checkStatus(t, ordinary.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)
	for _, path := range []string{"/admin/users", "/admin/settings", "/admin/transfers", "/admin/api/users", "/admin/api/settings", "/admin/api/transfers"} {
		checkStatus(t, ownerAdmin.request("GET", path, nil, nil), 404)
		checkStatus(t, ordinary.request("GET", path, nil, nil), 404)
	}
	if sessionCount(t, a, "owner") != 1 {
		t.Fatal("private-only mode must retire only administrator main sessions")
	}
	request := httptest.NewRequest("POST", a.cfg.OwnerURL+"/login", nil)
	err := a.grantSession(httptest.NewRecorder(), request, "owner", Transfer{}, u)
	var denied *problem
	if !errors.As(err, &denied) || denied.status != 401 {
		t.Fatalf("private-only deployment allocated a main administrator session: %v", err)
	}
	fresh := newBrowser(a, false)
	fresh.page(t, "/login")
	checkStatus(t, fresh.json("POST", "/login", map[string]string{"username": "admin", "password": "owner-password-for-validation"}), 401)
	settings := admin.request("GET", "/admin/api/settings", nil, nil)
	checkStatus(t, settings, 200)
	if strings.Contains(settings.Body.String(), "requirePrivateAdminSignIn") {
		t.Fatal("deployment policy should not be an editable server setting")
	}
	var input map[string]any
	data, _ := json.Marshal(a.settings())
	if err := json.Unmarshal(data, &input); err != nil {
		t.Fatal(err)
	}
	input["requirePrivateAdminSignIn"] = false
	checkStatus(t, admin.json("PUT", "/admin/api/settings", input), 400)
	checkStatus(t, fresh.json("POST", "/login", map[string]string{"username": "admin", "password": "owner-password-for-validation"}), 401)

	a = restartWithAdminSignInPolicy(t, a, true)
	admin.app, ordinary.app = a, a
	checkStatus(t, ordinary.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)
	a = restartWithAdminSignInPolicy(t, a, false)
	ownerAdmin.app = a
	checkStatus(t, ownerAdmin.request("GET", "/api/transfers", nil, nil), 401)
	loginAs(t, ownerAdmin, "admin", "owner-password-for-validation")
	checkStatus(t, ownerAdmin.request("GET", "/api/transfers", nil, nil), 200)
}

func TestAdminPasswordThrottleIsSharedBetweenSignInListeners(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.page(t, "/login")
	private := newBrowser(a, false)
	private.admin = true
	private.page(t, "/login")
	for i, client := range []*browser{owner, private, owner} {
		client.remoteAddr = []string{"192.0.2.1:1234", "192.0.2.2:1234", "192.0.2.3:1234"}[i]
		checkStatus(t, client.json("POST", "/login", map[string]string{"username": "admin", "password": "wrong"}), 401)
	}
	for _, client := range []*browser{owner, private} {
		response := client.json("POST", "/login", map[string]string{"username": "admin", "password": "owner-password-for-validation"})
		checkStatus(t, response, 429)
		if response.Header().Get("Retry-After") == "" {
			t.Fatal("shared account throttle should report a retry delay")
		}
	}
}
