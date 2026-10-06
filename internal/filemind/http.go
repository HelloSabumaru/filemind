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
	m.HandleFunc("GET /settings", a.accountPage("preferences", "owner"))
	m.HandleFunc("GET /api/preferences", a.ownerAPI(a.getUserPreferences))
	m.HandleFunc("PUT /api/preferences", a.ownerAPI(a.saveUserPreferences))
	m.HandleFunc("POST /api/password", a.ownerAPI(a.changePassword))
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
	m.HandleFunc("POST /api/transfers/{id}/files/{fileID}/restart", a.transferAPI(a.restartFile))
	for _, pattern := range []string{"OPTIONS /uploads/{$}", "POST /uploads/{$}", "HEAD /uploads/{id}", "PATCH /uploads/{id}"} {
		m.HandleFunc(pattern, a.ownerAPI(a.upload))
	}
	if !a.cfg.RequirePrivateAdminSignIn {
		a.registerAdminRoutes(m, "owner")
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
	m.HandleFunc("GET /setup", a.setupPage)
	m.HandleFunc("POST /setup", a.setup)
	m.HandleFunc("POST /logout", a.adminAPI(a.logout("admin")))
	m.HandleFunc("GET /admin/upload", a.accountPage("upload", "admin"))
	m.HandleFunc("GET /admin/my-transfers", a.accountPage("my-transfers", "admin"))
	m.HandleFunc("GET /admin/api/personal/config", a.adminAPI(a.configuration))
	m.HandleFunc("GET /admin/api/personal/transfers", a.adminAPI(a.listTransfers))
	m.HandleFunc("POST /admin/api/personal/transfers", a.adminAPI(a.createTransfer))
	for pattern, next := range map[string]http.HandlerFunc{
		"GET /admin/api/personal/transfers/{id}":                         a.getTransfer,
		"PATCH /admin/api/personal/transfers/{id}":                       a.editTransfer,
		"DELETE /admin/api/personal/transfers/{id}":                      a.deleteTransfer,
		"POST /admin/api/personal/transfers/{id}/publish":                a.publish,
		"POST /admin/api/personal/transfers/{id}/revoke":                 a.revokeTransfer,
		"GET /admin/api/personal/transfers/{id}/qr.png":                  a.qr,
		"POST /admin/api/personal/transfers/{id}/files/{fileID}/restart": a.restartFile,
	} {
		m.HandleFunc(pattern, a.accountTransferAPI("admin", next))
	}
	for _, pattern := range []string{"OPTIONS /admin/uploads/{$}", "POST /admin/uploads/{$}", "HEAD /admin/uploads/{id}", "PATCH /admin/uploads/{id}"} {
		m.HandleFunc(pattern, a.adminAPI(a.upload))
	}
	m.HandleFunc("GET /admin/preferences", a.accountPage("preferences", "admin"))
	m.HandleFunc("GET /admin/api/preferences", a.adminAPI(a.getUserPreferences))
	m.HandleFunc("PUT /admin/api/preferences", a.adminAPI(a.saveUserPreferences))
	m.HandleFunc("POST /admin/api/password", a.adminAPI(a.changePassword))
	a.registerAdminRoutes(m, "admin")
	m.HandleFunc("GET /assets/{name}", a.asset)
	return a.middleware(m, "admin")
}

func (a *App) registerAdminRoutes(m *http.ServeMux, kind string) {
	for path, page := range map[string]string{
		"/admin/users":     "users",
		"/admin/settings":  "settings",
		"/admin/transfers": "all-transfers",
	} {
		m.HandleFunc("GET "+path, a.accountPage(page, kind))
	}
	for pattern, next := range map[string]http.HandlerFunc{
		"GET /admin/api/transfers":              a.listAdminTransfers,
		"GET /admin/api/transfers/{id}":         a.getTransfer,
		"PATCH /admin/api/transfers/{id}":       a.editTransfer,
		"POST /admin/api/transfers/{id}/revoke": a.revokeTransfer,
		"DELETE /admin/api/transfers/{id}":      a.deleteTransfer,
		"GET /admin/api/transfers/{id}/qr.png":  a.qr,
		"GET /admin/api/users":                  a.listUsers,
		"POST /admin/api/users":                 a.createUser,
		"DELETE /admin/api/users/{id}":          a.deleteUser,
		"PATCH /admin/api/users/{id}":           a.editUser,
		"GET /admin/api/settings":               a.getSettings,
		"PUT /admin/api/settings":               a.saveSettings,
	} {
		m.HandleFunc(pattern, a.adminAccountAPI(kind, next))
	}
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
		requestID := randomID()
		r = r.WithContext(context.WithValue(r.Context(), requestIDKey{}, requestID))
		w.Header().Set("X-Request-ID", requestID)
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
			a.logger.Info("request", "request_id", requestID, "surface", surface, "route", route, "method", r.Method, "status", out.status, "bytes", out.bytes, "duration_ms", time.Since(start).Milliseconds())
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
				if surface == "public" {
					a.downloadError(w, r, 429, "Too many requests or incorrect passwords. Try again later.")
				} else {
					apiError(w, 429, "Too many requests or incorrect passwords. Try again later.")
				}
				return
			}
		}
		limit := int64(64 * 1024)
		if r.Method == http.MethodPatch && ((surface == "owner" && strings.HasPrefix(r.URL.Path, "/uploads/")) || (surface == "admin" && strings.HasPrefix(r.URL.Path, "/admin/uploads/"))) {
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

type accountKindContextKey struct{}

func (a *App) accountPasswordLimiter(r *http.Request) *limiter {
	if r.Context().Value(accountKindContextKey{}) == "admin" {
		return a.adminLimiter
	}
	return a.ownerLimiter
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
		ctx := context.WithValue(r.Context(), userContextKey{}, user)
		ctx = context.WithValue(ctx, accountKindContextKey{}, kind)
		next(w, r.WithContext(ctx))
	}
}

