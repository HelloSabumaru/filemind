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
	err = tx.QueryRowContext(ctx, "SELECT f.started,f.deleted,f.size,t.status FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id WHERE f.id=? AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?", info.ID, user.ID, user.AuthVersion).Scan(&started, &deleted, &size, &state)
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
	return &durableUpload{Upload: u, app: s.app, id: info.ID}, nil
}
func (s uploadStore) GetUpload(ctx context.Context, id string) (handler.Upload, error) {
	if !validID(id) {
		return nil, handler.ErrNotFound
	}
	user := userFromContext(ctx)
	var found int
	err := s.app.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id WHERE f.id=? AND f.started=1 AND f.deleted=0 AND t.status='draft' AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?", id, user.ID, user.AuthVersion).Scan(&found)
	if err != nil || found != 1 {
		return nil, handler.ErrNotFound
	}
	u, err := s.base.GetUpload(ctx, id)
	if err != nil {
		return nil, err
	}
	return &durableUpload{Upload: u, app: s.app, id: id}, nil
}

type durableUpload struct {
	handler.Upload
	app      *App
	id       string
	writeErr error
}

func (u *durableUpload) WriteChunk(ctx context.Context, offset int64, reader io.Reader) (int64, error) {
	n, err := u.Upload.WriteChunk(ctx, offset, reader)
	if syncErr := u.app.syncUpload(u.id); syncErr != nil {
		u.writeErr = syncErr
		u.app.logFailure("sync_upload_chunk", syncErr, "file_id", u.id)
		return n, handler.NewError("UPLOAD_SYNC_FAILED", "Upload could not be saved. Retry when storage is available.", 503)
	}
	u.writeErr = err
	return n, err
}

func (u *durableUpload) FinishUpload(ctx context.Context) error {
	if u.writeErr != nil {
		return u.writeErr
	}
	if err := u.Upload.FinishUpload(ctx); err != nil {
		return err
	}
	err := u.app.finalizeUpload(ctx, userFromContext(ctx), u.id)
	if err != nil {
		var p *problem
		if errors.As(err, &p) {
			return handler.NewError("UPLOAD_INVALID", p.message, p.status)
		}
		return handler.NewError("UPLOAD_UNAVAILABLE", "Upload could not be verified. Retry when storage is available.", 503)
	}
	return nil
}

func (a *App) syncUpload(id string) error {
	f, err := a.root.Open(id)
	if err != nil {
		return err
	}
	defer f.Close()
	if a.syncFile != nil {
		return a.syncFile(f)
	}
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
			err := a.store.db.QueryRowContext(event.Context, "SELECT f.name,f.size,f.deleted,t.status FROM files f JOIN transfers t ON t.id=f.transfer_id JOIN users u ON u.id=t.user_id WHERE f.id=? AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?", id, user.ID, user.AuthVersion).Scan(&name, &size, &deleted, &state)
			if err != nil || deleted || state != "draft" || size != event.Upload.Size {
				return handler.HTTPResponse{}, handler.FileInfoChanges{}, handler.NewError("INVALID_UPLOAD", "Upload unavailable", 409)
			}
			return handler.HTTPResponse{}, handler.FileInfoChanges{ID: id, MetaData: handler.MetaData{"filename": name, "file_id": id}}, nil
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
	if err = a.store.db.QueryRowContext(ctx, "SELECT t.status FROM transfers t JOIN users u ON u.id=t.user_id WHERE t.id=? AND u.id=? AND u.disabled=0 AND u.archived=0 AND u.auth_version=?", transferID, user.ID, user.AuthVersion).Scan(&state); err != nil || state != "draft" {
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
		if _, err = a.store.db.ExecContext(r.Context(), "UPDATE transfers SET touched_at=? WHERE id=(SELECT transfer_id FROM files WHERE id=?)", time.Now().Unix(), id); err != nil {
			a.operationError(w, r, "touch_upload", err)
			return
		}
	}
	a.uploadHandler.ServeHTTP(w, r)
}

func (a *App) publish(w http.ResponseWriter, r *http.Request) {
	t, err := a.publishTransfer(r.Context(), userFromContext(r.Context()), r.PathValue("id"))
	if err != nil {
		a.operationError(w, r, "publish_transfer", err)
		return
	}
	a.ownerJSON(w, t)
}

var _ handler.DataStore = uploadStore{}
