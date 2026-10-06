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
	if !strings.Contains(page.Body.String(), `href="`+a.cfg.AdminURL+`/admin/users"`) {
		t.Fatal("main navigation does not link to the admin listener")
	}
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, owner.request("GET", "/admin/api/users", nil, nil), 404)
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	owner.app = restarted
	checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
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
