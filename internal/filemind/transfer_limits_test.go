package filemind

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Populate bounded occupancy in a temporary database instead of generating requests.
func seedTransferMetadata(t *testing.T, a *App, userID, label, status string, count, filesPerTransfer int, purged bool) []string {
	t.Helper()
	now := time.Now().Unix()
	closed := int64(0)
	deleted := false
	if status == "deleted" || status == "expired" || status == "exhausted" {
		closed, deleted = now-3600, true
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	_, err = tx.Exec(`WITH RECURSIVE occupancy(n) AS (
 SELECT 1 WHERE ?>0 UNION ALL SELECT n+1 FROM occupancy WHERE n<?)
 INSERT INTO transfers(id,user_id,title,status,created_at,touched_at,expiry_seconds,download_limit,closed_at)
 SELECT lower(hex(randomblob(16))),?,?,?, ?,?,0,0,? FROM occupancy`, count, count, userID, label, status, now, now, closed)
	if err != nil {
		t.Fatal(err)
	}
	_, err = tx.Exec(`WITH RECURSIVE file_numbers(n) AS (
 SELECT 1 WHERE ?>0 UNION ALL SELECT n+1 FROM file_numbers WHERE n<?)
 INSERT INTO files(id,transfer_id,name,size,sha256,deleted,purged)
 SELECT lower(hex(randomblob(16))),t.id,'fixture-' || n,0,?,?,? FROM transfers t CROSS JOIN file_numbers
 WHERE t.user_id=? AND t.title=?`, filesPerTransfer, filesPerTransfer, testDigest(""), deleted, purged, userID, label)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := tx.Query("SELECT id FROM transfers WHERE user_id=? AND title=? ORDER BY rowid", userID, label)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	return ids
}

func metadataTestOwners(t *testing.T, a *App) (User, *browser, *browser) {
	t.Helper()
	admin := adminBrowser(t, a)
	first := addAccount(t, admin, "metadata-first", 1)
	second := addAccount(t, admin, "metadata-second", 1)
	alice, bob := newBrowser(a, false), newBrowser(a, false)
	loginAs(t, alice, first.Username, accountTestPassword)
	loginAs(t, bob, second.Username, accountTestPassword)
	return first, alice, bob
}

func emptyTransferInput(count int) map[string]any {
	files := []map[string]any{}
	for i := range count {
		files = append(files, map[string]any{"name": "empty-" + strconv.Itoa(i), "size": 0, "sha256": testDigest("")})
	}
	return map[string]any{"files": files}
}

func readMetadataUsage(t *testing.T, a *App, userID string) transferUsage {
	t.Helper()
	tx, err := a.store.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	usage, err := metadataUsage(context.Background(), tx, userID)
	if err != nil {
		t.Fatal(err)
	}
	return usage
}

func TestUserTransferLimitsPreserveAnotherAccountsCapacity(t *testing.T) {
	for _, item := range []struct {
		name, status string
		count        int
	}{
		{"drafts", "draft", maxUserDrafts}, {"active transfers", "published", maxUserActiveTransfers},
		{"retained transfers", "deleted", maxUserTransferRecords}, {"file records", "deleted", maxUserFileRecords / 100},
	} {
		t.Run(item.name, func(t *testing.T) {
			a := testApp(t)
			user, alice, bob := metadataTestOwners(t, a)
			files := 1
			if item.name == "file records" {
				files = 100
			}
			seedTransferMetadata(t, a, user.ID, "account occupancy", item.status, item.count, files, false)
			before := readMetadataUsage(t, a, user.ID)
			checkStatus(t, alice.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusConflict)
			if after := readMetadataUsage(t, a, user.ID); after != before {
				t.Fatal("failed creation changed account metadata")
			}
			checkStatus(t, bob.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusOK)
			if before.bytes != 0 {
				t.Fatal("fixture consumed byte quota")
			}
		})
	}
}

func TestDeletingADraftReleasesItsActiveAndMetadataCapacity(t *testing.T) {
	a := testApp(t)
	user, owner, _ := metadataTestOwners(t, a)
	ids := seedTransferMetadata(t, a, user.ID, "draft occupancy", "draft", maxUserDrafts, 1, false)
	checkStatus(t, owner.json("DELETE", "/api/transfers/"+ids[0], nil), http.StatusOK)
	checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusOK)
	usage := readMetadataUsage(t, a, user.ID)
	if usage.drafts != maxUserDrafts || usage.transfers != maxUserDrafts || usage.files != maxUserDrafts {
		t.Fatalf("deleted draft did not release metadata capacity: %+v", usage)
	}
}

