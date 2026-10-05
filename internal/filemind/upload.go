package filemind

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tus/tusd/v2/pkg/filestore"
	"github.com/tus/tusd/v2/pkg/handler"
	"github.com/tus/tusd/v2/pkg/memorylocker"
	expslog "golang.org/x/exp/slog"
)

type uploadStore struct {
	app  *App
	base filestore.FileStore
}

func (s uploadStore) NewUpload(ctx context.Context, info handler.FileInfo) (handler.Upload, error) {
	s.app.uploadMu.Lock()
	defer s.app.uploadMu.Unlock()
	tx, err := s.app.store.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var started, deleted bool
	var state string
	var size int64
	user := userFromContext(ctx)
	err = tx.QueryRowContext(ctx, "SELECT f.started,f.deleted,f.size,t.status FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id WHERE f.id=? AND u.id=? AND u.disabled=0 AND u.auth_version=?", info.ID, user.ID, user.AuthVersion).Scan(&started, &deleted, &size, &state)
	if err != nil || started || deleted || state != "draft" || info.Size != size {
		return nil, handler.NewError("UPLOAD_UNAVAILABLE", "Upload unavailable", 409)
	}
	space, e := freeSpace(s.app.cfg.DataDir)
	if e != nil || space-size < s.app.cfg.MinFreeSpace {
		return nil, handler.NewError("STORAGE_FULL", "Insufficient free disk space", 507)
	}
	u, err := s.base.NewUpload(ctx, info)
	if err != nil {
		return nil, err
	}
	if err = s.app.syncUpload(info.ID); err == nil {
		err = s.app.syncUpload(info.ID + ".info")
	}
	if err == nil {
		var dir *os.File
		dir, err = s.app.root.Open(".")
		if err == nil {
			err = dir.Sync()
			dir.Close()
		}
	}
	if err != nil {
		s.app.root.Remove(info.ID)
		s.app.root.Remove(info.ID + ".info")
		return nil, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE files SET started=1 WHERE id=?", info.ID)
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE transfers SET touched_at=? WHERE id=(SELECT transfer_id FROM files WHERE id=?)", time.Now().Unix(), info.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		s.app.root.Remove(info.ID)
		s.app.root.Remove(info.ID + ".info")
		return nil, err
	}
	return durableUpload{Upload: u, app: s.app, id: info.ID}, nil
}
func (s uploadStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	if !validID(id) {
		return nil, handler.ErrNotFound
	}
	user := userFromContext(ctx)
	var found int
	err := s.app.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id WHERE f.id=? AND f.started=1 AND f.deleted=0 AND t.status='draft' AND u.id=? AND u.disabled=0 AND u.auth_version=?", id, user.ID, user.AuthVersion).Scan(&found)
	if err != nil || found != 1 {
		return nil, handler.ErrNotFound
	}
	u, err := s.base.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	return durableUpload{Upload: u, app: s.app, id: id}, nil
}

type durableUpload struct {
	handler.Upload
	app *App
	id  string
}

func (u durableUpload) WriteChunk(ctx context.Context, offset int64, reader io.Reader) (int64, error) {
	n, err := u.Upload.WriteChunk(ctx, offset, reader)
	if syncErr := u.app.syncUpload(u.id); syncErr != nil {
		return n, syncErr
	}
	return n, err
}

