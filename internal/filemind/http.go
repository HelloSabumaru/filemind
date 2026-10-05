package filemind

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"strconv"
	"strings"
	"time"

	qrcode "github.com/skip2/go-qrcode"
)

func (a *App) OwnerHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", a.health)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/upload", http.StatusSeeOther) })
	m.HandleFunc("GET /login", a.loginPage("owner"))
	m.HandleFunc("POST /login", a.login("owner"))
	m.HandleFunc("GET /upload", a.accountPage("upload", "owner"))
	m.HandleFunc("GET /transfers", a.accountPage("transfers", "owner"))
	m.HandleFunc("POST /logout", a.ownerAPI(a.logout("owner")))
	m.HandleFunc("GET /api/config", a.ownerAPI(a.configuration))
	m.HandleFunc("GET /api/transfers", a.ownerAPI(a.listTransfers))
	m.HandleFunc("POST /api/transfers", a.ownerAPI(a.createTransfer))
	m.HandleFunc("GET /api/transfers/{id}", a.transferAPI(a.getTransfer))
	m.HandleFunc("PATCH /api/transfers/{id}", a.transferAPI(a.editTransfer))
	m.HandleFunc("DELETE /api/transfers/{id}", a.transferAPI(a.deleteTransfer))
	m.HandleFunc("POST /api/transfers/{id}/publish", a.transferAPI(a.publish))
	m.HandleFunc("POST /api/transfers/{id}/revoke", a.transferAPI(a.revokeTransfer))
	m.HandleFunc("GET /api/transfers/{id}/qr.png", a.transferAPI(a.qr))
	for _, pattern := range []string{"OPTIONS /uploads/{$}", "POST /uploads/{$}", "HEAD /uploads/{id}", "PATCH /uploads/{id}"} {
		m.HandleFunc(pattern, a.ownerAPI(a.upload))
	}
	m.HandleFunc("GET /assets/{name}", a.asset)
	return a.middleware(m, "owner")
}

func (a *App) AdminHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", a.health)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/admin/users", http.StatusSeeOther) })
	m.HandleFunc("GET /login", a.loginPage("admin"))
	m.HandleFunc("POST /login", a.login("admin"))
	m.HandleFunc("POST /logout", a.adminAPI(a.logout("admin")))
	m.HandleFunc("GET /admin/users", a.accountPage("users", "admin"))
	m.HandleFunc("GET /admin/settings", a.accountPage("settings", "admin"))
	m.HandleFunc("GET /admin/api/users", a.adminAPI(a.listUsers))
	m.HandleFunc("POST /admin/api/users", a.adminAPI(a.createUser))
	m.HandleFunc("PATCH /admin/api/users/{id}", a.adminAPI(a.editUser))
	m.HandleFunc("GET /admin/api/settings", a.adminAPI(a.getSettings))
	m.HandleFunc("PUT /admin/api/settings", a.adminAPI(a.saveSettings))
	m.HandleFunc("GET /assets/{name}", a.asset)
	return a.middleware(m, "admin")
}

func (a *App) PublicHandler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", a.health)
	m.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "Use the link you received to download a transfer.\n")
	})
	m.HandleFunc("GET /s/{token}", a.publicPage)
	m.HandleFunc("POST /s/{token}/unlock", a.unlock)
	m.HandleFunc("GET /s/{token}/files/{id}", a.download)
	m.HandleFunc("GET /s/{token}/archive", a.archive)
	m.HandleFunc("GET /assets/{name}", a.asset)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { notFound(w) })
	return a.middleware(m, "public")
}

