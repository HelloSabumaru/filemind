package filemind

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
)

func setupAdmin(t *testing.T, a *App, username, password string) *browser {
	t.Helper()
	b := newBrowser(a, false)
	b.admin = true
	b.page(t, "/setup")
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": username, "password": password}), 200)
	b.page(t, "/admin/users")
	return b
}

func TestInitialSetupIsAdminOnlyAndCreatesOneAccount(t *testing.T) {
	a := uninitializedTestApp(t)
	if pending, err := a.setupPending(context.Background()); err != nil || !pending {
		t.Fatal("fresh installation did not await setup", err)
	}
	for _, public := range []bool{false, true} {
		b := newBrowser(a, public)
		checkStatus(t, b.request("GET", "/setup", nil, nil), 404)
		checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "claimed", "password": "password"}), 404)
		checkStatus(t, b.request("GET", "/healthz", nil, nil), 200)
	}
	b := newBrowser(a, false)
	b.admin = true
	response := b.request("GET", "/login?theme=dark", nil, nil)
	checkStatus(t, response, http.StatusSeeOther)
	if response.Header().Get("Location") != "/setup?theme=dark" {
		t.Fatal("first admin sign-in did not lead to setup")
	}
	page := b.page(t, "/setup").Body.String()
	if !strings.Contains(page, "Create administrator") || strings.Contains(page, `id="logout"`) {
		t.Fatal("setup form did not present initial account creation")
	}
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "", "password": "password"}), 400)
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "name with spaces", "password": "password"}), 400)
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "first-admin", "password": ""}), 400)
	csrf := b.csrf
	b.csrf = "invalid"
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "first-admin", "password": "password"}), 403)
	b.csrf = csrf
	checkStatus(t, b.request("POST", "/setup", strings.NewReader(`{"username":"claimed","password":"password"}`), map[string]string{"Content-Type": "application/json", "Origin": a.cfg.OwnerURL}), 403)
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "first-admin", "password": "password"}), 200)
	b.page(t, "/admin/users")
	user := passwordTestUser(t, a, "first-admin")
	if !user.IsAdmin || !validUserID(user.ID) || user.StorageQuota != a.settings().DefaultUserQuota || !verifyPassword(user.PasswordHash, "password") || user.PasswordHash == "password" {
		t.Fatal("initial account did not have administrator credentials and defaults")
	}
	var count int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM users").Scan(&count); err != nil || count != 1 {
		t.Fatal("setup created additional accounts", err)
	}
	if pending, err := a.setupPending(context.Background()); err != nil || pending {
		t.Fatal("setup remained open after account creation", err)
	}
	checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "second-admin", "password": "password"}), 404)
	response = b.request("GET", "/setup", nil, nil)
	checkStatus(t, response, http.StatusSeeOther)
	if response.Header().Get("Location") != "/login" {
		t.Fatal("completed setup did not return to sign-in")
	}
	created := addAccount(t, b, "ordinary-user", 1024)
	if created.IsAdmin {
		t.Fatal("Users promoted a later account to administrator")
	}
	loginAs(t, newBrowser(a, false), created.Username, accountTestPassword)
}

func TestProductionInitialSetupPasswordMinimum(t *testing.T) {
	for _, tc := range []struct {
		name, password string
		valid          bool
	}{
		{"empty", "", false},
		{"single", "x", false},
		{"below-minimum", strings.Repeat("a", 14), false},
		{"unicode-below-minimum", strings.Repeat("🔐", 14), false},
		{"minimum", strings.Repeat("a", 15), true},
		{"unicode-minimum", strings.Repeat("🔐", 15), true},
		{"long", strings.Repeat("p", 1024), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := uninitializedTestApp(t)
			cfg := a.cfg
			a.Close()
			cfg.Development = false
			cfg.OwnerURL, cfg.PublicURL, cfg.AdminURL = "https://owner.example.test", "https://files.example.test", "https://admin.example.test"
			a, err := New(cfg, a.logger)
			if err != nil {
				t.Fatal("production startup required preexisting credentials", err)
			}
			t.Cleanup(a.Close)
			b := newBrowser(a, false)
			b.admin = true
			if !strings.Contains(b.page(t, "/setup").Body.String(), "Use at least 15 characters.") {
				t.Fatal("setup did not explain the production password minimum")
			}
			want := 400
			if tc.valid {
				want = 200
			}
			checkStatus(t, b.json("POST", "/setup", map[string]string{"username": "admin", "password": tc.password}), want)
			if pending, err := a.setupPending(context.Background()); err != nil || pending == tc.valid {
				t.Fatal("password validation incorrectly changed setup state", err)
			}
			if tc.valid {
				b.page(t, "/admin/users")
				checkStatus(t, b.json("POST", "/logout", nil), 200)
				loginAs(t, b, "admin", tc.password)
			}
		})
	}
}

