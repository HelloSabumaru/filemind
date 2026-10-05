package filemind

import (
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
