package filemind

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"strconv"
	"syscall"
)

type problem struct {
	status  int
	message string
}

func (p *problem) Error() string    { return p.message }
func invalid(message string) error  { return &problem{400, message} }
func conflict(message string) error { return &problem{409, message} }

type requestIDKey struct{}

func errorCategory(err error) string {
	var code interface{ Code() int }
	if errors.As(err, &code) {
		switch code.Code() & 255 {
		case 5, 6:
			return "database_busy"
		case 8:
			return "database_read_only"
		case 10:
			return "database_io"
		case 11:
			return "database_corrupt"
		case 13:
			return "disk_full"
		case 14:
			return "database_unavailable"
		case 19:
			return "database_constraint"
		default:
			return "database"
		}
	}
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, os.ErrPermission):
		return "permission"
	case errors.Is(err, syscall.ENOSPC):
		return "disk_full"
	case errors.Is(err, syscall.EIO):
		return "io"
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, os.ErrNotExist):
		return "missing"
	default:
		return "storage_or_internal"
	}
}

func (a *App) logFailure(operation string, err error, attrs ...any) {
	a.logger.Error("operation failed", append([]any{"operation", operation, "category", errorCategory(err)}, attrs...)...)
}

func (a *App) operationError(w http.ResponseWriter, r *http.Request, operation string, err error) {
	var throttle *passwordThrottleError
	if errors.As(err, &throttle) {
		w.Header().Set("Retry-After", strconv.Itoa(throttle.retry))
	}
	var p *problem
	if errors.As(err, &p) {
		apiError(w, p.status, p.message)
		return
	}
	if errors.Is(err, sql.ErrNoRows) {
		notFound(w)
		return
	}
	a.logFailure(operation, err, "request_id", r.Context().Value(requestIDKey{}))
	apiError(w, 503, "Storage operation failed. Try again later.")
}
