// Tests for the transcript assembly cache (agent_session_transcript_cache.go):
// the owner-inspection performance rework's contract set — a replay walk
// reads the session once (hit = probe + slice, zero full reads), the
// append-aware fingerprint invalidates on both append and retention delete,
// the LRU stays bounded at the shipped capacity, same-key concurrency joins
// one compute (single flight), a failed compute is never cached, and the
// transcript/system endpoints' behavior is byte-identical on the cached and
// recomputed paths (the lazyload cases TC-01..08/20/21 are behavior faces
// the cache must not move).
package requestlog

import (
	"container/list"
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// ---------------------------------------------------------------- fixtures

// seedTranscriptSession seeds one tool session of n requests whose bodies
// follow the resent-assistant ladder the paging fixture uses: request i's
// body carries [u1,a1,...,u_i], so blocks are distinct and a missing reply
// still attributes via the next request's resend. Returns the session id.
func seedTranscriptSession(t *testing.T, db *gorm.DB, sess string, n int) {
	t.Helper()
	base := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	claude := "claude-code"
	messages := func(upto int) string {
		parts := make([]string, 0, 2*upto)
		for j := 1; j <= upto; j++ {
			parts = append(parts,
				fmt.Sprintf(`{"role":"user","content":"q%d"}`, j),
				fmt.Sprintf(`{"role":"assistant","content":"r%d"}`, j))
		}
		return strings.Join(parts[:len(parts)-1], ",")
	}
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("req-%02d", i)
		sys := "S-one"
		if i >= 13 {
			sys = "S-two"
		}
		testutil.SeedRequestLog(t, db, id, base.Add(time.Duration(i)*time.Minute), func(r *model.RequestLog) {
			r.AgentClient = &claude
			r.AgentSessionID = &sess
			r.StatusCode = 200
		})
		if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
			RequestID:   id,
			RequestBody: fmt.Sprintf(`{"system":%q,"messages":[%s]}`, sys, messages(i)),
			ResponseBody: fmt.Sprintf(`{"content":[{"type":"text","text":"reply-%d"}]}`, i),
		}); err != nil {
			t.Fatalf("upsert body %s: %v", id, err)
		}
	}
}

// sqlCountingLogger records every SQL statement a traced session runs, so a
// test can assert which reads a request actually paid for (the probe's two
// scalars vs the assembly's full-row loads). Delegates nothing — test
// databases log nothing anyway.
type sqlCountingLogger struct {
	mu    sync.Mutex
	stmts []string
}

func (l *sqlCountingLogger) LogMode(logger.LogLevel) logger.Interface { return l }
func (l *sqlCountingLogger) Info(context.Context, string, ...interface{})  {}
func (l *sqlCountingLogger) Warn(context.Context, string, ...interface{})  {}
func (l *sqlCountingLogger) Error(context.Context, string, ...interface{}) {}
func (l *sqlCountingLogger) Trace(_ context.Context, _ time.Time, fc func() (string, int64), _ error) {
	sql, _ := fc()
	l.mu.Lock()
	l.stmts = append(l.stmts, sql)
	l.mu.Unlock()
}

func (l *sqlCountingLogger) reset() {
	l.mu.Lock()
	l.stmts = nil
	l.mu.Unlock()
}

func (l *sqlCountingLogger) statements() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.stmts...)
}

// fullRowReads counts the wide loads only the assembly path issues: SELECT *
// over request_logs or request_log_bodies. The fingerprint probe reads two
// scalars and never matches this predicate.
func (l *sqlCountingLogger) fullRowReads() int {
	n := 0
	for _, s := range l.statements() {
		if strings.Contains(s, "SELECT *") && (strings.Contains(s, "request_logs") || strings.Contains(s, "request_log_bodies")) {
			n++
		}
	}
	return n
}

// ---------------------------------------------------------------- AC: hit

