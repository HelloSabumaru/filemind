package filemind

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/tus/tusd/v2/pkg/handler"
)

func validDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func digestReader(ctx context.Context, reader io.Reader) (string, error) {
	hash := sha256.New()
	_, err := io.CopyBuffer(hash, &contextReader{ctx: ctx, reader: reader}, make([]byte, 64*1024))
	return hex.EncodeToString(hash.Sum(nil)), err
}

func (a *App) verifyPayload(ctx context.Context, f File) (string, os.FileInfo, error) {
	payload, err := a.root.Open(f.ID)
	if err != nil {
		return "", nil, err
	}
	defer payload.Close()
	stat, err := payload.Stat()
	if err != nil {
		return "", nil, err
	}
	if !stat.Mode().IsRegular() || stat.Size() != f.Size {
		return "", nil, conflict("Stored file length mismatch. Restart this file's upload.")
	}
	if err = a.syncUpload(f.ID); err != nil {
		return "", nil, err
	}
	digest, err := digestReader(ctx, payload)
	if err != nil {
		return "", nil, err
	}
	if f.SHA256 != digest {
		return "", nil, conflict("File checksum mismatch. Restart this file's upload with the original file.")
	}
	current, err := a.root.Stat(f.ID)
	if err != nil {
		return "", nil, err
	}
	if !os.SameFile(stat, current) || current.Size() != stat.Size() || !current.ModTime().Equal(stat.ModTime()) {
		return "", nil, conflict("Stored file changed during verification. Retry the upload.")
	}
	return digest, current, nil
}

// This is the only operation that promotes a payload to uploaded. Size alone
// never establishes integrity or durability, including after a restart.
func (a *App) finalizeUpload(ctx context.Context, user User, id string) error {
	release, err := a.acquireStorageWork(ctx, user.ID)
	if err != nil {
		return err
	}
	defer release()
	_, err = a.finalizePayload(ctx, user, id)
	return err
}

// Callers hold the file's Tus lock and a storage-work lease. Expensive file
// reads and synchronization never hold uploadMu or a database transaction.
func (a *App) finalizePayload(ctx context.Context, user User, id string) (os.FileInfo, error) {
	var f File
	var transferID string
	var revision int64
	f.ID = id
	a.uploadMu.Lock()
	err := a.store.db.QueryRowContext(ctx, `SELECT f.size,f.sha256,t.id,t.revision FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id
 WHERE f.id=? AND f.started=1 AND f.deleted=0 AND t.status='draft' AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?`, id, user.ID, user.AuthVersion).Scan(&f.Size, &f.SHA256, &transferID, &revision)
	if err != nil {
		a.uploadMu.Unlock()
		return nil, err
	}
	ctx, end := a.activate(ctx, transferID, randomID())
	a.uploadMu.Unlock()
	defer end()
	_, verified, verifyErr := a.verifyPayload(ctx, f)
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	message := ""
	if verifyErr != nil {
		message = "File could not be verified; retry or restart the upload."
		var p *problem
		if errors.As(verifyErr, &p) {
			message = p.message
		}
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	if verifyErr == nil {
		current, statErr := a.root.Stat(id)
		if statErr != nil {
			return nil, statErr
		}
		if !os.SameFile(verified, current) || current.Size() != verified.Size() || !current.ModTime().Equal(verified.ModTime()) {
			return nil, conflict("Stored file changed before completion. Retry the upload.")
		}
	}
	result, err := a.store.db.ExecContext(ctx, `UPDATE files SET uploaded=?,integrity_error=? WHERE id=? AND started=1 AND deleted=0 AND size=? AND sha256=?
 AND transfer_id IN (SELECT t.id FROM transfers t JOIN users u ON u.id=t.user_id WHERE t.id=? AND t.revision=? AND t.status='draft' AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?)`, verifyErr == nil, message, id, f.Size, f.SHA256, transferID, revision, user.ID, user.AuthVersion)
	if err != nil {
		return nil, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if changed != 1 {
		return nil, conflict("Upload changed or is no longer available. Refresh and retry.")
	}
	if verifyErr != nil {
		a.logFailure("finalize_upload", verifyErr, "file_id", id)
	}
	return verified, verifyErr
}

// Rebuild only server-owned metadata, never trust stored absolute paths. Atomic
// replacement also makes backups portable to a different data directory.
func (a *App) writeUploadInfo(f File) error {
	info := handler.FileInfo{ID: f.ID, Size: f.Size, MetaData: handler.MetaData{"filename": f.Name, "file_id": f.ID}, Storage: map[string]string{"Type": "filestore", "Path": filepath.Join(a.cfg.DataDir, "uploads", f.ID)}}
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	name := f.ID + ".info.tmp"
	file, err := a.root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer a.root.Remove(name)
	_, err = file.Write(data)
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = a.root.Rename(name, f.ID+".info"); err != nil {
		return err
	}
	return a.syncUpload(".")
}

func (a *App) restartFile(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	release, err := a.acquireStorageWork(r.Context(), user.ID)
	if err != nil {
		a.operationError(w, r, "restart_upload", err)
		return
	}
	defer release()
	id, transfer := r.PathValue("fileID"), r.PathValue("id")
	a.uploadMu.Lock()
	var found int
	err = a.store.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id
 WHERE f.id=? AND t.id=? AND t.status='draft' AND f.deleted=0 AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?`, id, transfer, user.ID, user.AuthVersion).Scan(&found)
	if !validID(id) || err != nil || found != 1 {
		a.uploadMu.Unlock()
		notFound(w)
		return
	}
	if a.transferActive(transfer) {
		a.uploadMu.Unlock()
		apiError(w, 409, "Pause active uploads before restarting this file.")
		return
	}
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err == nil {
		if err = currentAccount(r.Context(), tx, user, false); err == nil {
			var result sql.Result
			result, err = tx.ExecContext(r.Context(), `UPDATE files SET started=0,uploaded=0,integrity_error='' WHERE id=? AND deleted=0
 AND transfer_id IN (SELECT id FROM transfers WHERE id=? AND user_id=? AND status='draft')`, id, transfer, user.ID)
			if err == nil {
				var changed int64
				changed, err = result.RowsAffected()
				if err == nil && changed != 1 {
					err = conflict("Transfer changed; refresh and retry.")
				}
			}
		}
		if err == nil {
			_, err = tx.ExecContext(r.Context(), "UPDATE transfers SET touched_at=?,revision=revision+1 WHERE id=?", time.Now().Unix(), transfer)
		}
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	ctx, end := a.activate(r.Context(), transfer, randomID())
	a.uploadMu.Unlock()
	defer end()
	if err == nil {
		for _, name := range []string{id, id + ".info"} {
			if err = a.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				break
			}
			err = nil
		}
	}
	if err == nil {
		err = a.syncUpload(".")
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		a.operationError(w, r, "restart_upload", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
