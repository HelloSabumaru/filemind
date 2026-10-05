package filemind

import (
	"context"
	"errors"
	"os"
	"time"
)

const cleanupBatch = 512

// Mark inaccessible in the same transaction that checks reservations, then
// remove payloads. A crash between these steps leaves a retryable purge marker.
func (a *App) purge(ctx context.Context, transferID string, limit int) error {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT f.id FROM files f JOIN transfers t ON t.id=f.transfer_id
 WHERE f.purged=0 AND (?='' OR t.id=?) AND NOT EXISTS(SELECT 1 FROM reservations r WHERE r.file_id=f.id)
 AND (f.deleted=1 OR t.status IN ('expired','deleted') OR (t.status='published' AND t.download_limit>0 AND f.downloads>=t.download_limit)) LIMIT ?`, transferID, transferID, limit)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		if !validID(id) {
			rows.Close()
			return errors.New("invalid stored file identifier")
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.ExecContext(ctx, "UPDATE files SET deleted=1 WHERE id=?", id); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	for _, id := range ids {
		for _, name := range []string{id, id + ".info", id + ".info.tmp"} {
			if err = a.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if len(ids) > 0 {
		if err = a.syncUpload("."); err != nil {
			return err
		}
	}
	for _, id := range ids {
		if _, err = a.store.db.ExecContext(ctx, "UPDATE files SET purged=1 WHERE id=? AND deleted=1", id); err != nil {
			return err
		}
	}
	_, err = a.store.db.ExecContext(ctx, `UPDATE transfers SET status='exhausted',closed_at=?,revision=revision+1 WHERE status='published'
 AND (?='' OR id=?) AND NOT EXISTS(SELECT 1 FROM files f WHERE f.transfer_id=transfers.id AND f.deleted=0)`, time.Now().Unix(), transferID, transferID)
	return err
}

func (a *App) purgeTransfer(ctx context.Context, id string) error {
	a.cleanupMu.Lock()
	defer a.cleanupMu.Unlock()
	return a.purge(ctx, id, 100)
}

func (a *App) cleanup() (err error) {
	a.cleanupMu.Lock()
	defer a.cleanupMu.Unlock()
	defer func() {
		a.cleanupFailed.Store(err != nil)
		if err == nil {
			a.lastCleanup.Store(time.Now().Unix())
		}
	}()
	ctx := context.Background()
	now := time.Now().Unix()
	rows, err := a.store.db.Query(`SELECT id FROM transfers WHERE
 (status IN ('published','revoked') AND expires_at>0 AND expires_at<=?) OR (status='draft' AND touched_at<=?) LIMIT 100`, now, now-86400)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		// Recheck the predicate; a fresh upload or edit may have changed it since
		// selection. Publication checks this same revision before committing.
		result, e := a.store.db.Exec(`UPDATE transfers SET status='expired',closed_at=?,auth_version=auth_version+1,revision=revision+1 WHERE id=? AND
 ((status IN ('published','revoked') AND expires_at>0 AND expires_at<=?) OR (status='draft' AND touched_at<=?))`, now, id, now, now-86400)
		if e != nil {
			return e
		}
		changed, e := result.RowsAffected()
		if e != nil {
			return e
		}
		if changed > 0 {
			a.cancelTransfer(id)
		}
	}
	if err = a.purge(ctx, "", cleanupBatch); err != nil {
		return err
	}
	_, err = a.store.db.Exec(`DELETE FROM transfers WHERE id IN (SELECT id FROM transfers WHERE closed_at>0 AND closed_at<?
 AND NOT EXISTS(SELECT 1 FROM files f WHERE f.transfer_id=transfers.id AND f.purged=0) LIMIT 100)`, now-30*86400)
	if err != nil {
		return err
	}
	_, err = a.store.db.Exec("DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE expires_at<=? LIMIT 512)", now)
	if err != nil {
		return err
	}
	_, err = a.store.db.Exec(`DELETE FROM users WHERE archived=1 AND NOT EXISTS(SELECT 1 FROM transfers WHERE user_id=users.id) AND NOT EXISTS(SELECT 1 FROM sessions WHERE user_id=users.id)`)
	return err
}