// TestAssemblyCacheReplayWalkAssemblesOnce pins the cache's core promise at
// the closure level: a multi-page walk over one session probes on every page
// but assembles exactly once, and every page is served the SAME snapshot.
func TestAssemblyCacheReplayWalkAssemblesOnce(t *testing.T) {
	var probes, assembles int
	fp := repository.AgentSessionAssemblyFingerprint{RequestCount: 72, LastRequestID: "req-72"}
	replay := &assembledAgentSessionReplay{}
	c := newAgentSessionAssemblyCache(
		func(string) (repository.AgentSessionAssemblyFingerprint, error) {
			probes++
			return fp, nil
		},
		func(string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			assembles++
			return replay, fp, nil
		},
	)
	const pages = 5 // no-cursor first screen + four upscroll continuations
	for i := 0; i < pages; i++ {
		got, err := c.get("sess-walk")
		if err != nil {
			t.Fatalf("page %d: %v", i, err)
		}
		if got != replay {
			t.Fatalf("page %d served a different snapshot — every page must share the one assembly", i)
		}
	}
	if assembles != 1 {
		t.Fatalf("assembles = %d over %d pages, want exactly 1 — the walk must pay the full read once", assembles, pages)
	}
	if probes != pages {
		t.Fatalf("probes = %d over %d pages, want %d — every get probes (that is the invalidation cost)", probes, pages, pages)
	}
}

// TestGetAgentSessionTranscriptSecondPageZeroFullReads pins the hit path at
// the service level over a real database: after the first page assembles the
// session, the second page (a different cursor window of the same walk)
// issues ONLY the fingerprint probe — no full-row request_logs load, no
// request_log_bodies batch, nothing wide. Slice + serialize is all that is
// left, which is what makes the upscroll answer in milliseconds.
func TestGetAgentSessionTranscriptSecondPageZeroFullReads(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	cl := &sqlCountingLogger{}
	svc := NewRequestLogService(db.Session(&gorm.Session{Logger: cl}), "")
	const sess = "sess-hit"
	seedTranscriptSession(t, db, sess, 25)

	first, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if got := cl.fullRowReads(); got < 2 {
		t.Fatalf("first page full-row reads = %d, want at least 2 (rows + bodies) — the miss pays the full load", got)
	}

	cl.reset()
	second, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 5, Before: first.NextBefore})
	if err != nil {
		t.Fatalf("second page: %v", err)
	}
	stmts := cl.statements()
	if len(stmts) != 2 {
		t.Fatalf("second page ran %d statements %q, want exactly the 2 probe scalars (count + last request id)", len(stmts), stmts)
	}
	for _, s := range stmts {
		if !strings.Contains(s, "agent_session_id") {
			t.Fatalf("second page statement %q is not a session probe", s)
		}
	}
	if n := cl.fullRowReads(); n != 0 {
		t.Fatalf("second page full-row reads = %d, want 0 — the hit path must not re-read rows or bodies", n)
	}
	if len(second.Blocks) != 5 || second.Blocks[0].RequestID != "req-01" || second.Blocks[4].RequestID != "req-05" {
		ids := make([]string, 0, len(second.Blocks))
		for i := range second.Blocks {
			ids = append(ids, second.Blocks[i].RequestID)
		}
		t.Fatalf("second page = %v, want the oldest 5 blocks req-01..req-05", ids)
	}
	// The 20 newest blocks went to page one, so this 5-block window IS the
	// head of the session: nothing older, everything newer.
	if second.HasMoreBefore || second.NextBefore != "" {
		t.Fatalf("second page before-side = (%v, %q), want (false, \"\") — 5 blocks older than req-06 is the whole head", second.HasMoreBefore, second.NextBefore)
	}
	if !second.HasMoreAfter || second.NextAfter != "req-05" {
		t.Fatalf("second page after-side = (%v, %q), want (true, req-05)", second.HasMoreAfter, second.NextAfter)
	}
}

