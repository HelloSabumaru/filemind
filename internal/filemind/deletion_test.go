package filemind

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertTransferRemoved(t *testing.T, a *App, transfer Transfer) {
	t.Helper()
	for _, query := range []string{
		"SELECT COUNT(*) FROM transfers WHERE id=?",
		"SELECT COUNT(*) FROM files WHERE transfer_id=?",
		"SELECT COUNT(*) FROM sessions WHERE transfer_id=?",
		"SELECT COUNT(*) FROM reservations r JOIN files f ON f.id=r.file_id WHERE f.transfer_id=?",
	} {
		var count int
		if err := a.store.db.QueryRow(query, transfer.ID).Scan(&count); err != nil || count != 0 {
			t.Fatalf("deleted transfer still has database records: count=%d, err=%v", count, err)
		}
	}
	for _, file := range transfer.Files {
		for _, name := range []string{file.ID, file.ID + ".info", file.ID + ".info.tmp"} {
			if _, err := a.root.Stat(name); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("deleted transfer still has stored files", err)
			}
		}
	}
}

func TestDeleteRemovesFilesRecordsAndPublicSessions(t *testing.T) {
	for _, administrative := range []bool{false, true} {
		name := "owner"
		if administrative {
			name = "admin"
		}
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			transfer := publish(t, owner, draft(t, owner, 0, "transfer-password", "first", "second"), "first", "second")
			unrelated := draft(t, owner, 0, "", "keep")
			public := newBrowser(a, true)
			path := "/s/" + transfer.ShareToken
			public.page(t, path)
			checkStatus(t, public.json("POST", path+"/unlock", map[string]string{"password": "transfer-password"}), 200)
			actor, endpoint := owner, "/api/transfers/"
			if administrative {
				actor, endpoint = adminBrowser(t, a), "/admin/api/transfers/"
			}
			checkStatus(t, actor.json("DELETE", endpoint+transfer.ID, nil), 200)
			assertTransferRemoved(t, a, transfer)
			checkStatus(t, actor.request("GET", endpoint+transfer.ID, nil, nil), 404)
			checkStatus(t, public.request("GET", path, nil, nil), 404)
			checkStatus(t, public.request("GET", downloadPath(transfer, 0), nil, nil), 404)
			for _, client := range []*browser{owner, adminBrowser(t, a)} {
				list := "/api/transfers"
				if client.admin {
					list = "/admin/api/transfers"
				}
				response := client.request("GET", list, nil, nil)
				checkStatus(t, response, 200)
				if strings.Contains(response.Body.String(), transfer.ID) || !strings.Contains(response.Body.String(), unrelated.ID) {
					t.Fatal("deletion did not remove only its own entry from the list")
				}
			}
			if usage := readMetadataUsage(t, a, passwordTestUser(t, a, "test-owner").ID); usage.transfers != 1 || usage.files != 1 || usage.bytes != 4 {
				t.Fatal("deletion did not release transfer and file capacity", usage)
			}
		})
	}
}

func TestDeleteDefersRecordRemovalUntilDownloadsRelease(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "download-completion"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			a := testApp(t)
			owner := newBrowser(a, false)
			owner.login(t)
			transfer := publish(t, owner, draft(t, owner, 0, "", "reserved"), "reserved")
			transfer, err := a.store.transfer(context.Background(), transfer.ID)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := a.store.reserve(context.Background(), transfer, transfer.Files)
			if err != nil {
				t.Fatal(err)
			}
			checkStatus(t, owner.json("DELETE", "/api/transfers/"+transfer.ID, nil), 200)
			if _, err := a.store.transfer(context.Background(), transfer.ID); err != nil {
				t.Fatal("deletion discarded the pending cleanup record", err)
			}
			if _, err := a.root.Stat(transfer.Files[0].ID); err != nil {
				t.Fatal("deletion removed a reserved file", err)
			}
			checkStatus(t, owner.request("GET", "/api/transfers/"+transfer.ID, nil, nil), 404)
			if response := owner.request("GET", "/api/transfers", nil, nil); strings.Contains(response.Body.String(), transfer.ID) {
				t.Fatal("pending deletion remained visible")
			}
			if restart {
				a.Close()
				a, err = New(a.cfg, a.logger)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(a.Close)
			} else {
				a.complete(lease, false)
			}
			assertTransferRemoved(t, a, transfer)
		})
	}
}

func TestDeleteRetriesFailedPayloadRemoval(t *testing.T) {
	a := testApp(t)
	owner := newBrowser(a, false)
	owner.login(t)
	transfer := publish(t, owner, draft(t, owner, 0, "", "retry"), "retry")
	// A nonempty directory at a temporary metadata path makes removal fail
	// deterministically, including when the test runs with elevated privileges.
	blocked := filepath.Join(a.cfg.DataDir, "uploads", transfer.Files[0].ID+".info.tmp")
	if err := os.Mkdir(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(blocked, "fixture"), []byte("blocked"), 0600); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, owner.json("DELETE", "/api/transfers/"+transfer.ID, nil), 503)
	if _, err := a.store.transfer(context.Background(), transfer.ID); err != nil {
		t.Fatal("failed purge lost its retry record", err)
	}
	checkStatus(t, owner.request("GET", "/api/transfers/"+transfer.ID, nil, nil), 404)
	if err := os.RemoveAll(blocked); err != nil {
		t.Fatal(err)
	}
	if err := a.cleanup(); err != nil {
		t.Fatal(err)
	}
	assertTransferRemoved(t, a, transfer)
}
