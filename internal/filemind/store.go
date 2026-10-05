package filemind

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

type Transfer struct {
	ID               string `json:"id"`
	UserID           string `json:"-"`
	Title            string `json:"title"`
	Status           string `json:"status"`
	CreatedAt        int64  `json:"createdAt"`
	PublishedAt      int64  `json:"publishedAt"`
	ExpiresAt        int64  `json:"expiresAt"`
	ExpirySeconds    int64  `json:"expirySeconds"`
	DownloadLimit    int64  `json:"downloadLimit"`
	PasswordHash     string `json:"-"`
	PasswordRequired bool   `json:"passwordRequired"`
	AuthVersion      int64  `json:"-"`
	ShareToken       string `json:"-"`
	ShareURL         string `json:"shareUrl,omitempty"`
	Revision         int64  `json:"revision"`
	OwnerUsername    string `json:"ownerUsername,omitempty"`
	OwnerID          string `json:"ownerId,omitempty"`
	Files            []File `json:"files"`
}

type File struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Size           int64  `json:"size"`
	Started        bool   `json:"started"`
	Uploaded       bool   `json:"uploaded"`
	Downloads      int64  `json:"downloads"`
	Deleted        bool   `json:"deleted"`
	Reserved       int64  `json:"inProgress"`
	SHA256         string `json:"sha256"`
	IntegrityError string `json:"integrityError,omitempty"`
}

const schemaVersion = 3

type Store struct{ db *sql.DB }

func openStore(filename string) (*Store, error) {
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, err
	}
	if version != 0 && version != schemaVersion {
		db.Close()
		return nil, errors.New("unsupported database schema; start this version with a fresh data directory")
	}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	if version == 0 {
		// Initialize only an empty database. Do not modify an unversioned existing schema.
		var tables int
		if err = db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%'").Scan(&tables); err != nil {
			db.Close()
			return nil, err
		}
		if tables != 0 {
			db.Close()
			return nil, errors.New("unsupported database schema; start this version with a fresh data directory")
		}
		tx, beginErr := db.Begin()
		if beginErr != nil {
			db.Close()
			return nil, beginErr
		}
		_, err = tx.Exec(`CREATE TABLE settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE users (
 id TEXT PRIMARY KEY, username TEXT NOT NULL COLLATE NOCASE UNIQUE,
 password_hash TEXT NOT NULL, is_admin INTEGER NOT NULL DEFAULT 0,
 disabled INTEGER NOT NULL DEFAULT 0, storage_quota INTEGER NOT NULL DEFAULT 0 CHECK(storage_quota>=0),
 auth_version INTEGER NOT NULL DEFAULT 1, archived INTEGER NOT NULL DEFAULT 0);
CREATE TABLE transfers (
 id TEXT PRIMARY KEY, user_id TEXT NOT NULL REFERENCES users(id), title TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, touched_at INTEGER NOT NULL,
 published_at INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL DEFAULT 0, expiry_seconds INTEGER NOT NULL,
 download_limit INTEGER NOT NULL, password_hash TEXT NOT NULL DEFAULT '', auth_version INTEGER NOT NULL DEFAULT 1,
 share_token TEXT UNIQUE, closed_at INTEGER NOT NULL DEFAULT 0, revision INTEGER NOT NULL DEFAULT 1);
CREATE TABLE files (
 id TEXT PRIMARY KEY, transfer_id TEXT NOT NULL REFERENCES transfers(id) ON DELETE CASCADE, name TEXT NOT NULL,
 size INTEGER NOT NULL CHECK(size>=0), started INTEGER NOT NULL DEFAULT 0, uploaded INTEGER NOT NULL DEFAULT 0,
 downloads INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0,
 sha256 TEXT NOT NULL, integrity_error TEXT NOT NULL DEFAULT '', purged INTEGER NOT NULL DEFAULT 0);
CREATE INDEX files_transfer ON files(transfer_id);
CREATE INDEX transfers_user ON transfers(user_id);
CREATE TABLE sessions (
 token_hash TEXT PRIMARY KEY, kind TEXT NOT NULL, user_id TEXT REFERENCES users(id), transfer_id TEXT NOT NULL DEFAULT '', auth_version INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL);
CREATE INDEX sessions_expiry ON sessions(expires_at);
CREATE INDEX sessions_user ON sessions(user_id);
CREATE TABLE reservations (
 id TEXT NOT NULL, file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE, PRIMARY KEY(id,file_id));
CREATE INDEX reservations_file ON reservations(file_id);
CREATE INDEX transfers_maintenance ON transfers(status,expires_at,touched_at);
CREATE INDEX files_pending_purge ON files(purged);
PRAGMA user_version=3;`)
		if err == nil {
			err = tx.Commit()
		} else {
			tx.Rollback()
		}
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

