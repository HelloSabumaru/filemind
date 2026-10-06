package filemind

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"time"
)

const (
	maxUserDrafts                = 20
	maxUserActiveTransfers       = 100
	maxUserTransferRecords       = 200
	maxUserFileRecords           = 2000
	maxActiveTransfers           = 1000
	maxTransferRecords           = 10000
	maxFileRecords               = 50000
	maxTransferAttemptsPerMinute = 20
	maxTransferAttemptsPerDay    = 200
	transferHistoryBatch         = 100
)

type transferCreationRate struct {
	MinuteStart int64 `json:"minuteStart"`
	MinuteCount int   `json:"minuteCount"`
	DayStart    int64 `json:"dayStart"`
	DayCount    int   `json:"dayCount"`
}

func creationRateKey(userID string) string { return "transfer_creation:" + userID }

type transferCreationRateError struct {
	message    string
	retryAfter int64
}

func (e *transferCreationRateError) Error() string { return e.message }
func (e *transferCreationRateError) Unwrap() error {
	return &problem{http.StatusTooManyRequests, e.message}
}

// Reserve an attempt before hashing or allocating a draft. This counter is
// independent of transfer history, so deletion and pruning cannot reset it.
func (s *Store) reserveTransferCreation(ctx context.Context, user User) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = currentAccount(ctx, tx, user, false); err != nil {
		return err
	}
	var rate transferCreationRate
	var stored string
	err = tx.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", creationRateKey(user.ID)).Scan(&stored)
	if err == nil {
		if err = json.Unmarshal([]byte(stored), &rate); err != nil {
			return err
		}
		if rate.MinuteCount < 0 || rate.DayCount < 0 {
			return errors.New("invalid transfer creation counters")
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	now := time.Now().Unix()
	if rate.MinuteStart == 0 || now < rate.MinuteStart || now >= rate.MinuteStart+60 {
		rate.MinuteStart, rate.MinuteCount = now, 0
	}
	if rate.DayStart == 0 || now < rate.DayStart || now >= rate.DayStart+86400 {
		rate.DayStart, rate.DayCount = now, 0
	}
	if rate.DayCount >= maxTransferAttemptsPerDay {
		return &transferCreationRateError{"Daily transfer creation limit reached. Try again later.", rate.DayStart + 86400 - now}
	}
	if rate.MinuteCount >= maxTransferAttemptsPerMinute {
		return &transferCreationRateError{"Transfer creation is limited to 20 attempts per minute. Try again later.", rate.MinuteStart + 60 - now}
	}
	rate.MinuteCount++
	rate.DayCount++
	data, err := json.Marshal(rate)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", creationRateKey(user.ID), string(data)); err != nil {
		return err
	}
	return tx.Commit()
}

type transferUsage struct {
	drafts, active, transfers, files int
	bytes                            int64
}

func metadataUsage(ctx context.Context, tx *sql.Tx, userID string) (transferUsage, error) {
	var usage transferUsage
	query := "SELECT COUNT(*),COALESCE(SUM(status='draft'),0),COALESCE(SUM(status IN ('draft','published','revoked')),0) FROM transfers"
	args := []any{}
	fileQuery := "SELECT COUNT(*),COALESCE(SUM(CASE WHEN f.deleted=0 THEN f.size ELSE 0 END),0) FROM files f"
	if userID != "" {
		query += " WHERE user_id=?"
		fileQuery += " JOIN transfers t ON t.id=f.transfer_id WHERE t.user_id=?"
		args = append(args, userID)
	}
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&usage.transfers, &usage.drafts, &usage.active); err != nil {
		return usage, err
	}
	err := tx.QueryRowContext(ctx, fileQuery, args...).Scan(&usage.files, &usage.bytes)
	return usage, err
}

