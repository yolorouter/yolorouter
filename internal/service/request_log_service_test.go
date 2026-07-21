// Package service tests for GetRequestLogDetail's body-field
// composition (RequestLogDetail's 7 body columns, sourced from
// repository.GetRequestLogBodyByRequestID).
package service

import (
	"testing"
	"time"

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
