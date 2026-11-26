package action

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acme/ontology/internal/platform/apperr"
)

// RateLimit is a token bucket: Burst requests at once, refilled at PerMinute.
type RateLimit struct {
	PerMinute float64
	Burst     int
}

// Limiter limits action requests per actor and action type, in memory (per replica: with N
// replicas behind a load balancer the effective limit is up to N times higher).
type Limiter struct {
	Default RateLimit
	// PerAction overrides Default for specific action types.
	PerAction map[string]RateLimit
	// Agents, when set, replaces Default for AGENT callers (per-action overrides still win).
	Agents *RateLimit
	// MaxKeys bounds memory; when exceeded, idle buckets are dropped (default 100000).
	MaxKeys int

	now     func() time.Time
	mu      sync.Mutex
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

func (l *Limiter) limitFor(actor Actor, actionType string) RateLimit {
	if rl, ok := l.PerAction[actionType]; ok {
		return rl
	}
	if actor.Kind == ActorAgent && l.Agents != nil {
		return *l.Agents
	}
	return l.Default
}

// Allow takes n tokens for (actor, actionType). It returns 0 when allowed, otherwise how long to
// wait before n tokens are available. A limit with PerMinute <= 0 is unlimited.
func (l *Limiter) Allow(actor Actor, actionType string, n int) time.Duration {
	if l == nil {
		return 0
	}
	rl := l.limitFor(actor, actionType)
	if rl.PerMinute <= 0 {
		return 0
	}
	burst := float64(rl.Burst)
	if burst < 1 {
		burst = math.Max(1, math.Ceil(rl.PerMinute/60))
	}
	need := float64(n)
	if need > burst {
		need = burst // a batch larger than the burst may still run once the bucket is full
	}
	rate := rl.PerMinute / 60 // tokens per second
	now := time.Now
	if l.now != nil {
		now = l.now
	}
	t := now()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buckets == nil {
		l.buckets = map[string]*bucket{}
	}
	key := actor.ID + "\x00" + actionType
	b := l.buckets[key]
	if b == nil {
		l.evict(t, burst, rate)
		b = &bucket{tokens: burst, last: t}
		l.buckets[key] = b
	}
	b.tokens = math.Min(burst, b.tokens+t.Sub(b.last).Seconds()*rate)
	b.last = t
	if b.tokens >= need {
		b.tokens -= need
		return 0
	}
	return time.Duration((need - b.tokens) / rate * float64(time.Second))
}

// evict drops buckets that have refilled completely (indistinguishable from new ones).
func (l *Limiter) evict(t time.Time, burst, rate float64) {
	max := l.MaxKeys
	if max <= 0 {
		max = 100000
	}
	if len(l.buckets) < max {
		return
	}
	for k, b := range l.buckets {
		if b.tokens+t.Sub(b.last).Seconds()*rate >= burst {
			delete(l.buckets, k)
		}
	}
}

// RateLimited is the 429 returned when a limit is exceeded.
func RateLimited(w http.ResponseWriter, wait time.Duration) *apperr.Error {
	secs := int(math.Ceil(wait.Seconds()))
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	return apperr.New(http.StatusTooManyRequests, "RATE_LIMITED", "too many requests; retry in %ds", secs).
		WithDetails(map[string]any{"retryAfterSeconds": secs})
}

// ParseRateLimit parses "<perMinute>[/<burst>]", e.g. "60/10". An empty string means unlimited.
func ParseRateLimit(s string) (RateLimit, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return RateLimit{}, nil
	}
	rate, burst, _ := strings.Cut(s, "/")
	pm, err := strconv.ParseFloat(rate, 64)
	if err != nil || pm < 0 {
		return RateLimit{}, fmt.Errorf("invalid rate limit %q: want <perMinute>[/<burst>]", s)
	}
	rl := RateLimit{PerMinute: pm}
	if burst != "" {
		if rl.Burst, err = strconv.Atoi(burst); err != nil || rl.Burst < 1 {
			return RateLimit{}, fmt.Errorf("invalid burst in rate limit %q", s)
		}
	}
	return rl, nil
}

// ParseActionRateLimits parses "actionA=60/10,actionB=5".
func ParseActionRateLimits(s string) (map[string]RateLimit, error) {
	out := map[string]RateLimit{}
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part == "" {
			continue
		}
		name, spec, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(name) == "" {
			return nil, fmt.Errorf("invalid per-action rate limit %q: want <action>=<perMinute>[/<burst>]", part)
		}
		rl, err := ParseRateLimit(spec)
		if err != nil {
			return nil, err
		}
		out[strings.TrimSpace(name)] = rl
	}
	return out, nil
}
