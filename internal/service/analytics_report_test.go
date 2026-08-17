package service

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// These tests exercise the report pipeline directly — service in,
// rows/CSV out, no router. The aggregation behavior lives in
// repository/service, so its assertions belong here rather than behind an
// HTTP round-trip; the handler tests keep covering routing, envelopes and
// authz on top.

func seedReportLog(t *testing.T, svc *AnalyticsService, requestID string, status int, inputTokens int, cost int64, known bool) {
	t.Helper()
	now := time.Now().UTC()
	log := model.RequestLog{RequestID: requestID, ModelName: "m1", StatusCode: status,
		InputTokens: inputTokens, CostMicros: cost, CostKnown: known, CreatedAt: now}
	if err := repository.CreateRequestLog(svc.db, &log); err != nil {
		t.Fatalf("seed log %s: %v", requestID, err)
	}
}

// TestReportRateAndUnknownCostArithmetic: the success rate divides 2xx
// no-fail calls by ended calls (499 cancels excluded from the
// denominator), and unknown-cost rows count separately instead of
// masquerading as zero cost.
func TestReportRateAndUnknownCostArithmetic(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewAnalyticsService(db)
	seedReportLog(t, svc, "ok-1", 200, 10, 5, true)
	seedReportLog(t, svc, "ok-2", 200, 20, 7, true)
	seedReportLog(t, svc, "fail-1", 500, 0, 0, true)
	seedReportLog(t, svc, "cancel-1", 499, 0, 0, true) // excluded from ended
	seedReportLog(t, svc, "priceless", 200, 5, 0, false)

	res, err := svc.GetReport(DimensionModel, "", &repository.RequestLogFilter{}, AnalyticsOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	rows, ok := res.Rows.([]repository.ModelReportRow)
	if !ok || len(rows) != 1 {
		t.Fatalf("expected one m1 row, got %#v", res.Rows)
	}
	r := rows[0]
	// 3 successes (ok-1, ok-2, priceless) / 4 ended (all but the 499).
	if r.Calls != 5 || r.EndedCalls != 4 || r.SuccessCalls != 3 {
		t.Fatalf("counter mismatch: %+v", r)
	}
	if want := 0.75; r.SuccessRate != want {
		t.Fatalf("success rate: want %v, got %v", want, r.SuccessRate)
	}
	if r.UnknownCostCalls != 1 || r.CostMicros != 12 {
		t.Fatalf("cost fields: %+v", r)
	}
}

// TestReportCSVColumnOrder: every token-reporting dimension shares one
// metric tail, prefixed by its own key columns — the exact header order is
// the wire contract spreadsheets depend on.
func TestReportCSVColumnOrder(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewAnalyticsService(db)
	seedReportLog(t, svc, "ok-1", 200, 10, 5, true)

	tail := "calls,success_rate,input_tokens,output_tokens,cache_write_tokens,cache_read_tokens,cost_micros,unknown_cost_calls"
	want := map[string]string{
		DimensionModel:  "model_name," + tail,
		DimensionCaller: "api_key_id,username,key_prefix," + tail,
		DimensionUser:   "user_id,username," + tail,
		DimensionTime:   "bucket," + tail,
	}
	for dim, header := range want {
		headers, records, err := svc.BuildCSVRecords(dim, "", &repository.RequestLogFilter{}, AnalyticsOptions{}, time.Now().UTC())
		if err != nil {
			t.Fatalf("BuildCSVRecords(%s): %v", dim, err)
		}
		if got := strings.Join(headers, ","); got != header {
			t.Fatalf("%s header order: want %q, got %q", dim, header, got)
		}
		for _, rec := range records {
			if len(rec) != len(headers) {
				t.Fatalf("%s record width %d != header width %d", dim, len(rec), len(headers))
			}
		}
	}

	// Cell-to-header correspondence, asserted on one dimension with known
	// seeded values: one 200-status call, 10 input tokens, cost 5 micros.
	// Swapping any two cells inside the shared tail turns this red.
	headers, records, err := svc.BuildCSVRecords(DimensionModel, "", &repository.RequestLogFilter{}, AnalyticsOptions{}, time.Now().UTC())
	if err != nil {
		t.Fatalf("BuildCSVRecords(model): %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected one model row, got %d", len(records))
	}
	wantRow := []string{"m1", "1", "1.0000", "10", "0", "0", "0", "5", "0"}
	for i, cell := range records[0] {
		if cell != wantRow[i] {
			t.Fatalf("model CSV cell %q (column %s): want %q, got row %v", cell, headers[i], wantRow[i], records[0])
		}
	}
}

// TestEveryValidDimensionReportsAndRendersCSV locks the pairing the two
// dimension switches can silently lose: every entry in validDimensions must
// both run a report and render a CSV. runReport and buildCSV are separate
// switches, so a dimension added to the vocabulary without its buildCSV
// branch compiles fine and only fails here (the CSV path's default branch
// returns ErrInvalidDimension).
func TestEveryValidDimensionReportsAndRendersCSV(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewAnalyticsService(db)
	seedReportLog(t, svc, "ok-1", 200, 10, 5, true)

	for _, dim := range validDimensions {
		if _, err := svc.GetReport(dim, "", &repository.RequestLogFilter{}, AnalyticsOptions{}, time.Now().UTC()); err != nil {
			t.Fatalf("GetReport(%s): %v", dim, err)
		}
		headers, _, err := svc.BuildCSVRecords(dim, "", &repository.RequestLogFilter{}, AnalyticsOptions{}, time.Now().UTC())
		if err != nil {
			t.Fatalf("BuildCSVRecords(%s): %v", dim, err)
		}
		if len(headers) == 0 {
			t.Fatalf("BuildCSVRecords(%s): no headers", dim)
		}
	}
}

// TestValidDimensionsVocabulary pins the exact contents and order of
// validDimensions. Everything (allowlist, both error texts, the CSV column
// test below) derives from this slice, so a value silently dropping out of
// it would leave every derived artifact consistent-yet-wrong — this is the
// one assertion that compares the vocabulary against an independent
// hand-written expectation instead of against itself. Changing the
// vocabulary is a wire-contract change: update this list together with the
// runReport/buildCSV switches and the frontend dimension list.
func TestValidDimensionsVocabulary(t *testing.T) {
	want := []string{
		DimensionModel,
		DimensionProvider,
		DimensionCaller,
		DimensionUser,
		DimensionTime,
	}
	if !reflect.DeepEqual(validDimensions, want) {
		t.Fatalf("validDimensions changed: got %v, want %v", validDimensions, want)
	}
}

// TestErrInvalidDimensionListsEveryValidDimension: the error text derives
// from validDimensions, so every legal value must appear in it. This is the
// drift that motivated deriving the text — the hand-written version once
// omitted user while the handler's listed it. (Note this test is only
// meaningful against a text that stops deriving from the list; the
// vocabulary itself is pinned by TestValidDimensionsVocabulary.)
func TestErrInvalidDimensionListsEveryValidDimension(t *testing.T) {
	msg := ErrInvalidDimension.Error()
	for _, dim := range validDimensions {
		if !strings.Contains(msg, dim) {
			t.Fatalf("ErrInvalidDimension text missing %q: %s", dim, msg)
		}
	}
}
