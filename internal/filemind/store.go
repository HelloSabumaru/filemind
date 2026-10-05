package filemind

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	mathrand "math/rand/v2"
	"strings"
	"time"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

type Transfer struct {
	ID               string `json:"id"`
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
	Files            []File `json:"files"`
}

type File struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Started   bool   `json:"started"`
	Uploaded  bool   `json:"uploaded"`
	Downloads int64  `json:"downloads"`
	Deleted   bool   `json:"deleted"`
	Reserved  int64  `json:"inProgress"`
}

type Store struct{ db *sql.DB }

func openStore(filename string) (*Store, error) {
	db, err := sql.Open("sqlite", filename)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || (version != 0 && version != 1) {
		db.Close()
		return nil, errors.New("unsupported database schema")
	}
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS transfers (
 id TEXT PRIMARY KEY, title TEXT NOT NULL, status TEXT NOT NULL, created_at INTEGER NOT NULL, touched_at INTEGER NOT NULL,
 published_at INTEGER NOT NULL DEFAULT 0, expires_at INTEGER NOT NULL DEFAULT 0, expiry_seconds INTEGER NOT NULL,
 download_limit INTEGER NOT NULL, password_hash TEXT NOT NULL DEFAULT '', auth_version INTEGER NOT NULL DEFAULT 1,
 share_token TEXT UNIQUE, closed_at INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS files (
 id TEXT PRIMARY KEY, transfer_id TEXT NOT NULL REFERENCES transfers(id) ON DELETE CASCADE, name TEXT NOT NULL,
 size INTEGER NOT NULL CHECK(size>=0), started INTEGER NOT NULL DEFAULT 0, uploaded INTEGER NOT NULL DEFAULT 0,
 downloads INTEGER NOT NULL DEFAULT 0, deleted INTEGER NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS files_transfer ON files(transfer_id);
CREATE TABLE IF NOT EXISTS sessions (
 token_hash TEXT PRIMARY KEY, kind TEXT NOT NULL, transfer_id TEXT NOT NULL DEFAULT '', auth_version INTEGER NOT NULL DEFAULT 0,
 expires_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS sessions_expiry ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS reservations (
 id TEXT NOT NULL, file_id TEXT NOT NULL REFERENCES files(id) ON DELETE CASCADE, PRIMARY KEY(id,file_id));
PRAGMA user_version=1;`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db}, nil
}

const transferColumns = `id,title,status,created_at,published_at,expires_at,expiry_seconds,download_limit,password_hash,auth_version,COALESCE(share_token,'')`

func scanTransfer(row interface{ Scan(...any) error }) (Transfer, error) {
	var t Transfer
	err := row.Scan(&t.ID, &t.Title, &t.Status, &t.CreatedAt, &t.PublishedAt, &t.ExpiresAt, &t.ExpirySeconds, &t.DownloadLimit, &t.PasswordHash, &t.AuthVersion, &t.ShareToken)
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
	rows, err := s.db.QueryContext(ctx, `SELECT f.id,f.name,f.size,f.started,f.uploaded,f.downloads,f.deleted,(SELECT COUNT(*) FROM reservations r WHERE r.file_id=f.id) FROM files f WHERE transfer_id=? ORDER BY rowid`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []File{}
	for rows.Next() {
		var f File
		if err = rows.Scan(&f.ID, &f.Name, &f.Size, &f.Started, &f.Uploaded, &f.Downloads, &f.Deleted, &f.Reserved); err != nil {
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

func (s *Store) list(ctx context.Context, search string, offset int) ([]Transfer, error) {
	search = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(search)
	rows, err := s.db.QueryContext(ctx, `SELECT `+transferColumns+` FROM transfers WHERE title LIKE ? ESCAPE '\' OR id IN (SELECT transfer_id FROM files WHERE name LIKE ? ESCAPE '\') ORDER BY created_at DESC,rowid DESC LIMIT 50 OFFSET ?`, "%"+search+"%", "%"+search+"%", offset)
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
	for i := range out {
		out[i].Files, err = s.files(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

type newTransfer struct {
	Title         string `json:"title"`
	ExpirySeconds *int64 `json:"expirySeconds"`
	DownloadLimit *int64 `json:"downloadLimit"`
	Password      string `json:"password"`
	Files         []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

func (s *Store) create(ctx context.Context, input newTransfer, hash string, cfg Config) (string, error) {
	expiry, limit := int64(cfg.DefaultExpiry.Seconds()), cfg.DefaultDownloadLimit
	if input.ExpirySeconds != nil {
		expiry = *input.ExpirySeconds
	}
	if input.DownloadLimit != nil {
		limit = *input.DownloadLimit
	}
	if len(input.Files) == 0 || len(input.Files) > 100 || expiry < 0 || expiry > 31536000 || limit < 0 || limit > 1000000 || len(input.Title) > 200 {
		return "", errors.New("invalid transfer settings")
	}
	var total int64
	seen := map[string]map[int64]bool{}
	for _, f := range input.Files {
		if !validFilename(f.Name) || f.Size < 0 || f.Size > cfg.MaxFileSize || f.Size > cfg.MaxTransferSize-total {
			return "", errors.New("invalid filename or upload exceeds size limits")
		}
		if seen[f.Name] == nil {
			seen[f.Name] = map[int64]bool{}
		}
		if seen[f.Name][f.Size] {
			return "", errors.New("rename files with identical names and sizes before uploading")
		}
		seen[f.Name][f.Size] = true
		total += f.Size
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var used, count int64
	if err = tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(size),0) FROM files WHERE deleted=0").Scan(&used); err != nil {
		return "", err
	}
	if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM transfers WHERE status IN ('draft','published','revoked')").Scan(&count); err != nil {
		return "", err
	}
	if total > cfg.StorageQuota-used || count >= 1000 {
		return "", errors.New("storage quota or transfer limit reached")
	}
	id := randomID()
	title := strings.TrimSpace(input.Title)
	if title == "" {
		title = randomTransferName()
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO transfers(id,title,status,created_at,touched_at,expiry_seconds,download_limit,password_hash) VALUES(?,?,'draft',?,?,?,?,?)`, id, title, time.Now().Unix(), time.Now().Unix(), expiry, limit, hash)
	if err != nil {
		return "", err
	}
	for _, f := range input.Files {
		_, err = tx.ExecContext(ctx, "INSERT INTO files(id,transfer_id,name,size) VALUES(?,?,?,?)", randomID(), id, f.Name, f.Size)
		if err != nil {
			return "", err
		}
	}
	return id, tx.Commit()
}

func randomTransferName() string {
	adjectives := [...]string{
		"Amber", "Autumn", "Azure", "Blue", "Bold", "Bright", "Calm", "Clear",
		"Cloudy", "Coral", "Cosmic", "Cozy", "Crimson", "Dawn", "Deep", "Distant",
		"Dreamy", "Dusky", "Early", "Emerald", "Evening", "Frosty", "Gentle", "Golden",
		"Grand", "Green", "Happy", "Hidden", "Indigo", "Ivory", "Jade", "Kind",
		"Late", "Little", "Lively", "Lucky", "Lunar", "Mellow", "Misty", "Noble",
		"Orange", "Pale", "Peaceful", "Quiet", "Radiant", "Red", "Rocky", "Rosy",
		"Royal", "Ruby", "Sandy", "Scarlet", "Silent", "Silver", "Snowy", "Soft",
		"Solar", "Spring", "Still", "Summer", "Sunny", "Swift", "Warm", "Wild",
	}
	nouns := [...]string{
		"Acorn", "Badger", "Bay", "Bear", "Birch", "Bloom", "Breeze", "Brook",
		"Canyon", "Cedar", "Cherry", "Cloud", "Comet", "Cove", "Crane", "Creek",
		"Deer", "Delta", "Dove", "Dune", "Eagle", "Elm", "Falcon", "Fern",
		"Finch", "Forest", "Fox", "Garden", "Glen", "Grove", "Harbor", "Hawk",
		"Heron", "Hill", "Island", "Lake", "Leaf", "Lily", "Lotus", "Maple",
		"Meadow", "Moon", "Moss", "Mountain", "Oak", "Ocean", "Otter", "Owl",
		"Pebble", "Pine", "Raven", "Reef", "River", "Robin", "Shore", "Sky",
		"Sparrow", "Star", "Stone", "Stream", "Swan", "Valley", "Wave", "Willow",
	}
	return adjectives[mathrand.IntN(len(adjectives))] + " " + nouns[mathrand.IntN(len(nouns))]
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
		return "", errors.New("transfer unavailable")
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
			return "", errors.New("file unavailable")
		}
		if limit > 0 && reserved > 0 {
			return "", errors.New("download already in progress")
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

func (s *Store) closeTransfer(ctx context.Context, id, status string) error {
	query := "UPDATE transfers SET status=?,closed_at=?,auth_version=auth_version+1 WHERE id=?"
	if status == "revoked" {
		query += " AND status='published'"
	}
	result, err := s.db.ExecContext(ctx, query, status, time.Now().Unix(), id)
	if err != nil {
		return err
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
func storeError(err error) error { return fmt.Errorf("storage operation: %w", err) }
