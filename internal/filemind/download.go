package filemind

import (
	"archive/zip"
	"context"
	"database/sql"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	maxConcurrentDownloads  = 8
	maxDownloadsPerTransfer = 2
	maxDownloadsPerOwner    = 4
)

func (a *App) authorizedTransfer(w http.ResponseWriter, r *http.Request) (Transfer, bool) {
	t, err := a.store.publicTransfer(r.Context(), r.PathValue("token"))
	if err != nil {
		a.downloadOperationError(w, r, "read_public_transfer", err)
		return t, false
	}
	if !a.shareAuthenticated(r, t) {
		a.downloadError(w, r, 401, "Open the transfer page and enter its password first.")
		return t, false
	}
	return t, true
}
func (a *App) downloadCapacity(w http.ResponseWriter, r *http.Request, t Transfer) (func(), bool) {
	reject := func(message string) (func(), bool) {
		w.Header().Set("Retry-After", "10")
		a.downloadError(w, r, http.StatusTooManyRequests, message)
		return nil, false
	}
	// Files, ranges and ZIPs share these budgets. Rejections never queue or
	// retain entries; every partial acquisition is released before responding.
	releaseTransfer, err := a.downloadTransfers.acquire(t.ID)
	if err != nil {
		return reject("This transfer is already serving simultaneous downloads. Try again shortly.")
	}
	releaseOwner, err := a.downloadUsers.acquire(t.UserID)
	if err != nil {
		releaseTransfer()
		return reject("This sender is already serving simultaneous downloads. Try again shortly.")
	}
	ip := a.clientIP(r)
	release, allowed := a.limiter.download(ip)
	if !allowed {
		releaseOwner()
		releaseTransfer()
		return reject("Too many simultaneous downloads.")
	}
	select {
	case a.downloadSlots <- struct{}{}:
		return sync.OnceFunc(func() {
			<-a.downloadSlots
			release()
			releaseOwner()
			releaseTransfer()
		}), true
	default:
		release()
		releaseOwner()
		releaseTransfer()
		return reject("Downloads busy. Try again shortly.")
	}
}

type downloadStreamPolicy struct {
	idleTimeout, progressWindow time.Duration
	minimumWindowBytes          int64
}

func defaultDownloadStreamPolicy() downloadStreamPolicy {
	return downloadStreamPolicy{idleTimeout: 30 * time.Second, progressWindow: time.Minute, minimumWindowBytes: 16 * 1024 * 60}
}

type streamWriter struct {
	writer           http.ResponseWriter
	ctx              context.Context
	bytes            int64
	status           int
	err              error
	policy           downloadStreamPolicy
	now              func() time.Time
	progressDeadline time.Time
	progressBytes    int64
}

func newStreamWriter(w http.ResponseWriter, ctx context.Context, policy downloadStreamPolicy) *streamWriter {
	return &streamWriter{writer: w, ctx: ctx, policy: policy, now: time.Now, progressDeadline: time.Now().Add(policy.progressWindow)}
}

// A short successful write cannot postpone the progress deadline. Only a full
// window's byte budget renews it; each individual write/flush also has an idle
// deadline. The transfer/request deadline remains an upper bound on both.
func (w *streamWriter) prepare() error {
	if w.err != nil {
		return w.err
	}
	if err := w.ctx.Err(); err != nil {
		w.err = err
		return err
	}
	now := w.now()
	if !now.Before(w.progressDeadline) {
		w.err = context.DeadlineExceeded
		return w.err
	}
	deadline := minDeadline(now.Add(w.policy.idleTimeout), w.progressDeadline)
	if end, ok := w.ctx.Deadline(); ok {
		deadline = minDeadline(deadline, end)
	}
	controller := http.NewResponseController(w.writer)
	if err := controller.SetWriteDeadline(deadline); err != nil && !errors.Is(err, http.ErrNotSupported) {
		w.err = err
		return err
	}
	// Cancellation can race setting a future deadline after streamContext's
	// callback has interrupted the transport. Never overwrite that interruption.
	if err := w.ctx.Err(); err != nil {
		controller.SetWriteDeadline(now)
		w.err = err
		return err
	}
	return nil
}

func minDeadline(first, second time.Time) time.Time {
	if second.Before(first) {
		return second
	}
	return first
}

