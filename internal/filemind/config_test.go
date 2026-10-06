package filemind

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolateFilemindEnvironment(t *testing.T) {
	t.Helper()
	// Isolate deployment parsing from the developer's environment.
	for _, entry := range os.Environ() {
		name, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(name, "FILEMIND_") {
			t.Setenv(name, "")
			if err := os.Unsetenv(name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestPrivateAdminSignInEnvironment(t *testing.T) {
	isolateFilemindEnvironment(t)
	t.Setenv("FILEMIND_OWNER_URL", "https://owner.example")
	t.Setenv("FILEMIND_PUBLIC_URL", "https://files.example")
	t.Setenv("FILEMIND_ADMIN_URL", "https://admin.example")
	t.Setenv("FILEMIND_OWNER_PASSWORD_FILE", filepath.Join(t.TempDir(), "password"))
	const key = "FILEMIND_REQUIRE_PRIVATE_ADMIN_SIGN_IN"
	for _, test := range []struct {
		name, value string
		want        bool
		invalid     bool
	}{
		{name: "unset", want: true},
		{name: "false", value: "false"},
		{name: "true", value: "true", want: true},
		{name: "invalid", value: "private", invalid: true},
		{name: "empty", invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if test.name != "unset" {
				t.Setenv(key, test.value)
			}
			cfg, err := ConfigFromEnv()
			if test.invalid {
				if err == nil || err.Error() != "invalid "+key {
					t.Fatalf("invalid deployment policy was accepted: %v", err)
				}
				return
			}
			if err != nil || cfg.RequirePrivateAdminSignIn != test.want {
				t.Fatalf("private-only setting = %v, error = %v", cfg.RequirePrivateAdminSignIn, err)
			}
		})
	}
}

func TestEnvironmentDefaultKeepsAdministrationOnPrivateListener(t *testing.T) {
	isolateFilemindEnvironment(t)
	a := productionPasswordApp(t)
	ordinary := newBrowser(a, false)
	ordinary.login(t)
	mainAdmin := newBrowser(a, false)
	loginAs(t, mainAdmin, "admin", "owner-password-for-validation")
	privateAdmin := adminBrowser(t, a)
	base := a.cfg
	a.Close()
	t.Setenv("FILEMIND_DATA_DIR", base.DataDir)
	t.Setenv("FILEMIND_OWNER_PASSWORD_FILE", base.OwnerPasswordFile)
	t.Setenv("FILEMIND_OWNER_URL", base.OwnerURL)
	t.Setenv("FILEMIND_PUBLIC_URL", base.PublicURL)
	t.Setenv("FILEMIND_ADMIN_URL", base.AdminURL)
	t.Setenv("FILEMIND_MIN_FREE_SPACE", "0")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := New(cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	ordinary.app, mainAdmin.app, privateAdmin.app = restarted, restarted, restarted
	checkStatus(t, mainAdmin.request("GET", "/api/transfers", nil, nil), 401)
	checkStatus(t, ordinary.request("GET", "/api/transfers", nil, nil), 200)
	checkStatus(t, privateAdmin.request("GET", "/admin/api/users", nil, nil), 200)
	for _, path := range []string{"/admin/users", "/admin/transfers", "/admin/settings", "/admin/api/users", "/admin/api/transfers", "/admin/api/settings"} {
		checkStatus(t, mainAdmin.request("GET", path, nil, nil), 404)
	}
	fresh := newBrowser(restarted, false)
	fresh.page(t, "/login")
	checkStatus(t, fresh.json("POST", "/login", map[string]string{"username": "admin", "password": "owner-password-for-validation"}), 401)
	loginAs(t, newBrowser(restarted, false), "test-owner", "owner-password-for-validation")
}
