package filemind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func productionPasswordApp(t *testing.T) *App {
	t.Helper()
	a := testApp(t)
	cfg := a.cfg
	a.Close()
	cfg.Development = false
	cfg.OwnerURL, cfg.PublicURL, cfg.AdminURL = "https://owner.example.test", "https://public.example.test", "https://admin.example.test"
	restarted, err := New(cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	return restarted
}

func TestAccountPasswordMinimumCountsUnicodeCharacters(t *testing.T) {
	for _, password := range []string{"", "x", strings.Repeat("a", 14), strings.Repeat("界", 14), strings.Repeat("🔐", 14)} {
		if err := validateAccountPassword(password, false); err == nil {
			t.Fatal("production accepted fewer than 15 Unicode characters")
		}
	}
	for _, password := range []string{strings.Repeat("a", 15), strings.Repeat("界", 15), strings.Repeat("🔐", 15), "a phrase with spaces", strings.Repeat("p", 1024)} {
		if err := validateAccountPassword(password, false); err != nil {
			t.Fatal("valid production passphrase rejected", err)
		}
	}
	if err := validateAccountPassword("x", true); err != nil {
		t.Fatal("explicit development mode rejected its demo password", err)
	}
	if err := validateAccountPassword("", true); err == nil {
		t.Fatal("development mode accepted an empty password")
	}
}

func TestProductionPasswordMinimumAcrossAccountEndpoints(t *testing.T) {
	for _, private := range []bool{false, true} {
		name := "main"
		if private {
			name = "private"
		}
		t.Run(name, func(t *testing.T) {
			a := productionPasswordApp(t)
			admin := newBrowser(a, false)
			admin.admin = private
			loginAs(t, admin, "admin", "owner-password-for-validation")
			administrator := passwordTestUser(t, a, "admin")
			account := addAccount(t, admin, "password-policy", 1024)
			owner := newBrowser(a, false)
			loginAs(t, owner, account.Username, accountTestPassword)
			account = passwordTestUser(t, a, account.Username)
			for _, password := range []string{"", "x", strings.Repeat("a", 14), strings.Repeat("🔐", 14)} {
				created := admin.json("POST", "/admin/api/users", map[string]string{"username": "too-short", "password": password})
				checkStatus(t, created, 400)
				if !strings.Contains(created.Body.String(), "at least 15 characters") {
					t.Fatal("password rejection did not explain the minimum")
				}
				checkStatus(t, admin.json("PATCH", "/admin/api/users/"+account.ID, map[string]string{"password": password}), 400)
				checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": accountTestPassword, "newPassword": password}), 400)
				checkStatus(t, admin.json("PATCH", "/admin/api/users/"+administrator.ID, map[string]string{"currentPassword": "owner-password-for-validation", "password": password}), 400)
				passwordPath := "/api/password"
				if private {
					passwordPath = "/admin/api/password"
				}
				checkStatus(t, admin.json("POST", passwordPath, map[string]string{"currentPassword": "owner-password-for-validation", "newPassword": password}), 400)
			}
			for _, initial := range []User{account, administrator} {
				current := passwordTestUser(t, a, initial.Username)
				if current.AuthVersion != initial.AuthVersion || current.PasswordHash != initial.PasswordHash {
					t.Fatal("rejected password modified credentials or invalidated sessions")
				}
			}
			checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 200)
			checkStatus(t, admin.request("GET", "/admin/api/users", nil, nil), 200)
			var rejected int
			if err := a.store.db.QueryRow("SELECT COUNT(*) FROM users WHERE username='too-short'").Scan(&rejected); err != nil || rejected != 0 {
				t.Fatal("invalid password created an account", err)
			}
			unicodePassword := strings.Repeat("🔐", 15)
			checkStatus(t, admin.json("POST", "/admin/api/users", map[string]string{"username": "unicode-password", "password": unicodePassword}), 201)
			loginAs(t, newBrowser(a, false), "unicode-password", unicodePassword)
			checkStatus(t, admin.json("PATCH", "/admin/api/users/"+account.ID, map[string]string{"password": unicodePassword}), 200)
			checkStatus(t, owner.request("GET", "/api/transfers", nil, nil), 401)
			loginAs(t, owner, account.Username, unicodePassword)
			minimumPassword := strings.Repeat("a", 15)
			checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": unicodePassword, "newPassword": minimumPassword}), 200)
			loginAs(t, owner, account.Username, minimumPassword)
			checkStatus(t, admin.json("PATCH", "/admin/api/users/"+administrator.ID, map[string]string{"currentPassword": "owner-password-for-validation", "password": minimumPassword}), 200)
			loginAs(t, admin, "admin", minimumPassword)
			passwordPath := "/api/password"
			settingsPath := "/settings"
			if private {
				passwordPath, settingsPath = "/admin/api/password", "/admin/preferences"
			}
			if !strings.Contains(admin.page(t, settingsPath).Body.String(), "Use at least 15 characters.") || !strings.Contains(admin.page(t, "/admin/users").Body.String(), "Use at least 15 characters when setting a password.") {
				t.Fatal("account password forms do not explain the production minimum")
			}
			longPassword := strings.Repeat("p", 1024)
			checkStatus(t, admin.json("POST", passwordPath, map[string]string{"currentPassword": minimumPassword, "newPassword": longPassword}), 200)
			loginAs(t, admin, "admin", longPassword)
			checkStatus(t, admin.json("PATCH", "/admin/api/users/"+account.ID, map[string]int64{"storageQuota": 512}), 200)
		})
	}
}

