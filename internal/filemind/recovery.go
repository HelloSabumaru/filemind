package filemind

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/tus/tusd/v2/pkg/handler"
)

func (a *App) recover() error {
	if _, err := a.store.db.Exec("DELETE FROM reservations"); err != nil {
		return err
	}
	rows, err := a.store.db.Query(`SELECT f.id,f.name,f.size,f.started,f.uploaded,f.deleted,f.sha256,f.purged,t.status FROM files f JOIN transfers t ON t.id=f.transfer_id`)
	if err != nil {
		return err
	}
	type entry struct {
		file   File
		purged bool
		status string
	}
	entries := []entry{}
	known := map[string]bool{}
	for rows.Next() {
		var e entry
		f := &e.file
		if err = rows.Scan(&f.ID, &f.Name, &f.Size, &f.Started, &f.Uploaded, &f.Deleted, &f.SHA256, &e.purged, &e.status); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
		known[f.ID] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		f := e.file
		if !validID(f.ID) {
			return errors.New("invalid stored file identifier")
		}
		if e.purged || f.Deleted || e.status == "deleted" || e.status == "expired" {
			continue
		}
		if e.status == "draft" && !f.Started {
			for _, name := range []string{f.ID, f.ID + ".info"} {
				if err = a.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			if _, err = a.store.db.Exec("UPDATE files SET uploaded=0 WHERE id=?", f.ID); err != nil {
				return err
			}
			continue
		}
		info, statErr := a.root.Stat(f.ID)
		if errors.Is(statErr, os.ErrNotExist) && e.status == "draft" {
			if _, err = a.store.db.Exec("UPDATE files SET started=0,uploaded=0,integrity_error='' WHERE id=?", f.ID); err != nil {
				return err
			}
			continue
		}
		message := ""
		if statErr != nil {
			message = "Stored file is missing or unreadable."
		} else if !info.Mode().IsRegular() || info.Size() > f.Size {
			message = "Stored file length is invalid. Restart the upload or delete this transfer."
		} else if info.Size() < f.Size && e.status != "draft" {
			message = "Stored file is incomplete. Delete this transfer and upload it again."
		}
		if message != "" {
			if _, err = a.store.db.Exec("UPDATE files SET uploaded=0,integrity_error=? WHERE id=?", message, f.ID); err != nil {
				return err
			}
			a.logger.Warn("payload quarantined", "operation", "recover_payload", "category", "integrity", "file_id", f.ID)
			continue
		}
		if e.status == "draft" {
			data, readErr := a.root.ReadFile(f.ID + ".info")
			var saved handler.FileInfo
			if readErr != nil || json.Unmarshal(data, &saved) != nil || saved.ID != f.ID || saved.Size != f.Size || saved.Storage["Path"] != filepath.Join(a.cfg.DataDir, "uploads", f.ID) || saved.MetaData["filename"] != f.Name {
				if err = a.writeUploadInfo(f); err != nil {
					return err
				}
			}
			if info.Size() < f.Size {
				if _, err = a.store.db.Exec("UPDATE files SET uploaded=0,integrity_error='' WHERE id=?", f.ID); err != nil {
					return err
				}
				continue
			}
		}
		if info.Size() == f.Size {
			digest, _, verifyErr := a.verifyPayload(context.Background(), f)
			if verifyErr != nil {
				if _, err = a.store.db.Exec("UPDATE files SET uploaded=0,integrity_error='Stored file could not be verified. Restart the upload or delete this transfer.' WHERE id=?", f.ID); err != nil {
					return err
				}
				a.logFailure("recover_payload", verifyErr, "file_id", f.ID)
				continue
			}
			if _, err = a.store.db.Exec("UPDATE files SET uploaded=1,sha256=?,integrity_error='' WHERE id=?", digest, f.ID); err != nil {
				return err
			}
		}
	}
	dir, err := a.root.Open(".")
	if err != nil {
		return err
	}
	items, err := dir.ReadDir(-1)
	dir.Close()
	if err != nil {
		return err
	}
	for _, item := range items {
		id := strings.TrimSuffix(strings.TrimSuffix(item.Name(), ".tmp"), ".info")
		if validID(id) && (!known[id] || strings.HasSuffix(item.Name(), ".tmp")) {
			if err = a.root.Remove(item.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return a.syncUpload(".")
}