// TestGetAgentSessionTranscriptSystemSharesCache pins that the system
// endpoint rides the SAME cache: after a transcript page warmed the session,
// expanding the system group pays only the probe and returns the timeline
// the transcript's metadata already described.
func TestGetAgentSessionTranscriptSystemSharesCache(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	cl := &sqlCountingLogger{}
	svc := NewRequestLogService(db.Session(&gorm.Session{Logger: cl}), "")
	const sess = "sess-share"
	seedTranscriptSession(t, db, sess, 25)

	if _, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{Limit: 10}); err != nil {
		t.Fatalf("transcript page: %v", err)
	}
	cl.reset()
	sys, err := svc.GetAgentSessionTranscriptSystem(sess)
	if err != nil {
		t.Fatalf("transcript system: %v", err)
	}
	if n := cl.fullRowReads(); n != 0 {
		t.Fatalf("system endpoint after a warm transcript full-row reads = %d, want 0 — both endpoints share one cache", n)
	}
	if len(sys.System) != 2 || sys.System[0].Raw != "S-one" || sys.System[1].Raw != "S-two" {
		t.Fatalf("system timeline = %+v, want the two prompts (S-one at req-01, S-two at req-13)", sys.System)
	}
}

// -------------------------------------------------------- AC: invalidation

// TestTranscriptCacheInvalidatesOnAppendAndRetention pins the fingerprint's
// two invalidation faces over a real database: appending a request changes
// the key and the recomputed page carries the new block; the retention
// cleanup's shape (oldest rows deleted, latest request untouched) changes
// the count half alone and still forces the recompute.
func TestTranscriptCacheInvalidatesOnAppendAndRetention(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	cl := &sqlCountingLogger{}
	svc := NewRequestLogService(db.Session(&gorm.Session{Logger: cl}), "")
	sess := "sess-inv"
	seedTranscriptSession(t, db, sess, 3) // req-01..req-03

	tr1, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("first page: %v", err)
	}
	if tr1.Session.RequestCount != 3 || tr1.Blocks[len(tr1.Blocks)-1].RequestID != "req-03" {
		t.Fatalf("first page aggregate = %+v, want 3 requests ending at req-03", tr1.Session)
	}
	readsAfterFirst := cl.fullRowReads()

	// Append: a live session gains req-04.
	base := time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)
	claude := "claude-code"
	testutil.SeedRequestLog(t, db, "req-04", base.Add(4*time.Minute), func(r *model.RequestLog) {
		r.AgentClient = &claude
		r.AgentSessionID = &sess
		r.StatusCode = 200
	})
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:    "req-04",
		RequestBody:  `{"messages":[{"role":"user","content":"new"}` + strings.Repeat(`,{"role":"assistant","content":"r"},{"role":"user","content":"q"}`, 3) + `]}`,
		ResponseBody: `{"content":[{"type":"text","text":"reply-4"}]}`,
	}); err != nil {
		t.Fatalf("upsert body req-04: %v", err)
	}

	tr2, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("page after append: %v", err)
	}
	if tr2.Session.RequestCount != 4 {
		t.Fatalf("aggregate after append = %d requests, want 4 — the append must invalidate the cached snapshot", tr2.Session.RequestCount)
	}
	if last := tr2.Blocks[len(tr2.Blocks)-1]; last.RequestID != "req-04" {
		t.Fatalf("newest block after append = %s, want req-04 — the recomputed page carries the new block", last.RequestID)
	}
	readsAfterAppend := cl.fullRowReads()
	if readsAfterAppend == readsAfterFirst {
		t.Fatalf("append did not trigger a recompute (full-row reads %d → %d)", readsAfterFirst, readsAfterAppend)
	}

	// Retention: the oldest row is reaped while the latest request id stays
	// put — only the count half moves, and that alone must miss the cache.
	if err := db.Where("request_id = ?", "req-01").Delete(&model.RequestLog{}).Error; err != nil {
		t.Fatalf("delete req-01: %v", err)
	}
	tr3, err := svc.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("page after retention: %v", err)
	}
	if tr3.Session.RequestCount != 3 {
		t.Fatalf("aggregate after retention = %d requests, want 3 — the deletion must invalidate the cached snapshot", tr3.Session.RequestCount)
	}
	for _, b := range tr3.Blocks {
		if b.RequestID == "req-01" {
			t.Fatalf("retained block req-01 survived the recompute — a stale cache was served")
		}
	}
	if reads := cl.fullRowReads(); reads == readsAfterAppend {
		t.Fatalf("retention did not trigger a recompute (full-row reads %d → %d)", readsAfterAppend, reads)
	}
}

