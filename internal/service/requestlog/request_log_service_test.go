// Tests for GetRequestLogDetail's body-field
// composition (RequestLogDetail's 7 body columns, sourced from
// repository.GetRequestLogBodyByRequestID).
package requestlog

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func TestGetRequestLogDetailIncludesBodies(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db)
	now := time.Now().UTC()

	log := model.RequestLog{
		RequestID:  "req-with-body",
		ModelName:  "gpt-4o-mini",
		StatusCode: 200,
		Attempts:   1,
		DurationMs: 42,
		CreatedAt:  now,
	}
	if err := repository.CreateRequestLog(db, &log); err != nil {
		t.Fatalf("seed request_log: %v", err)
	}
	body := &model.RequestLogBody{
		RequestID:            "req-with-body",
		RequestBody:          `{"model":"gpt-4o-mini"}`,
		UpstreamRequestBody:  `{"model":"gpt-4o-mini","stream":false}`,
		ResponseBody:         `{"choices":[]}`,
		UpstreamResponseBody: `{"choices":[],"raw":true}`,
		StreamBodyPath:       "bodies/req-with-body.stream",
		StreamBodyTruncated:  true,
	}
	if err := repository.UpsertRequestLogBody(db, body); err != nil {
		t.Fatalf("seed request_log_body: %v", err)
	}

	detail, err := svc.GetRequestLogDetail("req-with-body")
	if err != nil {
		t.Fatalf("GetRequestLogDetail: %v", err)
	}
	if detail.RequestBody != body.RequestBody {
		t.Errorf("RequestBody: want %q, got %q", body.RequestBody, detail.RequestBody)
	}
	if detail.UpstreamRequestBody != body.UpstreamRequestBody {
		t.Errorf("UpstreamRequestBody: want %q, got %q", body.UpstreamRequestBody, detail.UpstreamRequestBody)
	}
	if detail.ResponseBody != body.ResponseBody {
		t.Errorf("ResponseBody: want %q, got %q", body.ResponseBody, detail.ResponseBody)
	}
	if detail.UpstreamResponseBody != body.UpstreamResponseBody {
		t.Errorf("UpstreamResponseBody: want %q, got %q", body.UpstreamResponseBody, detail.UpstreamResponseBody)
	}
	if detail.StreamBodyPath != body.StreamBodyPath {
		t.Errorf("StreamBodyPath: want %q, got %q", body.StreamBodyPath, detail.StreamBodyPath)
	}
	if !detail.StreamBodyTruncated {
		t.Errorf("StreamBodyTruncated: want true, got false")
	}
	if !detail.HasStreamBody {
		t.Errorf("HasStreamBody: want true, got false")
	}
}

func TestGetRequestLogDetailMissingBodyDegrades(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db)
	now := time.Now().UTC()

	log := model.RequestLog{
		RequestID:  "req-no-body",
		ModelName:  "gpt-4o-mini",
		StatusCode: 200,
		Attempts:   1,
		DurationMs: 42,
		CreatedAt:  now,
	}
	if err := repository.CreateRequestLog(db, &log); err != nil {
		t.Fatalf("seed request_log: %v", err)
	}

	detail, err := svc.GetRequestLogDetail("req-no-body")
	if err != nil {
		t.Fatalf("GetRequestLogDetail: %v", err)
	}
	if detail.RequestBody != "" || detail.UpstreamRequestBody != "" ||
		detail.ResponseBody != "" || detail.UpstreamResponseBody != "" ||
		detail.StreamBodyPath != "" {
		t.Errorf("expected zero-value body fields, got %+v", detail)
	}
	if detail.StreamBodyTruncated {
		t.Errorf("StreamBodyTruncated: want false, got true")
	}
	if detail.HasStreamBody {
		t.Errorf("HasStreamBody: want false, got true")
	}
}

