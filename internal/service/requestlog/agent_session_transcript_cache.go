// Bounded LRU cache for the transcript assembly — the owner-inspection
// performance rework. Assembling one session's replay re-reads the whole
// session (full request rows, the 1:1 body rows' large TEXT columns, every
// stream capture file) and re-parses tens of MiB of JSON; the cursor-paginated
// endpoints then hand back one ~1.5 MiB slice of it, so a replay walk paid
// that full price per page. This file keeps the assembly itself in a small
// keyed cache: the first page of a session pays the full read, every later
// page pays one microscopic fingerprint probe + slice + serialize.
//
// Correctness rests on sessions being append-only (plus retention deletes):
// the cache key is (session id, row count, last request id), so any change to
// the row set the assembly was derived from changes the key and the probe
// misses. The key is derived from the rows the assembly ACTUALLY read — not
// from the probe that triggered it — so an assembly that raced an append is
// filed under a fingerprint that describes exactly what it contains, and a
// later probe only hits when the session still looks like that snapshot.
//
// The cache lives on RequestLogService and is shared by the transcript and
// /transcript/system endpoints (both assemble the same session the same way);
// probe/assemble arrive as function fields so the unit tests can count reads
// and inject slow assemblies without a database.
package requestlog

import (
	"container/list"
	"sync"

	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/pkg/errcode"
)

// transcriptAssemblyCacheCapacity is the fixed number of sessions the
// assembly cache holds. Eight covers an admin walking one replay while a few
// other long sessions stay warm (the transcripts of a heavy day can be tens
// of MiB each, so the bound is a memory ceiling as much as a freshness one).
// Deliberately not configurable: one shipped value, no settings surface.
const transcriptAssemblyCacheCapacity = 8

// assembledAgentSessionReplay is the finished output of the load-and-assemble
// seam: the session aggregate, the full block sequence, and the raw system
// timeline. One per session snapshot; both read endpoints slice or project it
// without mutating (treat every field as immutable once cached).
type assembledAgentSessionReplay struct {
	summary AgentSessionListItem
	blocks  []AgentSessionReplayBlock
	system  []AgentSessionReplaySystem
}

// agentSessionAssemblyEntry is one cached session snapshot plus the
// handoff state that makes same-key callers WAIT for the in-flight compute
// instead of stacking a second one (one recompute per key, ever). done is
// closed exactly once, after replay/err/fingerprint are set; readers must
// receive on it before touching any other field. sessionID rides along only
// so eviction can unhook the map half of a back-of-list element it meets
// without its key.
type agentSessionAssemblyEntry struct {
	sessionID   string
	fingerprint repository.AgentSessionAssemblyFingerprint
	replay      *assembledAgentSessionReplay
	err         error
	done        chan struct{}
}

// agentSessionAssemblyCache is the bounded LRU itself. entries maps session
// id → list element; the list is ordered most-recently-used first and evicts
// from the back when an insert exceeds capacity. mu guards exactly the map +
// list bookkeeping — probes and assemblies run OUTSIDE it, so a slow compute
// never blocks another session's cache hit.
type agentSessionAssemblyCache struct {
	mu    sync.Mutex
	ll    *list.List               // values: *agentSessionAssemblyEntry, front = MRU
	items map[string]*list.Element // session id → element

	// probe returns the session's cheap change-detection fingerprint. It runs
	// on EVERY get, cached or not — that is the whole invalidation mechanism.
	probe func(sessionID string) (repository.AgentSessionAssemblyFingerprint, error)
	// assemble performs the full load (rows + bodies + stream captures) and
	// returns the finished replay under the fingerprint of the rows it
	// actually read. Runs at most once per cached snapshot.
	assemble func(sessionID string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error)
	// capacity is the LRU bound; the shipped value is
	// transcriptAssemblyCacheCapacity. A field, not the constant, so unit
	// tests can shrink it — production always constructs via
	// newAgentSessionAssemblyCache, which pins the shipped bound.
	capacity int
}

// newAgentSessionAssemblyCache builds the cache around the probe/assemble
// pair at the shipped capacity.
func newAgentSessionAssemblyCache(
	probe func(sessionID string) (repository.AgentSessionAssemblyFingerprint, error),
	assemble func(sessionID string) (*assembledAgentSessionReplay, repository.AgentSessionAssemblyFingerprint, error),
) agentSessionAssemblyCache {
	return agentSessionAssemblyCache{
		ll:       list.New(),
		items:    map[string]*list.Element{},
		probe:    probe,
		assemble: assemble,
		capacity: transcriptAssemblyCacheCapacity,
	}
}

