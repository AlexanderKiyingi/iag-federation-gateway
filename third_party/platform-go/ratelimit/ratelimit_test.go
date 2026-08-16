package ratelimit

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/alvor-technologies/iag-platform-go/middleware"
	"github.com/gin-gonic/gin"
)

func init() { gin.SetMode(gin.TestMode) }

// fakeClock is a manually-advanced time source for deterministic bucket tests.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time      { return c.t }
func (c *fakeClock) add(d time.Duration) { c.t = c.t.Add(d) }

func TestMemoryStore_TokenBucketAllowsThenThrottles(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewMemoryStore()
	s.now = clk.now
	rate := Rate{Requests: 3, Window: time.Minute}

	for i := 0; i < 3; i++ {
		ok, _, err := s.Allow(context.Background(), "k", rate)
		if err != nil || !ok {
			t.Fatalf("request %d should be allowed (ok=%v err=%v)", i, ok, err)
		}
	}
	ok, retry, err := s.Allow(context.Background(), "k", rate)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok {
		t.Fatal("4th request should be throttled")
	}
	if retry <= 0 {
		t.Fatalf("throttled response should report a positive retry-after, got %v", retry)
	}
}

func TestMemoryStore_Refills(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewMemoryStore()
	s.now = clk.now
	rate := Rate{Requests: 3, Window: time.Minute} // 0.05 tokens/sec

	for i := 0; i < 3; i++ {
		s.Allow(context.Background(), "k", rate)
	}
	if ok, _, _ := s.Allow(context.Background(), "k", rate); ok {
		t.Fatal("bucket should be empty")
	}
	clk.add(21 * time.Second) // ~1.05 tokens refilled
	if ok, _, _ := s.Allow(context.Background(), "k", rate); !ok {
		t.Fatal("bucket should have refilled one token after 21s")
	}
}

func TestMemoryStore_KeysAreIndependent(t *testing.T) {
	s := NewMemoryStore()
	rate := Rate{Requests: 1, Window: time.Minute}
	if ok, _, _ := s.Allow(context.Background(), "a", rate); !ok {
		t.Fatal("key a first request allowed")
	}
	if ok, _, _ := s.Allow(context.Background(), "b", rate); !ok {
		t.Fatal("key b must not be affected by key a")
	}
	if ok, _, _ := s.Allow(context.Background(), "a", rate); ok {
		t.Fatal("key a second request should throttle")
	}
}

func TestMemoryStore_EvictsIdleBuckets(t *testing.T) {
	clk := &fakeClock{t: time.Unix(1_700_000_000, 0)}
	s := NewMemoryStoreWithTTL(time.Minute)
	s.now = clk.now
	rate := Rate{Requests: 1, Window: time.Minute}

	s.Allow(context.Background(), "stale", rate)
	clk.add(2 * time.Minute) // stale bucket now past its TTL
	s.ops = sweepEvery - 1   // force a sweep on the next call
	s.Allow(context.Background(), "fresh", rate)

	if s.Len() != 1 {
		t.Fatalf("idle bucket should have been evicted, live=%d", s.Len())
	}
}

// erroringStore always fails, to exercise fail-open / fail-closed.
type erroringStore struct{}

func (erroringStore) Allow(context.Context, string, Rate) (bool, time.Duration, error) {
	return false, 0, errors.New("store down")
}

func doGET(t *testing.T, h gin.HandlerFunc, headers map[string]string, setup func(*gin.Engine)) *httptest.ResponseRecorder {
	t.Helper()
	r := gin.New()
	if setup != nil {
		setup(r)
	}
	r.Use(h)
	r.GET("/x", func(c *gin.Context) { c.String(http.StatusOK, "ok") })
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.RemoteAddr = "203.0.113.7:12345"
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMiddleware_ThrottlesWithRetryAfter(t *testing.T) {
	mw := Middleware(Config{Rate: Rate{Requests: 1, Window: time.Minute}})
	if w := doGET(t, mw, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("first request should pass, got %d", w.Code)
	}
	w := doGET(t, mw, nil, nil)
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request should be 429, got %d", w.Code)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 must carry a Retry-After header")
	}
}

func TestMiddleware_FailOpenVsClosed(t *testing.T) {
	open := Middleware(Config{Rate: PerMinute(10), Store: erroringStore{}})
	if w := doGET(t, open, nil, nil); w.Code != http.StatusOK {
		t.Fatalf("fail-open should allow on store error, got %d", w.Code)
	}
	closed := Middleware(Config{Rate: PerMinute(10), Store: erroringStore{}, FailClosed: true})
	if w := doGET(t, closed, nil, nil); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("fail-closed should 503 on store error, got %d", w.Code)
	}
}

func TestMiddleware_InvalidRateIsNoop(t *testing.T) {
	mw := Middleware(Config{Rate: Rate{Requests: 0, Window: time.Minute}})
	for i := 0; i < 5; i++ {
		if w := doGET(t, mw, nil, nil); w.Code != http.StatusOK {
			t.Fatalf("invalid rate must not throttle, got %d", w.Code)
		}
	}
}

func TestMiddleware_EmptyKeySkips(t *testing.T) {
	mw := Middleware(Config{Rate: PerMinute(1), Key: func(*gin.Context) string { return "" }})
	for i := 0; i < 3; i++ {
		if w := doGET(t, mw, nil, nil); w.Code != http.StatusOK {
			t.Fatalf("empty key should skip limiting, got %d", w.Code)
		}
	}
}

func TestByUserOrIP_PrefersPrincipal(t *testing.T) {
	// With a principal set, two different source IPs share the same user bucket.
	mw := Middleware(Config{Rate: PerMinute(1), Key: ByUserOrIP})
	setUser := func(r *gin.Engine) {
		r.Use(func(c *gin.Context) { c.Set(middleware.CtxPrincipalID, "user-123"); c.Next() })
	}
	if w := doGET(t, mw, nil, setUser); w.Code != http.StatusOK {
		t.Fatalf("first authed request should pass, got %d", w.Code)
	}
	if w := doGET(t, mw, nil, setUser); w.Code != http.StatusTooManyRequests {
		t.Fatalf("second authed request (same principal) should 429, got %d", w.Code)
	}
}

func TestParseTrustedProxies(t *testing.T) {
	cases := map[string][]string{
		"":                    nil,
		"none":                nil,
		"NONE":                nil,
		"10.0.0.0/8":          {"10.0.0.0/8"},
		"10.0.0.0/8, 1.2.3.4": {"10.0.0.0/8", "1.2.3.4"},
	}
	for in, want := range cases {
		got := ParseTrustedProxies(in)
		if len(got) != len(want) {
			t.Fatalf("%q -> %v, want %v", in, got, want)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%q[%d] = %q, want %q", in, i, got[i], want[i])
			}
		}
	}
}
