package analytics

import (
	"reflect"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// Real-DB tests for the history model-name supplement behind the analytics
// filter dropdown. The window is a code constant, so the probes below seed
// request-log rows at exact offsets around it and pin, as fixed assertions:
// which tier's names appear, that the boundary row itself is included, that
// a row one nanosecond older is not, that duplicates collapse, and that the
// list comes back sorted. Same in-package convention as the report tests:
// service in, rows out, no router.

// seedHistoryLog inserts one request-log row whose created_at is exactly at.
// The boundary probe depends on the stored value comparing equal to the
// query's bound parameter: both values travel through the same driver text
// format, so seeding with the cutoff instant itself is exact equality, not
// "close enough".
func seedHistoryLog(t *testing.T, svc *AnalyticsService, requestID, name string, at time.Time) {
	t.Helper()
	log := model.RequestLog{RequestID: requestID, ModelName: name, StatusCode: 200, CreatedAt: at}
	if err := repository.CreateRequestLog(svc.db, &log); err != nil {
		t.Fatalf("seed log %s: %v", requestID, err)
	}
}

// The three seeding tiers, one name each: deep in the window, exactly on the
// window's lower edge, and outside it (barely and far). The edge convention
// this test freezes: the window is INCLUSIVE at the lower bound — a row
// logged exactly historyModelNamesWindow before now still counts, matching
// the "created_at >= since" comparison the query runs — so boundary-name
// must appear and the one-nanosecond-older name must not. alpha-deep's 89-day
// offset also pins the constant's size: shrinking the window to any shorter
// span drops it from the list. Duplicate alpha-deep rows collapse to one
// entry, and the whole list comes back ascending.
func TestListHistoryModelNamesThreeTierWindow(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewAnalyticsService(db)
	now := time.Now().UTC()

	seedHistoryLog(t, svc, "fresh", "zeta-fresh", now.Add(-time.Hour))
	seedHistoryLog(t, svc, "deep", "alpha-deep", now.Add(-89*24*time.Hour))
	seedHistoryLog(t, svc, "boundary", "delta-boundary", now.Add(-historyModelNamesWindow))
	seedHistoryLog(t, svc, "one-ns-older", "epsilon-just-outside", now.Add(-historyModelNamesWindow-1))
	seedHistoryLog(t, svc, "ancient", "gamma-ancient", now.Add(-91*24*time.Hour))
	seedHistoryLog(t, svc, "dup-1", "alpha-deep", now.Add(-2*time.Hour))
	seedHistoryLog(t, svc, "dup-2", "alpha-deep", now.Add(-88*24*time.Hour))

	result, err := svc.ListHistoryModelNames(now)
	if err != nil {
		t.Fatalf("ListHistoryModelNames failed: %v", err)
	}
	want := []string{"alpha-deep", "delta-boundary", "zeta-fresh"}
	if !reflect.DeepEqual(result.Names, want) {
		t.Fatalf("names = %v, want %v (in-window deduped ascending; edge row included, one-ns-older and ancient excluded)", result.Names, want)
	}
	if result.WindowDays != 90 {
		t.Fatalf("window days = %d, want 90", result.WindowDays)
	}
}

// An empty request_logs table is the fresh-install case: the supplement
// must be an EMPTY LIST, not a nil slice — nil marshals to JSON null and
// the dropdown merge in the frontend would have to null-check.
func TestListHistoryModelNamesEmptyTable(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewAnalyticsService(db)

	result, err := svc.ListHistoryModelNames(time.Now().UTC())
	if err != nil {
		t.Fatalf("ListHistoryModelNames failed: %v", err)
	}
	if result.Names == nil {
		t.Fatalf("names = nil, want an empty (non-nil) slice")
	}
	if len(result.Names) != 0 {
		t.Fatalf("names = %v, want empty", result.Names)
	}
	if result.WindowDays != 90 {
		t.Fatalf("window days = %d, want 90", result.WindowDays)
	}
}
