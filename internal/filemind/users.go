package filemind

import (
	"context"
	"net/http"
	"strings"
)

type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	IsAdmin      bool   `json:"isAdmin"`
	Disabled     bool   `json:"disabled"`
	StorageQuota int64  `json:"storageQuota"`
	StorageUsed  int64  `json:"storageUsed"`
	AuthVersion  int64  `json:"-"`
	PasswordHash string `json:"-"`
}

type userContextKey struct{}

func userFromContext(ctx context.Context) User {
	user, _ := ctx.Value(userContextKey{}).(User)
	return user
}

const userColumns = "u.id,u.username,u.is_admin,u.disabled,u.storage_quota,u.auth_version,u.password_hash"

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var user User
	err := row.Scan(&user.ID, &user.Username, &user.IsAdmin, &user.Disabled, &user.StorageQuota, &user.AuthVersion, &user.PasswordHash)
	return user, err
}

func validUsername(username string) bool {
	if len(username) == 0 || len(username) > 100 {
		return false
	}
	for _, c := range username {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || strings.ContainsRune("._-@", c)) {
			return false
		}
	}
	return true
}

func (a *App) adminAPI(next http.HandlerFunc) http.HandlerFunc {
	return a.accountAPI("admin", next)
}

func (a *App) transferAPI(next http.HandlerFunc) http.HandlerFunc {
	return a.ownerAPI(func(w http.ResponseWriter, r *http.Request) {
		var found int
		id := r.PathValue("id")
		err := a.store.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM transfers WHERE id=? AND user_id=?", id, userFromContext(r.Context()).ID).Scan(&found)
		if !validID(id) || err != nil || found != 1 {
			notFound(w)
			return
		}
		next(w, r)
	})
}

func (a *App) listUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := a.store.db.QueryContext(r.Context(), `SELECT `+userColumns+`,
 (SELECT COALESCE(SUM(f.size),0) FROM files f JOIN transfers t ON t.id=f.transfer_id WHERE t.user_id=u.id AND f.deleted=0)
 FROM users u ORDER BY u.is_admin DESC,u.username`)
	if err != nil {
		apiError(w, 500, "Cannot read users.")
		return
	}
	defer rows.Close()
	users := []User{}
	for rows.Next() {
		var user User
		if err = rows.Scan(&user.ID, &user.Username, &user.IsAdmin, &user.Disabled, &user.StorageQuota, &user.AuthVersion, &user.PasswordHash, &user.StorageUsed); err != nil {
			apiError(w, 500, "Cannot read users.")
			return
		}
		users = append(users, user)
	}
	if rows.Err() != nil {
		apiError(w, 500, "Cannot read users.")
		return
	}
	writeJSON(w, users)
}

func (a *App) createUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Username     string `json:"username"`
		Password     string `json:"password"`
		StorageQuota *int64 `json:"storageQuota"`
	}
	if !decode(w, r, &input) {
		return
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	settings := a.settings()
	quota := settings.DefaultUserQuota
	if input.StorageQuota != nil {
		quota = *input.StorageQuota
	}
	input.Username = strings.TrimSpace(input.Username)
	if !validUsername(input.Username) || input.Password == "" || len(input.Password) > 256 || quota < 0 || quota > settings.StorageQuota {
		apiError(w, 400, "Check the username, password and quota.")
		return
	}
	select {
	case a.hashSlots <- struct{}{}:
		defer func() { <-a.hashSlots }()
	default:
		apiError(w, 429, "Password verification busy.")
		return
	}
	hash, err := hashPassword(input.Password)
	if err != nil {
		apiError(w, 500, "Cannot create account.")
		return
	}
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "Cannot create account.")
		return
	}
	defer tx.Rollback()
	var count, duplicate int
	if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*),COALESCE(SUM(username=?),0) FROM users", input.Username).Scan(&count, &duplicate); err != nil {
		apiError(w, 500, "Cannot create account.")
		return
	}
	if duplicate > 0 {
		apiError(w, 409, "Username already exists.")
		return
	}
	if count >= 1000 {
		apiError(w, 409, "Account limit reached.")
		return
	}
	user := User{ID: randomID(), Username: input.Username, StorageQuota: quota}
	if _, err = tx.ExecContext(r.Context(), "INSERT INTO users(id,username,password_hash,storage_quota) VALUES(?,?,?,?)", user.ID, user.Username, hash, user.StorageQuota); err != nil {
		apiError(w, 500, "Cannot create account.")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "Cannot create account.")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusCreated)
	writeJSON(w, user)
}

