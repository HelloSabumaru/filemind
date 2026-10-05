package filemind

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestUserDatePreferences(t *testing.T) {
	a := testApp(t)
	admin, owner := adminBrowser(t, a), newBrowser(a, false)
	owner.login(t)
	check := func(b *browser, path, expected string) {
		t.Helper()
		response := b.request("GET", path, nil, nil)
		checkStatus(t, response, 200)
		var preferences UserPreferences
		if err := json.Unmarshal(response.Body.Bytes(), &preferences); err != nil {
			t.Fatal(err)
		}
		if preferences.DateFormat != expected {
			t.Fatalf("date format = %q, want %q", preferences.DateFormat, expected)
		}
	}
	check(owner, "/api/preferences", "dd/mm/yyyy")
	checkStatus(t, owner.json("PUT", "/api/preferences", UserPreferences{DateFormat: "invalid"}), 400)
	checkStatus(t, owner.json("PUT", "/api/preferences", UserPreferences{DateFormat: "yyyy-mm-dd"}), 200)
	check(admin, "/admin/api/preferences", "yyyy-mm-dd")
	user := addAccount(t, admin, "date-user", 20)
	other := newBrowser(a, false)
	loginAs(t, other, user.Username, accountTestPassword)
	check(other, "/api/preferences", "dd/mm/yyyy")
	checkStatus(t, other.json("PUT", "/api/preferences", UserPreferences{DateFormat: "mm/dd/yyyy"}), 200)
	check(owner, "/api/preferences", "yyyy-mm-dd")
	a.Close()
	restarted, err := New(a.cfg, a.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restarted.Close)
	owner.app, admin.app, other.app = restarted, restarted, restarted
	check(owner, "/api/preferences", "yyyy-mm-dd")
	check(other, "/api/preferences", "mm/dd/yyyy")
	if html := owner.page(t, "/settings").Body.String(); !strings.Contains(html, `data-date-format="yyyy-mm-dd"`) {
		t.Fatal("saved preference missing from rendered settings")
	}
	checkStatus(t, admin.json("DELETE", "/admin/api/users/"+user.ID, nil), 200)
	var count int
	if err := restarted.store.db.QueryRow("SELECT count(*) FROM settings WHERE key=?", preferencesKey(user.ID)).Scan(&count); err != nil || count != 0 {
		t.Fatalf("deleted user's preferences remain: count=%d error=%v", count, err)
	}
}