func TestSetupCompletionAndChangedCredentialsPersistAcrossRestart(t *testing.T) {
	a := uninitializedTestApp(t)
	admin := setupAdmin(t, a, "chosen-admin", "initial-password")
	u := passwordTestUser(t, a, "chosen-admin")
	checkStatus(t, admin.json("POST", "/admin/api/password", map[string]string{"currentPassword": "initial-password", "newPassword": "changed-password"}), 200)
	current := passwordTestUser(t, a, "chosen-admin")
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	if pending, err := restarted.setupPending(context.Background()); err != nil || pending {
		t.Fatal("restart reopened setup", err)
	}
	stored := passwordTestUser(t, restarted, "chosen-admin")
	if stored.ID != u.ID || stored.PasswordHash != current.PasswordHash || stored.AuthVersion != current.AuthVersion {
		t.Fatal("restart reset the administrator's identity or credentials")
	}
	admin.app = restarted
	loginAs(t, admin, "chosen-admin", "changed-password")
	checkStatus(t, admin.json("POST", "/setup", map[string]string{"username": "replacement", "password": "password"}), 404)
	// Account loss must never silently reopen initial administrator registration.
	if _, err := restarted.store.db.Exec("DELETE FROM sessions; DELETE FROM users"); err != nil {
		t.Fatal(err)
	}
	if pending, err := restarted.setupPending(context.Background()); err != nil || pending {
		t.Fatal("losing account rows reopened completed setup", err)
	}
	restarted.Close()
	closed, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closed.Close)
	if pending, err := closed.setupPending(context.Background()); err != nil || pending {
		t.Fatal("restart after account loss reopened completed setup", err)
	}
	if err := closed.checkPersistence(context.Background()); err == nil {
		t.Fatal("missing administrator after setup was reported as healthy")
	}
}

func TestConcurrentInitialAdminClaimsAndRollback(t *testing.T) {
	a := uninitializedTestApp(t)
	hash, err := hashPassword("setup-password-for-validation")
	if err != nil {
		t.Fatal(err)
	}
	// Failing the completion write must also roll back the account insert.
	if _, err := a.store.db.Exec(`CREATE TRIGGER fail_setup BEFORE INSERT ON settings
 WHEN NEW.key='setup_complete' BEGIN SELECT RAISE(ABORT,'fixture setup failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.createInitialAdmin(context.Background(), "failed-admin", hash); err == nil {
		t.Fatal("fixture setup failure did not abort the claim")
	}
	if pending, err := a.setupPending(context.Background()); err != nil || !pending {
		t.Fatal("failed setup left an account or completion marker", err)
	}
	if _, err := a.store.db.Exec("DROP TRIGGER fail_setup"); err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan error, 2)
	start := make(chan struct{})
	for _, username := range []string{"first-claim", "second-claim"} {
		group.Go(func() {
			<-start
			_, err := a.createInitialAdmin(context.Background(), username, hash)
			results <- err
		})
	}
	close(start)
	group.Wait()
	close(results)
	successes := 0
	for err := range results {
		if err == nil {
			successes++
			continue
		}
		var rejected *problem
		if !errors.As(err, &rejected) || rejected.status != 409 {
			t.Fatal("concurrent setup did not reject the losing claim", err)
		}
	}
	var count int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM users WHERE is_admin=1").Scan(&count); err != nil || count != 1 || successes != 1 {
		t.Fatal("simultaneous setup created more than one administrator", err)
	}
}
