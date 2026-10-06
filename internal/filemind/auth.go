package filemind

import (
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
	if err == nil && kind == "admin" && !user.IsAdmin {
		return User{}, sql.ErrNoRows
	}
	return user, err
}

const maxSessionsPerKind = 10000

func (a *App) grantSession(w http.ResponseWriter, r *http.Request, kind string, t Transfer, user User) error {
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
	} else {
		var active int
		if err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM transfers WHERE id=? AND status='published' AND auth_version=? AND password_hash=? AND (expires_at=0 OR expires_at>?)", t.ID, t.AuthVersion, t.PasswordHash, now).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return conflict("Transfer changed; refresh and retry.")
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

func (a *App) initializeCredentials(password string) error {
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var id, bootstrapHash string
	err = tx.QueryRow("SELECT value FROM settings WHERE key='bootstrap_user'").Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		id = newUserID()
		bootstrapHash, err = hashPassword(password)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO users(id,username,password_hash,is_admin,storage_quota) VALUES(?,?,?,1,?)", id, a.cfg.OwnerUsername, bootstrapHash, a.settings().DefaultUserQuota); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO settings(key,value) VALUES('bootstrap_user',?),('bootstrap_hash',?)", id, bootstrapHash); err != nil {
			return err
		}
	} else if err != nil {
		return err
	} else {
		user, err := scanUser(tx.QueryRow("SELECT "+userColumns+" FROM users u WHERE u.id=?", id))
		if err != nil || !user.IsAdmin || user.Disabled {
			return errors.New("invalid stored administrator")
		}
		if err = tx.QueryRow("SELECT value FROM settings WHERE key='bootstrap_hash'").Scan(&bootstrapHash); err != nil {
			return err
		}
		changed := !verifyPassword(bootstrapHash, password)
		if changed {
			bootstrapHash, err = hashPassword(password)
			if err != nil {
				return err
			}
			if _, err = tx.Exec("UPDATE settings SET value=? WHERE key='bootstrap_hash'", bootstrapHash); err != nil {
				return err
			}
			user.PasswordHash = bootstrapHash
		}
		if changed || user.Username != a.cfg.OwnerUsername {
			if _, err = tx.Exec("UPDATE users SET username=?,password_hash=?,auth_version=auth_version+1 WHERE id=?", a.cfg.OwnerUsername, user.PasswordHash, id); err != nil {
				return err
			}
			if _, err = tx.Exec("DELETE FROM sessions WHERE user_id=?", id); err != nil {
				return err
			}
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	a.loginDummyHash = bootstrapHash
	return nil
}
