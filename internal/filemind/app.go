package filemind

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
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

	"github.com/tus/tusd/v2/pkg/handler"
)

type App struct {
	cfg                Config
	store              *Store
	root               *os.Root
	lock               *os.File
	csrfKey            []byte
	loginDummyHash     string
	templates          *template.Template
	logger             *slog.Logger
	uploadHandler      http.Handler
	adminUploadHandler http.Handler
	limiter            *limiter
	ownerLimiter       *limiter
	adminLimiter       *limiter
	accountPasswords   *passwordLimiter
	unknownPasswords   *passwordLimiter
	transferPasswords  *passwordLimiter
	hashSlots          chan struct{}
	adminHashSlots     chan struct{}
	hashUsers          *userWorkLimit
	integrityUsers     *userWorkLimit
	uploadUsers        *userWorkLimit
	uploadLocker       handler.Locker
	downloadSlots      chan struct{}
	integritySlots     chan struct{}
	syncFile           func(*os.File) error
	activeMu           sync.Mutex
	active             map[string]map[string]context.CancelFunc
	uploadMu           sync.Mutex
	cleanupMu          sync.Mutex
	spaceMu            sync.Mutex
	writeReserved      int64
	settingsMu         sync.Mutex
	preferences        atomic.Pointer[Settings]
	lastCleanup        atomic.Int64
	cleanupFailed      atomic.Bool
	healthMu           sync.Mutex
	healthChecked      time.Time
	healthError        error
}

func New(cfg Config, logger *slog.Logger) (_ *App, err error) {
	if err = cfg.Validate(); err != nil {
		return nil, err
	}
	cfg.OwnerURL, cfg.PublicURL = canonicalOrigin(cfg.OwnerURL), canonicalOrigin(cfg.PublicURL)
	cfg.AdminURL = canonicalOrigin(cfg.AdminURL)
	if logger == nil {
		logger = slog.Default()
	}
	passwordBytes, e := os.ReadFile(cfg.OwnerPasswordFile)
	if e != nil {
		return nil, errors.New("cannot read owner password file")
	}
	password := strings.TrimRight(string(passwordBytes), "\r\n")
	clear(passwordBytes)
	if err := validateAccountPassword(password, cfg.Development); err != nil {
		return nil, fmt.Errorf("administrator bootstrap password: %w", err)
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
	a := &App{cfg: cfg, lock: lock, logger: logger, hashSlots: make(chan struct{}, 2), downloadSlots: make(chan struct{}, 8), integritySlots: make(chan struct{}, 2), active: make(map[string]map[string]context.CancelFunc), limiter: newLimiter(), ownerLimiter: newLimiter(), adminLimiter: newLimiter()}
	a.adminHashSlots = make(chan struct{}, 1)
	a.hashUsers, a.integrityUsers, a.uploadUsers = newUserWorkLimit(1), newUserWorkLimit(1), newUserWorkLimit(2)
	a.accountPasswords, a.unknownPasswords, a.transferPasswords = newPasswordLimiter(), newPasswordLimiter(), newPasswordLimiter()
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
	if err = a.pruneSessionSubjects(context.Background()); err != nil {
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
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := a.cleanup(); err != nil {
				a.logFailure("cleanup", err)
			}
			probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
			a.probePersistence(probeCtx)
			cancel()
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

func (a *App) transferActive(id string) bool {
	a.activeMu.Lock()
	defer a.activeMu.Unlock()
	return len(a.active[id]) > 0
}
