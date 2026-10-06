package filemind

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPrivateAdminSignInEnvironment(t *testing.T) {
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
		{name: "unset"},
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
