package repository

import (
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// TestListRequestLogsFiltersByW3CTraceID pins the trace-id exact-match
// filter: a known trace-id hits exactly its own row (identified by
// request_id), rows without a trace-id are never matched by any value, and
// both an unknown trace-id and an explicitly-empty one yield zero rows —
// the pointer field keeps "filter off" (nil) and "filter by empty" (&"")
// as distinct inputs, since no stored id is ever the empty string.
func TestListRequestLogsFiltersByW3CTraceID(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	now := time.Now().UTC()
	const traceA = "4bf92f3577b34da6a3ce929d0e0e4736"
	const traceB = "00f067aa0ba902b712d0fbe0f6a5c1b4"
	seedTraced := func(requestID, traceID string) {
		t.Helper()
		id := traceID
		testutil.SeedRequestLog(t, db, requestID, now, func(r *model.RequestLog) {
			r.W3CTraceID = &id
		})
	}
	seedTraced("req-traced-a", traceA)
	seedTraced("req-traced-b", traceB)
	// Rows whose caller sent no usable traceparent read NULL — 2 of them,
	// to catch a filter that accidentally matches on the empty dimension
	// instead of the value.
	testutil.SeedRequestLog(t, db, "req-untraced-1", now, nil)
	testutil.SeedRequestLog(t, db, "req-untraced-2", now, nil)

	// listByTrace runs one filtered list and returns the request_ids it hit.
	listByTrace := func(v *string) (ids []string, total int64) {
		t.Helper()
		rows, total, err := ListRequestLogs(db, &RequestLogFilter{W3CTraceID: v})
		if err != nil {
			t.Fatalf("ListRequestLogs(%v): %v", v, err)
		}
		for _, r := range rows {
			ids = append(ids, r.RequestID)
		}
		return ids, total
	}

	// Each known trace-id hits exactly its own row, by request_id.
	for _, tc := range []struct {
		traceID     string
		wantRequest string
	}{
		{traceA, "req-traced-a"},
		{traceB, "req-traced-b"},
	} {
		v := tc.traceID
		ids, total := listByTrace(&v)
		if total != 1 || len(ids) != 1 || ids[0] != tc.wantRequest {
			t.Fatalf("filter by %s = (%v, total=%d), want exactly [%s]", tc.traceID, ids, total, tc.wantRequest)
		}
	}

	// An unknown trace-id matches nothing.
	unknown := "ffffffffffffffffffffffffffffffff"
	if ids, total := listByTrace(&unknown); total != 0 || len(ids) != 0 {
		t.Fatalf("filter by unknown trace-id = (%v, total=%d), want no rows", ids, total)
	}

	// An explicitly-empty value is a real constraint and matches nothing:
	// untraced rows are NULL (never equal to ''), and no traced row stores
	// an empty id. This is the arm that separates "filter by empty" from
	// "filter off".
	empty := ""
	if ids, total := listByTrace(&empty); total != 0 || len(ids) != 0 {
		t.Fatalf("filter by empty trace-id = (%v, total=%d), want no rows", ids, total)
	}

	// nil = filter off: every seeded row is visible, including the two
	// untraced ones — proving the exact-match arms above excluded them by
	// value, not because they were missing from the fixture.
	if ids, total := listByTrace(nil); total != 4 || len(ids) != 4 {
		t.Fatalf("no trace-id filter = (%v, total=%d), want all 4 seeded rows", ids, total)
	}
}