func TestProductionBootstrapPasswordMinimum(t *testing.T) {
	for _, existing := range []bool{false, true} {
		name := "fresh"
		if existing {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name, password string
				valid          bool
			}{
				{"empty", "", false},
				{"single", "x", false},
				{"below-minimum", strings.Repeat("a", 14), false},
				{"multibyte-below-minimum", strings.Repeat("🔐", 14), false},
				{"minimum", strings.Repeat("a", 15), true},
				{"unicode-minimum", strings.Repeat("🔐", 15), true},
				{"long", strings.Repeat("p", 1024), true},
			} {
				t.Run(tc.name, func(t *testing.T) {
					a := testApp(t)
					original := passwordTestUser(t, a, "admin")
					cfg := a.cfg
					a.Close()
					cfg.Development = false
					cfg.OwnerURL, cfg.PublicURL, cfg.AdminURL = "https://owner.example.test", "https://public.example.test", "https://admin.example.test"
					if !existing {
						cfg.DataDir = filepath.Join(t.TempDir(), "fresh-data")
					}
					if err := os.WriteFile(cfg.OwnerPasswordFile, []byte(tc.password+"\n"), 0600); err != nil {
						t.Fatal(err)
					}
					restarted, err := New(cfg, a.logger)
					if restarted != nil {
						t.Cleanup(restarted.Close)
					}
					if tc.valid {
						if err != nil {
							t.Fatal("valid bootstrap password rejected", err)
						}
						admin := newBrowser(restarted, false)
						admin.admin = true
						loginAs(t, admin, "admin", tc.password)
						return
					}
					if err == nil || !strings.Contains(err.Error(), "at least 15 characters") {
						t.Fatal("short bootstrap password did not prevent startup")
					}
					if !existing {
						if _, err := os.Stat(cfg.DataDir); !errors.Is(err, os.ErrNotExist) {
							t.Fatal("invalid bootstrap password initialized application data", err)
						}
						return
					}
					if err := os.WriteFile(cfg.OwnerPasswordFile, []byte("owner-password-for-validation\n"), 0600); err != nil {
						t.Fatal(err)
					}
					restored, err := New(cfg, a.logger)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(restored.Close)
					current := passwordTestUser(t, restored, "admin")
					if current.AuthVersion != original.AuthVersion || current.PasswordHash != original.PasswordHash {
						t.Fatal("rejected bootstrap reset altered stored credentials")
					}
				})
			}
		})
	}
}

func TestExistingAccountCanReplaceShortPasswordInProduction(t *testing.T) {
	a := productionPasswordApp(t)
	admin := adminBrowser(t, a)
	account := addAccount(t, admin, "existing-password", 1024)
	shortHash, err := hashPassword("x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = a.store.db.ExecContext(context.Background(), "UPDATE users SET password_hash=? WHERE id=?", shortHash, account.ID); err != nil {
		t.Fatal(err)
	}
	owner := newBrowser(a, false)
	loginAs(t, owner, account.Username, "x")
	checkStatus(t, owner.json("POST", "/api/password", map[string]string{"currentPassword": "x", "newPassword": "long replacement passphrase"}), 200)
	loginAs(t, owner, account.Username, "long replacement passphrase")
}