func TestMetadataPressurePrunesOnlySafeHistory(t *testing.T) {
	a := testApp(t)
	user, owner, _ := metadataTestOwners(t, a)
	ids := seedTransferMetadata(t, a, user.ID, "closed history", "deleted", maxUserTransferRecords, 1, true)
	var heldFile string
	if err := a.store.db.QueryRow("SELECT id FROM files WHERE transfer_id=?", ids[0]).Scan(&heldFile); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.db.Exec("INSERT INTO reservations(id,file_id) VALUES('history-hold',?)", heldFile); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.db.Exec("INSERT INTO sessions(token_hash,kind,transfer_id,expires_at) VALUES('history-session','share',?,?)", ids[1], time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusOK)
	usage := readMetadataUsage(t, a, user.ID)
	if usage.transfers != maxUserTransferRecords || usage.files != maxUserTransferRecords {
		t.Fatalf("history cap exceeded: %+v", usage)
	}
	var held, pruned, sessions int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM transfers WHERE id=?", ids[0]).Scan(&held); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM transfers WHERE id=?", ids[1]).Scan(&pruned); err != nil {
		t.Fatal(err)
	}
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM sessions WHERE transfer_id=?", ids[1]).Scan(&sessions); err != nil {
		t.Fatal(err)
	}
	if held != 1 || pruned != 0 || sessions != 0 {
		t.Fatal("history pruning ignored a reservation or left orphaned sessions")
	}
}

func TestPurgedFileHistoryReleasesFileRecordCapacity(t *testing.T) {
	a := testApp(t)
	user, owner, _ := metadataTestOwners(t, a)
	ids := seedTransferMetadata(t, a, user.ID, "file history", "deleted", maxUserFileRecords/100, 100, false)
	checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(2)), http.StatusConflict)
	if _, err := a.store.db.Exec("UPDATE files SET purged=1 WHERE transfer_id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(2)), http.StatusOK)
	usage := readMetadataUsage(t, a, user.ID)
	if usage.files != maxUserFileRecords-100+2 {
		t.Fatalf("file history accounting: %+v", usage)
	}
	var pending int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM transfers WHERE id=?", ids[1]).Scan(&pending); err != nil || pending != 1 {
		t.Fatal("pending payload history was removed", err)
	}
}

func TestGlobalTransferAndMetadataLimits(t *testing.T) {
	for _, item := range []struct {
		name, status string
		count, files int
	}{
		{"active", "published", maxActiveTransfers, 1},
		{"retained", "deleted", maxTransferRecords, 1},
		{"files", "deleted", maxFileRecords / 100, 100},
	} {
		t.Run(item.name, func(t *testing.T) {
			a := testApp(t)
			user, _, owner := metadataTestOwners(t, a)
			ids := seedTransferMetadata(t, a, user.ID, "server occupancy", item.status, item.count, item.files, false)
			checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusConflict)
			if _, err := a.store.db.Exec("UPDATE transfers SET status='deleted',closed_at=? WHERE id=?", time.Now().Unix()-3600, ids[0]); err != nil {
				t.Fatal(err)
			}
			if _, err := a.store.db.Exec("UPDATE files SET deleted=1,purged=1 WHERE transfer_id=?", ids[0]); err != nil {
				t.Fatal(err)
			}
			checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusOK)
			usage := readMetadataUsage(t, a, "")
			if usage.active > maxActiveTransfers || usage.transfers > maxTransferRecords || usage.files > maxFileRecords {
				t.Fatalf("global metadata bounds exceeded: %+v", usage)
			}
		})
	}
}

func TestFailedCreationRollsBackHistoryPruning(t *testing.T) {
	a := testApp(t)
	user, owner, other := metadataTestOwners(t, a)
	ids := seedTransferMetadata(t, a, user.ID, "account history", "deleted", maxUserTransferRecords, 1, false)
	var otherID string
	if err := a.store.db.QueryRow("SELECT id FROM users WHERE username='metadata-second'").Scan(&otherID); err != nil {
		t.Fatal(err)
	}
	seedTransferMetadata(t, a, otherID, "pending shared history", "deleted", (maxFileRecords-maxUserTransferRecords)/100, 100, false)
	if _, err := a.store.db.Exec("UPDATE files SET purged=1 WHERE transfer_id=?", ids[0]); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(2)), http.StatusConflict)
	var kept int
	if err := a.store.db.QueryRow("SELECT COUNT(*) FROM transfers WHERE id=?", ids[0]).Scan(&kept); err != nil || kept != 1 {
		t.Fatal("failed creation deleted history", err)
	}
	if readMetadataUsage(t, a, "").files != maxFileRecords {
		t.Fatal("failed creation partially pruned metadata")
	}
	checkStatus(t, other.request("GET", "/api/transfers", nil, nil), http.StatusOK)
}

