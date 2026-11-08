// Package server is the public face of leaks.pigfox.com.
//
// It starts the worker (the process that holds the leaks), restarts it on a
// timer or when it dies, and answers the page's three calls:
//
//	GET  /api/scenarios  the bugs, their fixes and their code (static)
//	GET  /api/state      both profiles, cached, plus caps and the reset timer
//	POST /api/run        {"scenario": "...", "variant": "leaky"|"fixed"}
//
// Visitors share one sandbox. Caps, a per-IP rate limit and a cached profile
// keep any one of them from hurting the box or the others.
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/pigfox/leak-demo/internal/scenarios"
	"github.com/pigfox/leak-demo/internal/worker"
)

// Config holds the server's settings.
type Config struct {
	Addr        string        // public listen address
	WorkerAddr  string        // loopback address for the worker
	Origins     []string      // allowed CORS origins
	ResetEvery  time.Duration // worker lifetime
	Cap         int           // leaked goroutines allowed per worker lifetime
	PerClick    int           // goroutines one click starts
	CacheFor    time.Duration // how long one profile read is reused
	RunRate     float64       // runs per second per IP
	RunBurst    int
	StateRate   float64 // state reads per second per IP
	StateBurst  int
	TrustProxy  bool // take the client address from Caddy's X-Forwarded-For
	Version     string
	WorkerStart func(addr string) (*exec.Cmd, error) // test hook
}

// Defaults fills unset fields.
func (c *Config) Defaults() {
	if c.Addr == "" {
		c.Addr = ":8549"
	}
	if c.WorkerAddr == "" {
		c.WorkerAddr = "127.0.0.1:8551"
	}
	if len(c.Origins) == 0 {
		c.Origins = []string{"https://pigfox.com", "https://dev.pigfox.com"}
	}
	if c.ResetEvery == 0 {
		c.ResetEvery = 10 * time.Minute
	}
	if c.Cap == 0 {
		c.Cap = 2000
	}
	if c.PerClick == 0 {
		c.PerClick = 10
	}
	if c.CacheFor == 0 {
		c.CacheFor = 3 * time.Second
	}
	if c.RunRate == 0 {
		c.RunRate, c.RunBurst = 1.0/3, 12 // 20 a minute, 12 at once: every button once
	}
	if c.StateRate == 0 {
		c.StateRate, c.StateBurst = 1, 20
	}
}

// Server is the public HTTP handler plus the worker supervisor.
type Server struct {
	cfg     Config
	log     *slog.Logger
	origins map[string]bool
	runs    *limiter
	reads   *limiter
	client  *http.Client
	mux     *http.ServeMux
	static  []byte // /api/scenarios body

	mu       sync.Mutex
	cmd      *exec.Cmd
	gen      int
	born     time.Time
	ready    bool
	created  int // leaky goroutines started this generation
	runsGen  int
	cache    []byte
	cachedAt time.Time
	cacheGen int
	readMu   sync.Mutex // one profile read at a time
	resets   chan struct{}
}

// New builds a server; Run starts the worker.
func New(cfg Config, log *slog.Logger) *Server {
	cfg.Defaults()
	s := &Server{
		cfg:     cfg,
		log:     log,
		origins: map[string]bool{},
		runs:    newLimiter(cfg.RunRate, cfg.RunBurst),
		reads:   newLimiter(cfg.StateRate, cfg.StateBurst),
		client:  &http.Client{Timeout: 5 * time.Second},
		resets:  make(chan struct{}, 1),
	}
	for _, o := range cfg.Origins {
		s.origins[strings.TrimRight(o, "/")] = true
	}
	s.static = scenariosBody(cfg)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /api/scenarios", s.scenarios)
	mux.HandleFunc("GET /api/state", s.state)
	mux.HandleFunc("POST /api/run", s.run)
	mux.HandleFunc("OPTIONS /api/", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /{$}", s.index)
	s.mux = mux
	return s
}

type scenarioOut struct {
	*scenarios.Scenario
	Leaky scenarios.Snippet  `json:"leaky"`
	Fixed *scenarios.Snippet `json:"fixed,omitempty"`
}

func scenariosBody(cfg Config) []byte {
	sn := scenarios.Snippets()
	var list []scenarioOut
	for _, sc := range scenarios.All {
		o := scenarioOut{Scenario: sc, Leaky: sn[sc.ID+"-leaky"]}
		if f, ok := sn[sc.ID+"-fixed"]; ok {
			o.Fixed = &f
		}
		list = append(list, o)
	}
	b, _ := json.Marshal(map[string]any{
		"go_version":  goVersion(),
		"version":     cfg.Version,
		"file":        scenarios.File,
		"healthy":     sn["healthy"],
		"pool_size":   worker.PoolSize,
		"per_click":   cfg.PerClick,
		"cap":         cfg.Cap,
		"reset_every": int(cfg.ResetEvery.Seconds()),
		"scenarios":   list,
	})
	return b
}

// ServeHTTP adds the common headers, CORS and a body limit.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	if o := r.Header.Get("Origin"); o != "" && s.origins[o] {
		h.Set("Access-Control-Allow-Origin", o)
		h.Set("Vary", "Origin")
		h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		h.Set("Access-Control-Allow-Headers", "Content-Type")
		h.Set("Access-Control-Max-Age", "600")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<10)
	s.mux.ServeHTTP(w, r)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) index(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintln(w, "leaks.pigfox.com: the API behind https://pigfox.com/demos/goroutine-leaks")
	fmt.Fprintln(w, "GET /api/scenarios, GET /api/state, POST /api/run")
}

