package filemind

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

func (a *App) authorizedTransfer(w http.ResponseWriter, r *http.Request) (Transfer, bool) {
	t, err := a.store.publicTransfer(r.Context(), r.PathValue("token"))
	if err != nil {
		notFound(w)
		return t, false
	}
	if !a.shareAuthenticated(r, t) {
		apiError(w, 401, "Open the transfer page and enter its password first.")
		return t, false
	}
	return t, true
}
func (a *App) downloadCapacity(w http.ResponseWriter, r *http.Request) (func(), bool) {
	ip := a.clientIP(r)
	if !a.limiter.download(ip) {
		w.Header().Set("Retry-After", "10")
		apiError(w, 429, "Too many simultaneous downloads.")
		return nil, false
	}
	select {
	case a.downloadSlots <- struct{}{}:
		return func() { <-a.downloadSlots; a.limiter.release(ip) }, true
	default:
		a.limiter.release(ip)
		w.Header().Set("Retry-After", "10")
		apiError(w, 429, "Downloads busy. Try again shortly.")
		return nil, false
	}
}

type streamWriter struct {
	writer http.ResponseWriter
	ctx    context.Context
	bytes  int64
	status int
}

func (w *streamWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.writer.WriteHeader(status)
}
func (w *streamWriter) Header() http.Header { return w.writer.Header() }
func (w *streamWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	http.NewResponseController(w.writer).SetWriteDeadline(time.Now().Add(time.Minute))
	if w.status == 0 {
		w.status = 200
	}
	n, err := w.writer.Write(data)
	w.bytes += int64(n)
	return n, err
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
	if err := a.store.finishReservation(lease, success); err != nil {
		a.logger.Error("download accounting failed")
		return
	}
	if err := a.cleanup(); err != nil {
		a.logger.Error("cleanup failed")
	}
}

func (a *App) download(w http.ResponseWriter, r *http.Request) {
	t, ok := a.authorizedTransfer(w, r)
	if !ok {
		return
	}
	var selected *File
	for i := range t.Files {
		if t.Files[i].ID == r.PathValue("id") && !t.Files[i].Deleted && (t.DownloadLimit == 0 || t.Files[i].Downloads < t.DownloadLimit) {
			selected = &t.Files[i]
			break
		}
	}
	if selected == nil {
		notFound(w)
		return
	}
	payload, err := a.root.Open(selected.ID)
	if err != nil {
		notFound(w)
		return
	}
	defer payload.Close()
	stat, err := payload.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Size() != selected.Size {
		apiError(w, 503, "File unavailable.")
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
	release, ok := a.downloadCapacity(w, r)
	if !ok {
		return
	}
	defer release()
	lease, err := a.store.reserve(r.Context(), t, []File{*selected})
	if err != nil {
		apiError(w, 409, "File unavailable or already downloading. Refresh and retry.")
		return
	}
	ctx, end := a.streamContext(r, w, t, lease)
	defer end()
	// Recheck after registering cancellation so revoke cannot race a new stream.
	current, err := a.store.publicTransfer(ctx, t.ShareToken)
	if err != nil || current.AuthVersion != t.AuthVersion {
		a.complete(lease, false)
		notFound(w)
		return
	}
	if t.DownloadLimit > 0 {
		w.Header().Set("Accept-Ranges", "none")
		w.Header().Set("Content-Length", strconv.FormatInt(selected.Size, 10))
		writer := &streamWriter{writer: w, ctx: ctx}
		n, err := io.CopyBuffer(writer, payload, make([]byte, 64*1024))
		if err == nil {
			err = http.NewResponseController(w).Flush()
		}
		success := err == nil && ctx.Err() == nil && n == selected.Size
		payload.Close()
		a.complete(lease, success)
		return
	}
	writer := &streamWriter{writer: w, ctx: ctx}
	http.ServeContent(writer, r, selected.Name, stat.ModTime(), payload)
	flushErr := http.NewResponseController(w).Flush()
	success := writer.status == 200 && writer.bytes == selected.Size && ctx.Err() == nil && flushErr == nil
	payload.Close()
	a.complete(lease, success)
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
		notFound(w)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": "filemind-" + t.ID[:8] + ".zip"}))
	w.Header().Set("Accept-Ranges", "none")
	if r.Method == http.MethodHead {
		return
	}
	release, ok := a.downloadCapacity(w, r)
	if !ok {
		return
	}
	defer release()
	lease, err := a.store.reserve(r.Context(), t, selected)
	if err != nil {
		apiError(w, 409, "A file is already downloading or unavailable. Refresh and retry.")
		return
	}
	ctx, end := a.streamContext(r, w, t, lease)
	defer end()
	current, err := a.store.publicTransfer(ctx, t.ShareToken)
	if err != nil || current.AuthVersion != t.AuthVersion {
		a.complete(lease, false)
		notFound(w)
		return
	}
	writer := &streamWriter{writer: w, ctx: ctx}
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
		err = http.NewResponseController(w).Flush()
	}
	a.complete(lease, err == nil && ctx.Err() == nil)
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