// TestRequestLogRowsCarryOwnerUsername: list rows, the detail view and the
// CSV export must resolve request_logs.user_id to the owning account's
// username (batch-joined, same as provider_name); a row with no attributed
// account (an auth-rejected request) renders an empty username instead of
// failing the lookup.
func TestRequestLogRowsCarryOwnerUsername(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db)
	now := time.Now().UTC()

	u := &model.User{Username: "carol", Role: model.RoleMember, Status: model.UserStatusEnabled,
		CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateUser(db, u); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	key := &model.APIKey{KeyHash: "h-carol", KeyPrefix: "sk-yr-ca", UserID: u.ID,
		Status: model.APIKeyStatusActive, AllowAllModels: true, CreatedAt: now, UpdatedAt: now}
	if err := repository.CreateAPIKey(db, key, nil, now); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	owned := model.RequestLog{RequestID: "req-owned", ModelName: "m", StatusCode: 200,
		APIKeyID: &key.ID, UserID: &u.ID, Attempts: 1, CreatedAt: now}
	if err := repository.CreateRequestLog(db, &owned); err != nil {
		t.Fatalf("seed owned row: %v", err)
	}
	orphan := model.RequestLog{RequestID: "req-orphan", StatusCode: 401, CreatedAt: now}
	if err := repository.CreateRequestLog(db, &orphan); err != nil {
		t.Fatalf("seed orphan row: %v", err)
	}
	// The multi-user backfill stamped user_id onto historical keyless rows;
	// display must still treat them as unowned (api_key_id gates resolution).
	backfilled := model.RequestLog{RequestID: "req-backfilled", StatusCode: 401,
		UserID: &u.ID, CreatedAt: now}
	if err := repository.CreateRequestLog(db, &backfilled); err != nil {
		t.Fatalf("seed backfilled row: %v", err)
	}

	items, _, err := svc.ListRequestLogs(&repository.RequestLogFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	got := map[string]string{}
	for _, it := range items {
		got[it.RequestID] = it.Username
	}
	if got["req-owned"] != "carol" {
		t.Fatalf("owned row username: want carol, got %q", got["req-owned"])
	}
	if got["req-orphan"] != "" {
		t.Fatalf("orphan row username: want empty, got %q", got["req-orphan"])
	}
	if got["req-backfilled"] != "" {
		t.Fatalf("keyless backfilled row username: want empty, got %q", got["req-backfilled"])
	}

	detail, err := svc.GetRequestLogDetail("req-owned")
	if err != nil {
		t.Fatalf("GetRequestLogDetail: %v", err)
	}
	if detail.Username != "carol" {
		t.Fatalf("detail username: want carol, got %q", detail.Username)
	}

	// CSV: the username column must exist and line up with its values.
	rows, err := svc.BuildExportRows(&repository.RequestLogFilter{})
	if err != nil {
		t.Fatalf("BuildExportRows: %v", err)
	}
	header := csvHeaderRow()
	col := -1
	for i, h := range header {
		if h == "username" {
			col = i
		}
	}
	if col < 0 {
		t.Fatalf("csv header missing username column: %v", header)
	}
	for _, it := range rows {
		rec := csvRowFromItem(it)
		if len(rec) != len(header) {
			t.Fatalf("csv record width %d != header width %d", len(rec), len(header))
		}
		if it.RequestID == "req-owned" && rec[col] != "carol" {
			t.Fatalf("csv username cell: want carol, got %q", rec[col])
		}
	}
}

// TestTruncateBodyRuneSafeBacksOffPartialRune pins the requestlog module's
// own copy of the rune-boundary backoff — the inline-body cap must never
// hand the detail page a string ending in a broken multi-byte sequence.
func TestTruncateBodyRuneSafeBacksOffPartialRune(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxBytes int
		want     string
	}{
		{"under limit unchanged", "héllo", 100, "héllo"},
		{"cut mid-rune backs off", "aé", 2, "a"},
		{"cut on boundary keeps rune", "aé", 3, "aé"},
		{"multibyte CJK backs off", "日本", 4, "日"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := truncateBodyRuneSafe(c.in, c.maxBytes)
			if got != c.want {
				t.Fatalf("truncateBodyRuneSafe(%q, %d) = %q, want %q", c.in, c.maxBytes, got, c.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result %q is not valid UTF-8", got)
			}
		})
	}
}

// TestGetRequestLogDetailCarriesPriceSnapshot: the detail DTO passes the four
// settled-price columns through untouched — present on a snapshotted row, nil
// (rendered as "no snapshot") on a row that predates the snapshot columns.
func TestGetRequestLogDetailCarriesPriceSnapshot(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db)
	now := time.Now().UTC()

	in, out, cw, cr := 3.0, 6.0, 3.75, 0.3
	testutil.SeedRequestLog(t, db, "req-snapshotted", now, func(r *model.RequestLog) {
		r.SettledInputPrice = &in
		r.SettledOutputPrice = &out
		r.SettledCacheWritePrice = &cw
		r.SettledCacheReadPrice = &cr
	})
	testutil.SeedRequestLog(t, db, "req-pre-snapshot", now, nil)

	detail, err := svc.GetRequestLogDetail("req-snapshotted")
	if err != nil {
		t.Fatalf("GetRequestLogDetail: %v", err)
	}
	if detail.SettledInputPrice == nil || *detail.SettledInputPrice != in {
		t.Errorf("SettledInputPrice = %v, want %v", detail.SettledInputPrice, in)
	}
	if detail.SettledOutputPrice == nil || *detail.SettledOutputPrice != out {
		t.Errorf("SettledOutputPrice = %v, want %v", detail.SettledOutputPrice, out)
	}
	if detail.SettledCacheWritePrice == nil || *detail.SettledCacheWritePrice != cw {
		t.Errorf("SettledCacheWritePrice = %v, want %v", detail.SettledCacheWritePrice, cw)
	}
	if detail.SettledCacheReadPrice == nil || *detail.SettledCacheReadPrice != cr {
		t.Errorf("SettledCacheReadPrice = %v, want %v", detail.SettledCacheReadPrice, cr)
	}

	bare, err := svc.GetRequestLogDetail("req-pre-snapshot")
	if err != nil {
		t.Fatalf("GetRequestLogDetail: %v", err)
	}
	if bare.SettledInputPrice != nil || bare.SettledOutputPrice != nil ||
		bare.SettledCacheWritePrice != nil || bare.SettledCacheReadPrice != nil {
		t.Error("pre-snapshot row must carry nil prices, not fabricated values")
	}
}

