package playground

// Unit pin for the video bridge's exit choreography: the caller-left
// path (runCtx cancelled — the console caller's SSE disconnect) and an
// in-flight stop-wait POST both touch the same wait.stopped field, and
// the registry mutex is the one synchronization edge the two sides
// share. The overlap is staged deterministically through the mutex
// itself: with the test holding videoWaitMu, a stop-wait call parks on
// the lock while the disconnect fires, so the stop's write and the
// bridge's exit read are concurrent by construction rather than by
// timing luck. Under -race the unguarded read is a guaranteed report
// against that version (the read needs no lock there, so it runs under
// the test's hold and stays unordered with the parked stop's write);
// the guarded read is ordered by the mutex and stays clean. Without
// -race the test still pins the postconditions: the bridge always
// terminates (taking the registry lock in the exit path must not
// deadlock against StopVideoWait, which cancels while holding it) and
// the registry entry always drains.

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/gateway"
	"github.com/yolorouter/yolorouter/internal/model"
)

// pendingVideoTasks is the smallest VideoTasks port the bridge can poll:
// every Get answers a young task, so the bridge never reaches a terminal
// reading and the wait's exit is always the context's.
type pendingVideoTasks struct{}

func (pendingVideoTasks) Create(context.Context, *gateway.VideoTask, time.Time) error {
	return nil
}
func (pendingVideoTasks) PrecheckBudget(context.Context, uint, string, string, int) error {
	return nil
}
func (pendingVideoTasks) Get(context.Context, uint, string, time.Time) (*gateway.VideoTask, error) {
	return &gateway.VideoTask{Status: gateway.VideoTaskPending}, nil
}

// recordingVideoSink collects the bridge's sink calls under a lock. The
// overlap test must not assert on WHICH event the race produced (either
// exit reading is legitimate when the disconnect and the stop race), but
// the sink itself is shared by both orderings, so its append must be
// synchronized — an unsynchronized one would be a race of its own.
type recordingVideoSink struct {
	mu     sync.Mutex
	events []string
}

func (s *recordingVideoSink) VideoDone(*ColumnAudit, int) { s.record("done") }
func (s *recordingVideoSink) VideoTaskFailed(code, _ string) {
	s.record("failed:" + code)
}
func (s *recordingVideoSink) VideoWaitStopped() { s.record("wait_stopped") }

func (s *recordingVideoSink) record(ev string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

// TestBridgeVideoTaskCallerLeftRacesInFlightStopWait runs the overlap
// window in a loop: per iteration a fresh bridge waits on a task that
// never completes, a stop-wait call parks on the registry lock the test
// holds, and the caller's disconnect fires under that hold. Run under
// -race this is the regression test for the unsynchronized stopped read;
// the loop exists so the parked-stop-vs-bridge-exit contention gets
// exercised from both sides across iterations.
func TestBridgeVideoTaskCallerLeftRacesInFlightStopWait(t *testing.T) {
	const owner uint = 7
	for i := 0; i < 50; i++ {
		s := &Service{
			videoTasks: pendingVideoTasks{},
			videoWaits: map[string]*videoWait{},
		}
		runCtx, cancelRun := context.WithCancel(context.Background())
		defer cancelRun()
		bridgeDone := make(chan struct{})
		sink := &recordingVideoSink{}
		go func() {
			defer close(bridgeDone)
			s.BridgeVideoTask(runCtx, &model.APIKey{UserID: owner}, "req-race", "task-race", sink)
		}()

		// Wait for the bridge to register its wait — the registry entry
		// is the stop-wait endpoint's target.
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			s.videoWaitMu.Lock()
			registered := s.videoWaits["req-race"] != nil
			s.videoWaitMu.Unlock()
			if registered {
				break
			}
			time.Sleep(time.Millisecond)
		}

		// Stage the overlap: hold the registry lock, park a stop-wait
		// call on it, then fire the console caller's disconnect while
		// the stop is still parked — the bridge's exit read of stopped
		// now overlaps the parked stop's write by construction.
		s.videoWaitMu.Lock()
		if s.videoWaits["req-race"] == nil {
			s.videoWaitMu.Unlock()
			t.Fatal("the bridge never registered its wait")
		}
		stopDone := make(chan bool, 1)
		go func() { stopDone <- s.StopVideoWait("req-race", owner) }()
		time.Sleep(2 * time.Millisecond) // let the stop reach the Lock call
		cancelRun()
		s.videoWaitMu.Unlock()

		select {
		case <-bridgeDone:
		case <-time.After(5 * time.Second):
			t.Fatal("the bridge must terminate when the caller leaves while a stop-wait is in flight")
		}
		select {
		case <-stopDone:
		case <-time.After(5 * time.Second):
			t.Fatal("the stop-wait call must return")
		}

		// The registry entry drains no matter which side won the race.
		s.videoWaitMu.Lock()
		drained := s.videoWaits["req-race"] == nil
		s.videoWaitMu.Unlock()
		if !drained {
			t.Fatal("the wait's registry entry must drain after the bridge exits")
		}
	}
}