// TestAssemblyCacheDifferentSessionsComputeIndependently pins the key's
// session-id half at the closure level: two sessions never share a snapshot
// even when their fingerprints coincide.
func TestAssemblyCacheDifferentSessionsComputeIndependently(t *testing.T) {
	fp := repository.AgentSessionAssemblyFingerprint{RequestCount: 5, LastRequestID: "r5"}
	perSession := map[string]*assembledAgentSessionReplay{
		"a": {summary: AgentSessionListItem{AgentSessionID: "a"}},
		"b": {summary: AgentSessionListItem{AgentSessionID: "b"}},
	}
	var assembles int
	c := newAgentSessionAssemblyCache(
		func(string) (repository.AgentSessionAssemblyFingerprint, error) { return fp, nil },
		func(id string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			assembles++
			return perSession[id], fp, nil
		},
	)
	ga, err := c.get("a")
	if err != nil {
		t.Fatalf("get a: %v", err)
	}
	gb, err := c.get("b")
	if err != nil {
		t.Fatalf("get b: %v", err)
	}
	if ga != perSession["a"] || gb != perSession["b"] {
		t.Fatalf("sessions shared snapshots: got (%p, %p), want each session's own", ga, gb)
	}
	if assembles != 2 {
		t.Fatalf("assembles = %d, want 2 — each session computes once", assembles)
	}
}

// --------------------------------------------------------------- AC: LRU

