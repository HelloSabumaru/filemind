package filemind

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
)

func TestColdBackupRestoreToDifferentDirectory(t *testing.T) {
	a := testApp(t)
	admin := adminBrowser(t, a)
	user := addAccount(t, admin, "restored-user", 500)
	owner := newBrowser(a, false)
	loginAs(t, owner, user.Username, accountTestPassword)
	shared := publish(t, owner, draft(t, owner, 0, "", "public backup"), "public backup")
	partial := draft(t, owner, 0, "", "abcdefgh")
	path := startFile(t, owner, partial.Files[0])
	patchFile(t, owner, path, 0, "abcd")
	settings := a.settings()
	settings.DefaultUserQuota = 123
	checkStatus(t, admin.json("PUT", "/admin/api/settings", settings), 200)
	cfg := a.cfg
	a.Close()
	backup := filepath.Join(t.TempDir(), "backup")
	if err := os.CopyFS(backup, os.DirFS(cfg.DataDir)); err != nil {
		t.Fatal(err)
	}
	cfg.DataDir = filepath.Join(t.TempDir(), "restored")
	if err := os.CopyFS(cfg.DataDir, os.DirFS(backup)); err != nil {
		t.Fatal(err)
	}
	restored, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(restored.Close)
	if restored.settings() != settings {
		t.Fatal("restore lost settings")
	}
	owner.app = restored
	checkStatus(t, owner.request("GET", "/api/transfers/"+shared.ID, nil, nil), 200)
	response := newBrowser(restored, true).request("GET", downloadPath(shared, 0), nil, nil)
	checkStatus(t, response, 200)
	if response.Body.String() != "public backup" {
		t.Fatal("restored payload differs")
	}
	patchFile(t, owner, path, 4, "efgh")
	checkStatus(t, owner.json("POST", "/api/transfers/"+partial.ID+"/publish", nil), 200)
}
