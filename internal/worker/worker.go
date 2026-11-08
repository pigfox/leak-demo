// Package worker is the disposable process that holds the demo's leaks.
//
// A leaked goroutine never ends: nothing can wake it, and the runtime does
// not collect it. The only way to give the memory back is to end the
// process. So the leaks live here, in a child process the public server
// starts, and a reset is the server killing this process and starting a
// fresh one. The worker listens on loopback only.
package worker

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime"
	"runtime/pprof"
	"strconv"
	"strings"
	"time"

	"github.com/pigfox/leak-demo/internal/profile"
	"github.com/pigfox/leak-demo/internal/scenarios"
)

// PoolSize is how many healthy idle workers every fresh process starts.
const PoolSize = 4

// MaxPerRun bounds one /run call, whatever the server asks for.
const MaxPerRun = 50

// Profile is what /profile returns.
type Profile struct {
	At         time.Time       `json:"at"`
	Goroutines int             `json:"goroutines"`
	HeapBytes  uint64          `json:"heap_bytes"`
	LeakCheck  int64           `json:"leak_check_ms"`
	FirstPass  int             `json:"first_pass_leaks"`
	Leaks      profile.Summary `json:"leaks"`
	Dump       profile.Summary `json:"dump"`
}

// Classify maps a scenarios-package function to its scenario id.
func Classify(fn string) string {
	if strings.HasPrefix(fn, "StartPool.") {
		return "healthy"
	}
	for _, s := range scenarios.All {
		if fn == s.Func || strings.HasPrefix(fn, s.Func+".") {
			return s.ID
		}
		if s.HasFix() && (fn == s.Func+"Fixed" || strings.HasPrefix(fn, s.Func+"Fixed.")) {
			return s.ID + "-fixed"
		}
	}
	return ""
}

func leakText() string {
	var b strings.Builder
	_ = pprof.Lookup("goroutineleak").WriteTo(&b, 1)
	return b.String()
}

// Read takes both profiles. The leak check runs twice: one pass can miss a
// leak whose primitive was still marked from earlier in the cycle, and the
// second catches it. The first pass's count is kept so the page can say so.
func Read() Profile {
	first := profile.ParseLeak(leakText(), Classify)
	start := time.Now()
	leaks := profile.ParseLeak(leakText(), Classify)
	ms := time.Since(start).Milliseconds()
	var d strings.Builder
	_ = pprof.Lookup("goroutine").WriteTo(&d, 2)
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	return Profile{
		At:         time.Now().UTC(),
		Goroutines: runtime.NumGoroutine(),
		HeapBytes:  m.HeapInuse,
		LeakCheck:  ms,
		FirstPass:  first.Total,
		Leaks:      leaks,
		Dump:       profile.ParseDump(d.String(), Classify),
	}
}

// Handler serves /run, /profile and /healthz.
func Handler(log *slog.Logger) http.Handler {
	scenarios.StartPool(PoolSize)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("POST /run", func(w http.ResponseWriter, r *http.Request) {
		s, ok := scenarios.ByID(r.URL.Query().Get("scenario"))
		if !ok {
			http.Error(w, "unknown scenario", http.StatusBadRequest)
			return
		}
		fixed := r.URL.Query().Get("variant") == "fixed"
		if fixed && !s.HasFix() {
			http.Error(w, "no fixed version", http.StatusBadRequest)
			return
		}
		n, err := strconv.Atoi(r.URL.Query().Get("n"))
		if err != nil || n < 1 || n > MaxPerRun {
			http.Error(w, "bad n", http.StatusBadRequest)
			return
		}
		s.Run(fixed, n)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /profile", func(w http.ResponseWriter, _ *http.Request) {
		p := Read()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(p)
	})
	_ = log
	return mux
}