// TestAssemblyCacheEvictsLeastRecentlyUsed pins the LRU mechanics on a
// shrunken cache (capacity 2): a hit refreshes recency, an insert past
// capacity evicts the least recently used session, and an evicted session's
// next get reassembles.
func TestAssemblyCacheEvictsLeastRecentlyUsed(t *testing.T) {
	var assembles int
	snapshot := func(id string) *assembledAgentSessionReplay {
		return &assembledAgentSessionReplay{summary: AgentSessionListItem{AgentSessionID: id}}
	}
	fp := func(id string) repository.AgentSessionAssemblyFingerprint {
		return repository.AgentSessionAssemblyFingerprint{RequestCount: 1, LastRequestID: id}
	}
	c := agentSessionAssemblyCache{
		ll:       list.New(),
		items:    map[string]*list.Element{},
		capacity: 2,
		probe:    func(id string) (repository.AgentSessionAssemblyFingerprint, error) { return fp(id), nil },
		assemble: func(id string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			assembles++
			return snapshot(id), fp(id), nil
		},
	}
	mustGet := func(id string) *assembledAgentSessionReplay {
		t.Helper()
		got, err := c.get(id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.summary.AgentSessionID != id {
			t.Fatalf("get %s served session %q", id, got.summary.AgentSessionID)
		}
		return got
	}

	mustGet("a") // [a]
	mustGet("b") // [b a]
	mustGet("a") // hit, refresh → [a b]
	mustGet("c") // insert, evict b → [c a]
	if assembles != 3 {
		t.Fatalf("assembles = %d after 3 distinct sessions, want 3 (the a-hit must not reassemble)", assembles)
	}
	mustGet("a") // still cached
	if assembles != 3 {
		t.Fatalf("a reassembled after being touched — LRU evicted the wrong end (assembles=%d)", assembles)
	}
	mustGet("b") // evicted earlier → reassemble → [b c]
	if assembles != 4 {
		t.Fatalf("b reassembly count = %d, want 4", assembles)
	}
	if len(c.items) != 2 || c.ll.Len() != 2 {
		t.Fatalf("cache index = (%d map, %d list), want the capacity bound 2 held", len(c.items), c.ll.Len())
	}
}

// TestAssemblyCacheShippedCapacityAndBound pins the shipped bound itself: 8
// sessions, the 9th evicts the least recently used, and the sessions the
// walk still touches keep hitting without reassembling.
func TestAssemblyCacheShippedCapacityAndBound(t *testing.T) {
	if transcriptAssemblyCacheCapacity != 8 {
		t.Fatalf("shipped capacity = %d, want 8 (fixed by the ticket — no settings surface)", transcriptAssemblyCacheCapacity)
	}
	var assembles int
	snapshot := func(id string) *assembledAgentSessionReplay {
		return &assembledAgentSessionReplay{summary: AgentSessionListItem{AgentSessionID: id}}
	}
	fp := func(id string) repository.AgentSessionAssemblyFingerprint {
		return repository.AgentSessionAssemblyFingerprint{RequestCount: 1, LastRequestID: id}
	}
	c := newAgentSessionAssemblyCache(
		func(id string) (repository.AgentSessionAssemblyFingerprint, error) { return fp(id), nil },
		func(id string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			assembles++
			return snapshot(id), fp(id), nil
		},
	)
	mustGet := func(id string) {
		t.Helper()
		got, err := c.get(id)
		if err != nil || got.summary.AgentSessionID != id {
			t.Fatalf("get %s = (%v, %v)", id, got, err)
		}
	}
	for i := 1; i <= 8; i++ {
		mustGet(fmt.Sprintf("s%d", i)) // MRU→LRU: s8..s1
	}
	if assembles != 8 {
		t.Fatalf("assembles = %d after 8 sessions, want 8", assembles)
	}
	mustGet("s1") // touch the oldest — now s2 is the LRU
	mustGet("s9") // 9th session: evicts s2, not s1
	if assembles != 9 {
		t.Fatalf("assembles = %d after the 9th session, want 9", assembles)
	}
	mustGet("s8") // s8 survived — still inside the bound
	mustGet("s1") // s1 survived the touch
	if assembles != 9 {
		t.Fatalf("assembles = %d after hitting s8/s1, want 9 — the eviction took exactly s2", assembles)
	}
	if len(c.items) != 8 || c.ll.Len() != 8 {
		t.Fatalf("cache index = (%d map, %d list), want the shipped bound 8 held", len(c.items), c.ll.Len())
	}
}

// -------------------------------------------------------- AC: concurrency

// TestAssemblyCacheSingleFlightPerKey pins the mutual exclusion the ticket
// demands: 16 concurrent gets of one session produce exactly ONE assembly
// (the rest join the in-flight compute), the assembly never runs twice in
// parallel, and every caller receives the same snapshot.
func TestAssemblyCacheSingleFlightPerKey(t *testing.T) {
	fp := repository.AgentSessionAssemblyFingerprint{RequestCount: 72, LastRequestID: "req-72"}
	replay := &assembledAgentSessionReplay{}
	var assembles, inFlight, maxInFlight int64
	c := newAgentSessionAssemblyCache(
		func(string) (repository.AgentSessionAssemblyFingerprint, error) { return fp, nil },
		func(string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			atomic.AddInt64(&assembles, 1)
			cur := atomic.AddInt64(&inFlight, 1)
			for {
				m := atomic.LoadInt64(&maxInFlight)
				if cur <= m || atomic.CompareAndSwapInt64(&maxInFlight, m, cur) {
					break
				}
			}
			time.Sleep(25 * time.Millisecond) // hold the door open for joiners
			atomic.AddInt64(&inFlight, -1)
			return replay, fp, nil
		},
	)
	const callers = 16
	start := make(chan struct{})
	var wg sync.WaitGroup
	gots := make([]*assembledAgentSessionReplay, callers)
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			gots[i], errs[i] = c.get("sess-storm")
		}(i)
	}
	close(start)
	wg.Wait()

	if n := atomic.LoadInt64(&assembles); n != 1 {
		t.Fatalf("assembles = %d over %d concurrent same-key callers, want 1 — the join path must collapse the storm", n, callers)
	}
	if m := atomic.LoadInt64(&maxInFlight); m != 1 {
		t.Fatalf("max concurrent assemblies = %d, want 1 — same-key computes must never overlap", m)
	}
	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("caller %d err = %v", i, errs[i])
		}
		if gots[i] != replay {
			t.Fatalf("caller %d received a different snapshot — joiners must get the compute they waited for", i)
		}
	}
}

