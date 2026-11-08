// Command leak-demo serves leaks.pigfox.com, the API behind
// pigfox.com/demos/goroutine-leaks: real goroutine leaks, found by Go 1.27's
// goroutine leak profile.
//
//	leak-demo                     serve on LISTEN_ADDR (default :8549)
//	leak-demo -worker <addr>      the child process that holds the leaks
//	leak-demo -healthcheck        GET 127.0.0.1:<port>/healthz, exit 0 on 200
//
// Settings: LISTEN_ADDR, WORKER_ADDR, ALLOWED_ORIGINS (comma separated),
// RESET_EVERY (a Go duration), LEAK_CAP, TRUST_FORWARDED (true behind Caddy).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/pigfox/leak-demo/internal/server"
)

var version = "dev"

func main() {
	workerAddr := flag.String("worker", "", "run as the worker on this loopback address")
	health := flag.Bool("healthcheck", false, "probe the local /healthz and exit")
	flag.Parse()
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *workerAddr != "" {
		if err := server.ListenWorker(*workerAddr, log.With("role", "worker")); err != nil {
			log.Error("worker", "err", err.Error())
			os.Exit(1)
		}
		return
	}

	cfg := server.Config{
		Addr:       os.Getenv("LISTEN_ADDR"),
		WorkerAddr: os.Getenv("WORKER_ADDR"),
		TrustProxy: os.Getenv("TRUST_FORWARDED") == "true",
		Version:    version,
	}
	if o := os.Getenv("ALLOWED_ORIGINS"); o != "" {
		for _, s := range strings.Split(o, ",") {
			if s = strings.TrimSpace(s); s != "" {
				cfg.Origins = append(cfg.Origins, s)
			}
		}
	}
	if v := os.Getenv("RESET_EVERY"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			fmt.Fprintln(os.Stderr, "config: RESET_EVERY must be a duration of at least 1m")
			os.Exit(2)
		}
		cfg.ResetEvery = d
	}
	if v := os.Getenv("LEAK_CAP"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 20000 {
			fmt.Fprintln(os.Stderr, "config: LEAK_CAP must be 10 to 20000")
			os.Exit(2)
		}
		cfg.Cap = n
	}
	cfg.Defaults()
	if *health {
		os.Exit(probe(cfg.Addr))
	}

	s := server.New(cfg, log)
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      15 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    16 << 10,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	log.Info("starting", "addr", cfg.Addr, "version", version, "reset_every", cfg.ResetEvery.String(), "cap", cfg.Cap)
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Error("serve", "err", err.Error())
			os.Exit(1)
		}
	case <-ctx.Done():
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		<-done
		log.Info("stopped")
	}
}

func probe(addr string) int {
	_, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 1
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://127.0.0.1:" + port + "/healthz")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}
