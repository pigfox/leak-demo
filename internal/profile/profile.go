// Package profile reads the runtime's goroutine profiles and turns them into
// the small, safe summary the page shows: groups of goroutines by where in
// the demo code they are stuck. Nothing outside the scenarios package is
// shown frame by frame; the rest is one "other" count.
package profile

import (
	"bufio"
	"sort"
	"strconv"
	"strings"
)

// Group is a set of goroutines stuck at the same place.
type Group struct {
	Count    int    `json:"count"`
	Scenario string `json:"scenario"` // scenario id, "healthy" or "" for other
	Func     string `json:"func"`     // for example fetchWithTimeout.func1
	Line     int    `json:"line"`     // line in scenarios.go
	State    string `json:"state,omitempty"`
}

// Summary is one profile, grouped.
type Summary struct {
	Total  int     `json:"total"`
	Other  int     `json:"other"`
	Groups []Group `json:"groups"`
}

// Classifier maps a short scenarios-package function name to a scenario id.
type Classifier func(fn string) string

const pkgMarker = "/internal/scenarios."

type frame struct {
	fn   string
	line int
}

// demoFrame returns the first frame in the scenarios package, if any.
func demoFrame(frames []frame) (frame, bool) {
	for _, f := range frames {
		if i := strings.Index(f.fn, pkgMarker); i >= 0 {
			return frame{fn: f.fn[i+len(pkgMarker):], line: f.line}, true
		}
	}
	return frame{}, false
}

// fileLine parses "/path/scenarios.go:42" (with an optional " +0x1c").
func fileLine(s string) (string, int) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, " "); i >= 0 {
		s = s[:i]
	}
	i := strings.LastIndex(s, ":")
	if i < 0 {
		return s, 0
	}
	n, _ := strconv.Atoi(s[i+1:])
	return s[:i], n
}

type key struct {
	fn, state string
	line      int
}

func build(m map[key]int, order []key, other, total int, classify Classifier) Summary {
	s := Summary{Total: total, Other: other, Groups: []Group{}}
	for _, k := range order {
		s.Groups = append(s.Groups, Group{Count: m[k], Scenario: classify(k.fn), Func: k.fn, Line: k.line, State: k.state})
	}
	sort.SliceStable(s.Groups, func(i, j int) bool { return s.Groups[i].Count > s.Groups[j].Count })
	return s
}

// ParseLeak reads a goroutineleak profile written with debug=1:
//
//	goroutineleak profile: total 15
//	3 @ 0x47fa4a 0x45d0d7 ...
//	#	0x4e366b	pkg.fn.func1+0x4b	/path/scenarios.go:45
func ParseLeak(text string, classify Classifier) Summary {
	m := map[key]int{}
	var order []key
	total, other := 0, 0
	count := 0
	var frames []frame
	flush := func() {
		if count == 0 {
			return
		}
		if f, ok := demoFrame(frames); ok {
			k := key{fn: f.fn, line: f.line}
			if _, seen := m[k]; !seen {
				order = append(order, k)
			}
			m[k] += count
		} else {
			other += count
		}
		count, frames = 0, nil
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "goroutineleak profile: total "):
			total, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(l, "goroutineleak profile: total ")))
		case strings.Contains(l, " @ ") && !strings.HasPrefix(l, "#"):
			flush()
			count, _ = strconv.Atoi(strings.TrimSpace(l[:strings.Index(l, " @ ")]))
		case strings.HasPrefix(l, "#"):
			f := strings.Fields(l)
			if len(f) >= 4 {
				fn := f[2]
				if i := strings.LastIndex(fn, "+0x"); i >= 0 {
					fn = fn[:i]
				}
				_, line := fileLine(f[3])
				frames = append(frames, frame{fn: fn, line: line})
			}
		case strings.TrimSpace(l) == "":
			flush()
		}
	}
	flush()
	return build(m, order, other, total, classify)
}

// ParseDump reads a goroutine profile written with debug=2 (the panic-style
// dump every Go version has printed). The "(leaked)" mark Go 1.27 adds after
// a leak check is removed: this panel shows what the old dump told you.
func ParseDump(text string, classify Classifier) Summary {
	m := map[key]int{}
	var order []key
	total, other := 0, 0
	state := ""
	var frames []frame
	var pendingFn string
	in := false
	flush := func() {
		if !in {
			return
		}
		total++
		if f, ok := demoFrame(frames); ok {
			k := key{fn: f.fn, line: f.line, state: state}
			if _, seen := m[k]; !seen {
				order = append(order, k)
			}
			m[k]++
		} else {
			other++
		}
		in, frames, state, pendingFn = false, nil, "", ""
	}
	sc := bufio.NewScanner(strings.NewReader(text))
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	for sc.Scan() {
		l := sc.Text()
		switch {
		case strings.HasPrefix(l, "goroutine ") && strings.HasSuffix(l, "]:"):
			flush()
			in = true
			if a, b := strings.Index(l, "["), strings.LastIndex(l, "]"); a >= 0 && b > a {
				state = l[a+1 : b]
				state = strings.TrimSuffix(state, " (leaked)")
				// "chan receive, 3 minutes" keeps only the wait reason.
				if i := strings.Index(state, ","); i >= 0 {
					state = state[:i]
				}
			}
		case strings.TrimSpace(l) == "":
			flush()
		case !in:
		case strings.HasPrefix(l, "\t"):
			if pendingFn != "" {
				_, line := fileLine(l)
				frames = append(frames, frame{fn: pendingFn, line: line})
				pendingFn = ""
			}
		case strings.HasPrefix(l, "created by "):
			// The creator is not where the goroutine is stuck: its file
			// line, which follows, is ignored because pendingFn is empty.
			pendingFn = ""
		default:
			fn := l
			if i := strings.LastIndex(fn, "("); i > 0 {
				fn = fn[:i]
			}
			pendingFn = fn
		}
	}
	flush()
	return build(m, order, other, total, classify)
}
