package scenarios

import "strings"

// Snippet is one marked block of this package's source.
type Snippet struct {
	Code string `json:"code"`
	// FirstLine is the line number in scenarios.go of Code's first line, so
	// a stack frame's line maps to Code line (line - FirstLine + 1).
	FirstLine int `json:"first_line"`
}

// Snippets returns every "demo:begin <name>" ... "demo:end" block, keyed by
// name (for example "send-leaky", "send-fixed", "healthy").
func Snippets() map[string]Snippet {
	out := map[string]Snippet{}
	lines := strings.Split(Source, "\n")
	name, first := "", 0
	var buf []string
	for i, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "// demo:begin "):
			name, first, buf = strings.TrimPrefix(t, "// demo:begin "), i+2, nil
		case t == "// demo:end" && name != "":
			for len(buf) > 0 && strings.TrimSpace(buf[len(buf)-1]) == "" {
				buf = buf[:len(buf)-1]
			}
			out[name] = Snippet{Code: strings.Join(buf, "\n"), FirstLine: first}
			name = ""
		case name != "":
			buf = append(buf, l)
		}
	}
	return out
}
