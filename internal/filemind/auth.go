package filemind

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"
)

func hashPassword(password string) (string, error) {
	if password == "" {
		return "", errors.New("password is required")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" || parts[2] != "v=19" || parts[3] != "m=19456,t=2,p=1" {
		return false
	}
	salt, e1 := base64.RawStdEncoding.DecodeString(parts[4])
	expected, e2 := base64.RawStdEncoding.DecodeString(parts[5])
	if e1 != nil || e2 != nil || len(salt) != 16 || len(expected) != 32 {
		return false
	}
	actual := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return subtle.ConstantTimeCompare(actual, expected) == 1
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
func (a *App) cookieName(kind string) string {
	prefix := "__Secure-"
	if a.cfg.Development {
		prefix = ""
	}
	return prefix + "filemind-" + kind
}
func (a *App) ownerToken(r *http.Request) string {
	return a.sessionToken(r, "owner")
}
func sessionCookie(kind string) string {
	if kind == "admin" {
		return "admin-session"
	}
	return "session"
}
func (a *App) sessionToken(r *http.Request, kind string) string {
	c, e := r.Cookie(a.cookieName(sessionCookie(kind)))
	if e != nil || !validToken(c.Value) {
		return ""
	}
	return c.Value
}
func (a *App) authenticatedUser(r *http.Request) (User, error) {
	return a.authenticatedAccount(r, "owner")
}
func (a *App) authenticatedAccount(r *http.Request, kind string) (User, error) {
	token := a.sessionToken(r, kind)
	if token == "" {
		return User{}, sql.ErrNoRows
	}
	user, err := scanUser(a.store.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=? AND s.kind=? AND s.auth_version=u.auth_version AND s.expires_at>? AND u.disabled=0 AND u.archived=0", tokenHash(token), kind, time.Now().Unix()))
	if err == nil && ((kind == "admin" && !user.IsAdmin) || (kind == "owner" && user.IsAdmin && a.cfg.RequirePrivateAdminSignIn)) {
		return User{}, sql.ErrNoRows
	}
	return user, err
}

const (
	maxSessionsPerKind        = 10000
	maxSessionsPerAccount     = 32
	maxSessionsPerTransfer    = 256
	maxPublicSessionsPerOwner = 1024
)

// Successful authentication at the subject's limit retires only its oldest
// sessions. It must not lock the account out or evict another subject's session.
func capSessionSubject(ctx context.Context, tx *sql.Tx, kind, column, subject string, maximum int) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sessions WHERE kind=? AND "+column+"=?", kind, subject).Scan(&count); err != nil {
		return err
	}
	if count < maximum {
		return nil
	}
	// column is an internal constant; rowid breaks ties for same-second sign-ins.
	_, err := tx.ExecContext(ctx, "DELETE FROM sessions WHERE token_hash IN (SELECT token_hash FROM sessions WHERE kind=? AND "+column+"=? ORDER BY expires_at,rowid LIMIT ?)", kind, subject, count-maximum+1)
	return err
}

// Public sessions have no account user_id. Attribute them through their
// transfers, including retained closed transfers, within the allocation transaction.
func capPublicSessionOwner(ctx context.Context, tx *sql.Tx, ownerID string) error {
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sessions
 WHERE kind='share' AND transfer_id IN (SELECT id FROM transfers WHERE user_id=?)`, ownerID).Scan(&count); err != nil {
		return err
	}
	if count < maxPublicSessionsPerOwner {
		return nil
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash IN
 (SELECT token_hash FROM sessions WHERE kind='share'
 AND transfer_id IN (SELECT id FROM transfers WHERE user_id=?)
 ORDER BY expires_at,rowid LIMIT ?)`, ownerID, count-maxPublicSessionsPerOwner+1)
	return err
}

