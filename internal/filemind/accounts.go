package filemind

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"time"
	"unicode/utf8"
)

const minAccountPasswordCharacters = 15

func accountPasswordMinimum(development bool) int {
	if development {
		return 1
	}
	return minAccountPasswordCharacters
}

func validateAccountPassword(password string, development bool) error {
	minimum := accountPasswordMinimum(development)
	if utf8.RuneCountInString(password) >= minimum {
		return nil
	}
	if development {
		return invalid("Enter a password.")
	}
	return invalid(fmt.Sprintf("Account passwords must contain at least %d characters.", minimum))
}

func (a *App) changePassword(w http.ResponseWriter, r *http.Request) {
	var input struct {
		CurrentPassword string `json:"currentPassword"`
		NewPassword     string `json:"newPassword"`
	}
	if !decode(w, r, &input) {
		return
	}
	if err := validateAccountPassword(input.NewPassword, a.cfg.Development); err != nil {
		a.operationError(w, r, "change_password", err)
		return
	}
	user := userFromContext(r.Context())
	source := a.accountPasswordLimiter(r)
	valid, err := a.verifyCredential(r, source, a.accountPasswords, user.ID, user.PasswordHash, input.CurrentPassword, true)
	if err != nil {
		a.operationError(w, r, "change_password", err)
		return
	}
	if !valid {
		apiError(w, 403, "Current password is incorrect.")
		return
	}
	hash, err := a.hashNewPassword(r.Context(), user, user.IsAdmin, input.NewPassword)
	if err != nil {
		a.operationError(w, r, "change_password", err)
		return
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		a.operationError(w, r, "change_password", err)
		return
	}
	defer tx.Rollback()
	if err = currentAccount(r.Context(), tx, user, adminRequest(r)); err == nil {
		_, err = tx.ExecContext(r.Context(), "UPDATE users SET password_hash=?,auth_version=auth_version+1 WHERE id=?", hash, user.ID)
	}
	if err == nil {
		_, err = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=?", user.ID)
	}
	if err == nil {
		err = tx.Commit()
	}
	if err != nil {
		a.operationError(w, r, "change_password", err)
		return
	}
	a.accountPasswords.reset(user.ID)
	if err = a.cancelUserUploads(context.Background(), user.ID); err != nil {
		a.logFailure("cancel_uploads", err, "user_id", user.ID)
	}
	a.logger.Info("account changed", "operation", "change_password", "actor_id", user.ID, "user_id", user.ID, "request_id", r.Context().Value(requestIDKey{}))
	writeJSON(w, map[string]bool{"ok": true})
}

// Archive without dropping transfer history or foreign-key references. Rename
// the tombstone so the username and the active-account slot can be reused.
func (a *App) archiveAccount(ctx context.Context, actor User, id string) error {
	if !validUserID(id) {
		return sql.ErrNoRows
	}
	a.uploadMu.Lock()
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		a.uploadMu.Unlock()
		return err
	}
	if err = currentAccount(ctx, tx, actor, true); err != nil {
		tx.Rollback()
		a.uploadMu.Unlock()
		return err
	}
	var admin, archived bool
	err = tx.QueryRowContext(ctx, "SELECT is_admin,archived FROM users WHERE id=?", id).Scan(&admin, &archived)
	if err != nil {
		tx.Rollback()
		a.uploadMu.Unlock()
		return err
	}
	if admin {
		tx.Rollback()
		a.uploadMu.Unlock()
		return invalid("The administrator cannot be deleted.")
	}
	if archived {
		tx.Rollback()
		a.uploadMu.Unlock()
		return nil
	}
	rows, err := tx.QueryContext(ctx, "SELECT id FROM transfers WHERE user_id=?", id)
	if err != nil {
		tx.Rollback()
		a.uploadMu.Unlock()
		return err
	}
	ids := []string{}
	for rows.Next() {
		var transfer string
		if err = rows.Scan(&transfer); err != nil {
			break
		}
		ids = append(ids, transfer)
	}
	if err == nil {
		err = rows.Err()
	}
	rows.Close()
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE users SET archived=1,disabled=1,username=?,auth_version=auth_version+1 WHERE id=?", "deleted-"+id, id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE user_id=?", id)
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "DELETE FROM settings WHERE key IN (?,?)", preferencesKey(id), creationRateKey(id))
	}
	if err == nil {
		_, err = tx.ExecContext(ctx, "UPDATE transfers SET status='deleted',closed_at=?,auth_version=auth_version+1,revision=revision+1 WHERE user_id=?", time.Now().Unix(), id)
	}
	if err == nil {
		err = tx.Commit()
	} else {
		tx.Rollback()
	}
	if err == nil {
		for _, transfer := range ids {
			a.cancelTransfer(transfer)
		}
	}
	a.uploadMu.Unlock()
	if err != nil {
		return err
	}
	for _, transfer := range ids {
		if err = a.purgeTransfer(ctx, transfer); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) deleteUser(w http.ResponseWriter, r *http.Request) {
	actor := userFromContext(r.Context())
	if err := a.archiveAccount(r.Context(), actor, r.PathValue("id")); err != nil {
		a.operationError(w, r, "archive_account", err)
		return
	}
	a.logger.Info("account changed", "operation", "archive_account", "actor_id", actor.ID, "user_id", r.PathValue("id"), "request_id", r.Context().Value(requestIDKey{}))
	writeJSON(w, map[string]bool{"ok": true})
}
