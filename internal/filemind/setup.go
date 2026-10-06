package filemind

import (
	"context"
	"net/http"
	"strconv"
	"strings"
)

const setupPendingQuery = `SELECT NOT EXISTS(SELECT 1 FROM settings WHERE key='setup_complete')
 AND NOT EXISTS(SELECT 1 FROM users)`

func (a *App) setupPending(ctx context.Context) (bool, error) {
	var pending bool
	err := a.store.db.QueryRowContext(ctx, setupPendingQuery).Scan(&pending)
	return pending, err
}

func (a *App) setupPage(w http.ResponseWriter, r *http.Request) {
	pending, err := a.setupPending(r.Context())
	if err != nil {
		a.operationError(w, r, "read_setup", err)
		return
	}
	if !pending {
		http.Redirect(w, r, withTheme(r, "/login"), http.StatusSeeOther)
		return
	}
	a.render(w, "owner", pageData{Page: "setup", AdminSurface: true,
		CSRF:                   a.csrfToken(w, r, "admin-setup", "/setup"),
		AccountPasswordMinimum: accountPasswordMinimum(a.cfg.Development)})
}

func (a *App) setup(w http.ResponseWriter, r *http.Request) {
	pending, err := a.setupPending(r.Context())
	if err != nil {
		a.operationError(w, r, "read_setup", err)
		return
	}
	if !pending {
		notFound(w)
		return
	}
	if !a.checkCSRF(r, "admin-setup", a.cfg.AdminURL) {
		apiError(w, http.StatusForbidden, "Refresh the setup page and retry.")
		return
	}
	var input struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	input.Username = strings.TrimSpace(input.Username)
	if !validUsername(input.Username) {
		apiError(w, http.StatusBadRequest, "Use a username with letters, numbers or ._-@.")
		return
	}
	if err = validateAccountPassword(input.Password, a.cfg.Development); err != nil {
		a.operationError(w, r, "setup_admin", err)
		return
	}
	if allowed, retry := a.adminLimiter.passwordAttempt(a.clientIP(r)); !allowed {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
		apiError(w, http.StatusTooManyRequests, "Too many attempts. Try again later.")
		return
	}
	// Hash outside the database transaction and use bounded admin capacity.
	hash, err := a.hashNewPassword(r.Context(), User{ID: "initial-admin"}, true, input.Password)
	if err != nil {
		a.operationError(w, r, "setup_admin", err)
		return
	}
	user, err := a.createInitialAdmin(r.Context(), input.Username, hash)
	if err == nil {
		err = a.grantSession(w, r, "admin", Transfer{}, user)
	}
	if err != nil {
		a.operationError(w, r, "setup_admin", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) createInitialAdmin(ctx context.Context, username, hash string) (User, error) {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer tx.Rollback()
	var pending bool
	if err = tx.QueryRowContext(ctx, setupPendingQuery).Scan(&pending); err != nil {
		return User{}, err
	}
	if !pending {
		return User{}, conflict("Setup has already been completed. Sign in to continue.")
	}
	user := User{ID: newUserID(), Username: username, PasswordHash: hash,
		IsAdmin: true, AuthVersion: 1, StorageQuota: a.settings().DefaultUserQuota}
	if _, err = tx.ExecContext(ctx, `INSERT INTO users(id,username,password_hash,is_admin,storage_quota)
 VALUES(?,?,?,1,?)`, user.ID, user.Username, user.PasswordHash, user.StorageQuota); err != nil {
		return User{}, err
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO settings(key,value) VALUES('setup_complete','1')"); err != nil {
		return User{}, err
	}
	if err = tx.Commit(); err != nil {
		return User{}, err
	}
	return user, nil
}
