package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"github.com/marmos91/dittofs/pkg/controlplane/api/dto"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
)

// fakeSnapshotRuntime is a minimal SnapshotRuntime test double. Each
// field overrides the corresponding method; the zero value returns nil.
type fakeSnapshotRuntime struct {
	createFn     func(ctx context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error)
	waitFn       func(ctx context.Context, share, snapID string) (*models.Snapshot, error)
	restoreFn    func(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error)
	getFn        func(ctx context.Context, share, snapID string) (*models.Snapshot, error)
	listFn       func(ctx context.Context, share string) ([]*models.Snapshot, error)
	deleteFn     func(ctx context.Context, share, snapID string) error
	usageFn      func(ctx context.Context, share, snapID string) (*runtime.SnapshotUsageReport, error)
	rehydrateFn  func(ctx context.Context, share, snapID, src, dst string) (*runtime.RehydrateJob, error)
	rehydrateJob *runtime.RehydrateJob
}

func (f *fakeSnapshotRuntime) RehydrateSnapshot(ctx context.Context, share, snapID, src, dst string) (*runtime.RehydrateJob, error) {
	if f.rehydrateFn != nil {
		return f.rehydrateFn(ctx, share, snapID, src, dst)
	}
	return &runtime.RehydrateJob{ID: "job-1", Share: share, SnapshotID: snapID, SourceVolume: src, Volume: dst}, nil
}

func (f *fakeSnapshotRuntime) GetRehydrateJob(id string) (*runtime.RehydrateJob, bool) {
	if f.rehydrateJob != nil && f.rehydrateJob.ID == id {
		return f.rehydrateJob, true
	}
	return nil, false
}

func (f *fakeSnapshotRuntime) SnapshotUsage(ctx context.Context, share, snapID string) (*runtime.SnapshotUsageReport, error) {
	if f.usageFn != nil {
		return f.usageFn(ctx, share, snapID)
	}
	return &runtime.SnapshotUsageReport{Share: share}, nil
}

func (f *fakeSnapshotRuntime) CreateSnapshot(ctx context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error) {
	if f.createFn != nil {
		return f.createFn(ctx, share, opts)
	}
	return "", nil
}
func (f *fakeSnapshotRuntime) WaitForSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error) {
	if f.waitFn != nil {
		return f.waitFn(ctx, share, snapID)
	}
	return nil, nil
}
func (f *fakeSnapshotRuntime) RestoreSnapshot(ctx context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error) {
	if f.restoreFn != nil {
		return f.restoreFn(ctx, share, snapID, opts)
	}
	return "", nil
}
func (f *fakeSnapshotRuntime) GetSnapshot(ctx context.Context, share, snapID string) (*models.Snapshot, error) {
	if f.getFn != nil {
		return f.getFn(ctx, share, snapID)
	}
	return nil, nil
}
func (f *fakeSnapshotRuntime) ListSnapshots(ctx context.Context, share string) ([]*models.Snapshot, error) {
	if f.listFn != nil {
		return f.listFn(ctx, share)
	}
	return nil, nil
}
func (f *fakeSnapshotRuntime) DeleteSnapshot(ctx context.Context, share, snapID string) error {
	if f.deleteFn != nil {
		return f.deleteFn(ctx, share, snapID)
	}
	return nil
}

// newSnapshotRouter wraps the SnapshotHandler in a chi router that
// matches the production route layout, so chi.URLParam resolves
// correctly under httptest.
func newSnapshotRouter(h *SnapshotHandler) http.Handler {
	r := chi.NewRouter()
	r.Route("/api/v1/shares", func(r chi.Router) {
		r.Route("/{name}/snapshots", func(r chi.Router) {
			r.Post("/", h.Create)
			r.Get("/", h.List)
			r.Get("/usage", h.Usage)
			r.Get("/rehydrate/{jobID}", h.GetRehydrateJob)
			r.Get("/{id}", h.Get)
			r.Delete("/{id}", h.Remove)
			r.Post("/{id}/restore", h.Restore)
			r.Post("/{id}/rehydrate", h.Rehydrate)
		})
	})
	return r
}