func writeCreationRate(t *testing.T, a *App, userID string, rate transferCreationRate) {
	t.Helper()
	data, err := json.Marshal(rate)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.store.putSetting(creationRateKey(userID), string(data)); err != nil {
		t.Fatal(err)
	}
}

func TestCreationRateSurvivesHistoryRemovalAndRestart(t *testing.T) {
	for _, daily := range []bool{false, true} {
		t.Run(strconv.FormatBool(daily), func(t *testing.T) {
			a := testApp(t)
			user, owner, _ := metadataTestOwners(t, a)
			draft := readTransfer(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)))
			now := time.Now().Unix()
			rate := transferCreationRate{MinuteStart: now, MinuteCount: maxTransferAttemptsPerMinute, DayStart: now, DayCount: maxTransferAttemptsPerMinute}
			if daily {
				rate.MinuteCount = 0
				rate.DayCount = maxTransferAttemptsPerDay
			}
			writeCreationRate(t, a, user.ID, rate)
			checkStatus(t, owner.json("DELETE", "/api/transfers/"+draft.ID, nil), http.StatusOK)
			if _, err := a.store.db.Exec("UPDATE transfers SET closed_at=? WHERE id=?", now-31*86400, draft.ID); err != nil {
				t.Fatal(err)
			}
			if err := a.cleanup(); err != nil {
				t.Fatal(err)
			}
			if readMetadataUsage(t, a, user.ID).transfers != 0 {
				t.Fatal("history was not removed")
			}
			a.Close()
			restarted, err := New(a.cfg, a.logger)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(restarted.Close)
			owner.app = restarted
			response := owner.json("POST", "/api/transfers", emptyTransferInput(1))
			checkStatus(t, response, http.StatusTooManyRequests)
			retry, err := strconv.Atoi(response.Header().Get("Retry-After"))
			if err != nil || retry <= 0 || retry > 86400 {
				t.Fatal("missing rate-limit retry delay")
			}
			if daily && !strings.Contains(response.Body.String(), "Daily") {
				t.Fatal("daily limit was not enforced")
			}
			rate.MinuteStart = now - 61
			rate.DayStart = now - 86401
			writeCreationRate(t, restarted, user.ID, rate)
			checkStatus(t, owner.json("POST", "/api/transfers", emptyTransferInput(1)), http.StatusOK)
		})
	}
}

func TestConcurrentCreationsRespectDraftAndAttemptLimits(t *testing.T) {
	for _, rateLimited := range []bool{false, true} {
		t.Run(strconv.FormatBool(rateLimited), func(t *testing.T) {
			a := testApp(t)
			user, owner, _ := metadataTestOwners(t, a)
			wantCapped := http.StatusConflict
			if rateLimited {
				now := time.Now().Unix()
				writeCreationRate(t, a, user.ID, transferCreationRate{MinuteStart: now, MinuteCount: maxTransferAttemptsPerMinute - 1, DayStart: now, DayCount: maxTransferAttemptsPerMinute - 1})
				wantCapped = http.StatusTooManyRequests
			} else {
				seedTransferMetadata(t, a, user.ID, "concurrent drafts", "draft", maxUserDrafts-1, 1, false)
			}
			var group sync.WaitGroup
			statuses := make(chan int, 2)
			for range 2 {
				client := newBrowser(a, false)
				client.csrf = owner.csrf
				for name, cookie := range owner.cookies {
					client.cookies[name] = cookie
				}
				group.Go(func() { statuses <- client.json("POST", "/api/transfers", emptyTransferInput(1)).Code })
			}
			group.Wait()
			close(statuses)
			successes, capped := 0, 0
			for status := range statuses {
				if status == http.StatusOK {
					successes++
				} else if status == wantCapped {
					capped++
				} else {
					t.Fatalf("unexpected status %d", status)
				}
			}
			if successes != 1 || capped != 1 {
				t.Fatal("concurrent creations escaped the limit")
			}
			if readMetadataUsage(t, a, user.ID).drafts > maxUserDrafts {
				t.Fatal("draft capacity exceeded")
			}
		})
	}
}