func (w *streamWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.writer.WriteHeader(status)
}
func (w *streamWriter) Header() http.Header { return w.writer.Header() }
func (w *streamWriter) Write(data []byte) (int, error) {
	if err := w.prepare(); err != nil {
		return 0, err
	}
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.writer.Write(data)
	if err == nil && n != len(data) {
		err = io.ErrShortWrite
	}
	w.bytes += int64(n)
	w.progressBytes += int64(n)
	if err == nil {
		now := w.now()
		if !now.Before(w.progressDeadline) {
			err = context.DeadlineExceeded
		} else if w.progressBytes >= w.policy.minimumWindowBytes {
			w.progressDeadline = now.Add(w.policy.progressWindow)
			w.progressBytes = 0
		}
	}
	w.err = err
	return n, err
}

func (w *streamWriter) flush() error {
	if err := w.prepare(); err != nil {
		return err
	}
	w.err = http.NewResponseController(w.writer).Flush()
	return w.err
}
func (w *streamWriter) Unwrap() http.ResponseWriter { return w.writer }

func (a *App) streamContext(r *http.Request, w http.ResponseWriter, t Transfer, lease string) (context.Context, func()) {
	ctx, end := a.activate(r.Context(), t.ID, lease)
	var expire context.CancelFunc
	if t.ExpiresAt > 0 {
		ctx, expire = context.WithDeadline(ctx, time.Unix(t.ExpiresAt, 0))
	} else {
		ctx, expire = context.WithCancel(ctx)
	}
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(done); http.NewResponseController(w).SetWriteDeadline(time.Now()) })
	return ctx, func() {
		if !stop() {
			<-done
		}
		expire()
		end()
		http.NewResponseController(w).SetWriteDeadline(time.Time{})
	}
}

func (a *App) complete(lease string, success bool) {
	var transferID string
	err := a.store.db.QueryRow("SELECT f.transfer_id FROM reservations r JOIN files f ON f.id=r.file_id WHERE r.id=? LIMIT 1", lease).Scan(&transferID)
	if err != nil {
		a.logFailure("find_download_reservation", err, "reservation_id", lease)
		return
	}
	if err = a.store.finishReservation(lease, success); err != nil {
		a.logFailure("download_accounting", err, "reservation_id", lease)
		return
	}
	if err = a.purgeTransfer(context.Background(), transferID); err != nil {
		a.logFailure("purge_transfer", err, "transfer_id", transferID)
	}
}

func (a *App) download(w http.ResponseWriter, r *http.Request) {
	t, ok := a.authorizedTransfer(w, r)
	if !ok {
		return
	}
	var selected *File
	for i := range t.Files {
		if t.Files[i].ID == r.PathValue("id") && t.Files[i].Uploaded && !t.Files[i].Deleted && (t.DownloadLimit == 0 || t.Files[i].Downloads < t.DownloadLimit) {
			selected = &t.Files[i]
			break
		}
	}
	if selected == nil {
		a.downloadError(w, r, 404, "Transfer unavailable.")
		return
	}
	payload, err := a.root.Open(selected.ID)
	if err != nil {
		a.downloadError(w, r, 404, "Transfer unavailable.")
		return
	}
	defer payload.Close()
	stat, err := payload.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != selected.Size {
		a.downloadError(w, r, 503, "File unavailable.")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": selected.Name}))
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(selected.Size, 10))
		if t.DownloadLimit == 0 {
			w.Header().Set("Accept-Ranges", "bytes")
		} else {
			w.Header().Set("Accept-Ranges", "none")
		}
		return
	}
	release, ok := a.downloadCapacity(w, r, t)
	if !ok {
		return
	}
	defer release()
	lease, err := a.store.reserve(r.Context(), t, []File{*selected})
	if err != nil {
		a.downloadOperationError(w, r, "reserve_download", err)
		return
	}
	ctx, end := a.streamContext(r, w, t, lease)
	defer end()
	// Recheck after registering cancellation so revoke cannot race a new stream.
	current, err := a.store.publicTransfer(ctx, t.ShareToken)
	if err != nil || current.AuthVersion != t.AuthVersion {
		a.complete(lease, false)
		a.downloadError(w, r, 404, "Transfer unavailable.")
		return
	}
	if t.DownloadLimit > 0 {
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("Content-Length", strconv.FormatInt(selected.Size, 10))
		writer := newStreamWriter(w, ctx, a.downloadPolicy)
		n, err := io.CopyBuffer(writer, payload, make([]byte, 64*1024))
		if err == nil {
			err = writer.flush()
		}
		success := err == nil && ctx.Err() == nil && n == selected.Size
		payload.Close()
		a.complete(lease, success)
		if !success {
			panic(http.ErrAbortHandler)
		}
		return
	}
	writer := newStreamWriter(w, ctx, a.downloadPolicy)
	http.ServeContent(writer, r, selected.Name, stat.ModTime(), payload)
	flushErr := writer.flush()
	success := writer.status == 200 && writer.bytes == selected.Size && ctx.Err() == nil && flushErr == nil
	payload.Close()
	a.complete(lease, success)
	if writer.err != nil || ctx.Err() != nil || flushErr != nil {
		panic(http.ErrAbortHandler)
	}
}