func (a *App) pruneSessionSubjects(ctx context.Context) error {
	tx, err := a.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "DELETE FROM sessions WHERE expires_at<=?", time.Now().Unix()); err != nil {
		return err
	}
	for _, subject := range []struct {
		where, column string
		maximum       int
	}{
		{"kind IN ('owner','admin')", "user_id", maxSessionsPerAccount},
		{"kind='share'", "transfer_id", maxSessionsPerTransfer},
	} {
		_, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash IN
 (SELECT token_hash FROM (SELECT token_hash,ROW_NUMBER() OVER
 (PARTITION BY kind,`+subject.column+` ORDER BY expires_at DESC,rowid DESC) AS position
 FROM sessions WHERE `+subject.where+`) WHERE position>?)`, subject.maximum)
		if err != nil {
			return err
		}
	}
	// Apply this after the per-transfer cap so both limits hold. Retain the
	// newest sessions across all of each uploader's transfers.
	if _, err = tx.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash IN
 (SELECT token_hash FROM (SELECT s.token_hash,ROW_NUMBER() OVER
 (PARTITION BY t.user_id ORDER BY s.expires_at DESC,s.rowid DESC) AS position
 FROM sessions s JOIN transfers t ON t.id=s.transfer_id WHERE s.kind='share')
 WHERE position>?)`, maxPublicSessionsPerOwner); err != nil {
		return err
	}
	return tx.Commit()
}

func (a *App) grantSession(w http.ResponseWriter, r *http.Request, kind string, t Transfer, user User) error {
	if kind == "owner" && user.IsAdmin && a.cfg.RequirePrivateAdminSignIn {
		return &problem{401, "Incorrect username or password."}
	}
	if kind == "share" && t.PasswordHash == "" {
		return nil
	}
	lifetime := time.Hour
	name := a.cookieName("share-" + t.ID)
	path := "/s/" + t.ShareToken
	switch kind {
	case "owner", "admin":
		lifetime = 24 * time.Hour
		name = a.cookieName(sessionCookie(kind))
		path = "/"
	case "share":
	default:
		return errors.New("invalid session kind")
	}
	previous := ""
	if cookie, err := r.Cookie(name); err == nil && validToken(cookie.Value) {
		previous = tokenHash(cookie.Value)
	}
	now := time.Now().Unix()
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE kind=? AND expires_at<=?", kind, now); err != nil {
		return err
	}
	var userID any
	version := t.AuthVersion
	if kind == "owner" || kind == "admin" {
		if err = currentAccount(r.Context(), tx, user, kind == "admin"); err != nil {
			return err
		}
		userID, version = user.ID, user.AuthVersion
		// A new sign-in rotates this browser's cookie and retires its old session.
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=? AND kind=?", previous, kind); err != nil {
			return err
		}
		if err = capSessionSubject(r.Context(), tx, kind, "user_id", user.ID, maxSessionsPerAccount); err != nil {
			return err
		}
	} else {
		var ownerID string
		err = tx.QueryRowContext(r.Context(), "SELECT user_id FROM transfers WHERE id=? AND status='published' AND auth_version=? AND password_hash=? AND (expires_at=0 OR expires_at>?)", t.ID, t.AuthVersion, t.PasswordHash, now).Scan(&ownerID)
		if errors.Is(err, sql.ErrNoRows) {
			return conflict("Transfer changed; refresh and retry.")
		}
		if err != nil {
			return err
		}
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE kind='share' AND transfer_id=? AND auth_version<>?", t.ID, t.AuthVersion); err != nil {
			return err
		}
		var reusable int
		if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM sessions WHERE token_hash=? AND kind='share' AND transfer_id=? AND auth_version=? AND expires_at>?", previous, t.ID, t.AuthVersion, now).Scan(&reusable); err != nil {
			return err
		}
		if reusable == 1 {
			return tx.Commit()
		}
		if _, err = tx.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=? AND kind='share' AND transfer_id=?", previous, t.ID); err != nil {
			return err
		}
		if err = capSessionSubject(r.Context(), tx, kind, "transfer_id", t.ID, maxSessionsPerTransfer); err != nil {
			return err
		}
		if err = capPublicSessionOwner(r.Context(), tx, ownerID); err != nil {
			return err
		}
	}
	var count int
	if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM sessions WHERE kind=?", kind).Scan(&count); err != nil {
		return err
	}
	if count >= maxSessionsPerKind {
		message := "Sign-in session limit reached. Try again later."
		if kind == "share" {
			message = "Download session limit reached. Try again later."
		}
		return &problem{http.StatusServiceUnavailable, message}
	}
	token := randomToken()
	_, err = tx.ExecContext(r.Context(), "INSERT INTO sessions(token_hash,kind,user_id,transfer_id,auth_version,expires_at) VALUES(?,?,?,?,?,?)", tokenHash(token), kind, userID, t.ID, version, now+int64(lifetime.Seconds()))
	if err != nil {
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: path, Secure: !a.cfg.Development, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: int(lifetime.Seconds())})
	return nil
}

