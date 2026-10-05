package filemind

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
)

const defaultDateFormat = "dd/mm/yyyy"

type UserPreferences struct {
	DateFormat string `json:"dateFormat"`
}

func validDateFormat(format string) bool {
	return format == defaultDateFormat || format == "mm/dd/yyyy" || format == "yyyy-mm-dd"
}

func preferencesKey(userID string) string { return "user_preferences:" + userID }

func (a *App) userPreferences(ctx context.Context, userID string) (UserPreferences, error) {
	preferences := UserPreferences{DateFormat: defaultDateFormat}
	var stored string
	err := a.store.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", preferencesKey(userID)).Scan(&stored)
	if errors.Is(err, sql.ErrNoRows) {
		return preferences, nil
	}
	if err != nil {
		return preferences, err
	}
	if json.Unmarshal([]byte(stored), &preferences) != nil || !validDateFormat(preferences.DateFormat) {
		return preferences, errors.New("invalid stored user preferences")
	}
	return preferences, nil
}

func (a *App) getUserPreferences(w http.ResponseWriter, r *http.Request) {
	preferences, err := a.userPreferences(r.Context(), userFromContext(r.Context()).ID)
	if err != nil {
		a.operationError(w, r, "read_preferences", err)
		return
	}
	writeJSON(w, preferences)
}

func (a *App) saveUserPreferences(w http.ResponseWriter, r *http.Request) {
	var preferences UserPreferences
	if !decode(w, r, &preferences) {
		return
	}
	if !validDateFormat(preferences.DateFormat) {
		apiError(w, 400, "Choose a valid date format.")
		return
	}
	user := userFromContext(r.Context())
	data, err := json.Marshal(preferences)
	if err != nil {
		a.operationError(w, r, "save_preferences", err)
		return
	}
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.operationError(w, r, "save_preferences", err)
		return
	}
	defer tx.Rollback()
	if err = currentAccount(r.Context(), tx, user, adminRequest(r)); err == nil {
		_, err = tx.ExecContext(r.Context(), "INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", preferencesKey(user.ID), string(data))
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.operationError(w, r, "save_preferences", err)
		return
	}
	writeJSON(w, preferences)
}
