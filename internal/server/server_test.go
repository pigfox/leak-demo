package server

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/pigfox/leak-demo/internal/worker"
)

// newTest serves the worker in-process on an httptest listener and gives the
// supervisor a stand-in child (sleep), so the whole server path runs.
func newTest(t *testing.T, cfg Config) (*Server, *httptest.Server) {
	t.Helper()
	w := httptest.NewServer(worker.Handler(slog.New(slog.DiscardHandler)))
	t.Cleanup(w.Close)
	cfg.WorkerAddr = strings.TrimPrefix(w.URL, "http://")
	cfg.WorkerStart = func(string) (*exec.Cmd, error) {
		c := exec.Command("sleep", "3600")
		return c, c.Start()
	}
	s := New(cfg, slog.New(slog.DiscardHandler))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go s.Run(ctx)
	for range 100 {
		if _, ok := s.status(); ok {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	pub := httptest.NewServer(s)
	t.Cleanup(pub.Close)
	return s, pub
}

func post(t *testing.T, url, body string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url+"/api/run", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func TestRunValidates(t *testing.T) {
	_, pub := newTest(t, Config{})
	for body, want := range map[string]int{
		`{"scenario":"nope","variant":"leaky"}`:      400,
		`{"scenario":"send","variant":"weird"}`:      400,
		`{"scenario":"blindspot","variant":"fixed"}`: 400,
		`not json`:                              400,
		`{"scenario":"send","variant":"fixed"}`: 200,
	} {
		if code, _ := post(t, pub.URL, body); code != want {
			t.Errorf("%s: %d, want %d", body, code, want)
		}
	}
}

func TestCapAndRateLimit(t *testing.T) {
	_, pub := newTest(t, Config{Cap: 30, PerClick: 10, RunRate: 0.001, RunBurst: 5})
	codes := []int{}
	for range 5 {
		c, _ := post(t, pub.URL, `{"scenario":"select","variant":"leaky"}`)
		codes = append(codes, c)
	}
	// Three fit under the cap of 30, then the sandbox is full.
	if want := []int{200, 200, 200, 409, 409}; !equal(codes, want) {
		t.Errorf("codes %v, want %v", codes, want)
	}
	if c, _ := post(t, pub.URL, `{"scenario":"select","variant":"leaky"}`); c != 429 {
		t.Errorf("after the burst: %d, want 429", c)
	}
}

func equal(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestStateAndCORS(t *testing.T) {
	_, pub := newTest(t, Config{})
	post(t, pub.URL, `{"scenario":"mutex","variant":"leaky"}`)
	req, _ := http.NewRequest(http.MethodGet, pub.URL+"/api/state", nil)
	req.Header.Set("Origin", "https://pigfox.com")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if got := resp.Header.Get("Access-Control-Allow-Origin"); got != "https://pigfox.com" {
		t.Errorf("CORS origin %q", got)
	}
	b, _ := io.ReadAll(resp.Body)
	var st struct {
		Status  status `json:"status"`
		Profile worker.Profile
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatalf("state is not JSON: %v: %s", err, b)
	}
	if st.Status.Created != 10 || st.Profile.Leaks.Total < 10 {
		t.Errorf("created %d, leaks %d", st.Status.Created, st.Profile.Leaks.Total)
	}

	req, _ = http.NewRequest(http.MethodGet, pub.URL+"/api/state", nil)
	req.Header.Set("Origin", "https://evil.example")
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if got := resp2.Header.Get("Access-Control-Allow-Origin"); got != "" {
		t.Errorf("foreign origin allowed: %q", got)
	}
}

func TestScenariosBody(t *testing.T) {
	_, pub := newTest(t, Config{})
	resp, err := http.Get(pub.URL + "/api/scenarios")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body struct {
		Scenarios []struct {
			ID    string `json:"id"`
			Leaky struct {
				Code string `json:"code"`
			} `json:"leaky"`
			Fixed *struct {
				Code string `json:"code"`
			} `json:"fixed"`
		} `json:"scenarios"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if len(body.Scenarios) != 6 {
		t.Fatalf("%d scenarios, want 6", len(body.Scenarios))
	}
	for _, s := range body.Scenarios {
		if s.Leaky.Code == "" {
			t.Errorf("%s: no code", s.ID)
		}
		if (s.Fixed == nil) != (s.ID == "blindspot") {
			t.Errorf("%s: fixed present = %v", s.ID, s.Fixed != nil)
		}
	}
}