// TestAssemblyCacheFailedComputeIsNotCached pins the error path: a failed
// assembly is returned to its caller (and to the callers that joined it) but
// never cached — the next caller recomputes and succeeds.
func TestAssemblyCacheFailedComputeIsNotCached(t *testing.T) {
	fp := repository.AgentSessionAssemblyFingerprint{RequestCount: 3, LastRequestID: "r3"}
	replay := &assembledAgentSessionReplay{}
	boom := errors.New("boom")
	var calls int
	fail := atomic.Bool{}
	fail.Store(true)
	c := newAgentSessionAssemblyCache(
		func(string) (repository.AgentSessionAssemblyFingerprint, error) { return fp, nil },
		func(string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			calls++
			if fail.Swap(false) {
				return nil, fp, boom
			}
			return replay, fp, nil
		},
	)
	if _, err := c.get("sess-err"); !errors.Is(err, boom) {
		t.Fatalf("first get err = %v, want the assembly failure", err)
	}
	if _, err := c.get("sess-err"); err != nil {
		t.Fatalf("second get err = %v, want the retry to succeed — failures are never cached", err)
	}
	if calls != 2 {
		t.Fatalf("assembles = %d, want 2 (failure + retry)", calls)
	}
	if got, err := c.get("sess-err"); err != nil || got != replay {
		t.Fatalf("third get = (%v, %v), want the cached success", got, err)
	}
	if calls != 2 {
		t.Fatalf("assembles = %d after a cached success, want 2", calls)
	}
}

// TestAssemblyCacheProbeErrorPropagates pins that a probe failure fails the
// request without attempting an assembly (the read path's own convention:
// fail loud, serve nothing stale).
func TestAssemblyCacheProbeErrorPropagates(t *testing.T) {
	probeErr := errors.New("probe down")
	var assembles int
	c := newAgentSessionAssemblyCache(
		func(string) (repository.AgentSessionAssemblyFingerprint, error) { return repository.AgentSessionAssemblyFingerprint{}, probeErr },
		func(string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error) {
			assembles++
			return &assembledAgentSessionReplay{}, repository.AgentSessionAssemblyFingerprint{}, nil
		},
	)
	if _, err := c.get("sess"); !errors.Is(err, probeErr) {
		t.Fatalf("get err = %v, want the probe failure", err)
	}
	if assembles != 0 {
		t.Fatalf("assembles = %d after a probe failure, want 0", assembles)
	}
}

// ------------------------------------------------- AC: contract unchanged