func (a *App) archive(w http.ResponseWriter, r *http.Request) {
	t, ok := a.authorizedTransfer(w, r)
	if !ok {
		return
	}
	selected := []File{}
	for _, f := range t.Files {
		if !f.Deleted && f.Uploaded && (t.DownloadLimit == 0 || f.Downloads < t.DownloadLimit) {
			selected = append(selected, f)
		}
	}
	if len(selected) == 0 {
		a.downloadError(w, r, 404, "Transfer unavailable.")
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "filemind-" + t.ID[:8] + ".zip"}))
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == http.MethodHead {
		return
	}
	release, ok := a.downloadCapacity(w, r, t)
	if !ok {
		return
	}
	defer release()
	lease, err := a.store.reserve(r.Context(), t, selected)
	if err != nil {
		a.downloadOperationError(w, r, "reserve_download", err)
		return
	}
	ctx, end := a.streamContext(r, w, t, lease)
	defer end()
	current, err := a.store.publicTransfer(ctx, t.ShareToken)
	if err != nil || current.AuthVersion != t.AuthVersion {
		a.complete(lease, false)
		a.downloadError(w, r, 404, "Transfer unavailable.")
		return
	}
	writer := newStreamWriter(w, ctx, a.downloadPolicy)
	archive := zip.NewWriter(writer)
	names := map[string]bool{}
	for _, f := range selected {
		if ctx.Err() != nil {
			err = ctx.Err()
			break
		}
		name := f.Name
		for i := 2; names[name]; i++ {
			extension := filepath.Ext(f.Name)
			name = strings.TrimSuffix(f.Name, extension) + " (" + strconv.Itoa(i) + ")" + extension
		}
		names[name] = true
		var payload *os.File
		payload, err = a.root.Open(f.ID)
		if err != nil {
			break
		}
		info, statErr := payload.Stat()
		if statErr != nil || info.Size() != f.Size || !info.Mode().IsRegular() {
			payload.Close()
			err = errors.New("payload unavailable")
			break
		}
		header := &zip.FileHeader{Name: name, Method: zip.Store}
		header.SetMode(0600)
		var entry io.Writer
		entry, err = archive.CreateHeader(header)
		if err == nil {
			var n int64
			n, err = io.CopyBuffer(entry, &contextReader{ctx: ctx, reader: payload}, make([]byte, 64*1024))
			if err == nil && n != f.Size {
				err = io.ErrUnexpectedEOF
			}
		}
		payload.Close()
		if err != nil {
			break
		}
	}
	if err == nil {
		err = archive.Close()
	}
	if err == nil {
		err = writer.flush()
	}
	a.complete(lease, err == nil && ctx.Err() == nil)
	if err != nil || ctx.Err() != nil {
		a.logFailure("stream_archive", err, "transfer_id", t.ID)
		if writer.status == 0 {
			a.downloadError(w, r, 503, "A stored file is unavailable. Please contact the sender.")
		} else {
			panic(http.ErrAbortHandler)
		}
	}
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(data []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(data)
}

// Use the actual GET result for browser errors; a separate HEAD request cannot
// reserve availability and successful attachments must never be buffered.
func (a *App) downloadError(w http.ResponseWriter, r *http.Request, status int, message string) {
	if !strings.Contains(r.Header.Get("Accept"), "text/html") {
		apiError(w, status, message)
		return
	}
	back := ""
	parts := strings.Split(r.URL.Path, "/")
	if len(parts) >= 3 && parts[1] == "s" && validToken(parts[2]) {
		back = "/s/" + parts[2]
	}
	w.Header().Del("Content-Disposition")
	w.Header().Del("Content-Length")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	requestID, _ := r.Context().Value(requestIDKey{}).(string)
	a.render(w, "download-error", pageData{Error: message, BackURL: back, RequestID: requestID})
}

func (a *App) downloadOperationError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var p *problem
	switch {
	case errors.As(err, &p):
		a.downloadError(w, r, p.status, p.message)
	case errors.Is(err, sql.ErrNoRows):
		a.downloadError(w, r, 404, "Transfer unavailable.")
	default:
		a.logFailure(operation, err, "request_id", r.Context().Value(requestIDKey{}))
		a.downloadError(w, r, 503, "Storage unavailable. Please retry later.")
	}
}