// TestImageUsageDigest pins the snapshot parser's contract: only a parsed
// snapshot with a positive delivered count yields a digest, and everything
// else — no snapshot, unparseable JSON, a zero count — reads as "not an
// image-settled row".
func TestImageUsageDigest(t *testing.T) {
	cases := []struct {
		name      string
		snapshot  string
		wantCount int
		wantPrice *float64
	}{
		{"resolved snapshot", `{"actual_n":2,"unit_price":0.25}`, 2, ptrFloat(0.25)},
		{"empty", "", 0, nil},
		{"unparseable", "{not-json", 0, nil},
		{"zero delivered", `{"actual_n":0,"unit_price":0.25}`, 0, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			count, price := imageUsageDigest(c.snapshot)
			if count != c.wantCount {
				t.Fatalf("count = %d, want %d", count, c.wantCount)
			}
			switch {
			case c.wantPrice == nil:
				if price != nil {
					t.Fatalf("price = %v, want nil", *price)
				}
			case price == nil:
				t.Fatalf("price = nil, want %v", *c.wantPrice)
			case *price != *c.wantPrice:
				t.Fatalf("price = %v, want %v", *price, *c.wantPrice)
			}
		})
	}
}

func ptrFloat(v float64) *float64 { return &v }

// TestListRowsCarryImageUsage: a per-image settled row surfaces its digest on
// the list DTO (count + unit price) and renders one billing unit in the CSV;
// a token row keeps its token figures and reads billing_unit=token.
func TestListRowsCarryImageUsage(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db)
	now := time.Now().UTC()

	imageRow := model.RequestLog{RequestID: "req-image", ModelName: "wan2.2-image", StatusCode: 200,
		Attempts: 1, CreatedAt: now,
		ImagePricingSnapshot: `{"billing_mode":"image","request_quality":"standard","request_size":"1024*1024",` +
			`"request_n":4,"actual_n":2,"unit_price":0.25,"price_source":"tier","unit":"image","source":"payload"}`}
	if err := repository.CreateRequestLog(db, &imageRow); err != nil {
		t.Fatalf("seed image row: %v", err)
	}
	tokenRow := model.RequestLog{RequestID: "req-text", ModelName: "gpt-4o", StatusCode: 200,
		InputTokens: 12, OutputTokens: 34, Attempts: 1, CreatedAt: now}
	if err := repository.CreateRequestLog(db, &tokenRow); err != nil {
		t.Fatalf("seed token row: %v", err)
	}

	items, _, err := svc.ListRequestLogs(&repository.RequestLogFilter{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("ListRequestLogs: %v", err)
	}
	byID := map[string]RequestLogListItem{}
	for _, it := range items {
		byID[it.RequestID] = it
	}
	img := byID["req-image"]
	if img.ImageCount != 2 || img.ImageUnitPrice == nil || *img.ImageUnitPrice != 0.25 {
		t.Fatalf("image row digest: count=%d price=%v, want 2 and 0.25", img.ImageCount, img.ImageUnitPrice)
	}
	txt := byID["req-text"]
	if txt.ImageCount != 0 || txt.ImageUnitPrice != nil {
		t.Fatalf("token row digest: count=%d price=%v, want 0 and nil", txt.ImageCount, txt.ImageUnitPrice)
	}

	// CSV: the usage columns line up with their header and carry one billing
	// unit per row.
	header := csvHeaderRow()
	col := func(name string) int {
		for i, h := range header {
			if h == name {
				return i
			}
		}
		t.Fatalf("csv header missing %q: %v", name, header)
		return -1
	}
	unit, count, price := col("billing_unit"), col("image_count"), col("image_unit_price")
	if unit < 0 || count < 0 || price < 0 {
		t.Fatalf("usage columns not all present: %v", header)
	}
	for _, it := range items {
		rec := csvRowFromItem(it)
		if len(rec) != len(header) {
			t.Fatalf("csv record width %d != header width %d", len(rec), len(header))
		}
		switch it.RequestID {
		case "req-image":
			if rec[unit] != "image" || rec[count] != "2" || rec[price] != "0.25" {
				t.Fatalf("image csv cells: %q %q %q", rec[unit], rec[count], rec[price])
			}
		case "req-text":
			if rec[unit] != "token" || rec[count] != "" || rec[price] != "" {
				t.Fatalf("token csv cells: %q %q %q", rec[unit], rec[count], rec[price])
			}
		}
	}
}