// TestTranscriptCacheContractIdenticalAcrossPaths re-runs the lazyload
// behavior faces (TC-01..08/21 shapes the service owns) through BOTH paths —
// first call (recompute) and every later call (cache hit), plus a
// brand-new service over the same data (independent recompute) — and
// requires byte-identical answers everywhere: cursor windows, boundary
// limits, lenient bad cursors, the anchor-only advance exception, byte-
// budget slices, the system timeline, and the not-found error. The cache may
// change what a page costs, never what it means.
func TestTranscriptCacheContractIdenticalAcrossPaths(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	warm := NewRequestLogService(db, "")
	const sess = "sess-contract"
	seedTranscriptSession(t, db, sess, 25)

	shapes := []AgentSessionTranscriptPage{
		{},                                   // TC-01: latest page, default limit
		{Limit: 7},                           // latest window under the default
		{Limit: 0},                           // boundary: default fallback
		{Limit: -3},                          // boundary: default fallback
		{Limit: 999999},                      // boundary: over-max fallback
		{Limit: 10, Before: "req-16"},        // TC-02: exclusive before page
		{Limit: 10, Before: "req-01"},        // known cursor, empty window
		{Before: "req-nope"},                 // TC-04 lenient: unknown cursor
		{After: "req-nope"},                  // TC-04 lenient: unknown cursor
		{Limit: 4, After: "req-05"},          // TC-03: inclusive anchor page
		{After: "req-25"},                    // anchor at the newest block
		{ByteBudget: 4096},                   // TC-05: budget-truncated latest page
		{ByteBudget: 4096, Before: "req-20"}, // budget-truncated before page
		{ByteBudget: 1, After: "req-07"},     // TC-06 shape: anchor-only page, advance exception
	}
	fresh := NewRequestLogService(db, "")
	for i, shape := range shapes {
		first, err := warm.GetAgentSessionTranscript(sess, shape)
		if err != nil {
			t.Fatalf("shape %d %+v (recompute): %v", i, shape, err)
		}
		cached, err := warm.GetAgentSessionTranscript(sess, shape)
		if err != nil {
			t.Fatalf("shape %d %+v (cache hit): %v", i, shape, err)
		}
		if !reflect.DeepEqual(first, cached) {
			t.Fatalf("shape %d %+v: cached page ≠ recomputed page\ncached:   %+v\nrecomputed: %+v", i, shape, cached, first)
		}
		independent, err := fresh.GetAgentSessionTranscript(sess, shape)
		if err != nil {
			t.Fatalf("shape %d %+v (fresh service): %v", i, shape, err)
		}
		if !reflect.DeepEqual(first, independent) {
			t.Fatalf("shape %d %+v: warm service answer ≠ fresh service answer", i, shape)
		}
	}

	// The advance exception specifically (shape's next_after names the block
	// beyond the anchor-only page — TC-21's never-stall continuation).
	anchorOnly, err := warm.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{ByteBudget: 1, After: "req-07"})
	if err != nil {
		t.Fatalf("anchor-only page: %v", err)
	}
	if len(anchorOnly.Blocks) != 1 || anchorOnly.Blocks[0].RequestID != "req-07" {
		t.Fatalf("anchor-only page = %d blocks starting at %+v, want exactly the anchor req-07", len(anchorOnly.Blocks), anchorOnly.Blocks)
	}
	if !anchorOnly.HasMoreAfter || anchorOnly.NextAfter != "req-08" {
		t.Fatalf("anchor-only continuation = (%v, %q), want (true, req-08) — re-anchoring on the anchor would stall forever", anchorOnly.HasMoreAfter, anchorOnly.NextAfter)
	}

	// The system timeline: raw endpoint twice + alignment with the metadata
	// (TC-08's two-endpoint contract, held identically on the hit path).
	sys1, err := warm.GetAgentSessionTranscriptSystem(sess)
	if err != nil {
		t.Fatalf("system endpoint (first): %v", err)
	}
	sys2, err := warm.GetAgentSessionTranscriptSystem(sess)
	if err != nil {
		t.Fatalf("system endpoint (cached): %v", err)
	}
	if !reflect.DeepEqual(sys1, sys2) {
		t.Fatalf("cached system timeline ≠ recomputed system timeline")
	}
	meta, err := warm.GetAgentSessionTranscript(sess, AgentSessionTranscriptPage{})
	if err != nil {
		t.Fatalf("transcript for metadata: %v", err)
	}
	if len(meta.System) != len(sys1.System) || len(meta.System) != 2 {
		t.Fatalf("timeline lengths = meta %d / raw %d, want 2/2 index-aligned", len(meta.System), len(sys1.System))
	}
	for i := range sys1.System {
		if meta.System[i].SeenAtRequest != sys1.System[i].SeenAtRequest {
			t.Fatalf("system entry %d seen_at_request = meta %q / raw %q", i, meta.System[i].SeenAtRequest, sys1.System[i].SeenAtRequest)
		}
	}

	// TC-20's service leg: unknown session keeps answering the domain
	// not-found on every call (never cached into an error or an empty hit).
	for i := 0; i < 2; i++ {
		if _, err := warm.GetAgentSessionTranscript("nope", AgentSessionTranscriptPage{}); !errors.Is(err, errcode.ErrAgentSessionNotFound) {
			t.Fatalf("unknown session transcript call %d err = %v, want ErrAgentSessionNotFound", i, err)
		}
		if _, err := warm.GetAgentSessionTranscriptSystem("nope"); !errors.Is(err, errcode.ErrAgentSessionNotFound) {
			t.Fatalf("unknown session system call %d err = %v, want ErrAgentSessionNotFound", i, err)
		}
	}
}