type responseLog struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (w *responseLog) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}
func (w *responseLog) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = 200
	}
	n, e := w.ResponseWriter.Write(p)
	w.bytes += int64(n)
	return n, e
}
func (w *responseLog) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (a *App) middleware(next http.Handler, surface string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		out := &responseLog{ResponseWriter: w}
		w = out
		defer func() {
			if out.status == 0 {
				out.status = 200
			}
			route := r.Pattern
			if route == "" {
				route = "unknown"
			}
			a.logger.Info("request", "surface", surface, "route", route, "method", r.Method, "status", out.status, "bytes", out.bytes, "duration_ms", time.Since(start).Milliseconds())
		}()
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self'; connect-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if r.URL.Path != "/healthz" {
			limit := a.limiter
			if surface == "owner" {
				limit = a.ownerLimiter
			} else if surface == "admin" {
				limit = a.adminLimiter
			}
			if allowed, retry := limit.allow(a.clientIP(r), surface == "public"); !allowed {
				w.Header().Set("Retry-After", strconv.Itoa(retry))
				apiError(w, 429, "Too many requests or incorrect passwords. Try again later.")
				return
			}
		}
		limit := int64(64 * 1024)
		if surface == "owner" && r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/uploads/") {
			limit = 8 * 1024 * 1024
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
		next.ServeHTTP(w, r)
	})
}

func (a *App) ownerAPI(next http.HandlerFunc) http.HandlerFunc {
	return a.accountAPI("owner", next)
}

func (a *App) accountOptions(kind string) (origin, scope, home string) {
	if kind == "admin" {
		return a.cfg.AdminURL, "admin-account", "/admin/users"
	}
	return a.cfg.OwnerURL, "account", "/upload"
}

func (a *App) accountAPI(kind string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := a.authenticatedAccount(r, kind)
		if err != nil {
			apiError(w, 401, "Sign in to continue.")
			return
		}
		origin, scope, _ := a.accountOptions(kind)
		if r.Method != "GET" && r.Method != "HEAD" && r.Method != "OPTIONS" && !a.checkCSRF(r, scope, origin) {
			apiError(w, 403, "Request verification failed. Refresh and retry.")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), userContextKey{}, user)))
	}
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		a.logger.Error("page rendering failed")
	}
}

type pageData struct {
	Page, CSRF, Error string
	Transfer          Transfer
	Locked            bool
	Config            Config
	User              User
	AdminSurface      bool
}

func (a *App) loginPage(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, scope, home := a.accountOptions(kind)
		if _, err := a.authenticatedAccount(r, kind); err == nil {
			http.Redirect(w, r, home, 303)
			return
		}
		a.render(w, "owner", pageData{Page: "login", CSRF: a.csrfToken(w, r, scope, "/"), Config: a.cfg, AdminSurface: kind == "admin"})
	}
}
func (a *App) accountPage(page, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := a.authenticatedAccount(r, kind)
		if err != nil {
			http.Redirect(w, r, "/login", 303)
			return
		}
		_, scope, _ := a.accountOptions(kind)
		a.render(w, "owner", pageData{Page: page, CSRF: a.csrfToken(w, r, scope, "/"), Config: a.cfg, User: user, AdminSurface: kind == "admin"})
	}
}

