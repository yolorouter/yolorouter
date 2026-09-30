// Tests for GetRequestLogDetail's body-field
// composition (RequestLogDetail's 7 body columns, sourced from
// repository.GetRequestLogBodyByRequestID, plus the stream capture file the
// body row's stream_body_path names — readStreamBodyInline).
package requestlog

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/repository"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// seedDetailWithCapture plants a request_log row and a body row whose
// stream_body_path is capturePath — the caller separately writes whatever
// should (or should not) exist on disk under the service's bodies dir.
func seedDetailWithCapture(t *testing.T, db *gorm.DB, requestID, capturePath string) {
	t.Helper()
	testutil.SeedRequestLog(t, db, requestID, time.Now().UTC(), func(r *model.RequestLog) {
		r.IsStream = true
	})
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      requestID,
		StreamBodyPath: capturePath,
	}); err != nil {
		t.Fatalf("seed request_log_body for %s: %v", requestID, err)
	}
}

func TestGetRequestLogDetailIncludesBodies(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
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
	svc := NewRequestLogService(db, "")
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
	svc := NewRequestLogService(db, "")
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
	svc := NewRequestLogService(db, "")
	now := time.Now().UTC()

	in, out, cw, cr := 3.0, 6.0, 3.75, 0.3
	testutil.SeedRequestLog(t, db, "req-snapshotted", now, func(r *model.RequestLog) {
		r.SettledInputPrice = &in
		r.SettledOutputPrice = &out
		r.SettledCacheWritePrice = &cw
		r.SettledCacheReadPrice = &cr
	})
	testutil.SeedRequestLog(t, db, "req-pre-snapshot", now, nil)
	// A settled video row rides along: the detail page's video-duration
	// row reads the usage half of the settlement digest.
	testutil.SeedRequestLog(t, db, "req-video-settled", now, func(r *model.RequestLog) {
		r.UsageSeconds = 4
		r.CostMicros = 2_800_000
		r.CostKnown = true
	})
	videoDetail, err := svc.GetRequestLogDetail("req-video-settled")
	if err != nil {
		t.Fatalf("GetRequestLogDetail(video): %v", err)
	}
	if videoDetail.UsageSeconds != 4 {
		t.Errorf("video detail UsageSeconds = %d, want 4", videoDetail.UsageSeconds)
	}

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

// TestListRowsCarryUsageDigest: a per-image settled row surfaces its digest
// on the list DTO (count + unit price), a settled video row its seconds, and
// each renders one billing unit in the CSV; a token row keeps its token
// figures and reads billing_unit=token.
func TestListRowsCarryUsageDigest(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, "")
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
	// A settled video row: the settlement projection stamps cost and the
	// delivered seconds together; the token counts stay zero.
	videoRow := model.RequestLog{RequestID: "req-video", ModelName: "seedance-2-0", StatusCode: 200,
		Attempts: 1, CreatedAt: now, CostMicros: 2_800_000, CostKnown: true, UsageSeconds: 4}
	if err := repository.CreateRequestLog(db, &videoRow); err != nil {
		t.Fatalf("seed video row: %v", err)
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
	if txt.ImageCount != 0 || txt.ImageUnitPrice != nil || txt.UsageSeconds != 0 {
		t.Fatalf("token row digest: count=%d price=%v seconds=%d, want 0, nil and 0", txt.ImageCount, txt.ImageUnitPrice, txt.UsageSeconds)
	}
	vid := byID["req-video"]
	if vid.UsageSeconds != 4 || vid.ImageCount != 0 {
		t.Fatalf("video row digest: seconds=%d count=%d, want 4 and 0", vid.UsageSeconds, vid.ImageCount)
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
	unit, count, price, seconds := col("billing_unit"), col("image_count"), col("image_unit_price"), col("usage_seconds")
	if unit < 0 || count < 0 || price < 0 || seconds < 0 {
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
			if rec[unit] != "token" || rec[count] != "" || rec[price] != "" || rec[seconds] != "" {
				t.Fatalf("token csv cells: %q %q %q %q", rec[unit], rec[count], rec[price], rec[seconds])
			}
		case "req-video":
			if rec[unit] != "video" || rec[seconds] != "4" || rec[count] != "" || rec[price] != "" {
				t.Fatalf("video csv cells: %q %q %q %q", rec[unit], rec[count], rec[price], rec[seconds])
			}
		}
	}
}

// TestGetRequestLogDetailInlinesStreamBodyContent pins stream_body's two
// content arms: a capture within the 1 MiB inline cap returns its exact
// bytes; one past the cap is cut to the cap with the same human-readable
// truncation marker the DB-backed body columns use, stating the on-disk
// size — never the partially-read length.
func TestGetRequestLogDetailInlinesStreamBodyContent(t *testing.T) {
	cases := []struct {
		name     string
		fileSize int
	}{
		{"within cap returns exact bytes", 4096},
		{"over cap truncates with marker", maxInlineBodyBytes + 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := testutil.NewSQLiteDB(t)
			bodiesDir := t.TempDir()
			svc := NewRequestLogService(db, bodiesDir)
			content := bytes.Repeat([]byte("data: {\"delta\":\"chunk\"}\n\n"), tc.fileSize/24+1)
			content = content[:tc.fileSize]
			if err := os.WriteFile(filepath.Join(bodiesDir, "req-inline.stream"), content, 0o600); err != nil {
				t.Fatalf("write capture file: %v", err)
			}
			seedDetailWithCapture(t, db, "req-inline", "req-inline.stream")

			detail, err := svc.GetRequestLogDetail("req-inline")
			if err != nil {
				t.Fatalf("GetRequestLogDetail: %v", err)
			}
			if tc.fileSize <= maxInlineBodyBytes {
				if detail.StreamBody != string(content) {
					t.Fatalf("stream_body: want the file's exact %d bytes, got %d bytes", len(content), len(detail.StreamBody))
				}
				return
			}
			// ASCII content, so the rune-safe cut lands exactly on the cap.
			if !strings.HasPrefix(detail.StreamBody, string(content[:maxInlineBodyBytes])) {
				t.Fatalf("stream_body: want the file's first %d bytes as prefix", maxInlineBodyBytes)
			}
			wantMarker := fmt.Sprintf(inlineTruncationMarker, maxInlineBodyBytes, tc.fileSize)
			if !strings.HasSuffix(detail.StreamBody, wantMarker) {
				t.Fatalf("stream_body: want marker %q, got tail %q", wantMarker, detail.StreamBody[maxInlineBodyBytes:])
			}
			if got := len(detail.StreamBody); got != maxInlineBodyBytes+len(wantMarker) {
				t.Fatalf("stream_body length: want %d, got %d", maxInlineBodyBytes+len(wantMarker), got)
			}
		})
	}
}