func (s *Server) healthz(w http.ResponseWriter, _ *http.Request) {
	s.mu.Lock()
	ready := s.ready
	s.mu.Unlock()
	if !ready {
		http.Error(w, "worker starting", http.StatusServiceUnavailable)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) scenarios(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write(s.static)
}

type status struct {
	Generation int    `json:"generation"`
	Born       string `json:"started_at"`
	ResetIn    int    `json:"reset_in_s"`
	Cap        int    `json:"cap"`
	Created    int    `json:"leaks_started"`
	Runs       int    `json:"runs"`
}

func (s *Server) status() (status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	left := s.cfg.ResetEvery - time.Since(s.born)
	return status{
		Generation: s.gen,
		Born:       s.born.UTC().Format(time.RFC3339),
		ResetIn:    max(int(left.Seconds()), 0),
		Cap:        s.cfg.Cap,
		Created:    s.created,
		Runs:       s.runsGen,
	}, s.ready
}

func (s *Server) state(w http.ResponseWriter, r *http.Request) {
	if ok, wait := s.reads.allow(clientIP(r, s.cfg.TrustProxy), time.Now()); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(wait))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "slow down", "retry_after_s": wait})
		return
	}
	st, ready := s.status()
	if !ready {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"resetting": true, "status": st})
		return
	}
	prof, err := s.profile(r.Context(), st.Generation)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"resetting": true, "status": st})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprintf(w, `{"status":%s,"profile":%s}`, mustJSON(st), prof)
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

// profile returns the worker's profile, reading it at most once per
// CacheFor: every read runs the leak-detecting GC twice.
func (s *Server) profile(ctx context.Context, gen int) ([]byte, error) {
	s.readMu.Lock()
	defer s.readMu.Unlock()
	s.mu.Lock()
	if s.cache != nil && s.cacheGen == gen && time.Since(s.cachedAt) < s.cfg.CacheFor {
		b := s.cache
		s.mu.Unlock()
		return b, nil
	}
	s.mu.Unlock()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+s.cfg.WorkerAddr+"/profile", nil)
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("worker profile: %s", resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	b = []byte(strings.TrimSpace(string(b)))
	s.mu.Lock()
	s.cache, s.cachedAt, s.cacheGen = b, time.Now(), gen
	s.mu.Unlock()
	return b, nil
}

type runReq struct {
	Scenario string `json:"scenario"`
	Variant  string `json:"variant"`
}