func (a *App) login(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		origin, scope, _ := a.accountOptions(kind)
		if !a.checkCSRF(r, scope, origin) {
			apiError(w, 403, "Refresh the sign-in page and retry.")
			return
		}
		var input struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if !decode(w, r, &input) {
			return
		}
		select {
		case a.hashSlots <- struct{}{}:
			defer func() { <-a.hashSlots }()
		default:
			apiError(w, 429, "Password verification busy. Try again shortly.")
			return
		}
		user, err := scanUser(a.store.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users u WHERE u.username=?", strings.TrimSpace(input.Username)))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			apiError(w, 503, "Sign-in unavailable. Try again later.")
			return
		}
		hash := user.PasswordHash
		if err != nil {
			hash = a.loginDummyHash
		}
		valid := verifyPassword(hash, input.Password)
		if !valid || err != nil || user.Disabled || (kind == "admin" && !user.IsAdmin) {
			limit := a.ownerLimiter
			if kind == "admin" {
				limit = a.adminLimiter
			}
			limit.fail(a.clientIP(r))
			apiError(w, 401, "Incorrect username or password.")
			return
		}
		if err := a.grantSession(w, kind, Transfer{}, user); err != nil {
			apiError(w, 503, "Sign-in unavailable. Try again later.")
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}
func (a *App) logout(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := a.store.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=? AND kind=?", tokenHash(a.sessionToken(r, kind)), kind)
		if err != nil {
			apiError(w, 500, "Cannot sign out.")
			return
		}
		http.SetCookie(w, &http.Cookie{Name: a.cookieName(sessionCookie(kind)), Value: "", Path: "/", Secure: !a.cfg.Development, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: -1})
		writeJSON(w, map[string]bool{"ok": true})
	}
}
func (a *App) configuration(w http.ResponseWriter, r *http.Request) {
	user := userFromContext(r.Context())
	settings := a.settings()
	var used int64
	if err := a.store.db.QueryRowContext(r.Context(), "SELECT COALESCE(SUM(f.size),0) FROM files f JOIN transfers t ON t.id=f.transfer_id WHERE t.user_id=? AND f.deleted=0", user.ID).Scan(&used); err != nil {
		apiError(w, 500, "Cannot read storage usage.")
		return
	}
	quota := settings.StorageQuota
	if user.StorageQuota > 0 {
		quota = min(quota, user.StorageQuota)
	}
	writeJSON(w, map[string]any{"expirySeconds": settings.ExpirySeconds, "downloadLimit": settings.DownloadLimit, "maxFileSize": settings.MaxFileSize, "maxTransferSize": settings.MaxTransferSize, "storageQuota": quota, "storageUsed": used})
}
func (a *App) ownerJSON(w http.ResponseWriter, t Transfer) {
	if t.Status == "published" {
		t.ShareURL = strings.TrimRight(a.cfg.PublicURL, "/") + "/s/" + t.ShareToken
	}
	writeJSON(w, t)
}
func (a *App) listTransfers(w http.ResponseWriter, r *http.Request) {
	search := r.URL.Query().Get("q")
	offset, e := strconv.Atoi(r.URL.Query().Get("offset"))
	if e != nil {
		offset = 0
	}
	if offset < 0 || offset > 100000 || len(search) > 200 {
		apiError(w, 400, "Invalid search.")
		return
	}
	transfers, err := a.store.list(r.Context(), search, offset)
	if err != nil {
		apiError(w, 500, "Cannot read transfers.")
		return
	}
	for i := range transfers {
		if transfers[i].Status == "published" {
			transfers[i].ShareURL = strings.TrimRight(a.cfg.PublicURL, "/") + "/s/" + transfers[i].ShareToken
		}
	}
	writeJSON(w, transfers)
}
func (a *App) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, e := a.store.transfer(r.Context(), r.PathValue("id"))
	if e != nil {
		notFound(w)
		return
	}
	a.ownerJSON(w, t)
}
func (a *App) createTransfer(w http.ResponseWriter, r *http.Request) {
	var input newTransfer
	if !decode(w, r, &input) {
		return
	}
	a.settingsMu.Lock()
	defer a.settingsMu.Unlock()
	settings := a.settings()
	var total int64
	for _, f := range input.Files {
		if f.Size < 0 || f.Size > settings.MaxTransferSize-total {
			apiError(w, 400, "Transfer exceeds size limits.")
			return
		}
		total += f.Size
	}
	space, err := freeSpace(a.cfg.DataDir)
	if err != nil || space-total < a.cfg.MinFreeSpace {
		apiError(w, 507, "Insufficient free disk space.")
		return
	}
	hash := ""
	if input.Password != "" {
		select {
		case a.hashSlots <- struct{}{}:
			defer func() { <-a.hashSlots }()
		default:
			apiError(w, 429, "Password verification busy.")
			return
		}
		hash, err = hashPassword(input.Password)
		if err != nil {
			apiError(w, 400, err.Error())
			return
		}
	}
	id, err := a.store.create(r.Context(), input, hash, settings)
	if err != nil {
		apiError(w, 400, err.Error())
		return
	}
	t, err := a.store.transfer(r.Context(), id)
	if err != nil {
		apiError(w, 500, "Cannot read new transfer.")
		return
	}
	a.ownerJSON(w, t)
}