// Only history whose payload purge has finished can be removed. Revoked
// transfers retain their payloads and continue to consume active capacity.
func pruneTransferHistory(ctx context.Context, tx *sql.Tx, userID string, needTransfers, needFiles int, olderThan int64) (transferUsage, error) {
	var freed transferUsage
	if needTransfers <= 0 && needFiles <= 0 {
		return freed, nil
	}
	if olderThan <= 0 {
		// Leave recent closures alone while their final responses are completing.
		olderThan = time.Now().Unix() - 60
	}
	// The purge-state index has low selectivity; choosing it in this correlated
	// check would scan the global pending-file set once for every transfer.
	query := `SELECT t.id,(SELECT COUNT(*) FROM files WHERE transfer_id=t.id) FROM transfers t
 WHERE t.status IN ('deleted','expired','exhausted') AND t.closed_at>0
 AND NOT EXISTS(SELECT 1 FROM files INDEXED BY files_transfer WHERE transfer_id=t.id AND purged=0)
 AND NOT EXISTS(SELECT 1 FROM reservations r JOIN files f ON f.id=r.file_id WHERE f.transfer_id=t.id)`
	args := []any{}
	if userID != "" {
		query += " AND t.user_id=?"
		args = append(args, userID)
	}
	if olderThan > 0 {
		query += " AND t.closed_at<?"
		args = append(args, olderThan)
	}
	query += " ORDER BY t.closed_at,t.rowid LIMIT ?"
	args = append(args, transferHistoryBatch)
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return freed, err
	}
	type candidate struct {
		id    string
		files int
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err = rows.Scan(&item.id, &item.files); err != nil {
			break
		}
		candidates = append(candidates, item)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err != nil {
		return freed, err
	}
	for _, item := range candidates {
		if freed.transfers >= needTransfers && freed.files >= needFiles {
			break
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE kind='share' AND transfer_id=?", item.id); err != nil {
			return freed, err
		}
		if _, err = tx.ExecContext(ctx, "DELETE FROM transfers WHERE id=?", item.id); err != nil {
			return freed, err
		}
		freed.transfers++
		freed.files += item.files
	}
	return freed, nil
}

func enforceTransferLimits(ctx context.Context, tx *sql.Tx, userID string, files int, bytes, quota, serverQuota int64) error {
	user, err := metadataUsage(ctx, tx, userID)
	if err != nil {
		return err
	}
	if user.drafts >= maxUserDrafts {
		return conflict("Account draft limit reached. Finish or delete an existing draft.")
	}
	if user.active >= maxUserActiveTransfers {
		return conflict("Account active transfer limit reached. Delete an existing transfer.")
	}
	if quota > 0 && bytes > quota-user.bytes {
		return conflict("Account storage quota reached.")
	}
	global, err := metadataUsage(ctx, tx, "")
	if err != nil {
		return err
	}
	if global.active >= maxActiveTransfers {
		return conflict("Server active transfer limit reached.")
	}
	if bytes > serverQuota-global.bytes {
		return conflict("Server storage quota reached.")
	}
	freed, err := pruneTransferHistory(ctx, tx, userID, user.transfers+1-maxUserTransferRecords, user.files+files-maxUserFileRecords, 0)
	if err != nil {
		return err
	}
	user.transfers -= freed.transfers
	user.files -= freed.files
	global.transfers -= freed.transfers
	global.files -= freed.files
	if user.transfers >= maxUserTransferRecords {
		return conflict("Account transfer metadata limit reached. Delete an existing transfer or wait for cleanup.")
	}
	if user.files+files > maxUserFileRecords {
		return conflict("Account file record limit reached. Delete an existing transfer or wait for cleanup.")
	}
	// Prefer reclaiming this account's closed history before another user's.
	for _, scope := range []string{userID, ""} {
		freed, err = pruneTransferHistory(ctx, tx, scope, global.transfers+1-maxTransferRecords, global.files+files-maxFileRecords, 0)
		if err != nil {
			return err
		}
		global.transfers -= freed.transfers
		global.files -= freed.files
	}
	if global.transfers >= maxTransferRecords {
		return conflict("Server transfer metadata limit reached.")
	}
	if global.files+files > maxFileRecords {
		return conflict("Server file record limit reached.")
	}
	return nil
}