const transferColumns = `id,user_id,title,status,created_at,published_at,expires_at,expiry_seconds,download_limit,password_hash,auth_version,COALESCE(share_token,''),revision,COALESCE((SELECT username FROM users WHERE users.id=transfers.user_id),'')`

func scanTransfer(row interface{ Scan(...any) error }) (Transfer, error) {
	var t Transfer
	err := row.Scan(&t.ID, &t.UserID, &t.Title, &t.Status, &t.CreatedAt, &t.PublishedAt, &t.ExpiresAt, &t.ExpirySeconds, &t.DownloadLimit, &t.PasswordHash, &t.AuthVersion, &t.ShareToken, &t.Revision, &t.OwnerUsername)
	t.PasswordRequired = t.PasswordHash != ""
	return t, err
}

func (s *Store) transfer(ctx context.Context, id string) (Transfer, error) {
	t, err := scanTransfer(s.db.QueryRowContext(ctx, "SELECT "+transferColumns+" FROM transfers WHERE id=?", id))
	if err != nil {
		return t, err
	}
	t.Files, err = s.files(ctx, t.ID)
	return t, err
}

func (s *Store) publicTransfer(ctx context.Context, token string) (Transfer, error) {
	if !validToken(token) {
		return Transfer{}, sql.ErrNoRows
	}
	t, err := scanTransfer(s.db.QueryRowContext(ctx, "SELECT "+transferColumns+" FROM transfers WHERE share_token=? AND status='published' AND (expires_at=0 OR expires_at>?)", token, time.Now().Unix()))
	if err != nil {
		return t, err
	}
	t.Files, err = s.files(ctx, t.ID)
	if err != nil {
		return t, err
	}
	for _, f := range t.Files {
		if f.Uploaded && !f.Deleted && (t.DownloadLimit == 0 || f.Downloads < t.DownloadLimit) {
			return t, nil
		}
	}
	return Transfer{}, sql.ErrNoRows
}