func (a *App) editTransfer(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Title         *string `json:"title"`
		ExpirySeconds *int64  `json:"expirySeconds"`
		DownloadLimit *int64  `json:"downloadLimit"`
		Password      *string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	t, err := a.store.transfer(r.Context(), r.PathValue("id"))
	if err != nil {
		notFound(w)
		return
	}
	if t.Status != "published" && t.Status != "draft" {
		apiError(w, 409, "Closed transfers cannot be changed.")
		return
	}
	if input.Title != nil {
		if len(*input.Title) > 200 || strings.TrimSpace(*input.Title) == "" {
			apiError(w, 400, "Invalid title.")
			return
		}
		t.Title = *input.Title
	}
	if input.ExpirySeconds != nil {
		if *input.ExpirySeconds < 0 || *input.ExpirySeconds > 31536000 {
			apiError(w, 400, "Invalid expiry.")
			return
		}
		t.ExpirySeconds = *input.ExpirySeconds
		if t.Status == "published" {
			t.ExpiresAt = 0
			if t.ExpirySeconds > 0 {
				t.ExpiresAt = time.Now().Unix() + t.ExpirySeconds
			}
		}
	}
	if input.DownloadLimit != nil {
		if *input.DownloadLimit < 0 || *input.DownloadLimit > 1000000 {
			apiError(w, 400, "Invalid download limit.")
			return
		}
		t.DownloadLimit = *input.DownloadLimit
	}
	if input.Password != nil {
		if *input.Password == "" {
			t.PasswordHash = ""
		} else {
			select {
			case a.hashSlots <- struct{}{}:
				defer func() { <-a.hashSlots }()
			default:
				apiError(w, 429, "Password verification busy.")
				return
			}
			t.PasswordHash, err = hashPassword(*input.Password)
			if err != nil {
				apiError(w, 400, err.Error())
				return
			}
		}
	}
	tx, err := a.store.db.BeginTx(r.Context(), nil)
	if err != nil {
		apiError(w, 500, "Cannot change transfer.")
		return
	}
	defer tx.Rollback()
	var active int
	err = tx.QueryRowContext(r.Context(), "SELECT COUNT(*) FROM reservations r JOIN files f ON f.id=r.file_id WHERE f.transfer_id=?", t.ID).Scan(&active)
	if err != nil {
		apiError(w, 500, "Cannot change transfer.")
		return
	}
	if active > 0 {
		apiError(w, 409, "Wait for active downloads to finish, or revoke the transfer.")
		return
	}
	result, err := tx.ExecContext(r.Context(), "UPDATE transfers SET title=?,expiry_seconds=?,expires_at=?,download_limit=?,password_hash=?,auth_version=auth_version+1 WHERE id=? AND auth_version=? AND status IN ('draft','published') AND (status='draft' OR expires_at=0 OR expires_at>?)", t.Title, t.ExpirySeconds, t.ExpiresAt, t.DownloadLimit, t.PasswordHash, t.ID, t.AuthVersion, time.Now().Unix())
	if err != nil {
		apiError(w, 500, "Cannot change transfer.")
		return
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		apiError(w, 409, "Transfer changed; refresh and retry.")
		return
	}
	if err = tx.Commit(); err != nil {
		apiError(w, 500, "Cannot change transfer.")
		return
	}
	if err = a.cleanup(); err != nil {
		a.logger.Error("cleanup failed")
	}
	t, err = a.store.transfer(r.Context(), t.ID)
	if err != nil {
		apiError(w, 500, "Cannot read transfer.")
		return
	}
	a.ownerJSON(w, t)
}