// TestGetRequestLogDetailStreamBodyEmptyWhenNoCapture pins every "" arm: no
// body row at all (a non-streaming request), a body row with no capture path,
// a path whose file is gone from disk, a capture path occupied by a directory
// (os.Open succeeds, the read itself fails — readStreamBodyInline's
// io.ReadAll error arm), and a service with no bodies dir wired — each
// serializes stream_body as "" instead of failing the detail.
func TestGetRequestLogDetailStreamBodyEmptyWhenNoCapture(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	bodiesDir := t.TempDir()

	testutil.SeedRequestLog(t, db, "req-nonstream", time.Now().UTC(), func(r *model.RequestLog) {
		r.IsStream = false
	})
	testutil.SeedRequestLog(t, db, "req-empty-path", time.Now().UTC(), nil)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{RequestID: "req-empty-path"}); err != nil {
		t.Fatalf("seed empty-path body row: %v", err)
	}
	// Path names a file that was never written under bodiesDir (capture
	// rotated away, or the DB outlived the data dir).
	testutil.SeedRequestLog(t, db, "req-gone-file", time.Now().UTC(), nil)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      "req-gone-file",
		StreamBodyPath: "req-gone-file.stream",
	}); err != nil {
		t.Fatalf("seed gone-file body row: %v", err)
	}
	// The capture path names a directory squatting under bodiesDir: os.Open
	// succeeds on a directory but reading it fails (EISDIR on Linux), driving
	// readStreamBodyInline's io.ReadAll error arm — the one miss whose file
	// still opens — which must degrade to "" like every other miss.
	if err := os.Mkdir(filepath.Join(bodiesDir, "req-dir-capture.stream"), 0o755); err != nil {
		t.Fatalf("mkdir dir-at-capture-path: %v", err)
	}
	testutil.SeedRequestLog(t, db, "req-dir-capture", time.Now().UTC(), nil)
	if err := repository.UpsertRequestLogBody(db, &model.RequestLogBody{
		RequestID:      "req-dir-capture",
		StreamBodyPath: "req-dir-capture.stream",
	}); err != nil {
		t.Fatalf("seed dir-capture body row: %v", err)
	}

	for _, tc := range []struct {
		name      string
		svc       *RequestLogService
		requestID string
	}{
		{"no body row", NewRequestLogService(db, bodiesDir), "req-nonstream"},
		{"empty capture path", NewRequestLogService(db, bodiesDir), "req-empty-path"},
		{"file gone from disk", NewRequestLogService(db, bodiesDir), "req-gone-file"},
		{"capture path is a directory", NewRequestLogService(db, bodiesDir), "req-dir-capture"},
		{"no bodies dir wired", NewRequestLogService(db, ""), "req-gone-file"},
	} {
		detail, err := tc.svc.GetRequestLogDetail(tc.requestID)
		if err != nil {
			t.Fatalf("%s: GetRequestLogDetail: %v", tc.name, err)
		}
		if detail.StreamBody != "" {
			t.Errorf("%s: stream_body: want \"\", got %q", tc.name, detail.StreamBody)
		}
	}
}

