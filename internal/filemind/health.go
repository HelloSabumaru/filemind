package filemind

import (
	"context"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"
)

type maintenanceStatus struct {
	LastCleanup        int64 `json:"lastCleanup"`
	CleanupFailed      bool  `json:"cleanupFailed"`
	PurgeBacklog       int64 `json:"purgeBacklog"`
	FreeSpace          int64 `json:"freeSpace"`
	ActiveDownloads    int   `json:"activeDownloads"`
	ActiveStreams      int   `json:"activeStreams"`
	DatabaseWaitCount  int64 `json:"databaseWaitCount"`
	DatabaseWaitMillis int64 `json:"databaseWaitMillis"`
}

func (a *App) maintenance(ctx context.Context) (maintenanceStatus, error) {
	s := maintenanceStatus{LastCleanup: a.lastCleanup.Load(), CleanupFailed: a.cleanupFailed.Load(), ActiveDownloads: len(a.downloadSlots)}
	stats := a.store.db.Stats()
	s.DatabaseWaitCount = stats.WaitCount
	s.DatabaseWaitMillis = stats.WaitDuration.Milliseconds()
	a.activeMu.Lock()
	for _, entries := range a.active {
		s.ActiveStreams += len(entries)
	}
	a.activeMu.Unlock()
	err := a.store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files f JOIN transfers t ON t.id=f.transfer_id WHERE f.purged=0
 AND (f.deleted=1 OR t.status IN ('expired','deleted') OR (t.status='published' AND t.download_limit>0 AND f.downloads>=t.download_limit))`).Scan(&s.PurgeBacklog)
	if err != nil {
		return s, err
	}
	s.FreeSpace, err = freeSpace(a.cfg.DataDir)
	return s, err
}

// Cache expensive durable probes so a public health endpoint cannot force a
// disk sync on every request. The periodic worker also checks readiness.
func (a *App) probePersistence(ctx context.Context) error {
	a.healthMu.Lock()
	defer a.healthMu.Unlock()
	if time.Since(a.healthChecked) < 30*time.Second {
		return a.healthError
	}
	a.healthChecked = time.Now()
	a.healthError = a.checkPersistence(ctx)
	if a.healthError != nil {
		a.logFailure("persistence_probe", a.healthError)
	}
	return a.healthError
}

func (a *App) checkPersistence(ctx context.Context) error {
	var admins int
	if err := a.store.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM users WHERE is_admin=1 AND disabled=0 AND archived=0").Scan(&admins); err != nil {
		return err
	}
	if admins == 0 {
		pending, err := a.setupPending(ctx)
		if err != nil {
			return err
		}
		if !pending {
			return errors.New("administrator unavailable")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := a.root.OpenFile(".health", os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer a.root.Remove(".health")
	_, err = file.Write([]byte{0})
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
	if _, err = a.store.db.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('health_check',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", strconv.FormatInt(time.Now().UnixNano(), 10)); err != nil {
		return err
	}
	return a.syncUpload(".")
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	space, err := freeSpace(a.cfg.DataDir)
	if err == nil && space >= a.cfg.MinFreeSpace {
		err = a.probePersistence(ctx)
	} else if err == nil {
		err = errors.New("insufficient free space")
	}
	if err != nil || a.cleanupFailed.Load() || time.Now().Unix()-a.lastCleanup.Load() > 90 {
		http.Error(w, "unhealthy", 503)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Write([]byte("ok\n"))
}
