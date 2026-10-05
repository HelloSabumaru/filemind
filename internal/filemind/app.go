package filemind

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"html/template"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type App struct {
	cfg            Config
	store          *Store
	root           *os.Root
	lock           *os.File
	csrfKey        []byte
	loginDummyHash string
	templates      *template.Template
	logger         *slog.Logger
	uploadHandler  http.Handler
	limiter        *limiter
	ownerLimiter   *limiter
	hashSlots      chan struct{}
	downloadSlots  chan struct{}
	activeMu       sync.Mutex
	active         map[string]map[string]context.CancelFunc
	uploadMu       sync.Mutex
	cleanupMu      sync.Mutex
	spaceMu        sync.Mutex
	writeReserved  int64
	settingsMu     sync.Mutex
	preferences    atomic.Pointer[Settings]
}

func New(cfg Config, logger *slog.Logger) (_ *App, err error) {
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.OwnerURL, cfg.PublicURL = canonicalOrigin(cfg.OwnerURL), canonicalOrigin(cfg.PublicURL)
	if logger == nil {
		logger = slog.Default()
	}
	passwordBytes, e := os.ReadFile(cfg.OwnerPasswordFile)
	if e != nil {
		return nil, errors.New("cannot read owner password file")
	}
	password := strings.TrimRight(string(passwordBytes), "\r\n")
	clear(passwordBytes)
	if password == "" || len(password) > 256 {
		return nil, errors.New("owner password must contain 1–256 bytes")
	}
	if !cfg.Development && len(password) < 16 {
		return nil, errors.New("owner password must contain 16–256 bytes")
	}
	if err = os.MkdirAll(filepath.Join(cfg.DataDir, "uploads"), 0700); err != nil {
		return nil, storeError(err)
	}
	if err = os.Chmod(cfg.DataDir, 0700); err != nil {
		return nil, storeError(err)
	}
	lock, e := os.OpenFile(filepath.Join(cfg.DataDir, "filemind.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		lock.Close()
		return nil, errors.New("data directory is already in use")
	}
	a := &App{cfg: cfg, lock: lock, logger: logger, hashSlots: make(chan struct{}, 2), downloadSlots: make(chan struct{}, 8), active: make(map[string]map[string]context.CancelFunc), limiter: newLimiter(), ownerLimiter: newLimiter()}
	defer func() {
		if err != nil {
			a.Close()
		}
	}()
	a.store, err = openStore(filepath.Join(cfg.DataDir, "filemind.sqlite"))
	if err != nil {
		return nil, err
	}
	if err = os.Chmod(filepath.Join(cfg.DataDir, "filemind.sqlite"), 0600); err != nil {
		return nil, err
	}
	a.root, err = os.OpenRoot(filepath.Join(cfg.DataDir, "uploads"))
	if err != nil {
		return nil, err
	}
	if err = a.initializeSettings(); err != nil {
		return nil, err
	}
	if err = a.initializeCredentials(password); err != nil {
		return nil, err
	}
	if cfg.Demo {
		if err = a.initializeDemoUser(); err != nil {
			return nil, err
		}
	}
	key, e := a.store.setting("csrf_key")
	if errors.Is(e, sql.ErrNoRows) {
		key = randomToken()
		e = a.store.putSetting("csrf_key", key)
	}
	if e != nil {
		return nil, e
	}
	a.csrfKey, err = base64.RawURLEncoding.DecodeString(key)
	if err != nil || len(a.csrfKey) != 32 {
		return nil, errors.New("invalid stored CSRF key")
	}
	a.templates, err = template.New("pages").Funcs(template.FuncMap{"bytes": formatBytes}).ParseFS(webFiles, "web/*.html")
	if err != nil {
		return nil, err
	}
	if err = a.recover(); err != nil {
		return nil, err
	}
	if err = a.initializeUploads(); err != nil {
		return nil, err
	}
	if err = a.cleanup(); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *App) Close() {
	a.activeMu.Lock()
	for _, entries := range a.active {
		for _, cancel := range entries {
			cancel()
		}
	}
	a.activeMu.Unlock()
	if a.root != nil {
		a.root.Close()
	}
	if a.store != nil {
		a.store.db.Close()
	}
	if a.lock != nil {
		a.lock.Close()
	}
}

func (a *App) Background(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.cleanup(); err != nil {
				a.logger.Error("cleanup failed")
			}
		}
	}
}

func freeSpace(path string) (int64, error) {
	var s syscall.Statfs_t
	if err := syscall.Statfs(path, &s); err != nil {
		return 0, err
	}
	return int64(s.Bavail) * int64(s.Bsize), nil
}

