package filemind

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
)

const defaultUserQuota int64 = 20 * 1024 * 1024 * 1024

type Settings struct {
	DefaultUserQuota int64 `json:"defaultUserQuota"`
	ExpirySeconds    int64 `json:"expirySeconds"`
	DownloadLimit    int64 `json:"downloadLimit"`
	MaxFileSize      int64 `json:"maxFileSize"`
	MaxTransferSize  int64 `json:"maxTransferSize"`
	StorageQuota     int64 `json:"storageQuota"`
}

func (s Settings) validate() error {
	if s.DefaultUserQuota < 0 || s.DefaultUserQuota > s.StorageQuota || s.ExpirySeconds < 0 || s.ExpirySeconds > 31536000 || s.DownloadLimit < 0 || s.DownloadLimit > 1000000 || s.MaxFileSize <= 0 || s.MaxTransferSize < s.MaxFileSize || s.StorageQuota < s.MaxTransferSize || s.StorageQuota > 1<<50 {
		return errors.New("check quotas, upload sizes and transfer defaults")
	}
	return nil
}

func (a *App) settings() Settings { return *a.preferences.Load() }

func (a *App) initializeSettings() error {
	settings := Settings{
		DefaultUserQuota: min(defaultUserQuota, a.cfg.StorageQuota),
		ExpirySeconds:    int64(a.cfg.DefaultExpiry.Seconds()),
		DownloadLimit:    a.cfg.DefaultDownloadLimit,
		MaxFileSize:      a.cfg.MaxFileSize,
		MaxTransferSize:  a.cfg.MaxTransferSize,
		StorageQuota:     a.cfg.StorageQuota,
	}
	stored, err := a.store.setting("preferences")
	if err == nil {
		if json.Unmarshal([]byte(stored), &settings) != nil || settings.validate() != nil {
			return errors.New("invalid stored settings")
		}
	} else if errors.Is(err, sql.ErrNoRows) {
		data, err := json.Marshal(settings)
		if err != nil {
			return err
		}
		if err = a.store.putSetting("preferences", string(data)); err != nil {
			return err
		}
	} else {
		return err
	}
	a.preferences.Store(&settings)
	return nil
}

func (a *App) getSettings(w http.ResponseWriter, r *http.Request) {
	var used int64
	if err := a.store.db.QueryRowContext(r.Context(), "SELECT COALESCE(SUM(size),0) FROM files WHERE deleted=0").Scan(&used); err != nil {
		apiError(w, 500, "Cannot read settings.")
		return
	}
	writeJSON(w, struct {
		Settings
		StorageUsed int64 `json:"storageUsed"`
	}{a.settings(), used})
}

func (a *App) saveSettings(w http.ResponseWriter, r *http.Request) {
	var settings Settings
	if !decode(w, r, &settings) {
		return
	}
	if err := settings.validate(); err != nil {
		apiError(w, 400, err.Error())
		return
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "Cannot save settings.")
		return
	}
	defer tx.Rollback()
	var used int64
	if err = tx.QueryRowContext(r.Context(), "SELECT COALESCE(SUM(size),0) FROM files WHERE deleted=0").Scan(&used); err != nil {
		apiError(w, 500, "Cannot save settings.")
		return
	}
	if settings.StorageQuota < used {
		apiError(w, 409, "Storage limit is below the currently reserved space.")
		return
	}
	data, err := json.Marshal(settings)
	if err != nil {
		apiError(w, 500, "Cannot save settings.")
		return
	}
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO settings(key,value) VALUES('preferences',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", string(data)); err != nil {
		apiError(w, 500, "Cannot save settings.")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "Cannot save settings.")
		return
	}
	a.preferences.Store(&settings)
	writeJSON(w, settings)
}
