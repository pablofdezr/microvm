package api

import (
	"math"
	"net/http"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/pablofdezr/microvm/internal/auth"
)

// Rate limiting is keyed on the resolved principal, and the key is the whole of
// the design.
//
// Not the raw token: a tenant with three keys would get three allowances, so the
// limit would be a limit on key hygiene rather than on load. Not the address
// either -- an address is not an identity. One tenant behind a NAT would be
// charged for its neighbours' traffic, and one tenant with a pool of egress
// addresses would be charged for almost none of its own. The tenant is the only
// key a caller cannot mint more of, which is exactly the property a limit needs.

// rateBucketTTL is how long a bucket outlives its last request.
//
// Anything idle this long has refilled to full, so keeping it says nothing a
// fresh bucket would not -- and dropping it is what keeps the map the size of
// the traffic rather than the size of the token list.
const rateBucketTTL = 10 * time.Minute

// maxRateBuckets bounds the map whatever the traffic. A bucket per tenant is
// small, but "small times however many tenants exist" is not a number this
// package gets to choose, and a limiter that exhausts the daemon's memory has
// failed worse than one that lets a tenant through.
const maxRateBuckets = 10_000

// rateSweepInterval throttles the walk that collects idle buckets. Without it a
// stream of first-time tenants would each pay a walk of the whole map, under the
// one lock every request takes. A bucket therefore outlives its last request by
// up to rateBucketTTL plus this, and nothing depends on the exact moment.
const rateSweepInterval = time.Minute

// rateBucket is one principal's allowance: what is left, and when that was true.
type rateBucket struct {
	tokens float64
	last   time.Time
}

// rateLimiter is a token bucket per principal.
type rateLimiter struct {
	// now is the clock, injected because a rate limiter tested by sleeping is a
	// test that is either slow or flaky.
	now func() time.Time

	mu      sync.Mutex
	buckets map[string]*rateBucket
	// lastSweep is when idle buckets were last collected, which is what keeps that
	// walk off the path a new tenant's first request takes.
	lastSweep time.Time
}

func newRateLimiter() *rateLimiter {
	return &rateLimiter{now: time.Now, buckets: make(map[string]*rateBucket)}
}

// allow spends one request from key's allowance.
//
// It returns how long until there would be one, and whether this request is
// allowed. The wait is the useful half: a caller told only "no" retries at once,
// which is how a limiter turns one flood into two.
func (l *rateLimiter) allow(key string, rps float64) (time.Duration, bool) {
	if rps <= 0 {
		return 0, true // unlimited, and the default
	}

	// A second's worth of burst, and never less than one request. Without it a
	// limit of 10/s refuses the second of two calls that arrive in the same
	// millisecond -- ordinary client behaviour, not abuse.
	burst := math.Max(rps, 1)

	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		// Sweeping here rather than per request keeps the hot path O(1): the cost
		// is paid by a tenant arriving, not by every call it then makes.
		l.evictLocked(now)
		b = &rateBucket{tokens: burst, last: now}
		l.buckets[key] = b
	} else {
		// max(0): a clock that went backwards must not drain the bucket.
		b.tokens = math.Min(burst, b.tokens+math.Max(now.Sub(b.last).Seconds(), 0)*rps)
		b.last = now
	}

	if b.tokens < 1 {
		return retryAfterFor((1 - b.tokens) / rps), false
	}
	b.tokens--
	return 0, true
}

// retryAfterFor turns a wait in seconds into what Retry-After can carry.
//
// Rounded up, and never below a second: the header is integer seconds, and
// rounding down would send the caller back before there is anything for them.
func retryAfterFor(seconds float64) time.Duration {
	return time.Duration(math.Max(math.Ceil(seconds), 1)) * time.Second
}

// evictLocked drops idle buckets, and the least recently used tenth if the map
// is still full.
func (l *rateLimiter) evictLocked(now time.Time) {
	if now.Sub(l.lastSweep) >= rateSweepInterval {
		l.lastSweep = now
		for k, b := range l.buckets {
			if now.Sub(b.last) > rateBucketTTL {
				delete(l.buckets, k)
			}
		}
	}
	if len(l.buckets) < maxRateBuckets {
		return
	}

	// A tenth, not the single oldest: one victim per arrival would mean a full
	// scan for every new tenant from the moment the cap is reached, and that scan
	// runs under the lock every request takes. Freeing a batch amortizes it to
	// nothing.
	//
	// Losing a bucket hands its tenant a fresh allowance, which is a smaller wrong
	// than a map that grows until the daemon dies and takes every sandbox on the
	// node with it.
	type aged struct {
		key  string
		last time.Time
	}
	all := make([]aged, 0, len(l.buckets))
	for k, b := range l.buckets {
		all = append(all, aged{key: k, last: b.last})
	}
	slices.SortFunc(all, func(a, b aged) int { return a.last.Compare(b.last) })

	for _, a := range all[:max(len(all)/10, 1)] {
		delete(l.buckets, a.key)
	}
}

// size is how many buckets are held. For the test that asserts the map is
// bounded, which is the only way that property can be checked from outside.
func (l *rateLimiter) size() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.buckets)
}

// rateKey is the bucket a principal spends from: its tenant, and its rate.
//
// The tenant is the whole point -- it is the one key a caller cannot mint more of,
// so three keys for one tenant share one allowance and rotating them buys nothing.
// The rate is in the key because two keys of one tenant may carry different ones,
// and a single bucket for both would take its burst and its refill from whichever
// called last: one request on a 100 rps key would refill the bucket a 1 rps key
// then drains, so the tighter limit would not be enforced at all. Keys at one rate
// still share one bucket, which is the property the limit rests on.
func rateKey(p *auth.Principal) string {
	return p.Tenant + "|" + strconv.FormatFloat(p.MaxRequestsPerSecond, 'g', -1, 64)
}

// withRateLimit refuses a caller asking faster than their key allows.
//
// It runs after auth, and that ordering is load-bearing twice over. It is what
// gives the limiter a resolved principal to charge, and it is what bounds the
// map: an unauthenticated flood is refused with a 401 before it can name a
// bucket, so the keys that exist are the tenants that exist. A limiter in front
// of auth could only key on something the caller sends, and a map keyed on a
// caller-controlled string is a memory exhaustion primitive handed to strangers.
func (s *Server) withRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := principalFrom(r.Context())
		if p == nil {
			// Auth disabled. There is no identity to charge, and a dev daemon with
			// a single caller has nobody to protect from them.
			next.ServeHTTP(w, r)
			return
		}

		retryAfter, ok := s.limiter.allow(rateKey(p), p.MaxRequestsPerSecond)
		if !ok {
			// Debug, not Warn: a refused flood must not become a logged flood, and
			// the caller is already being told in the reply.
			s.log.Debug("rate limited",
				"tenant", p.Tenant, "path", r.URL.Path, "rps", p.MaxRequestsPerSecond)
			s.writeAPIError(w, r, rateLimitError(p.MaxRequestsPerSecond, retryAfter))
			return
		}
		next.ServeHTTP(w, r)
	})
}
