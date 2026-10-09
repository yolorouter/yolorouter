package handler

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"

	"github.com/yolorouter/yolorouter/internal/audit"
	"github.com/yolorouter/yolorouter/internal/middleware"
	"github.com/yolorouter/yolorouter/internal/model"
	"github.com/yolorouter/yolorouter/internal/selfupdate"
	"github.com/yolorouter/yolorouter/internal/testutil"
)

// auditUpdateFixture mounts the real one-click update handler under the
// real audit capture layer (request-id + group-root middleware + started
// worker over a migrated sqlite database) — the same chain shape
// production assembles for POST /api/admin/system/update, minus the
// session layer (the row's identity is NULL here, which is orthogonal to
// what these cases pin). The worker's flush interval outlives the test, so
// the rows appear only when stop drains — the deterministic exit path.
type auditUpdateFixture struct {
	engine *gin.Engine
	db     *gorm.DB
	stop   func()
}

func newAuditUpdateFixture(t *testing.T, fake *fakeUpdate) *auditUpdateFixture {
	t.Helper()
	db := testutil.NewSQLiteDB(t)
	worker := audit.NewWorker(audit.Config{DB: db, FlushInterval: time.Hour})
	stop := worker.Start(context.Background())
	t.Cleanup(stop)

	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(middleware.RequestID())
	group := engine.Group("/api/admin")
	group.Use(audit.Middleware(db, worker))
	group.POST("/system/update", PostSystemUpdate(selfupdate.ModeInPlace, nil, fake.apply, fake.restart))

	return &auditUpdateFixture{engine: engine, db: db, stop: stop}
}

func (f *auditUpdateFixture) post(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/system/update", nil)
	w := httptest.NewRecorder()
	f.engine.ServeHTTP(w, req)
	return w
}

// rows returns the persisted audit rows (call after f.stop).
func (f *auditUpdateFixture) rows(t *testing.T) []model.AdminAuditLog {
	t.Helper()
	f.stop()
	var rows []model.AdminAuditLog
	if err := f.db.Find(&rows).Error; err != nil {
		t.Fatalf("list audit rows: %v", err)
	}
	return rows
}

// TestSystemUpdateAuditRowCarriesTargetVersion is the one-click update's
// audit contract (the explicit-layer half): the POST lands
// exactly one row under the route's own action, and its detail carries
// the release the attempt resolved to — the fact only this handler knows,
// parked so the automatic layer's route-shaped row speaks it. Both
// success outcomes park: an applied update names what it installed, an
// up-to-date check names what it verified.
func TestSystemUpdateAuditRowCarriesTargetVersion(t *testing.T) {
	t.Run("updated", func(t *testing.T) {
		fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.9.9", BackupPath: "/x/yolorouter.bak"}}
		f := newAuditUpdateFixture(t, fake)

		w := f.post(t)
		if w.Code != http.StatusOK {
			t.Fatalf("update must answer 200, got %d: %s", w.Code, w.Body.String())
		}

		rows := f.rows(t)
		if len(rows) != 1 {
			t.Fatalf("one update POST must land exactly one row, got %d", len(rows))
		}
		row := rows[0]
		if row.Action != "system.update" {
			t.Errorf("action = %q, want system.update", row.Action)
		}
		if row.Status != model.AuditStatusSuccess {
			t.Errorf("status = %q, want success", row.Status)
		}
		if row.Detail != `{"target_version":"v9.9.9"}` {
			t.Errorf("detail = %s, want the applied release's target version", row.Detail)
		}
		// The row reconciles with the response's request id — the same
		// correlation key every automatic-layer row carries.
		if row.RequestID != w.Header().Get("X-Request-Id") {
			t.Errorf("request_id = %q, want the response header's %q", row.RequestID, w.Header().Get("X-Request-Id"))
		}
	})

	t.Run("up_to_date", func(t *testing.T) {
		fake := &fakeUpdate{result: selfupdate.Result{Target: "v9.8.0", UpToDate: true}}
		f := newAuditUpdateFixture(t, fake)

		w := f.post(t)
		if w.Code != http.StatusOK {
			t.Fatalf("up-to-date check must answer 200, got %d: %s", w.Code, w.Body.String())
		}

		rows := f.rows(t)
		if len(rows) != 1 {
			t.Fatalf("one update POST must land exactly one row, got %d", len(rows))
		}
		if rows[0].Detail != `{"target_version":"v9.8.0"}` {
			t.Errorf("detail = %s, want the checked release's target version", rows[0].Detail)
		}
	})

	t.Run("failed apply parks no target", func(t *testing.T) {
		fake := &fakeUpdate{err: errors.New("download refused")}
		f := newAuditUpdateFixture(t, fake)

		if w := f.post(t); w.Code != http.StatusInternalServerError {
			t.Fatalf("failed apply must answer 500, got %d", w.Code)
		}

		rows := f.rows(t)
		if len(rows) != 1 {
			t.Fatalf("the failed POST must still land exactly one row, got %d", len(rows))
		}
		row := rows[0]
		if row.Status != model.AuditStatusError {
			t.Errorf("status = %q, want error", row.Status)
		}
		if row.Detail != "" {
			t.Errorf("a failed apply resolved to no release; detail = %s, want empty", row.Detail)
		}
	})
}