func (a *App) render(w http.ResponseWriter, name string, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.templates.ExecuteTemplate(w, name, data); err != nil {
		a.logFailure("render_page", err)
	}
}

type pageData struct {
	Page, CSRF, Error, BackURL, RequestID string
	DateFormat                            string
	AccountPasswordMinimum                int
	Transfer                              Transfer
	Locked                                bool
	Config                                Config
	User                                  User
	AdminSurface                          bool
	AdminSection                          bool
	AllTransfers                          bool
}

func (a *App) loginPage(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, scope, home := a.accountOptions(kind)
		if kind == "admin" {
			pending, err := a.setupPending(r.Context())
			if err != nil {
				a.operationError(w, r, "read_setup", err)
				return
			}
			if pending {
				http.Redirect(w, r, withTheme(r, "/setup"), http.StatusSeeOther)
				return
			}
		}
		if _, err := a.authenticatedAccount(r, kind); err == nil {
			http.Redirect(w, r, withTheme(r, home), 303)
			return
		}
		a.render(w, "owner", pageData{Page: "login", CSRF: a.csrfToken(w, r, scope, "/"), Config: a.cfg, AdminSurface: kind == "admin"})
	}
}
func (a *App) accountPage(page, kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		user, err := a.authenticatedAccount(r, kind)
		if err != nil {
			http.Redirect(w, r, withTheme(r, "/login"), 303)
			return
		}
		adminSection := page == "users" || page == "settings" || page == "all-transfers"
		if adminSection && !user.IsAdmin {
			notFound(w)
			return
		}
		_, scope, _ := a.accountOptions(kind)
		preferences, err := a.userPreferences(r.Context(), user.ID)
		if err != nil {
			a.operationError(w, r, "read_preferences", err)
			return
		}
		displayPage := page
		if page == "my-transfers" || page == "all-transfers" {
			displayPage = "transfers"
		}
		a.render(w, "owner", pageData{Page: displayPage, CSRF: a.csrfToken(w, r, scope, "/"), Config: a.cfg, User: user, AdminSurface: kind == "admin", AdminSection: adminSection, AllTransfers: page == "all-transfers", DateFormat: preferences.DateFormat, AccountPasswordMinimum: accountPasswordMinimum(a.cfg.Development)})
	}
}