// newSnapshotRouterWithGlobalTimeout mirrors the production router's global
// short request timeout (middleware.Timeout) applied to every route, so a
// test can assert the restore handler is not cancelled by it (issue #842).
func newSnapshotRouterWithGlobalTimeout(h *SnapshotHandler, d time.Duration) http.Handler {
	r := chi.NewRouter()
	r.Use(middleware.Timeout(d))
	r.Route("/api/v1/shares", func(r chi.Router) {
		r.Route("/{name}/snapshots", func(r chi.Router) {
			r.Post("/{id}/restore", h.Restore)
			r.Post("/{id}/rehydrate", h.Rehydrate)
		})
	})
	return r
}

// TestSnapshotHandler_Restore_DecoupledFromGlobalTimeout asserts that the
// router's short global request timeout does not abort a restore whose own
// budget is larger — the pre-restore safety-snapshot drain must be allowed to
// run past the global handler deadline AND the client must still receive the
// real 200 success response, not the middleware's 504 (issue #842).
func TestSnapshotHandler_Restore_DecoupledFromGlobalTimeout(t *testing.T) {
	gotCtxErr := make(chan error, 1)
	fake := &fakeSnapshotRuntime{
		restoreFn: func(ctx context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
			// Simulate work that outlasts the global 20ms timeout but
			// finishes well inside the restore budget.
			select {
			case <-ctx.Done():
				gotCtxErr <- ctx.Err()
				return "", ctx.Err()
			case <-time.After(80 * time.Millisecond):
				gotCtxErr <- nil
				return "safety-1", nil
			}
		},
	}
	// Generous restore budget; tiny global request timeout.
	h := NewSnapshotHandler(fake, 5*time.Second, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouterWithGlobalTimeout(h, 20*time.Millisecond).ServeHTTP(rr, req)

	select {
	case err := <-gotCtxErr:
		if err != nil {
			t.Fatalf("restore was cancelled by the global request timeout: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("restoreFn never returned")
	}

	// The decoupling is only real if the client gets the genuine success
	// response. The chi middleware.Timeout fires its deferred 504 WriteHeader
	// after the handler returns (the request context's deadline elapsed during
	// the 80ms restore); the handler must have already written 200 + body so
	// that stale 504 is a no-op. Asserting only the inner ctx error would pass
	// even if the response had been clobbered to 504.
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (middleware 504 must not clobber the success response): body=%s",
			rr.Code, rr.Body.String())
	}
	var got dto.RestoreSnapshotResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode body: %v (body=%s)", err, rr.Body.String())
	}
	if got.SafetySnapshotID != "safety-1" || got.SnapshotID != "snap-1" || got.Share != "/data" {
		t.Fatalf("body = %+v, want {snap-1, /data, safety-1}", got)
	}
}