func (a *App) syncUpload(id string) error {
	f, err := a.root.Open(id)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (a *App) reserveWrite(size int64) (func(), error) {
	a.spaceMu.Lock()
	defer a.spaceMu.Unlock()
	space, err := freeSpace(a.cfg.DataDir)
	if err != nil || space-a.writeReserved-size < a.cfg.MinFreeSpace {
		return nil, errors.New("insufficient free disk space")
	}
	a.writeReserved += size
	return func() { a.spaceMu.Lock(); a.writeReserved -= size; a.spaceMu.Unlock() }, nil
}

func (a *App) initializeUploads() error {
	base := filestore.New(filepath.Join(a.cfg.DataDir, "uploads"))
	composer := handler.NewStoreComposer()
	composer.UseCore(uploadStore{a, base})
	memorylocker.New().UseIn(composer)
	h, err := handler.NewHandler(handler.Config{
		// Drafts reserve validated sizes, so older drafts can finish after size limits change.
		StoreComposer: composer, BasePath: strings.TrimRight(a.cfg.OwnerURL, "/") + "/uploads/", DisableDownload: true, DisableTermination: true, DisableConcatenation: true,
		Cors: &handler.CorsConfig{Disable: true}, NetworkTimeout: 60 * time.Second,
		Logger: expslog.New(expslog.NewTextHandler(io.Discard, nil)),
		PreUploadCreateCallback: func(event handler.HookEvent) (handler.HTTPResponse, handler.FileInfoChanges, error) {
			id := event.Upload.MetaData["file_id"]
			if !validID(id) || event.Upload.SizeIsDeferred {
				return handler.HTTPResponse{}, handler.FileInfoChanges{}, handler.NewError("INVALID_UPLOAD", "Known file size and identifier required", 400)
			}
			var name, state string
			var size int64
			var deleted bool
			user := userFromContext(event.Context)
			err := a.store.db.QueryRowContext(event.Context, "SELECT f.name,f.size,f.deleted,t.status FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id WHERE f.id=? AND u.id=? AND u.disabled=0 AND u.auth_version=?", id, user.ID, user.AuthVersion).Scan(&name, &size, &deleted, &state)
			if err != nil || deleted || state != "draft" || size != event.Upload.Size {
				return handler.HTTPResponse{}, handler.FileInfoChanges{}, handler.NewError("INVALID_UPLOAD", "Upload unavailable", 409)
			}
			return handler.HTTPResponse{}, handler.FileInfoChanges{ID: id, MetaData: handler.MetaData{"filename": name, "file_id": id}}, nil
		},
		PreFinishResponseCallback: func(event handler.HookEvent) (handler.HTTPResponse, error) {
			f, err := a.root.Open(event.Upload.ID)
			if err != nil {
				return handler.HTTPResponse{}, err
			}
			info, err := f.Stat()
			f.Close()
			if err != nil || info.Size() != event.Upload.Size {
				return handler.HTTPResponse{}, errors.New("upload length mismatch")
			}
			user := userFromContext(event.Context)
			result, err := a.store.db.ExecContext(event.Context, "UPDATE files SET uploaded=1 WHERE id=? AND deleted=0 AND transfer_id IN (SELECT t.id FROM transfers t JOIN users u ON u.id=t.user_id WHERE t.status='draft' AND u.id=? AND u.disabled=0 AND u.auth_version=?)", event.Upload.ID, user.ID, user.AuthVersion)
			if err == nil {
				if changed, e := result.RowsAffected(); e != nil || changed != 1 {
					err = errors.New("upload unavailable")
				}
			}
			return handler.HTTPResponse{}, err
		},
	})
	if err != nil {
		return err
	}
	a.uploadHandler = http.StripPrefix("/uploads/", h)
	return nil
}

func (a *App) upload(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodOptions {
		a.uploadHandler.ServeHTTP(w, r)
		return
	}
	if r.Method == http.MethodPost {
		release, err := a.reserveWrite(64 * 1024)
		if err != nil {
			apiError(w, 507, "Insufficient free disk space.")
			return
		}
		defer release()
		a.uploadHandler.ServeHTTP(w, r)
		return
	}
	id := r.PathValue("id")
	if !validID(id) {
		notFound(w)
		return
	}
	var state, transferID string
	var deleted bool
	user := userFromContext(r.Context())
	err := a.store.db.QueryRowContext(r.Context(), "SELECT t.status,f.deleted,t.id FROM files f JOIN transfers t ON t.id=f.transfer_id WHERE f.id=? AND t.user_id=?", id, user.ID).Scan(&state, &deleted, &transferID)
	if err != nil || deleted || state != "draft" {
		notFound(w)
		return
	}
	ctx, end := a.activate(r.Context(), transferID, randomID())
	defer end()
	if err = a.store.db.QueryRowContext(ctx, "SELECT t.status FROM transfers t JOIN users u ON u.id=t.user_id WHERE t.id=? AND u.id=? AND u.disabled=0 AND u.auth_version=?", transferID, user.ID, user.AuthVersion).Scan(&state); err != nil || state != "draft" {
		notFound(w)
		return
	}
	controller := http.NewResponseController(w)
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		defer close(done)
		controller.SetReadDeadline(time.Now())
		controller.SetWriteDeadline(time.Now())
	})
	defer func() {
		if !stop() {
			<-done
		}
		controller.SetReadDeadline(time.Time{})
		controller.SetWriteDeadline(time.Time{})
	}()
	r = r.WithContext(ctx)
	if r.Method == http.MethodPatch {
		size := int64(8 * 1024 * 1024)
		if r.ContentLength >= 0 {
			size = min(size, r.ContentLength)
		}
		release, err := a.reserveWrite(size)
		if err != nil {
			apiError(w, 507, "Insufficient free disk space.")
			return
		}
		defer release()
		a.store.db.ExecContext(r.Context(), "UPDATE transfers SET touched_at=? WHERE id=(SELECT transfer_id FROM files WHERE id=?)", time.Now().Unix(), id)
	}
	a.uploadHandler.ServeHTTP(w, r)
}

func (a *App) publish(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	t, err := a.store.transfer(r.Context(), id)
	if err != nil {
		notFound(w)
		return
	}
	if t.Status == "published" {
		a.ownerJSON(w, t)
		return
	}
	if t.Status != "draft" {
		apiError(w, 409, "Transfer cannot be published.")
		return
	}
	for _, f := range t.Files {
		if !f.Started || f.Deleted {
			apiError(w, 409, "Finish all uploads before sharing.")
			return
		}
		payload, e := a.root.Open(f.ID)
		if e != nil {
			apiError(w, 409, "File unavailable.")
			return
		}
		stat, e := payload.Stat()
		payload.Close()
		if e != nil || !stat.Mode().IsRegular() || stat.Size() != f.Size {
			apiError(w, 409, "File length mismatch.")
			return
		}
		if !f.Uploaded {
			if e = a.syncUpload(f.ID); e != nil {
				apiError(w, 503, "Cannot finish uploaded file.")
				return
			}
			if _, e = a.store.db.ExecContext(r.Context(), "UPDATE files SET uploaded=1 WHERE id=?", f.ID); e != nil {
				apiError(w, 503, "Cannot finish uploaded file.")
				return
			}
		}
	}
	now := time.Now().Unix()
	expiry := int64(0)
	if t.ExpirySeconds > 0 {
		expiry = now + t.ExpirySeconds
	}
	token := randomToken()
	result, err := a.store.db.ExecContext(r.Context(), "UPDATE transfers SET status='published',published_at=?,expires_at=?,share_token=? WHERE id=? AND status='draft'", now, expiry, token, id)
	if err != nil {
		apiError(w, 500, "Cannot publish transfer.")
		return
	}
	changed, _ := result.RowsAffected()
	if changed != 1 {
		apiError(w, 409, "Transfer changed; refresh and retry.")
		return
	}
	t, err = a.store.transfer(r.Context(), id)
	if err != nil {
		apiError(w, 500, "Cannot read transfer.")
		return
	}
	a.ownerJSON(w, t)
}

var _ handler.DataStore = uploadStore{}
