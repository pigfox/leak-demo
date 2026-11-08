package worker

import (
	"strings"
	"testing"
	"time"

	"github.com/pigfox/leak-demo/internal/scenarios"
)

func count(p Profile, scenario string, dump bool) int {
	s := p.Leaks
	if dump {
		s = p.Dump
	}
	n := 0
	for _, g := range s.Groups {
		if g.Scenario == scenario {
			n += g.Count
		}
	}
	return n
}

// TestRealLeaks runs every scenario in this test process and checks what the
// Go runtime's own leak profile reports: each leaky version is found, each
// fixed version leaves nothing, and the blind spot is not reported.
func TestRealLeaks(t *testing.T) {
	const n = 7
	for _, s := range scenarios.All {
		s.Run(false, n)
		if s.HasFix() {
			s.Run(true, n)
		}
	}
	time.Sleep(100 * time.Millisecond) // the fixed versions finish, the leaks park
	p := Read()
	for _, s := range scenarios.All {
		want := n
		if !s.Detects {
			want = 0
		}
		if got := count(p, s.ID, false); got != want {
			t.Errorf("%s: leak profile reports %d, want %d", s.ID, got, want)
		}
		if got := count(p, s.ID, true); got != n {
			t.Errorf("%s: goroutine dump shows %d, want %d", s.ID, got, n)
		}
		if got := count(p, s.ID+"-fixed", true) + count(p, s.ID+"-fixed", false); got != 0 {
			t.Errorf("%s fixed: %d goroutines left, want 0", s.ID, got)
		}
	}
	if p.Leaks.Other != 0 {
		t.Errorf("%d leaks outside the scenarios", p.Leaks.Other)
	}
}

// TestLinesPointIntoSnippets checks that every leak's line falls inside its
// scenario's leaky snippet, on the line marked "blocks forever".
func TestLinesPointIntoSnippets(t *testing.T) {
	sn := scenarios.Snippets()
	for _, s := range scenarios.All {
		s.Run(false, 1)
	}
	time.Sleep(50 * time.Millisecond)
	p := Read()
	groups := append(p.Leaks.Groups, p.Dump.Groups...)
	seen := map[string]bool{}
	for _, g := range groups {
		if g.Scenario == "" || g.Scenario == "healthy" || strings.HasSuffix(g.Scenario, "-fixed") {
			continue
		}
		code := sn[g.Scenario+"-leaky"]
		lines := strings.Split(code.Code, "\n")
		i := g.Line - code.FirstLine
		if i < 0 || i >= len(lines) {
			t.Errorf("%s: line %d is outside its snippet (first line %d, %d lines)", g.Scenario, g.Line, code.FirstLine, len(lines))
			continue
		}
		if !strings.Contains(lines[i], "forever") && !strings.Contains(lines[i], "not reported") {
			t.Errorf("%s: line %d is %q, not the stuck line", g.Scenario, g.Line, strings.TrimSpace(lines[i]))
		}
		seen[g.Scenario] = true
	}
	for _, s := range scenarios.All {
		if !seen[s.ID] {
			t.Errorf("%s: no group seen", s.ID)
		}
	}
}

func TestClassify(t *testing.T) {
	cases := map[string]string{
		"fetchWithTimeout.func1":      "send",
		"fetchWithTimeoutFixed.func1": "send-fixed",
		"processBatch.func2":          "waitgroup",
		"StartPool.func1":             "healthy",
		"Snippets":                    "",
	}
	for fn, want := range cases {
		if got := Classify(fn); got != want {
			t.Errorf("Classify(%q) = %q, want %q", fn, got, want)
		}
	}
}