func TestSnapshotHandler_Create_HappyPath(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		createFn: func(_ context.Context, share string, opts runtime.CreateSnapshotOpts) (string, error) {
			if share != "/data" {
				t.Fatalf("share = %q, want /data", share)
			}
			if !opts.NoVerify {
				t.Fatalf("opts.NoVerify = false, want true")
			}
			return "snap-1", nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	body := bytes.NewBufferString(`{"no_verify":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: body=%s", rr.Code, rr.Body.String())
	}
	if loc := rr.Header().Get("Location"); loc != "/api/v1/shares/%2Fdata/snapshots/snap-1" {
		t.Fatalf("Location = %q, want /api/v1/shares/%%2Fdata/snapshots/snap-1", loc)
	}
	var got dto.CreateSnapshotResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SnapshotID != "snap-1" || got.Share != "/data" {
		t.Fatalf("body = %+v, want {snap-1, /data}", got)
	}
}

// TestSnapshotHandler_Create_ForwardsName asserts the request body's name
// is forwarded to CreateSnapshotOpts.Name (was previously dropped).
func TestSnapshotHandler_Create_ForwardsName(t *testing.T) {
	var gotName string
	fake := &fakeSnapshotRuntime{
		createFn: func(_ context.Context, _ string, opts runtime.CreateSnapshotOpts) (string, error) {
			gotName = opts.Name
			return "snap-1", nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	body := bytes.NewBufferString(`{"name":"weekly"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/", body)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: body=%s", rr.Code, rr.Body.String())
	}
	if gotName != "weekly" {
		t.Fatalf("opts.Name = %q, want weekly", gotName)
	}
}

// TestSnapshotHandler_Get_IncludesName asserts the model's Name is surfaced
// in the wire DTO on the show endpoint.
func TestSnapshotHandler_Get_IncludesName(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, snapID string) (*models.Snapshot, error) {
			return &models.Snapshot{ID: snapID, Name: "weekly", ShareName: "/data", State: models.StateReady}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Name != "weekly" {
		t.Fatalf("name = %q, want weekly", got.Name)
	}
}

func TestSnapshotHandler_List_EmptyArrayNotNull(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		listFn: func(_ context.Context, _ string) ([]*models.Snapshot, error) {
			return []*models.Snapshot{}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if got := strings.TrimSpace(rr.Body.String()); got != "[]" {
		t.Fatalf("body = %q, want []", got)
	}
}

func TestSnapshotHandler_List_PopulatedRecord(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		listFn: func(_ context.Context, _ string) ([]*models.Snapshot, error) {
			return []*models.Snapshot{
				{ID: "snap-a", ShareName: "/data", State: models.StateReady, RemoteDurable: true},
				{ID: "snap-b", ShareName: "/data", State: models.StateCreating},
			}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got []dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}
	if got[0].ID != "snap-a" || !got[0].RemoteDurable {
		t.Fatalf("got[0] = %+v", got[0])
	}
}

func TestSnapshotHandler_Get_HappyPath(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, snapID string) (*models.Snapshot, error) {
			return &models.Snapshot{ID: snapID, ShareName: "/data", State: models.StateReady, RemoteDurable: true}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.ID != "snap-1" || got.State != models.StateReady {
		t.Fatalf("body = %+v", got)
	}
}

func TestSnapshotHandler_Get_ScheduledRoundTrips(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, snapID string) (*models.Snapshot, error) {
			return &models.Snapshot{ID: snapID, ShareName: "/data", State: models.StateReady, Scheduled: true}, nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	var got dto.Snapshot
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Scheduled {
		t.Fatalf("scheduled = false, want true (body=%+v)", got)
	}
}

func TestSnapshotHandler_Get_NotFound(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		getFn: func(_ context.Context, _, _ string) (*models.Snapshot, error) {
			return nil, models.ErrSnapshotNotFound
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/missing", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotHandler_Delete_NoContent(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		deleteFn: func(_ context.Context, _, _ string) error { return nil },
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/shares/data/snapshots/snap-1", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", rr.Code)
	}
}

func TestSnapshotHandler_Delete_NotFound(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		deleteFn: func(_ context.Context, _, _ string) error { return models.ErrSnapshotNotFound },
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/shares/data/snapshots/missing", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestSnapshotHandler_Restore_HappyPath(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		restoreFn: func(_ context.Context, share, snapID string, opts runtime.RestoreSnapshotOpts) (string, error) {
			if share != "/data" || snapID != "snap-1" || !opts.AllowNonDurable {
				t.Fatalf("restore args = (%q, %q, %+v)", share, snapID, opts)
			}
			return "safety-9", nil
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	body := bytes.NewBufferString(`{"allow_non_durable":true}`)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", body)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: body=%s", rr.Code, rr.Body.String())
	}
	var got dto.RestoreSnapshotResponse
	if err := json.NewDecoder(rr.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SafetySnapshotID != "safety-9" {
		t.Fatalf("safety_snapshot_id = %q, want safety-9 (sourced directly from runtime return)", got.SafetySnapshotID)
	}
	if got.SnapshotID != "snap-1" || got.Share != "/data" {
		t.Fatalf("body = %+v", got)
	}
}