func withTheme(r *http.Request, destination string) string {
	if theme := r.URL.Query().Get("theme"); theme == "light" || theme == "dark" {
		return destination + "?theme=" + theme
	}
	return destination
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
		user, err := scanUser(a.store.db.QueryRowContext(r.Context(), "SELECT "+userColumns+" FROM users u WHERE u.username=?", strings.TrimSpace(input.Username)))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			a.operationError(w, r, "sign_in", err)
			return
		}
		eligible := err == nil && !user.Disabled && ((kind == "admin" && user.IsAdmin) || (kind == "owner" && (!user.IsAdmin || !a.cfg.RequirePrivateAdminSignIn)))
		hash, key, targets := a.loginDummyHash, unknownPasswordKey(input.Username), a.unknownPasswords
		if eligible {
			hash, key, targets = user.PasswordHash, user.ID, a.accountPasswords
		}
		source := a.ownerLimiter
		if kind == "admin" {
			source = a.adminLimiter
		}
		valid, verifyErr := a.verifyCredential(r, source, targets, key, hash, input.Password, eligible)
		if verifyErr != nil {
			a.operationError(w, r, "sign_in", verifyErr)
			return
		}
		if !valid {
			apiError(w, 401, "Incorrect username or password.")
			return
		}
		if err := a.grantSession(w, r, kind, Transfer{}, user); err != nil {
			a.operationError(w, r, "sign_in", err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})
	}
}
func (a *App) logout(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, err := a.store.db.ExecContext(r.Context(), "DELETE FROM sessions WHERE token_hash=? AND kind=?", tokenHash(a.sessionToken(r, kind)), kind)
		if err != nil {
			a.operationError(w, r, "sign_out", err)
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
		a.operationError(w, r, "read_usage", err)
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
	a.transferList(w, r, userFromContext(r.Context()).ID)
}
func (a *App) listAdminTransfers(w http.ResponseWriter, r *http.Request) {
	a.transferList(w, r, "")
}
func (a *App) transferList(w http.ResponseWriter, r *http.Request, userID string) {
	search := r.URL.Query().Get("q")
	offset, e := strconv.Atoi(r.URL.Query().Get("offset"))
	if e != nil {
		offset = 0
	}
	if offset < 0 || offset > 100000 || len(search) > 200 {
		apiError(w, 400, "Invalid search.")
		return
	}
	transfers, err := a.store.list(r.Context(), userID, search, offset)
	if err != nil {
		a.operationError(w, r, "list_transfers", err)
		return
	}
	for i := range transfers {
		if adminRequest(r) {
			transfers[i].OwnerID = transfers[i].UserID
		}
		if transfers[i].Status == "published" {
			transfers[i].ShareURL = strings.TrimRight(a.cfg.PublicURL, "/") + "/s/" + transfers[i].ShareToken
		}
	}
	writeJSON(w, transfers)
}
func (a *App) getTransfer(w http.ResponseWriter, r *http.Request) {
	t, e := a.store.transfer(r.Context(), r.PathValue("id"))
	if e != nil {
		a.operationError(w, r, "read_transfer", e)
		return
	}
	if t.Status == "deleted" {
		notFound(w)
		return
	}
	if adminRequest(r) {
		t.OwnerID = t.UserID
	}
	a.ownerJSON(w, t)
}
func (a *App) createTransfer(w http.ResponseWriter, r *http.Request) {
	var input newTransfer
	if !decode(w, r, &input) {
		return
	}
	t, err := a.newDraft(r.Context(), userFromContext(r.Context()), input)
	if err != nil {
		var rate *transferCreationRateError
		if errors.As(err, &rate) {
			w.Header().Set("Retry-After", strconv.FormatInt(rate.retryAfter, 10))
		}
		a.operationError(w, r, "create_transfer", err)
		return
	}
	a.ownerJSON(w, t)
}

func (a *App) editTransfer(w http.ResponseWriter, r *http.Request) {
	var input transferPatch
	if !decode(w, r, &input) {
		return
	}
	t, err := a.updateTransfer(r.Context(), userFromContext(r.Context()), r.PathValue("id"), input, adminRequest(r))
	if err != nil {
		a.operationError(w, r, "edit_transfer", err)
		return
	}
	a.ownerJSON(w, t)
}

func (a *App) close(w http.ResponseWriter, r *http.Request, status string) {
	if err := a.closeTransfer(r.Context(), userFromContext(r.Context()), r.PathValue("id"), status, adminRequest(r)); err != nil {
		a.operationError(w, r, "close_transfer", err)
		return
	}
	a.logger.Info("transfer changed", "operation", status, "actor_id", userFromContext(r.Context()).ID, "transfer_id", r.PathValue("id"), "request_id", r.Context().Value(requestIDKey{}))
	writeJSON(w, map[string]bool{"ok": true})
}
func (a *App) deleteTransfer(w http.ResponseWriter, r *http.Request) { a.close(w, r, "deleted") }
func (a *App) revokeTransfer(w http.ResponseWriter, r *http.Request) { a.close(w, r, "revoked") }
func (a *App) qr(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.transfer(r.Context(), r.PathValue("id"))
	if err != nil {
		a.operationError(w, r, "read_transfer", err)
		return
	}
	if t.Status != "published" || (t.ExpiresAt > 0 && t.ExpiresAt <= time.Now().Unix()) {
		notFound(w)
		return
	}
	png, err := qrcode.Encode(strings.TrimRight(a.cfg.PublicURL, "/")+"/s/"+t.ShareToken, qrcode.Medium, 384)
	if err != nil {
		a.operationError(w, r, "generate_qr", err)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

func (a *App) publicPage(w http.ResponseWriter, r *http.Request) {
	t, err := a.store.publicTransfer(r.Context(), r.PathValue("token"))
	if err != nil {
		a.downloadOperationError(w, r, "read_public_transfer", err)
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
		a.downloadOperationError(w, r, "read_public_transfer", err)
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
	if t.PasswordHash == "" {
		writeJSON(w, map[string]bool{"ok": true})
		return
	}
	valid, err := a.verifyCredential(r, a.limiter, a.transferPasswords, t.ID, t.PasswordHash, input.Password, true)
	if err != nil {
		a.operationError(w, r, "authorize_download", err)
		return
	}
	if !valid {
		apiError(w, 403, "Incorrect password.")
		return
	}
	if err = a.grantSession(w, r, "share", t, User{}); err != nil {
		a.operationError(w, r, "authorize_download", err)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (a *App) asset(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if name != "app.js" && name != "hash-worker.js" && name != "style.css" && name != "icon.svg" {
		notFound(w)
		return
	}
	content, err := fs.ReadFile(webFiles, "web/dist/"+name)
	if err != nil {
		notFound(w)
		return
	}
	switch name {
	case "app.js", "hash-worker.js":
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
