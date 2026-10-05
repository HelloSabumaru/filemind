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
	if password == "" || len(password) > 256 {
		return "", errors.New("password must contain 1–256 bytes")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt, 2, 19*1024, 1, 32)
	return fmt.Sprintf("$argon2id$v=19$m=19456,t=2,p=1$%s$%s", base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(encoded, password string) bool {
	if len(password) > 256 {
		return false
	}
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
	c, e := r.Cookie(a.cookieName("session"))
	if e != nil || !validToken(c.Value) {
		return ""
	}
	return c.Value
}
func (a *App) authenticatedUser(r *http.Request) (User, error) {
	token := a.ownerToken(r)
	if token == "" {
		return User{}, sql.ErrNoRows
	}
	return scanUser(a.store.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users u JOIN sessions s ON s.user_id=u.id WHERE s.token_hash=? AND s.kind='owner' AND s.auth_version=u.auth_version AND s.expires_at>? AND u.disabled=0", tokenHash(token), time.Now().Unix()))
}

func (a *App) grantSession(w http.ResponseWriter, kind string, t Transfer, user User) error {
	token := randomToken()
	lifetime := time.Hour
	name := a.cookieName("share-" + t.ID)
	path := "/s/" + t.ShareToken
	if kind == "owner" {
		lifetime = 24 * time.Hour
		name = a.cookieName("session")
		path = "/"
	}
	tx, err := a.store.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("DELETE FROM sessions WHERE expires_at<=?", time.Now().Unix()); err != nil {
		return err
	}
	var count int
	if err = tx.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
		return err
	}
	if count >= 10000 {
		return errors.New("session capacity reached")
	}
	var userID any
	version := t.AuthVersion
	if kind == "owner" {
		var active int
		if err = tx.QueryRow("SELECT COUNT(*) FROM users WHERE id=? AND auth_version=? AND disabled=0", user.ID, user.AuthVersion).Scan(&active); err != nil {
			return err
		}
		if active != 1 {
			return errors.New("account changed; sign in again")
		}
		userID, version = user.ID, user.AuthVersion
	}
	_, err = tx.Exec("INSERT INTO sessions(token_hash,kind,user_id,transfer_id,auth_version,expires_at) VALUES(?,?,?,?,?,?)", tokenHash(token), kind, userID, t.ID, version, time.Now().Add(lifetime).Unix())
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
		id = randomID()
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
