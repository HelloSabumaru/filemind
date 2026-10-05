package filemind

import (
	"context"
	"crypto/sha256"
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

func (a *App) verifyPayload(ctx context.Context, f File) (string, error) {
	select {
	case a.integritySlots <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	defer func() { <-a.integritySlots }()
	payload, err := a.root.Open(f.ID)
	if err != nil {
		return "", err
	}
	defer payload.Close()
	stat, err := payload.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() != f.Size {
		return "", conflict("Stored file length mismatch. Restart this file's upload.")
	}
	if err = a.syncUpload(f.ID); err != nil {
		return "", err
	}
	digest, err := digestReader(ctx, payload)
	if err != nil {
		return "", err
	}
	if f.SHA256 != digest {
		return "", conflict("File checksum mismatch. Restart this file's upload with the original file.")
	}
	return digest, nil
}

// This is the only operation that promotes a payload to uploaded. Size alone
// never establishes integrity or durability, including after a restart.
func (a *App) finalizeUpload(ctx context.Context, user User, id string) error {
	var f File
	f.ID = id
	err := a.store.db.QueryRowContext(ctx, `SELECT f.size,f.sha256 FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id
 WHERE f.id=? AND f.started=1 AND f.deleted=0 AND t.status='draft' AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?`, id, user.ID, user.AuthVersion).Scan(&f.Size, &f.SHA256)
	if err != nil {
		return err
	}
	digest, err := a.verifyPayload(ctx, f)
	if err != nil {
		message := "File could not be verified; retry or restart the upload."
		var p *problem
		if errors.As(err, &p) {
			message = p.message
		}
		a.store.db.ExecContext(ctx, "UPDATE files SET uploaded=0,integrity_error=? WHERE id=?", message, id)
		a.logFailure("finalize_upload", err, "file_id", id)
		return err
	}
	result, err := a.store.db.ExecContext(ctx, `UPDATE files SET uploaded=1,sha256=?,integrity_error='' WHERE id=? AND deleted=0
 AND transfer_id IN (SELECT t.id FROM transfers t JOIN users u ON u.id=t.user_id WHERE t.status='draft' AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?)`, digest, id, user.ID, user.AuthVersion)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err == nil && changed != 1 {
		return conflict("Upload is no longer available.")
	}
	return err
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
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	id, transfer := r.PathValue("fileID"), r.PathValue("id")
	user := userFromContext(r.Context())
	var found int
	err := a.store.db.QueryRowContext(r.Context(), `SELECT COUNT(*) FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id
 WHERE f.id=? AND t.id=? AND t.status='draft' AND f.deleted=0 AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?`, id, transfer, user.ID, user.AuthVersion).Scan(&found)
	if !validID(id) || err != nil || found != 1 {
		notFound(w)
		return
	}
	if a.transferActive(transfer) {
		apiError(w, 409, "Pause active uploads before restarting this file.")
		return
	}
	// Reset the durable marker before removing content; recovery repairs either
	// possible crash ordering and never publishes this file in between.
	if _, err = a.store.db.ExecContext(r.Context(), "UPDATE files SET started=0,uploaded=0,integrity_error='' WHERE id=?", id); err == nil {
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
	if err != nil {
		a.operationError(w, r, "restart_upload", err)
		return
	}
	a.store.db.ExecContext(r.Context(), "UPDATE transfers SET touched_at=? WHERE id=?", time.Now().Unix(), transfer)
	writeJSON(w, map[string]bool{"ok": true})
}
