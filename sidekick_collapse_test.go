package sidekick

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/caddyserver/caddy/v2"
	"github.com/caddyserver/caddy/v2/modules/caddyhttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// slowOrigin serves a fixed body after a delay, so concurrent requests genuinely
// overlap and collapsing has something to collapse.
type slowOrigin struct {
	body    []byte
	delay   time.Duration
	calls   atomic.Int32
	release chan struct{} // when non-nil, the handler blocks until closed
}

func (o *slowOrigin) ServeHTTP(w http.ResponseWriter, r *http.Request) error {
	o.calls.Add(1)

	if o.release != nil {
		<-o.release
	}
	if o.delay > 0 {
		time.Sleep(o.delay)
	}

	w.Header().Set("Content-Type", "video/mp4")
	w.Header().Set("Etag", `"slow"`)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(o.body))
	return nil
}

func mustReadBody(t *testing.T, resp *http.Response) []byte {
	t.Helper()
	b, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return b
}

// TestCollapse_ConcurrentColdRangesFillOnce is the point of the feature: N clients
// seeking into the same cold video must not each pull the whole object from origin.
func TestCollapse_ConcurrentColdRangesFillOnce(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	s.RangeFillCollapseWait = caddy.Duration(5 * time.Second)

	origin := &slowOrigin{body: mediaBody(64 * 1024), delay: 150 * time.Millisecond}
	const path = "/wp-content/uploads/concurrent.mp4"

	const clients = 10
	var wg sync.WaitGroup
	results := make([]*http.Response, clients)
	bodies := make([][]byte, clients)

	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest("GET", path, nil)
			// Distinct windows so a shared-state bug shows up as wrong bytes.
			start := i * 100
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, start+99))
			rec := httptest.NewRecorder()
			require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(origin.ServeHTTP)))
			results[i] = rec.Result()
			bodies[i] = mustReadBody(t, results[i])
		}(i)
	}
	wg.Wait()

	assert.Equal(t, int32(1), origin.calls.Load(),
		"a cold object must be fetched from the origin exactly once")

	for i := 0; i < clients; i++ {
		start := i * 100
		assert.Equal(t, http.StatusPartialContent, results[i].StatusCode, "client %d", i)
		assert.Equal(t, origin.body[start:start+100], bodies[i],
			"client %d received the wrong byte window", i)
	}
}

// TestCollapse_FollowerTimesOutAndStillSucceeds is the degrade-open guarantee: a
// follower whose leader is slower than the collapse window must still get a correct
// response rather than hanging or failing.
func TestCollapse_FollowerTimesOutAndStillSucceeds(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	s.RangeFillCollapseWait = caddy.Duration(50 * time.Millisecond)

	release := make(chan struct{})
	origin := &slowOrigin{body: mediaBody(64 * 1024), release: release}
	const path = "/wp-content/uploads/slow.mp4"

	// Leader: blocks in the origin until released.
	leaderDone := make(chan struct{})
	go func() {
		defer close(leaderDone)
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Range", "bytes=0-99")
		rec := httptest.NewRecorder()
		_ = s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(origin.ServeHTTP))
	}()

	// Give the leader time to register as in-flight.
	time.Sleep(20 * time.Millisecond)

	// Follower: waits 50ms, gives up, goes to the origin itself.
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("Range", "bytes=200-299")
	rec := httptest.NewRecorder()

	followerStart := time.Now()
	go func() {
		// Unblock the leader once the follower has certainly timed out.
		time.Sleep(120 * time.Millisecond)
		close(release)
	}()
	require.NoError(t, s.ServeHTTP(rec, req, caddyhttp.HandlerFunc(origin.ServeHTTP)))
	elapsed := time.Since(followerStart)

	resp := rec.Result()
	body := mustReadBody(t, resp)

	assert.Equal(t, http.StatusPartialContent, resp.StatusCode,
		"a timed-out follower must still get a correct range")
	assert.Equal(t, origin.body[200:300], body)
	assert.Equal(t, "BYPASS", resp.Header.Get(CacheHeaderName),
		"the fallback path serves straight from the origin")
	assert.Less(t, elapsed, 2*time.Second, "follower must not wait indefinitely")

	<-leaderDone
}

// TestCollapse_FollowerHonorsClientDisconnect ensures a follower does not sit on a
// dead connection for the whole collapse window.
func TestCollapse_FollowerHonorsClientDisconnect(t *testing.T) {
	s, _ := rangeTestSidekick(t)
	s.RangeFillCollapseWait = caddy.Duration(30 * time.Second)

	release := make(chan struct{})
	defer close(release)
	origin := &slowOrigin{body: mediaBody(16 * 1024), release: release}
	const path = "/wp-content/uploads/hang.mp4"

	go func() {
		req := httptest.NewRequest("GET", path, nil)
		req.Header.Set("Range", "bytes=0-99")
		_ = s.ServeHTTP(httptest.NewRecorder(), req, caddyhttp.HandlerFunc(origin.ServeHTTP))
	}()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", path, nil).WithContext(ctx)
	req.Header.Set("Range", "bytes=200-299")

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := s.ServeHTTP(httptest.NewRecorder(), req, caddyhttp.HandlerFunc(origin.ServeHTTP))
	elapsed := time.Since(start)

	assert.Error(t, err, "a disconnected follower should surface the context error")
	assert.Less(t, elapsed, 5*time.Second, "must abandon the wait on disconnect")
}

// TestCollapse_NonRangeMissesAreNotCollapsed pins the deliberate scoping decision:
// ordinary misses are cheap, so they keep their existing concurrent behavior rather
// than serializing behind a leader.
func TestCollapse_NonRangeMissesAreNotCollapsed(t *testing.T) {
	s, _ := rangeTestSidekick(t)

	release := make(chan struct{})
	origin := &slowOrigin{body: mediaBody(8 * 1024), release: release}
	const path = "/wp-content/uploads/plain.mp4"

	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest("GET", path, nil) // no Range
			_ = s.ServeHTTP(httptest.NewRecorder(), req, caddyhttp.HandlerFunc(origin.ServeHTTP))
		}()
	}

	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	assert.Greater(t, origin.calls.Load(), int32(1),
		"non-range misses are intentionally left uncollapsed")
}

// TestReleaseFill_IsIdempotentAndScoped covers the in-flight record lifetime: release
// must wake followers exactly once and must not evict a newer generation's record.
func TestReleaseFill_IsIdempotentAndScoped(t *testing.T) {
	h := &SyncHandler{inFlight: make(map[string]*inflightFill)}

	first, isLeader := h.acquireFill("k")
	require.True(t, isLeader)

	second, isLeader := h.acquireFill("k")
	require.False(t, isLeader, "a second caller joins rather than leading")
	require.Same(t, first, second)

	h.releaseFill("k", first)
	select {
	case <-first.done:
	default:
		t.Fatal("release must wake followers")
	}

	// A second release of the same record must not panic on a double close.
	h.releaseFill("k", first)

	// A new generation is now independent, and a late release of the old record
	// must not evict it.
	third, isLeader := h.acquireFill("k")
	require.True(t, isLeader, "after release the next caller leads again")
	require.NotSame(t, first, third)

	h.releaseFill("k", first)
	h.inFlightMu.Lock()
	cur, stillThere := h.inFlight["k"]
	h.inFlightMu.Unlock()

	assert.True(t, stillThere, "a stale release must not evict the current record")
	assert.Same(t, third, cur)
}
