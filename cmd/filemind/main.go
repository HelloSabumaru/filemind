package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"filemind/internal/filemind"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "healthcheck" {
		if err := healthcheck(); err != nil {
			fmt.Fprintln(os.Stderr, "unhealthy")
			os.Exit(1)
		}
		return
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	syscall.Umask(0077)
	cfg, err := filemind.ConfigFromEnv()
	if err != nil {
		logger.Error(err.Error())
		os.Exit(1)
	}
	a, err := filemind.New(cfg, logger)
	if err != nil {
		logger.Error("startup failed", "reason", safeStartupError(err))
		os.Exit(1)
	}
	defer a.Close()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	owner, err := net.Listen("tcp", cfg.OwnerListen)
	if err != nil {
		logger.Error("owner listener unavailable")
		os.Exit(1)
	}
	public, err := net.Listen("tcp", cfg.PublicListen)
	if err != nil {
		owner.Close()
		logger.Error("public listener unavailable")
		os.Exit(1)
	}
	slots := []chan struct{}{make(chan struct{}, 16), make(chan struct{}, 112)}
	servers := []*http.Server{server(a.OwnerHandler(), logger), server(a.PublicHandler(), logger)}
	listeners := []net.Listener{owner, public}
	failures := make(chan error, 2)
	for i, s := range servers {
		go func() {
			failures <- s.Serve(&limitedListener{Listener: listeners[i], slots: slots[i], done: make(chan struct{})})
		}()
	}
	go a.Background(ctx)
	logger.Info("Filemind ready")
	select {
	case <-ctx.Done():
	case err = <-failures:
		if !errors.Is(err, http.ErrServerClosed) {
			logger.Error("listener failed")
		}
		stop()
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, s := range servers {
		if err = s.Shutdown(shutdown); err != nil {
			s.Close()
		}
	}
}

func safeStartupError(err error) string {
	// Filesystem and database errors can contain private paths or SQL values.
	for _, prefix := range []string{"owner password", "cannot read owner", "data directory", "invalid stored", "invalid storage", "invalid listener", "HTTPS", "origins", "owner and public", "listeners must"} {
		if strings.HasPrefix(err.Error(), prefix) {
			return err.Error()
		}
	}
	return "check data volume permissions, owner credentials, and configuration"
}
func server(h http.Handler, logger *slog.Logger) *http.Server {
	return &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: time.Minute, WriteTimeout: time.Minute, IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 * 1024, ErrorLog: log.New(httpErrorWriter{logger}, "", 0)}
}

type httpErrorWriter struct{ logger *slog.Logger }

func (w httpErrorWriter) Write(p []byte) (int, error) {
	// Raw HTTP diagnostics can include client addresses and panic contents.
	w.logger.Error("HTTP server error")
	return len(p), nil
}

type limitedListener struct {
	net.Listener
	slots chan struct{}
	done  chan struct{}
	once  sync.Once
}

func (l *limitedListener) Close() error {
	l.once.Do(func() { close(l.done) })
	return l.Listener.Close()
}

func (l *limitedListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	select {
	case l.slots <- struct{}{}:
		return &limitedConn{Conn: conn, slots: l.slots}, nil
	case <-l.done:
		conn.Close()
		return nil, net.ErrClosed
	}
}

type limitedConn struct {
	net.Conn
	slots chan struct{}
	once  sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(func() { <-c.slots })
	return err
}

func healthcheck() error {
	client := &http.Client{Timeout: 3 * time.Second}
	for _, key := range []string{"FILEMIND_OWNER_LISTEN", "FILEMIND_PUBLIC_LISTEN"} {
		address := os.Getenv(key)
		if address == "" {
			address = ":8080"
			if key == "FILEMIND_PUBLIC_LISTEN" {
				address = ":8081"
			}
		}
		_, port, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		r, err := client.Get("http://127.0.0.1:" + port + "/healthz")
		if err != nil {
			return err
		}
		r.Body.Close()
		if r.StatusCode != 200 {
			return errors.New("healthcheck failed")
		}
	}
	return nil
}