// TestGetRequestLogDetailStreamBodyStaysInsideBodiesDir pins the safety
// contract: the stored stream_body_path is untrusted. A traversal path
// ("../elsewhere/x.stream") and an absolute path must both resolve to the
// bare filename under bodiesDir — reading bodiesDir's own file of that name,
// never the file the raw path points at outside it.
func TestGetRequestLogDetailStreamBodyStaysInsideBodiesDir(t *testing.T) {
	root := t.TempDir()
	bodiesDir := filepath.Join(root, "bodies")
	escapeDir := filepath.Join(root, "elsewhere")
	for _, dir := range []string{bodiesDir, escapeDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	// Same basename on both sides of the boundary: whichever content comes
	// back names which side the reader actually opened.
	if err := os.WriteFile(filepath.Join(bodiesDir, "dup.stream"), []byte("INSIDE"), 0o600); err != nil {
		t.Fatalf("write inside capture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(escapeDir, "dup.stream"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatalf("write outside decoy: %v", err)
	}
	// Outside-only basename: nothing of this name exists under bodiesDir.
	if err := os.WriteFile(filepath.Join(escapeDir, "only-outside.stream"), []byte("OUTSIDE"), 0o600); err != nil {
		t.Fatalf("write outside-only decoy: %v", err)
	}

	db := testutil.NewSQLiteDB(t)
	svc := NewRequestLogService(db, bodiesDir)
	seedDetailWithCapture(t, db, "req-traversal", "../elsewhere/dup.stream")
	seedDetailWithCapture(t, db, "req-absolute", filepath.Join(escapeDir, "dup.stream"))
	seedDetailWithCapture(t, db, "req-only-outside", "../elsewhere/only-outside.stream")

	for _, tc := range []struct {
		requestID string
		want      string
	}{
		{"req-traversal", "INSIDE"},
		{"req-absolute", "INSIDE"},
		// The basename is absent under bodiesDir, so nothing is read — not
		// even though a file exists exactly where the raw path points.
		{"req-only-outside", ""},
	} {
		detail, err := svc.GetRequestLogDetail(tc.requestID)
		if err != nil {
			t.Fatalf("%s: GetRequestLogDetail: %v", tc.requestID, err)
		}
		if detail.StreamBody != tc.want {
			t.Errorf("%s: stream_body: want %q, got %q", tc.requestID, tc.want, detail.StreamBody)
		}
	}
}