func (s *Server) run(w http.ResponseWriter, r *http.Request) {
	var in runReq
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "send JSON: {\"scenario\": \"send\", \"variant\": \"leaky\"}"})
		return
	}
	sc, ok := scenarios.ByID(in.Scenario)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "unknown scenario"})
		return
	}
	fixed := in.Variant == "fixed"
	if in.Variant != "leaky" && !fixed {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "variant is leaky or fixed"})
		return
	}
	if fixed && !sc.HasFix() {
		writeJSON(w, http.StatusBadRequest, map[string]any{"error": "the blind spot has no fixed version"})
		return
	}
	ip := clientIP(r, s.cfg.TrustProxy)
	if ok, wait := s.runs.allow(ip, time.Now()); !ok {
		w.Header().Set("Retry-After", fmt.Sprint(wait))
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"error": "slow down", "retry_after_s": wait})
		return
	}
	n := s.cfg.PerClick
	s.mu.Lock()
	if !s.ready {
		s.mu.Unlock()
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"resetting": true})
		return
	}
	if !fixed && s.created+n > s.cfg.Cap {
		st := s.cfg.ResetEvery - time.Since(s.born)
		s.mu.Unlock()
		writeJSON(w, http.StatusConflict, map[string]any{"error": "the sandbox is full", "reset_in_s": max(int(st.Seconds()), 0)})
		return
	}
	if !fixed {
		s.created += n
	}
	s.runsGen++
	gen := s.gen
	s.mu.Unlock()

	u := fmt.Sprintf("http://%s/run?scenario=%s&variant=%s&n=%d", s.cfg.WorkerAddr, sc.ID, in.Variant, n)
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, u, nil)
	resp, err := s.client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"resetting": true})
		return
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "worker refused"})
		return
	}
	// A run changes the picture: the next state read takes a new profile.
	s.mu.Lock()
	if s.cacheGen == gen {
		s.cache = nil
	}
	s.mu.Unlock()
	s.log.Info("run", "scenario", sc.ID, "variant", in.Variant, "n", n, "client", maskIP(ip), "generation", gen)
	writeJSON(w, http.StatusOK, map[string]any{"started": n, "scenario": sc.ID, "variant": in.Variant})
}

// Reset asks the supervisor for a fresh worker now (tests, signals).
func (s *Server) Reset() {
	select {
	case s.resets <- struct{}{}:
	default:
	}
}

// Run supervises the worker until ctx ends: start it, wait until it answers,
// replace it every ResetEvery or as soon as it dies.
func (s *Server) Run(ctx context.Context) {
	for ctx.Err() == nil {
		cmd, err := s.start()
		if err != nil {
			s.log.Error("worker start", "err", err.Error())
			select {
			case <-ctx.Done():
				return
			case <-time.After(2 * time.Second):
			}
			continue
		}
		died := make(chan error, 1)
		go func() { died <- cmd.Wait() }()
		if !s.waitReady(ctx, died) {
			kill(cmd)
			<-died
			continue
		}
		s.mu.Lock()
		s.gen++
		s.born, s.ready, s.created, s.runsGen, s.cache = time.Now(), true, 0, 0, nil
		gen := s.gen
		s.mu.Unlock()
		s.log.Info("worker ready", "generation", gen, "pid", cmd.Process.Pid)

		timer := time.NewTimer(s.cfg.ResetEvery)
		reason := ""
		select {
		case <-ctx.Done():
			reason = "shutdown"
		case err := <-died:
			reason = "worker exited: " + errString(err)
		case <-timer.C:
			reason = "timer"
		case <-s.resets:
			reason = "requested"
		}
		timer.Stop()
		s.mu.Lock()
		s.ready = false
		s.mu.Unlock()
		if reason != "" && !strings.HasPrefix(reason, "worker exited") {
			kill(cmd)
			<-died
		}
		s.log.Info("worker reset", "generation", gen, "reason", reason)
	}
}

func errString(err error) string {
	if err == nil {
		return "status 0"
	}
	return err.Error()
}

func kill(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (s *Server) start() (*exec.Cmd, error) {
	if s.cfg.WorkerStart != nil {
		return s.cfg.WorkerStart(s.cfg.WorkerAddr)
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, "-worker", s.cfg.WorkerAddr)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Env = append(os.Environ(), "GOMEMLIMIT=64MiB")
	return cmd, cmd.Start()
}

func (s *Server) waitReady(ctx context.Context, died chan error) bool {
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return false
		case err := <-died:
			s.log.Error("worker died while starting", "err", errString(err))
			died <- err
			return false
		case <-time.After(50 * time.Millisecond):
		}
		resp, err := s.client.Get("http://" + s.cfg.WorkerAddr + "/healthz")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
	}
	return false
}

// ListenWorker serves the worker on its loopback address (the -worker mode).
func ListenWorker(addr string, log *slog.Logger) error {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || (host != "127.0.0.1" && host != "::1" && host != "localhost") {
		return errors.New("the worker listens on loopback only")
	}
	srv := &http.Server{Addr: addr, Handler: worker.Handler(log), ReadHeaderTimeout: 5 * time.Second}
	return srv.ListenAndServe()
}