func (a *App) shareAuthenticated(r *http.Request, t Transfer) bool {
	if t.PasswordHash == "" {
		return true
	}
	c, err := r.Cookie(a.cookieName("share-" + t.ID))
	if err != nil || !validToken(c.Value) {
		return false
	}
	var count int
	err = a.store.db.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM sessions WHERE token_hash=? AND kind='share' AND transfer_id=? AND auth_version=? AND expires_at>?", tokenHash(c.Value), t.ID, t.AuthVersion, time.Now().Unix()).Scan(&count)
	return err == nil && count == 1
}

func (a *App) csrfScope(r *http.Request, scope string) string {
	if scope == "account" {
		return scope + ":" + a.ownerToken(r)
	}
	if scope == "admin-account" {
		return scope + ":" + a.sessionToken(r, "admin")
	}
	return scope
}

func (a *App) csrfName(scope string) string { return a.cookieName("csrf-" + tokenHash(scope)[:16]) }
func (a *App) signCSRF(nonce, scope string) string {
	h := hmac.New(sha256.New, a.csrfKey)
	h.Write([]byte(scope + ":" + nonce))
	return base64.RawURLEncoding.EncodeToString(h.Sum(nil))
}
func (a *App) csrfToken(w http.ResponseWriter, r *http.Request, scope, path string) string {
	name := a.csrfName(scope)
	signedScope := a.csrfScope(r, scope)
	if c, err := r.Cookie(name); err == nil && a.validCSRF(c.Value, signedScope) {
		return c.Value
	}
	nonce := randomToken()
	token := nonce + "." + a.signCSRF(nonce, signedScope)
	http.SetCookie(w, &http.Cookie{Name: name, Value: token, Path: path, Secure: !a.cfg.Development, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 86400})
	return token
}

func (a *App) validCSRF(token, scope string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || !validToken(parts[0]) {
		return false
	}
	return hmac.Equal([]byte(parts[1]), []byte(a.signCSRF(parts[0], scope)))
}

func (a *App) checkCSRF(r *http.Request, scope, origin string) bool {
	if r.Header.Get("Origin") != strings.TrimRight(origin, "/") {
		return false
	}
	c, err := r.Cookie(a.csrfName(scope))
	if err != nil {
		return false
	}
	token := r.Header.Get("X-CSRF-Token")
	if token == "" && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
		if err = r.ParseForm(); err != nil {
			return false
		}
		token = r.PostForm.Get("csrf")
	}
	return hmac.Equal([]byte(c.Value), []byte(token)) && a.validCSRF(token, a.csrfScope(r, scope))
}

// Existing accounts close setup without changing their credentials. The durable
// marker keeps setup closed even if account rows are later lost or removed.
func (a *App) initializeAuthentication() error {
	dummyHash, err := hashPassword(randomToken())
	if err != nil {
		return err
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("INSERT OR IGNORE INTO settings(key,value) SELECT 'setup_complete','1' WHERE EXISTS(SELECT 1 FROM users)"); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM settings WHERE key IN ('bootstrap_user','bootstrap_hash')"); err != nil {
		return err
	}
	if a.cfg.RequirePrivateAdminSignIn {
		if _, err = tx.Exec("DELETE FROM sessions WHERE kind='owner' AND user_id IN (SELECT id FROM users WHERE is_admin=1)"); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.loginDummyHash = dummyHash
	return nil
}