func (a *App) editUser(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Password     *string `json:"password"`
		Disabled     *bool   `json:"disabled"`
		StorageQuota *int64  `json:"storageQuota"`
	}
	if !decode(w, r, &input) {
		return
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	id := r.PathValue("id")
	if !validID(id) {
		apiError(w, 404, "Account unavailable.")
		return
	}
	if input.StorageQuota != nil && (*input.StorageQuota < 0 || *input.StorageQuota > a.settings().StorageQuota) {
		apiError(w, 400, "Invalid storage quota.")
		return
	}
	var hash string
	if input.Password != nil {
		if *input.Password == "" || len(*input.Password) > 256 {
			apiError(w, 400, "Password must contain 1–256 bytes.")
			return
		}
		select {
		case a.hashSlots <- struct{}{}:
			defer func() { <-a.hashSlots }()
		default:
			apiError(w, 429, "Password verification busy.")
			return
		}
		var err error
		hash, err = hashPassword(*input.Password)
		if err != nil {
			apiError(w, 500, "Cannot change account.")
			return
		}
	}
	a.uploadMu.Lock()
	defer a.uploadMu.Unlock()
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "Cannot change account.")
		return
	}
	defer tx.Rollback()
	user, err := scanUser(tx.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users u WHERE u.id=?", id))
	if err != nil {
		apiError(w, 404, "Account unavailable.")
		return
	}
	if user.IsAdmin && input.Disabled != nil && *input.Disabled {
		apiError(w, 400, "The administrator cannot be disabled.")
		return
	}
	invalidate := input.Password != nil || (input.Disabled != nil && *input.Disabled != user.Disabled)
	if input.Password != nil {
		user.PasswordHash = hash
	}
	if input.Disabled != nil {
		user.Disabled = *input.Disabled
	}
	if input.StorageQuota != nil {
		user.StorageQuota = *input.StorageQuota
	}
	if invalidate {
		user.AuthVersion++
	}
	if _, err = tx.ExecContext(r.Context(), "UPDATE users SET password_hash=?,disabled=?,storage_quota=?,auth_version=? WHERE id=?", user.PasswordHash, user.Disabled, user.StorageQuota, user.AuthVersion, id); err != nil {
		apiError(w, 500, "Cannot change account.")
		return
	}
	if invalidate {
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE user_id=?", id); err != nil {
			apiError(w, 500, "Cannot change account.")
			return
		}
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "Cannot change account.")
		return
	}
	if invalidate {
		if err = a.cancelUserUploads(context.Background(), id); err != nil {
			a.logger.Error("upload cancellation failed")
		}
	}
	writeJSON(w, user)
}

func (a *App) cancelUserUploads(ctx context.Context, userID string) error {
	rows, err := a.store.db.QueryContext(ctx, "SELECT id FROM transfers WHERE user_id=? AND status='draft'", userID)
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
		a.cancelTransfer(id)
	}
	return nil
}

func (a *App) initializeDemoUser() error {
	var found int
	err := a.store.db.QueryRow("SELECT COUNT(*) FROM users WHERE username='user'").Scan(&found)
	if err != nil || found > 0 {
		return err
	}
	hash, err := hashPassword("password")
	if err != nil {
		return err
	}
	_, err = a.store.db.Exec("INSERT INTO users(id,username,password_hash,storage_quota) VALUES(?,'user',?,?)", randomID(), hash, a.settings().DefaultUserQuota)
	return err
}