// get returns the session's assembled replay from the cache when the probe
// still matches, computing and caching it otherwise. Every caller — the
// transcript endpoint, the system endpoint, page after page of one replay
// walk — lands here, so the cache is the single assembly seam.
//
// Concurrency contract: callers that arrive while a snapshot for the session
// is still computing JOIN that compute (they block on done) rather than
// starting a second one — the per-key single-flight that keeps an N-viewer
// replay walk from becoming an N-way recompute storm. A caller whose probe
// fingerprint no longer matches a COMPLETED entry replaces it with a fresh
// compute; the displaced snapshot's joined readers keep their (self-
// consistent) view. A failed compute is never cached: the entry is dropped so
// the next caller retries.
func (c *agentSessionAssemblyCache) get(sessionID string) (*assembledAgentSessionReplay, error) {
	fp, err := c.probe(sessionID)
	if err != nil {
		return nil, err
	}
	if fp.RequestCount == 0 {
		// Same mapping the uncached seam applied to an empty row set — the
		// probe just finds out cheaper. Never cached: the miss costs two
		// scalar queries.
		return nil, errcode.ErrAgentSessionNotFound
	}

	// Phase 1 (locked): decide which entry this caller belongs to. Four
	// outcomes — completed hit, in-flight join, stale replace, fresh insert.
	c.mu.Lock()
	if el, ok := c.items[sessionID]; ok {
		entry := el.Value.(*agentSessionAssemblyEntry)
		select {
		case <-entry.done:
			if entry.err == nil && entry.fingerprint == fp {
				c.ll.MoveToFront(el) // a hit is a use; the walk stays warm
				c.mu.Unlock()
				return entry.replay, nil
			}
			// Stale (the session gained/lost rows) or a failed compute kept
			// until now — either way this snapshot must not serve again.
			c.lockedRemove(sessionID, el)
		default:
			// In-flight: join the compute already running for this session.
			// The running read started before this caller's return, so its
			// snapshot is a legal answer for this read even if the probe saw
			// a row or two more — reads concurrent with an append may land on
			// either side of it.
			c.mu.Unlock()
			<-entry.done
			return entry.replay, entry.err
		}
	}
	entry := &agentSessionAssemblyEntry{sessionID: sessionID, done: make(chan struct{})}
	el := c.ll.PushFront(entry)
	c.items[sessionID] = el
	for c.ll.Len() > c.capacity {
		c.lockedEvictOldest()
	}
	c.mu.Unlock()

	// Phase 2 (unlocked): this caller won the race to compute. The heavy
	// read runs with mu released — other sessions' hits never wait on it.
	replay, assembledFP, aerr := c.assemble(sessionID)
	entry.replay, entry.err = replay, aerr
	entry.fingerprint = assembledFP
	close(entry.done)

	if aerr != nil {
		// Never cache a failure. The entry may already have been replaced or
		// evicted mid-compute — only drop what is still ours.
		c.mu.Lock()
		if cur, ok := c.items[sessionID]; ok && cur == el {
			c.lockedRemove(sessionID, el)
		}
		c.mu.Unlock()
		return replay, aerr
	}

	// The compute stayed current enough to keep: mark it used. If capacity
	// pressure evicted this element mid-compute, MoveToFront is a no-op for
	// a detached element — the snapshot stays valid for the joined readers
	// and simply is not cached for the next caller.
	c.mu.Lock()
	c.ll.MoveToFront(el)
	c.mu.Unlock()
	return replay, nil
}

// lockedRemove unhooks an element from both halves of the index. Callers
// must hold c.mu and pass the element the map still points at.
func (c *agentSessionAssemblyCache) lockedRemove(sessionID string, el *list.Element) {
	delete(c.items, sessionID)
	c.ll.Remove(el)
}

// lockedEvictOldest drops the least-recently-used entry when an insert pushed
// the cache past capacity. An evicted entry that is still computing is simply
// orphaned: its creator fills it, its joined readers receive it, and the next
// caller of that session inserts a fresh one.
func (c *agentSessionAssemblyCache) lockedEvictOldest() {
	oldest := c.ll.Back()
	if oldest == nil {
		return
	}
	delete(c.items, oldest.Value.(*agentSessionAssemblyEntry).sessionID)
	c.ll.Remove(oldest)
}