func (s *Store) files(ctx context.Context, id string) ([]File, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.name,f.size,f.started,f.uploaded,f.downloads,f.deleted,(SELECT COUNT(*) FROM reservations r WHERE r.file_id=f.id),f.sha256,f.integrity_error FROM files f WHERE transfer_id=? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err = rows.Scan(&f.ID, &f.Name, &f.Size, &f.Started, &f.Uploaded, &f.Downloads, &f.Deleted, &f.Reserved, &f.SHA256, &f.IntegrityError); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) setting(key string) (string, error) {
	var value string
	err := s.db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&value)
	return value, err
}
func (s *Store) putSetting(key, value string) error {
	_, err := s.db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

func (s *Store) list(ctx context.Context, userID, search string, offset int) ([]Transfer, error) {
	search = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search)
	rows, err := s.db.QueryContext(ctx, `SELECT `+transferColumns+` FROM transfers WHERE (?='' OR user_id=?) AND (title LIKE ? ESCAPE '\' OR id IN (SELECT transfer_id FROM files WHERE name LIKE ? ESCAPE '\')) ORDER BY created_at DESC,rowid DESC LIMIT 50 OFFSET ?`, userID, userID, "%"+search+"%", "%"+search+"%", offset)
	if err != nil {
		return nil, err
	}
	out := []Transfer{}
	for rows.Next() {
		t, e := scanTransfer(rows)
		if e != nil {
			rows.Close()
			return nil, e
		}
		out = append(out, t)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}
	byID := make(map[string]int, len(out))
	ids := make([]any, len(out))
	marks := make([]string, len(out))
	for i := range out {
		byID[out[i].ID] = i
		ids[i] = out[i].ID
		marks[i] = "?"
		out[i].Files = []File{}
	}
	files, err := s.db.QueryContext(ctx, `SELECT f.transfer_id,f.id,f.name,f.size,f.started,f.uploaded,f.downloads,f.deleted,(SELECT COUNT(*) FROM reservations r WHERE r.file_id=f.id),f.sha256,f.integrity_error FROM files f WHERE f.transfer_id IN (`+strings.Join(marks, ",")+`) ORDER BY f.rowid`, ids...)
	if err != nil {
		return nil, err
	}
	defer files.Close()
	for files.Next() {
		var id string
		var f File
		if err = files.Scan(&id, &f.ID, &f.Name, &f.Size, &f.Started, &f.Uploaded, &f.Downloads, &f.Deleted, &f.Reserved, &f.SHA256, &f.IntegrityError); err != nil {
			return nil, err
		}
		i := byID[id]
		out[i].Files = append(out[i].Files, f)
	}
	if err = files.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

type newTransfer struct {
	Title         string `json:"title"`
	ExpirySeconds *int64 `json:"expirySeconds"`
	DownloadLimit *int64 `json:"downloadLimit"`
	Password      string `json:"password"`
	Files         []struct {
		Name   string `json:"name"`
		Size   int64  `json:"size"`
		SHA256 string `json:"sha256"`
	} `json:"files"`
}

func (s *Store) create(ctx context.Context, user User, input newTransfer, hash string, settings Settings) (string, error) {
	expiry, limit := settings.ExpirySeconds, settings.DownloadLimit
	if input.ExpirySeconds != nil {
		expiry = *input.ExpirySeconds
	}
	if input.DownloadLimit != nil {
		limit = *input.DownloadLimit
	}
	if len(input.Files) == 0 || len(input.Files) > 100 || expiry < 0 || expiry > 31536000 || limit < 0 || limit > 1000000 || len(input.Title) > 200 {
		return "", invalid("Invalid transfer settings.")
	}
	var total int64
	seen := map[string]map[int64]bool{}
	for _, f := range input.Files {
		if !validDigest(f.SHA256) || !validFilename(f.Name) || f.Size < 0 || f.Size > settings.MaxFileSize || f.Size > settings.MaxTransferSize-total {
			return "", invalid("Invalid filename, checksum, or upload exceeds size limits.")
		}
		if seen[f.Name] == nil {
			seen[f.Name] = map[int64]bool{}
		}
		if seen[f.Name][f.Size] {
			return "", invalid("Rename files with identical names and sizes before uploading.")
		}
		seen[f.Name][f.Size] = true
		total += f.Size
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var quota, version int64
	var disabled bool
	err = tx.QueryRowContext(ctx, "SELECT storage_quota,auth_version,(disabled OR archived) FROM users WHERE id=?", user.ID).Scan(&quota, &version, &disabled)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if err != nil || disabled || version != user.AuthVersion {
		return "", &problem{401, "Account unavailable; sign in again."}
	}
	var used, count int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size),0) FROM files WHERE deleted=0").Scan(&used); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM transfers WHERE status IN ('draft','published','revoked')").Scan(&count); err != nil {
		return "", err
	}
	if total > settings.StorageQuota-used || count >= 1000 {
		return "", conflict("Storage quota or transfer limit reached.")
	}
	if quota > 0 {
		var userUsed int64
		if err = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(f.size),0) FROM files f JOIN transfers t ON t.id=f.transfer_id WHERE t.user_id=? AND f.deleted=0", user.ID).Scan(&userUsed); err != nil {
			return "", err
		}
		if total > quota-userUsed {
			return "", conflict("Account storage quota reached.")
		}
	}
	id := randomID()
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = defaultTransferTitle(input.Files[0].Name, len(input.Files))
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transfers(id,user_id,title,status,created_at,touched_at,expiry_seconds,download_limit,password_hash) VALUES(?,?,?,'draft',?,?,?,?,?)`, id, user.ID, title, time.Now().Unix(), time.Now().Unix(), expiry, limit, hash)
	if err != nil {
		return "", err
	}
	for _, f := range input.Files {
		_, err = tx.ExecContext(ctx, "INSERT INTO files(id,transfer_id,name,size,sha256) VALUES(?,?,?,?,?)", randomID(), id, f.Name, f.Size, f.SHA256)
		if err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

func defaultTransferTitle(filename string, count int) string {
	suffix := ""
	if count > 1 {
		suffix = fmt.Sprintf(" + %d more", count-1)
	}
	if len(filename)+len(suffix) > 200 {
		filename = filename[:200-len(suffix)-len("…")]
		for !utf8.ValidString(filename) {
			filename = filename[:len(filename)-1]
		}
		filename += "…"
	}
	return filename + suffix
}

func randomID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b[:])
}
func randomToken() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}
func validToken(value string) bool {
	if len(value) != 43 {
		return false
	}
	b, e := base64.RawURLEncoding.DecodeString(value)
	return e == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == value
}
func validID(value string) bool {
	if len(value) != 32 {
		return false
	}
	_, e := hex.DecodeString(value)
	return e == nil && strings.ToLower(value) == value
}
func validFilename(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 255 || !utf8.ValidString(value) || strings.ContainsAny(value, "/\\") {
		return false
	}
	for _, r := range value {
		if r < 32 || r == 127 {
			return false
		}
	}
	return true
}

func (s *Store) reserve(ctx context.Context, t Transfer, selected []File) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var status string
	var expiry, limit, version int64
	if err = tx.QueryRowContext(ctx, "SELECT status,expires_at,download_limit,auth_version FROM transfers WHERE id=?", t.ID).Scan(&status, &expiry, &limit, &version); err != nil {
		return "", err
	}
	if status != "published" || version != t.AuthVersion || (expiry != 0 && expiry <= time.Now().Unix()) {
		return "", &problem{404, "Transfer unavailable."}
	}
	id := randomID()
	for _, f := range selected {
		var uploaded, deleted bool
		var downloads, reserved int64
		err = tx.QueryRowContext(ctx, `SELECT uploaded,deleted,downloads,(SELECT COUNT(*) FROM reservations WHERE file_id=files.id) FROM files WHERE id=? AND transfer_id=?`, f.ID, t.ID).Scan(&uploaded, &deleted, &downloads, &reserved)
		if err != nil {
			return "", err
		}
		if !uploaded || deleted || (limit > 0 && downloads >= limit) {
			return "", conflict("File unavailable. Refresh and retry.")
		}
		if limit > 0 && reserved > 0 {
			return "", conflict("A file is already downloading. Refresh and retry.")
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO reservations(id,file_id) VALUES(?,?)", id, f.ID); err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

func (s *Store) finishReservation(id string, completed bool) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if completed {
		if _, err = tx.Exec("UPDATE files SET downloads=downloads+1 WHERE id IN (SELECT file_id FROM reservations WHERE id=?)", id); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("DELETE FROM reservations WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func storeError(err error) error { return fmt.Errorf("storage operation: %w", err) }
