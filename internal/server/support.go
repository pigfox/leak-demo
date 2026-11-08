package server

import (
	"net"
	"net/http"
	"runtime"
	"strings"
	"sync"
	"time"
)

func goVersion() string { return runtime.Version() }

type bucket struct {
	tokens float64
	last   time.Time
}

// limiter is a token bucket per client (as in rpc-gateway).
type limiter struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	buckets map[string]*bucket
	swept   time.Time
}

func newLimiter(rate float64, burst int) *limiter {
	return &limiter{rate: rate, burst: float64(burst), buckets: map[string]*bucket{}}
}

// allow takes one token; when refused it says how many whole seconds until
// the next one.
func (l *limiter) allow(key string, now time.Time) (bool, int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.Sub(l.swept) > time.Minute {
		for k, b := range l.buckets {
			if now.Sub(b.last).Seconds()*l.rate >= l.burst {
				delete(l.buckets, k)
			}
		}
		l.swept = now
	}
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now
	if b.tokens < 1 {
		wait := int((1-b.tokens)/l.rate + 0.999)
		return false, max(wait, 1)
	}
	b.tokens--
	return true, 0
}

// clientIP is the rightmost X-Forwarded-For entry, the one Caddy set, when
// the server trusts its proxy; else the TCP peer.
func clientIP(r *http.Request, trust bool) string {
	if trust {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if ip := strings.TrimSpace(parts[len(parts)-1]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// maskIP keeps logs free of full addresses: /24 for IPv4, /48 for IPv6.
func maskIP(s string) string {
	ip := net.ParseIP(s)
	if ip == nil {
		return "invalid"
	}
	if v4 := ip.To4(); v4 != nil {
		return net.IPv4(v4[0], v4[1], v4[2], 0).String() + "/24"
	}
	return ip.Mask(net.CIDRMask(48, 128)).String() + "/48"
}