func (a *App) recover() error {
	if _, err := a.store.db.Exec("DELETE FROM reservations"); err != nil {
		return err
	}
	rows, err := a.store.db.Query("SELECT f.id,f.started,f.uploaded,f.deleted,f.size,t.status FROM files f JOIN transfers t ON t.id=f.transfer_id")
	if err != nil {
		return err
	}
	type entry struct {
		id                         string
		started, uploaded, deleted bool
		size                       int64
		status                     string
	}
	entries := []entry{}
	known := map[string]bool{}
	for rows.Next() {
		var e entry
		if err = rows.Scan(&e.id, &e.started, &e.uploaded, &e.deleted, &e.size, &e.status); err != nil {
			rows.Close()
			return err
		}
		entries = append(entries, e)
		known[e.id] = true
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !validID(e.id) {
			return errors.New("invalid stored file identifier")
		}
		f, openErr := a.root.Open(e.id)
		if errors.Is(openErr, os.ErrNotExist) {
			if e.status == "draft" {
				_, err = a.store.db.Exec("UPDATE files SET started=0,uploaded=0 WHERE id=?", e.id)
			} else {
				_, err = a.store.db.Exec("UPDATE files SET deleted=1 WHERE id=?", e.id)
			}
			if err != nil {
				return err
			}
			continue
		}
		if openErr != nil {
			return openErr
		}
		stat, statErr := f.Stat()
		f.Close()
		if statErr != nil {
			return statErr
		}
		if !stat.Mode().IsRegular() || stat.Size() > e.size {
			return errors.New("invalid stored payload")
		}
		if e.status == "draft" && stat.Size() == e.size && e.started {
			_, err = a.store.db.Exec("UPDATE files SET uploaded=1 WHERE id=?", e.id)
			if err != nil {
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
		id := strings.TrimSuffix(item.Name(), ".info")
		if validID(id) && !known[id] {
			if err = a.root.Remove(item.Name()); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (a *App) cleanup() error {
	a.cleanupMu.Lock()
	defer a.cleanupMu.Unlock()
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	now := time.Now().Unix()
	rows, err := a.store.db.Query("SELECT id FROM transfers WHERE (status IN ('published','revoked') AND expires_at>0 AND expires_at<=?) OR (status='draft' AND touched_at<=?)", now, now-86400)
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
		if err = a.store.closeTransfer(context.Background(), id, "expired"); err != nil {
			return err
		}
		a.cancelTransfer(id)
	}
	rows, err = a.store.db.Query(`SELECT f.id FROM files f JOIN transfers t ON t.id=f.transfer_id WHERE NOT EXISTS(SELECT 1 FROM reservations r WHERE r.file_id=f.id) AND (f.deleted=1 OR t.status IN ('expired','deleted') OR (t.status='published' AND t.download_limit>0 AND f.downloads>=t.download_limit))`)
	if err != nil {
		return err
	}
	files := []string{}
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		files = append(files, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range files {
		if !validID(id) {
			return errors.New("invalid stored file identifier")
		}
		for _, name := range []string{id, id + ".info"} {
			if err = a.root.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if _, err = a.store.db.Exec("UPDATE files SET deleted=1 WHERE id=?", id); err != nil {
			return err
		}
	}
	_, err = a.store.db.Exec(`UPDATE transfers SET status='exhausted',closed_at=? WHERE status='published' AND NOT EXISTS(SELECT 1 FROM files f WHERE f.transfer_id=transfers.id AND f.deleted=0)`, now)
	if err != nil {
		return err
	}
	_, err = a.store.db.Exec("DELETE FROM transfers WHERE closed_at>0 AND closed_at<? AND NOT EXISTS(SELECT 1 FROM files f WHERE f.transfer_id=transfers.id AND f.deleted=0)", now-30*86400)
	if err != nil {
		return err
	}
	_, err = a.store.db.Exec("DELETE FROM sessions WHERE expires_at<=?", now)
	return err
}

func (a *App) cancelTransfer(id string) {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	for _, cancel := range a.active[id] {
		cancel()
	}
}

func (a *App) activate(ctx context.Context, transfer, id string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	a.activeMu.Lock()
	if a.active[transfer] == nil {
		a.active[transfer] = map[string]context.CancelFunc{}
	}
	a.active[transfer][id] = cancel
	a.activeMu.Unlock()
	return ctx, func() {
		cancel()
		a.activeMu.Lock()
		delete(a.active[transfer], id)
		if len(a.active[transfer]) == 0 {
			delete(a.active, transfer)
		}
		a.activeMu.Unlock()
	}
}
