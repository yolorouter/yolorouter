package repository

import (
	"testing"

	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

func TestUpsertRequestLogBodyInsertThenUpdate(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	row := &model.RequestLogBody{
		RequestID:   "req_upsert_1",
		RequestBody: "hello", UpstreamRequestBody: "u1",
		ResponseBody: "resp1", UpstreamResponseBody: "raw1",
	}
	if err := UpsertRequestLogBody(db, row); err != nil {
		t.Fatalf("first upsert: %v", err)
	}
	// second call with the same request_id must DO UPDATE, not duplicate
	row2 := &model.RequestLogBody{
		RequestID:   "req_upsert_1",
		RequestBody: "hello2", ResponseBody: "resp2",
	}
	if err := UpsertRequestLogBody(db, row2); err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	got, err := GetRequestLogBodyByRequestID(db, "req_upsert_1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatalf("expected a row, got nil")
	}
	if got.RequestBody != "hello2" || got.ResponseBody != "resp2" {
		t.Fatalf("upsert did not overwrite: %+v", got)
	}

	var count int64
	if err := db.Model(&model.RequestLogBody{}).Where("request_id = ?", "req_upsert_1").Count(&count).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 row for request_id, got %d", count)
	}
}

func TestGetRequestLogBodyByRequestIDNotFound(t *testing.T) {
	db := testutil.NewSQLiteDB(t)
	got, err := GetRequestLogBodyByRequestID(db, "missing")
	if err != nil {
		t.Fatalf("not-found should return (nil,nil), got err: %v", err)
	}
	if got != nil {
		t.Fatalf("not-found should return nil body, got %+v", got)
	}
}
