package filemind

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

type transferPatch struct {
	Revision      int64   `json:"revision"`
	Title         *string `json:"title"`
	ExpirySeconds *int64  `json:"expirySeconds"`
	DownloadLimit *int64  `json:"downloadLimit"`
	Password      *string `json:"password"`
}

func currentAccount(ctx context.Context, tx *sql.Tx, user User, admin bool) error {
	var found int
	err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE id=? AND auth_version=? AND disabled=0 AND archived=0 AND (?=0 OR is_admin=1)", user.ID, user.AuthVersion, admin).Scan(&found)
	if err != nil {
		return err
	}
	if found != 1 {
		return &problem{401, "Account changed; sign in again."}
	}
	return nil
}

func (a *App) transferForAccount(ctx context.Context, user User, id string, admin bool) (Transfer, error) {
	if !validID(id) {
		return Transfer{}, sql.ErrNoRows
	}
	t, err := a.store.transfer(ctx, id)
	if err == nil && t.UserID != user.ID && !(admin && user.IsAdmin) {
		err = sql.ErrNoRows
	}
	return t, err
}

func (a *App) newDraft(ctx context.Context, user User, input newTransfer) (Transfer, error) {
	if err := a.store.reserveTransferCreation(ctx, user); err != nil {
		return Transfer{}, err
	}
	settings := a.settings()
	var total int64
	for _, f := range input.Files {
		if f.Size < 0 || f.Size > settings.MaxTransferSize-total {
			return Transfer{}, invalid("Transfer exceeds size limits.")
		}
		total += f.Size
	}
	space, err := freeSpace(a.cfg.DataDir)
	if err != nil {
		return Transfer{}, err
	}
	if space-total < a.cfg.MinFreeSpace {
		return Transfer{}, &problem{507, "Insufficient free disk space."}
	}
	hash := ""
	if input.Password != "" {
		hash, err = a.transferPassword(ctx, user, input.Password)
		if err != nil {
			return Transfer{}, err
		}
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	id, err := a.store.create(ctx, user, input, hash, a.settings())
	if err != nil {
		return Transfer{}, err
	}
	return a.store.transfer(ctx, id)
}

func (a *App) transferPassword(ctx context.Context, user User, password string) (string, error) {
	if password == "" {
		return "", invalid("Enter a password.")
	}
	return a.hashNewPassword(ctx, user, false, password)
}

func (a *App) publishTransfer(ctx context.Context, user User, id string) (Transfer, error) {
	release, err := a.acquireStorageWork(ctx, user.ID)
	if err != nil {
		return Transfer{}, err
	}
	defer release()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	a.uploadMu.Lock()
	t, err := a.transferForAccount(ctx, user, id, false)
	if err != nil || t.Status == "published" {
		a.uploadMu.Unlock()
		return t, err
	}
	if t.Status != "draft" {
		a.uploadMu.Unlock()
		return t, conflict("Transfer cannot be published.")
	}
	ctx, end := a.activate(ctx, id, randomID())
	a.uploadMu.Unlock()
	defer end()
	var locks []handler.Lock
	defer func() {
		for _, lock := range locks {
			lock.Unlock()
		}
	}()
	verified := make([]os.FileInfo, len(t.Files))
	for i, f := range t.Files {
		if !f.Started || f.Deleted {
			return t, conflict("Finish all uploads before sharing.")
		}
		// Share the Tus locker with chunk writes. Waiting and hashing only
		// affect this transfer, and an admin can cancel them immediately.
		lock, e := a.uploadLocker.NewLock(f.ID)
		if e != nil {
			return t, e
		}
		if e = lock.Lock(ctx, cancel); e != nil {
			if ctx.Err() != nil {
				return t, ctx.Err()
			}
			return t, conflict("File is busy. Pause uploads and retry.")
		}
		locks = append(locks, lock)
		if !f.Uploaded {
			if verified[i], err = a.finalizePayload(ctx, user, f.ID); err != nil {
				return t, err
			}
		}
		info, e := a.root.Stat(f.ID)
		if e != nil {
			return t, e
		}
		if !f.Uploaded && (!os.SameFile(verified[i], info) || !info.ModTime().Equal(verified[i].ModTime())) {
			return t, conflict("Stored file changed after verification. Retry publication.")
		}
		if !info.Mode().IsRegular() || info.Size() != f.Size {
			return t, conflict("Stored file length mismatch.")
		}
		if err = a.syncUpload(f.ID); err != nil {
			return t, err
		}
		verified[i] = info
	}
	if err = ctx.Err(); err != nil {
		return t, err
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	for i, f := range t.Files {
		info, e := a.root.Stat(f.ID)
		if e != nil {
			return t, e
		}
		if !os.SameFile(verified[i], info) || info.Size() != verified[i].Size() || !info.ModTime().Equal(verified[i].ModTime()) {
			return t, conflict("Stored file changed. Retry publication.")
		}
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	if err = currentAccount(ctx, tx, user, false); err != nil {
		return t, err
	}
	var ready int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM files WHERE transfer_id=? AND started=1 AND uploaded=1 AND deleted=0", id).Scan(&ready); err != nil {
		return t, err
	}
	if ready != len(t.Files) {
		return t, conflict("Uploads changed. Finish all files before sharing.")
	}
	now := time.Now().Unix()
	expiry := int64(0)
	if t.ExpirySeconds > 0 {
		expiry = now + t.ExpirySeconds
	}
	result, err := tx.ExecContext(ctx, "UPDATE transfers SET status='published',published_at=?,expires_at=?,share_token=?,revision=revision+1 WHERE id=? AND user_id=? AND status='draft' AND revision=?", now, expiry, randomToken(), id, user.ID, t.Revision)
	if err != nil {
		return t, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return t, err
	}
	if changed != 1 {
		return t, conflict("Transfer changed; refresh and retry.")
	}
	if err = tx.Commit(); err != nil {
		return t, err
	}
	return a.store.transfer(ctx, id)
}

func (a *App) updateTransfer(ctx context.Context, user User, id string, input transferPatch, admin bool) (Transfer, error) {
	if input.Revision < 1 {
		return Transfer{}, invalid("Transfer revision is required. Refresh and retry.")
	}
	// Serialize state transitions with publication and physical deletion. Hash a
	// new password first, so this short critical section does not include Argon2.
	var hash string
	var err error
	if input.Password != nil && *input.Password != "" {
		hash, err = a.transferPassword(ctx, user, *input.Password)
		if err != nil {
			return Transfer{}, err
		}
	}
	a.uploadMu.Lock()
	locked := true
	defer func() {
		if locked {
			a.uploadMu.Unlock()
		}
	}()
	t, err := a.transferForAccount(ctx, user, id, admin)
	if err != nil {
		return t, err
	}
	if t.Revision != input.Revision {
		return t, conflict("Transfer changed; refresh and retry.")
	}
	if t.Status != "draft" && t.Status != "published" {
		return t, conflict("Closed transfers cannot be changed.")
	}
	if input.Title != nil {
		if len(*input.Title) > 200 || strings.TrimSpace(*input.Title) == "" {
			return t, invalid("Invalid title.")
		}
		t.Title = strings.TrimSpace(*input.Title)
	}
	if input.ExpirySeconds != nil {
		if *input.ExpirySeconds < 0 || *input.ExpirySeconds > 31536000 {
			return t, invalid("Invalid expiry.")
		}
		t.ExpirySeconds = *input.ExpirySeconds
		if t.Status == "published" {
			t.ExpiresAt = 0
			if t.ExpirySeconds > 0 {
				t.ExpiresAt = time.Now().Unix() + t.ExpirySeconds
			}
		}
	}
	if input.DownloadLimit != nil {
		if *input.DownloadLimit < 0 || *input.DownloadLimit > 1000000 {
			return t, invalid("Invalid download limit.")
		}
		t.DownloadLimit = *input.DownloadLimit
	}
	if input.Password != nil {
		t.PasswordHash = hash
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return t, err
	}
	defer tx.Rollback()
	if err = currentAccount(ctx, tx, user, admin); err != nil {
		return t, err
	}
	var active int
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM reservations r JOIN files f ON f.id=r.file_id WHERE f.transfer_id=?", id).Scan(&active); err != nil {
		return t, err
	}
	if active > 0 {
		return t, conflict("Wait for active downloads to finish, or revoke the transfer.")
	}
	result, err := tx.ExecContext(ctx, `UPDATE transfers SET title=?,expiry_seconds=?,expires_at=?,download_limit=?,password_hash=?,auth_version=auth_version+1,revision=revision+1
 WHERE id=? AND revision=? AND status=? AND (status='draft' OR expires_at=0 OR expires_at>?)`, t.Title, t.ExpirySeconds, t.ExpiresAt, t.DownloadLimit, t.PasswordHash, id, input.Revision, t.Status, time.Now().Unix())
	if err != nil {
		return t, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return t, err
	}
	if changed != 1 {
		return t, conflict("Transfer changed or expired; refresh and retry.")
	}
	if err = tx.Commit(); err != nil {
		return t, err
	}
	if input.Password != nil {
		a.transferPasswords.reset(id)
	}
	a.uploadMu.Unlock()
	locked = false
	if err = a.purgeTransfer(ctx, id); err != nil {
		a.logFailure("purge_transfer", err, "transfer_id", id)
	}
	return a.store.transfer(ctx, id)
}

func (a *App) closeTransfer(ctx context.Context, user User, id, status string, admin bool) error {
	a.uploadMu.Lock()
	t, err := a.transferForAccount(ctx, user, id, admin)
	if err != nil {
		a.uploadMu.Unlock()
		return err
	}
	if status != "deleted" && status != "revoked" {
		a.uploadMu.Unlock()
		return invalid("Invalid transfer operation.")
	}
	if status == "revoked" && t.Status != "published" {
		a.uploadMu.Unlock()
		return conflict("Only published transfers can be revoked.")
	}
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		a.uploadMu.Unlock()
		return err
	}
	if err = currentAccount(ctx, tx, user, admin); err == nil {
		var result sql.Result
		result, err = tx.ExecContext(ctx, "UPDATE transfers SET status=?,closed_at=?,auth_version=auth_version+1,revision=revision+1 WHERE id=? AND revision=?", status, time.Now().Unix(), id, t.Revision)
		if err == nil {
			var changed int64
			changed, err = result.RowsAffected()
			if err == nil && changed != 1 {
				err = conflict("Transfer changed; refresh and retry.")
			}
		}
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err == nil {
		a.cancelTransfer(id)
	}
	a.uploadMu.Unlock()
	if err != nil {
		return err
	}
	if err = a.purgeTransfer(ctx, id); err != nil {
		a.logFailure("purge_transfer", err, "transfer_id", id)
		return err
	}
	return nil
}

func adminRequest(r *http.Request) bool { return strings.HasPrefix(r.URL.Path, "/admin/") }