func TestSnapshotHandler_Restore_ContextTimeout(t *testing.T) {
	gotCtxErr := make(chan error, 1)
	fake := &fakeSnapshotRuntime{
		restoreFn: func(ctx context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
			select {
			case <-ctx.Done():
				gotCtxErr <- ctx.Err()
				return "", ctx.Err()
			case <-time.After(50 * time.Millisecond):
				gotCtxErr <- nil
				return "should-not-happen", nil
			}
		},
	}
	// 1ms timeout — restoreFn's 50ms branch should never complete.
	h := NewSnapshotHandler(fake, 1*time.Millisecond, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	select {
	case err := <-gotCtxErr:
		if err == nil {
			t.Fatalf("restoreFn finished without ctx cancellation; got nil")
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("ctx err = %v, want DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatalf("restoreFn never returned within 1s")
	}
}

// TestMapSnapshotError_SentinelTable asserts every typed sentinel maps to
// the canonical HTTP status code listed in the plan's mapping table.
func TestMapSnapshotError_SentinelTable(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want int
	}{
		{"SnapshotNotFound", models.ErrSnapshotNotFound, http.StatusNotFound},
		{"LocalStoreUnsupported", models.ErrSnapshotLocalStoreUnsupported, http.StatusBadRequest},
		{"ShareNotFound", shares.ErrShareNotFound, http.StatusNotFound},
		{"ShareEnabled", models.ErrShareEnabled, http.StatusConflict},
		{"SnapshotNotDurable", models.ErrSnapshotNotDurable, http.StatusPreconditionFailed},
		{"RetryTargetNotFound", models.ErrSnapshotRetryTargetNotFound, http.StatusNotFound},
		{"RetryTargetNotFailed", models.ErrSnapshotRetryTargetNotFailed, http.StatusConflict},
		{"StateConflict", models.ErrSnapshotStateConflict, http.StatusConflict},
		{"InFlight", models.ErrSnapshotInFlight, http.StatusConflict},
		{"MarkerProtected", models.ErrSnapshotMarkerProtected, http.StatusConflict},
		{"Open", models.ErrSnapshotOpen, http.StatusConflict},
		{"DrainTimeout", models.ErrSnapshotDrainTimeout, http.StatusGatewayTimeout},
		{"MetadataDumpMissing", models.ErrSnapshotMetadataDumpMissing, http.StatusInternalServerError},
		{"MetadataStoreNotResetable", models.ErrMetadataStoreNotResetable, http.StatusInternalServerError},
		{"BackupFailed", models.ErrSnapshotBackupFailed, http.StatusInternalServerError},
		{"VerifyFailed", models.ErrSnapshotVerifyFailed, http.StatusInternalServerError},
		{"RestoreSafetySnapFailed", models.ErrRestoreSafetySnapFailed, http.StatusInternalServerError},
		{"RestoreAborted", models.ErrRestoreAborted, http.StatusInternalServerError},
		{"RestoreVerifyFailed", models.ErrRestoreVerifyFailed, http.StatusInternalServerError},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			// Wrap once to exercise errors.Is matching (the runtime
			// always wraps with %w + context).
			wrapped := fmt.Errorf("layer: %w", tc.err)
			if !mapSnapshotError(rr, wrapped) {
				t.Fatalf("mapSnapshotError returned false; want true")
			}
			if rr.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestMapSnapshotError_NilReturnsFalse(t *testing.T) {
	rr := httptest.NewRecorder()
	if mapSnapshotError(rr, nil) {
		t.Fatalf("mapSnapshotError(nil) = true, want false")
	}
	if rr.Code != 200 && rr.Code != 0 {
		t.Fatalf("status = %d, want untouched", rr.Code)
	}
}

func TestMapSnapshotError_UnknownReturnsFalse(t *testing.T) {
	rr := httptest.NewRecorder()
	if mapSnapshotError(rr, errors.New("some unknown error")) {
		t.Fatalf("mapSnapshotError(unknown) = true, want false")
	}
}

// TestSnapshotHandler_Restore_SanitizedMessage asserts the 500 body for
// a wrapped sentinel does not leak the underlying error message.
func TestSnapshotHandler_Restore_SanitizedMessage(t *testing.T) {
	leaky := errors.New("internal path /var/lib/dittofs/secret leaked here")
	fake := &fakeSnapshotRuntime{
		restoreFn: func(_ context.Context, _, _ string, _ runtime.RestoreSnapshotOpts) (string, error) {
			return "safety-1", fmt.Errorf("wrapped: %w: %v", models.ErrRestoreAborted, leaky)
		},
	}
	h := NewSnapshotHandler(fake, 30*time.Second, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/restore", nil)
	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "secret") || strings.Contains(rr.Body.String(), "/var/lib") {
		t.Fatalf("body leaks internal detail: %s", rr.Body.String())
	}
}

// TestSnapshotHandler_Usage_RouteIsReachable asserts that /snapshots/usage is
// matched as a literal segment and not swallowed by /{id}. Registering the
// routes in the wrong order would make every usage request look up a snapshot
// named "usage", which is a 404 an operator would read as "no snapshots".
func TestSnapshotHandler_Usage_RouteIsReachable(t *testing.T) {
	var gotShare, gotID string
	fake := &fakeSnapshotRuntime{
		usageFn: func(_ context.Context, share, snapID string) (*runtime.SnapshotUsageReport, error) {
			gotShare, gotID = share, snapID
			return &runtime.SnapshotUsageReport{
				Share:               share,
				TotalChunks:         7,
				SumOfSnapshotChunks: 20,
				Snapshots: []runtime.SnapshotUsage{
					{ID: "snap-a", Chunks: 7, Unique: 2, Shared: 5},
					{ID: "snap-b", Chunks: 7, Unique: 0, Shared: 7},
					{ID: "snap-c", Chunks: 6, Unique: 5, Shared: 1},
				},
			}, nil
		},
	}
	h := NewSnapshotHandler(fake, time.Second, nil)

	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/usage", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rr.Code, rr.Body.String())
	}
	// resolveShare normalizes to a leading slash, so the runtime sees "/data".
	if gotShare != "/data" {
		t.Errorf("handler saw share %q, want %q", gotShare, "/data")
	}
	if gotID != "" {
		t.Errorf("handler saw id %q, want empty: no ?id= was given", gotID)
	}

	var body dto.SnapshotUsageReport
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// The arithmetic must survive the wire: a report that drops the union or the
	// sum loses the whole point of the endpoint.
	if body.TotalChunks != 7 || body.SumOfSnapshotChunks != 20 {
		t.Errorf("total=%d sum=%d, want 7 and 20", body.TotalChunks, body.SumOfSnapshotChunks)
	}
	if len(body.Snapshots) != 3 {
		t.Fatalf("snapshots = %d, want 3", len(body.Snapshots))
	}
	if body.Snapshots[1].Unique != 0 || body.Snapshots[1].Shared != 7 {
		t.Errorf("snap-b unique=%d shared=%d, want 0 and 7: a snapshot sharing everything must say so",
			body.Snapshots[1].Unique, body.Snapshots[1].Shared)
	}
}

// TestSnapshotHandler_Usage_PassesTheIDFilter asserts the ?id= form reaches the
// runtime, since that is the whole difference between the share report and the
// per-snapshot one.
func TestSnapshotHandler_Usage_PassesTheIDFilter(t *testing.T) {
	var gotID string
	fake := &fakeSnapshotRuntime{
		usageFn: func(_ context.Context, share, snapID string) (*runtime.SnapshotUsageReport, error) {
			gotID = snapID
			return &runtime.SnapshotUsageReport{Share: share}, nil
		},
	}
	h := NewSnapshotHandler(fake, time.Second, nil)

	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/usage?id=snap-abc", nil))

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if gotID != "snap-abc" {
		t.Errorf("handler saw id %q, want %q", gotID, "snap-abc")
	}
}

// TestSnapshotHandler_Rehydrate_Returns202AndTheJob pins the asynchronous
// contract: a rehydrate that returned 200 would tell the caller the volume is
// ready when the job has only started, and a caller that then mounted it would
// find nothing there.
func TestSnapshotHandler_Rehydrate_Returns202AndTheJob(t *testing.T) {
	var gotSrc, gotDst string
	fake := &fakeSnapshotRuntime{
		rehydrateFn: func(_ context.Context, share, snapID, src, dst string) (*runtime.RehydrateJob, error) {
			gotSrc, gotDst = src, dst
			return &runtime.RehydrateJob{
				ID:           "job-42",
				State:        runtime.RehydrateStatePending,
				Share:        share,
				SnapshotID:   snapID,
				SourceVolume: src,
				Volume:       dst,
				StartedAt:    time.Now(),
			}, nil
		},
	}
	h := NewSnapshotHandler(fake, time.Second, nil)

	body := `{"source_volume":"boot","volume":"restored"}`
	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/rehydrate",
		strings.NewReader(body))
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202 (body %s)", rr.Code, rr.Body.String())
	}
	if gotSrc != "boot" || gotDst != "restored" {
		t.Errorf("handler saw src=%q dst=%q, want boot and restored", gotSrc, gotDst)
	}
	var job dto.RehydrateJob
	if err := json.Unmarshal(rr.Body.Bytes(), &job); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if job.ID != "job-42" || job.State != runtime.RehydrateStatePending {
		t.Errorf("job = %+v, want id job-42 state pending", job)
	}
}

// TestSnapshotHandler_Rehydrate_RefusesARequestWithoutBothNames pins that the
// two names are not silently defaulted. A snapshot holds every volume in the
// share, so there is no sensible default source, and defaulting the destination
// would create a volume the caller did not name.
func TestSnapshotHandler_Rehydrate_RefusesARequestWithoutBothNames(t *testing.T) {
	called := false
	fake := &fakeSnapshotRuntime{
		rehydrateFn: func(_ context.Context, _, _, _, _ string) (*runtime.RehydrateJob, error) {
			called = true
			return nil, errors.New("rehydrate: both the snapshot's volume name and the destination volume name are required")
		},
	}
	h := NewSnapshotHandler(fake, time.Second, nil)

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/shares/data/snapshots/snap-1/rehydrate",
		strings.NewReader(`{"volume":"restored"}`))
	newSnapshotRouter(h).ServeHTTP(rr, req)

	if !called {
		t.Fatal("the handler never reached the runtime, so the missing-name error path is untested")
	}
	if rr.Code == http.StatusAccepted {
		t.Fatalf("status = 202 for a request with no source volume")
	}
}

// TestSnapshotHandler_GetRehydrateJob_RefusesAnotherSharesJob pins the scope
// check. A job ID is a UUID, but an operator in one share must not be able to
// read another share's volume names through it.
func TestSnapshotHandler_GetRehydrateJob_RefusesAnotherSharesJob(t *testing.T) {
	fake := &fakeSnapshotRuntime{
		rehydrateJob: &runtime.RehydrateJob{ID: "job-9", Share: "/other", Volume: "secret"},
	}
	h := NewSnapshotHandler(fake, time.Second, nil)

	rr := httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/rehydrate/job-9", nil))
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: a job on another share must not be readable", rr.Code)
	}

	// The owning share gets it.
	fake.rehydrateJob = &runtime.RehydrateJob{ID: "job-9", Share: "/data", Volume: "restored"}
	rr = httptest.NewRecorder()
	newSnapshotRouter(h).ServeHTTP(rr,
		httptest.NewRequest(http.MethodGet, "/api/v1/shares/data/snapshots/rehydrate/job-9", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 for the owning share (body %s)", rr.Code, rr.Body.String())
	}
}
