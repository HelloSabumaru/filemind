package filemind

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestFilenameTransferTitlesRemainEditable(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	for _, item := range []struct {
		name, title, first, want string
		count                    int
	}{
		{name: "single", first: "invoice.pdf", count: 1, want: "invoice.pdf"},
		{name: "multiple", first: "invoice.pdf", count: 4, want: "invoice.pdf + 3 more"},
		{name: "custom", title: "  Client documents  ", first: "invoice.pdf", count: 2, want: "Client documents"},
		{name: "blank title", title: "   ", first: "invoice.pdf", count: 1, want: "invoice.pdf"},
		{name: "long ASCII", first: strings.Repeat("a", 240) + ".pdf", count: 1},
		{name: "long Unicode", first: strings.Repeat("文", 80) + ".pdf", count: 2},
	} {
		t.Run(item.name, func(t *testing.T) {
			files := []map[string]any{{"name": item.first, "size": 0, "sha256": testDigest("")}}
			for i := 1; i < item.count; i++ {
				files = append(files, map[string]any{"name": "file-" + strconv.Itoa(i) + ".txt", "size": 0, "sha256": testDigest("")})
			}
			transfer := readTransfer(t, owner.json("POST", "/api/transfers", map[string]any{"title": item.title, "files": files}))
			if item.want != "" && transfer.Title != item.want {
				t.Fatalf("title = %q, want %q", transfer.Title, item.want)
			}
			if !utf8.ValidString(transfer.Title) || len(transfer.Title) > 200 {
				t.Fatalf("default title exceeds limits or splits Unicode: %q", transfer.Title)
			}
			if item.want == "" && !strings.Contains(transfer.Title, "…") {
				t.Fatal("long title is missing its truncation marker")
			}
			if item.want == "" && item.count > 1 && !strings.HasSuffix(transfer.Title, " + 1 more") {
				t.Fatal("long title lost its additional file count")
			}
			if len(transfer.Files) != item.count || transfer.Files[0].Name != item.first {
				t.Fatal("default title changed the original files")
			}
			checkStatus(t, owner.json("PATCH", "/api/transfers/"+transfer.ID, map[string]any{"title": transfer.Title}), 200)
		})
	}
}

func TestUnsupportedSchemaIsRejectedWithoutChangingData(t *testing.T) {
	for _, version := range []int{0, 1, 2, schemaVersion + 1} {
		t.Run(strconv.Itoa(version), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "filemind.sqlite")
			db, err := sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = db.Exec("CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES('keep'); PRAGMA user_version=" + strconv.Itoa(version)); err != nil {
				t.Fatal(err)
			}
			if err = db.Close(); err != nil {
				t.Fatal(err)
			}
			store, err := openStore(path)
			if err == nil {
				store.db.Close()
				t.Fatal("existing unsupported schema accepted")
			}
			if !strings.Contains(err.Error(), "fresh data directory") {
				t.Fatal(err)
			}
			db, err = sql.Open("sqlite", path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var actual, tables int
			var value string
			if err = db.QueryRow("PRAGMA user_version").Scan(&actual); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow("SELECT value FROM sentinel").Scan(&value); err != nil {
				t.Fatal(err)
			}
			if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table'").Scan(&tables); err != nil {
				t.Fatal(err)
			}
			if actual != version || value != "keep" || tables != 1 {
				t.Fatal("unsupported database was modified")
			}
		})
	}
}