func (a *App) close(w http.ResponseWriter, r *http.Request, status string) {
	if err := a.store.closeTransfer(r.Context(), r.PathValue("id"), status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			notFound(w)
		} else {
			apiError(w, 500, "Cannot close transfer.")
		}
		return
	}
	a.cancelTransfer(r.PathValue("id"))
	if err := a.cleanup(); err != nil {
		a.logger.Error("cleanup failed")
	}
	writeJSON(w, map[string]bool{"ok": true})
}
func (a *App) deleteTransfer(w http.ResponseWriter, r *http.Request) { a.close(w, r, "deleted") }
func (a *App) revokeTransfer(w http.ResponseWriter, r *http.Request) { a.close(w, r, "revoked") }
func (a *App) qr(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.transfer(r.Context(), r.PathValue("id"))
	if err != nil || t.Status != "published" || (t.ExpiresAt > 0 && t.ExpiresAt <= time.Now().Unix()) {
		notFound(w)
		return
	}
	png, err := qrcode.Encode(strings.TrimRight(a.cfg.PublicURL, "/")+"/s/"+t.ShareToken, qrcode.Medium, 384)
	if err != nil {
		apiError(w, 500, "Cannot generate QR code.")
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

func (a *App) publicPage(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.publicTransfer(r.Context(), r.PathValue("token"))
	if err != nil {
		notFound(w)
		return
	}
	locked := !a.shareAuthenticated(r, t)
	if locked {
		t.Files = nil
		t.Title = "Password-protected transfer"
	}
	a.render(w, "public", pageData{Transfer: t, Locked: locked, CSRF: a.csrfToken(w, r, "share:"+t.ID, "/s/"+t.ShareToken)})
}
func (a *App) unlock(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.publicTransfer(r.Context(), r.PathValue("token"))
	if err != nil {
		notFound(w)
		return
	}
	if !a.checkCSRF(r, "share:"+t.ID, a.cfg.PublicURL) {
		apiError(w, 403, "Refresh the download page and retry.")
		return
	}
	var input struct {
		Password string `json:"password"`
	}
	if !decode(w, r, &input) {
		return
	}
	if t.PasswordHash != "" {
		select {
		case a.hashSlots <- struct{}{}:
			defer func() { <-a.hashSlots }()
		default:
			apiError(w, 429, "Password verification busy.")
			return
		}
		if !verifyPassword(t.PasswordHash, input.Password) {
			a.limiter.fail(a.clientIP(r))
			apiError(w, 403, "Incorrect password.")
			return
		}
	}
	if err = a.grantSession(w, "share", t, User{}); err != nil {
		apiError(w, 503, "Download authorization unavailable.")
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.db.PingContext(r.Context()); err != nil {
		http.Error(w, "unhealthy", 503)
		return
	}
	info, err := a.root.Stat(".")
	space, spaceErr := freeSpace(a.cfg.DataDir)
	if err != nil || !info.IsDir() || spaceErr != nil || space < a.cfg.MinFreeSpace {
		http.Error(w, "unhealthy", 503)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	io.WriteString(w, "ok\n")
}
func (a *App) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "app.js" && name != "style.css" && name != "icon.svg" {
		notFound(w)
		return
	}
	content, err := fs.ReadFile(webFiles, "web/dist/"+name)
	if err != nil {
		notFound(w)
		return
	}
	switch name {
	case "app.js":
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
	case "style.css":
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case "icon.svg":
		w.Header().Set("Content-Type", "image/svg+xml")
	}
	w.Write(content)
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		apiError(w, 415, "Use application/json.")
		return false
	}
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		apiError(w, 400, "Invalid request.")
		return false
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		apiError(w, 400, "Invalid request.")
		return false
	}
	return true
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(value)
}
func apiError(w http.ResponseWriter, status int, message string) {
	w.Header().Del("Content-Disposition")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": message})
}
func notFound(w http.ResponseWriter) {
	w.Header().Del("Content-Disposition")
	w.Header().Del("Content-Length")
	http.Error(w, "Transfer unavailable.", 404)
}
func formatBytes(n int64) string {
	if n < 1024 {
		return strconv.FormatInt(n, 10) + " B"
	}
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	v := float64(n)
	for _, unit := range units {
		v /= 1024
		if v < 1024 {
			return strconv.FormatFloat(v, 'f', 1, 64) + " " + unit
		}
	}
	return "large"
}
