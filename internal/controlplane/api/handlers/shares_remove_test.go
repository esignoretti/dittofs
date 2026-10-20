package handlers

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/marmos91/dittofs/internal/adapter/common"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// shareServer is one server's control plane and runtime over real stores in
// temp dirs (an on-disk Badger metadata store "md", a memory block store "mem"
// and a journal root), with the share handler in front.
type shareServer struct {
	t       *testing.T
	cp      store.Store
	rt      *runtime.Runtime
	h       *ShareHandler
	journal string
	blockID string
}

func newShareServer(t *testing.T) *shareServer {
	t.Helper()
	return newShareServerWith(t, nil)
}

// newShareServerWith is newShareServer with the metadata store passed through
// wrap before the runtime sees it, so a test can stand between the two.
func newShareServerWith(t *testing.T, wrap func(metadata.Store) metadata.Store) *shareServer {
	t.Helper()
	ctx := context.Background()

	cp, err := store.New(&store.Config{Type: store.DatabaseTypeSQLite, SQLite: store.SQLiteConfig{Path: ":memory:"}})
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	md, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("open badger: %v", err)
	}
	t.Cleanup(func() { _ = md.Close() })

	rt := runtime.New(cp)
	journal := t.TempDir()
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{JournalRoot: journal})
	t.Cleanup(func() {
		for _, name := range rt.ListShares() {
			_ = rt.RemoveShare(name)
		}
	})

	if _, err := cp.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "md", Type: "badger"}); err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	var served metadata.Store = md
	if wrap != nil {
		served = wrap(md)
	}
	if err := rt.RegisterMetadataStore("md", served); err != nil {
		t.Fatalf("RegisterMetadataStore: %v", err)
	}
	blockID, err := cp.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "mem", Type: "memory"})
	if err != nil {
		t.Fatalf("CreateBlockStore: %v", err)
	}
	return &shareServer{t: t, cp: cp, rt: rt, h: NewShareHandler(cp, rt), journal: journal, blockID: blockID}
}

func (s *shareServer) create(name string) {
	s.t.Helper()
	body := `{"name":"` + name + `","metadata_store_id":"md","block_store":"mem","default_permission":"read-write"}`
	w := httptest.NewRecorder()
	s.h.Create(w, httptest.NewRequest(http.MethodPost, "/api/v1/shares", strings.NewReader(body)))
	if w.Code != http.StatusCreated {
		s.t.Fatalf("create %s = %d, body=%s", name, w.Code, w.Body.String())
	}
	// Create answers 201 even when the runtime refuses the share.
	if _, err := s.rt.GetRootHandle(name); err != nil {
		s.t.Fatalf("share %s is not served: %v", name, err)
	}
}

func (s *shareServer) remove(name string) int {
	s.t.Helper()
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("name", strings.TrimPrefix(name, "/"))
	r := httptest.NewRequest(http.MethodDelete, "/api/v1/shares"+name, nil)
	w := httptest.NewRecorder()
	s.h.Remove(w, r.WithContext(context.WithValue(r.Context(), chi.RouteCtxKey, rctx)))
	return w.Code
}

// write creates dir/name under the share root and writes data to it.
func (s *shareServer) write(share, dir, name string, data []byte) metadata.FileHandle {
	s.t.Helper()
	ctx := context.Background()
	actx := metadata.NewSystemAuthContext(ctx)
	svc := s.rt.GetMetadataService()

	root, err := s.rt.GetRootHandle(share)
	if err != nil {
		s.t.Fatalf("GetRootHandle %s: %v", share, err)
	}
	d, _, err := svc.CreateDirectory(actx, root, dir, &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		s.t.Fatalf("CreateDirectory %s: %v", dir, err)
	}
	dh, err := metadata.EncodeFileHandle(d)
	if err != nil {
		s.t.Fatalf("EncodeFileHandle: %v", err)
	}
	f, _, err := svc.CreateFile(actx, dh, name, &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		s.t.Fatalf("CreateFile %s: %v", name, err)
	}
	fh, err := metadata.EncodeFileHandle(f)
	if err != nil {
		s.t.Fatalf("EncodeFileHandle: %v", err)
	}
	bs, err := s.rt.GetBlockStoreForHandle(ctx, fh)
	if err != nil {
		s.t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	if err := common.WriteToBlockStore(ctx, bs, f.PayloadID, data, 0); err != nil {
		s.t.Fatalf("WriteToBlockStore: %v", err)
	}
	if err := common.CommitBlockStore(ctx, bs, f.PayloadID); err != nil {
		s.t.Fatalf("CommitBlockStore: %v", err)
	}
	return fh
}

// read reads n bytes of the file the way an NFS READ does: the attributes
// through the read fast path, which caches them, then the bytes.
func (s *shareServer) read(h metadata.FileHandle, n int) string {
	s.t.Helper()
	ctx := context.Background()
	f, err := s.rt.GetMetadataService().GetFileForRead(ctx, h)
	if err != nil {
		s.t.Fatalf("GetFileForRead: %v", err)
	}
	bs, err := s.rt.GetBlockStoreForHandle(ctx, h)
	if err != nil {
		s.t.Fatalf("GetBlockStoreForHandle: %v", err)
	}
	res, err := common.ReadFromBlockStore(ctx, bs, f.PayloadID, 0, uint32(n))
	if err != nil {
		s.t.Fatalf("ReadFromBlockStore: %v", err)
	}
	defer res.Release()
	return string(bytes.Clone(res.Data))
}

// Removing a share deletes its metadata, journal and local blocks, so a share
// created again under its name starts empty, while a share beside it on the
// same metadata store keeps its files. A restart, unlike a removal, attaches
// the share's root again.
func TestShareRemove_RecreatedShareStartsEmpty(t *testing.T) {
	ctx := context.Background()
	s := newShareServer(t)
	s.create("/team")
	s.create("/team-archive")
	const oldText, keptText = "written to the removed share", "written to the share beside it"
	old := s.write("/team", "docs", "old.txt", []byte(oldText))
	kept := s.write("/team-archive", "docs", "kept.txt", []byte(keptText))
	if got := s.read(old, len(oldText)); got != oldText {
		t.Fatalf("read %q, want %q", got, oldText)
	}

	for _, name := range []string{"/team", "/team-archive"} {
		if err := s.rt.RemoveShare(name); err != nil {
			t.Fatalf("RemoveShare %s: %v", name, err)
		}
	}
	if err := runtime.LoadSharesFromStore(ctx, s.rt, s.cp); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}
	if got := s.read(old, len(oldText)); got != oldText {
		t.Fatalf("after a restart the share reads %q, want %q", got, oldText)
	}

	if code := s.remove("/team"); code != http.StatusNoContent {
		t.Fatalf("remove /team = %d, want 204", code)
	}
	if _, err := os.Stat(shares.ShareJournalDir(s.journal, "/team")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed share's journal directory is still there: %v", err)
	}

	s.create("/team")
	svc := s.rt.GetMetadataService()
	actx := metadata.NewSystemAuthContext(ctx)
	root, err := s.rt.GetRootHandle("/team")
	if err != nil {
		t.Fatalf("GetRootHandle: %v", err)
	}
	page, err := svc.ReadDirectory(actx, root, 0, 1<<16)
	if err != nil {
		t.Fatalf("ReadDirectory: %v", err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("the new share lists %d entries of the old one, first %q", len(page.Entries), page.Entries[0].Name)
	}
	if _, err := svc.Lookup(actx, root, "docs"); !metadata.IsNotFoundError(err) {
		t.Fatalf("the old share's directory resolves in the new share: %v", err)
	}
	if _, err := svc.GetFileForRead(ctx, old); !metadata.IsNotFoundError(err) {
		t.Fatalf("a handle into the removed share still resolves: %v", err)
	}

	if got := s.read(kept, len(keptText)); got != keptText {
		t.Fatalf("the other share reads %q, want %q", got, keptText)
	}
	if _, err := os.Stat(shares.ShareJournalDir(s.journal, "/team-archive")); err != nil {
		t.Fatalf("the other share's journal directory: %v", err)
	}
}

// A purge that fails keeps the share's row, so no share can take its name
// over what the purge left behind.
func TestShareRemove_FailedPurgeKeepsShare(t *testing.T) {
	ctx := context.Background()
	s := newShareServer(t)

	// A metadata store this server never loaded cannot be purged.
	unloaded, err := s.cp.CreateMetadataStore(ctx, &models.MetadataStoreConfig{Name: "unloaded", Type: "badger"})
	if err != nil {
		t.Fatalf("CreateMetadataStore: %v", err)
	}
	share := &models.Share{Name: "/stuck", MetadataStoreID: unloaded, BlockStoreID: s.blockID}
	if _, err := s.cp.CreateShare(ctx, share); err != nil {
		t.Fatalf("CreateShare: %v", err)
	}

	if code := s.remove("/stuck"); code != http.StatusInternalServerError {
		t.Fatalf("remove /stuck = %d, want 500", code)
	}
	if _, err := s.cp.GetShare(ctx, "/stuck"); err != nil {
		t.Fatalf("the share's row went with a failed purge: %v", err)
	}
}

// pausingStore holds the first DeleteShare it receives until resume is closed,
// so a test can act while a removal is between closing a share and purging it.
type pausingStore struct {
	metadata.Store
	held           atomic.Bool
	paused, resume chan struct{}
}

func (p *pausingStore) DeleteShare(ctx context.Context, name string) error {
	if p.held.CompareAndSwap(false, true) {
		close(p.paused)
		<-p.resume
	}
	return p.Store.DeleteShare(ctx, name)
}

// A removal still purging a share keeps its name: a second removal of it is
// refused rather than purging the same directories alongside the first, and
// the share can be created again once the first removal is done.
func TestShareRemove_ConcurrentRemovalIsRefused(t *testing.T) {
	ps := &pausingStore{paused: make(chan struct{}), resume: make(chan struct{})}
	s := newShareServerWith(t, func(md metadata.Store) metadata.Store {
		ps.Store = md
		return ps
	})
	s.create("/team")

	first := make(chan int, 1)
	go func() { first <- s.remove("/team") }()
	<-ps.paused
	resume := sync.OnceFunc(func() { close(ps.resume) })
	defer resume()

	if code := s.remove("/team"); code != http.StatusConflict {
		t.Fatalf("a second removal while the first is purging = %d, want 409", code)
	}
	resume()
	if code := <-first; code != http.StatusNoContent {
		t.Fatalf("the first removal = %d, want 204", code)
	}
	if _, err := os.Stat(shares.ShareJournalDir(s.journal, "/team")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the removed share's journal directory is still there: %v", err)
	}
	s.create("/team")
}

// Removing a share by its ID purges that share: the registry, the metadata
// store and the journal directory are keyed by name, so the ID must be resolved
// to the share's name before any of them is touched.
func TestShareRemove_ByIDPurgesTheShare(t *testing.T) {
	ctx := context.Background()
	s := newShareServer(t)
	s.create("/team")
	s.write("/team", "docs", "old.txt", []byte("old"))
	row, err := s.cp.GetShare(ctx, "/team")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}

	if code := s.remove("/" + row.ID); code != http.StatusNoContent {
		t.Fatalf("remove by ID = %d, want 204", code)
	}
	if _, err := s.rt.GetRootHandle("/team"); err == nil {
		t.Fatal("the share removed by ID is still served")
	}
	if _, err := os.Stat(shares.ShareJournalDir(s.journal, "/team")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the share's journal directory is still there: %v", err)
	}
	md, err := s.rt.GetMetadataStore("md")
	if err != nil {
		t.Fatalf("GetMetadataStore: %v", err)
	}
	names, err := md.ListShares(ctx)
	if err != nil {
		t.Fatalf("ListShares: %v", err)
	}
	if slices.Contains(names, "/team") {
		t.Fatalf("the metadata store still holds the share removed by ID: %v", names)
	}
}

// A purge whose request is cancelled before it can look up the share's
// metadata store deletes nothing: the store is not unknown, only unreached,
// so its records and the share's journal both stay for a repeated removal.
func TestShareRemove_CancelledPurgeKeepsJournal(t *testing.T) {
	ctx := context.Background()
	s := newShareServer(t)
	s.create("/team")
	h := s.write("/team", "docs", "a.txt", []byte("kept"))
	if err := s.rt.RemoveShare("/team"); err != nil {
		t.Fatalf("RemoveShare: %v", err)
	}

	row, err := s.cp.GetShare(ctx, "/team")
	if err != nil {
		t.Fatalf("GetShare: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err := s.rt.PurgeShare(cancelled, row); err == nil {
		t.Fatal("a purge with a cancelled request succeeded")
	}
	if _, err := os.Stat(shares.ShareJournalDir(s.journal, "/team")); err != nil {
		t.Fatalf("a cancelled purge removed the journal directory: %v", err)
	}
	if err := runtime.LoadSharesFromStore(ctx, s.rt, s.cp); err != nil {
		t.Fatalf("LoadSharesFromStore: %v", err)
	}
	if got := s.read(h, 4); got != "kept" {
		t.Fatalf("after a cancelled purge the share reads %q, want %q", got, "kept")
	}
}

// pause holds the first call through hit after arm until resume is closed.
type pause struct {
	armed          atomic.Bool
	paused, resume chan struct{}
}

func newPause() *pause { return &pause{paused: make(chan struct{}), resume: make(chan struct{})} }

func (p *pause) hit() {
	if p.armed.CompareAndSwap(true, false) {
		close(p.paused)
		<-p.resume
	}
}

// pausingControlPlane holds the share handler between its control-plane calls:
// after a share lookup by name or ID, and before or after a share row is
// deleted.
type pausingControlPlane struct {
	store.Store
	afterGetShare, beforeDeleteShare, afterDeleteShare *pause
}

func (p *pausingControlPlane) GetShare(ctx context.Context, name string) (*models.Share, error) {
	share, err := p.Store.GetShare(ctx, name)
	p.afterGetShare.hit()
	return share, err
}

func (p *pausingControlPlane) DeleteShare(ctx context.Context, name string) error {
	p.beforeDeleteShare.hit()
	err := p.Store.DeleteShare(ctx, name)
	p.afterDeleteShare.hit()
	return err
}

func (s *shareServer) pauseControlPlane() *pausingControlPlane {
	p := &pausingControlPlane{Store: s.cp, afterGetShare: newPause(), beforeDeleteShare: newPause(), afterDeleteShare: newPause()}
	s.h = NewShareHandler(p, s.rt)
	return p
}

// A removal that found its share before another removal finished and the name
// was taken again leaves the share now under the name alone: what it found is
// gone, and the share it would reach by the name is another one.
func TestShareRemove_StaleLookupLeavesTheNewShare(t *testing.T) {
	s := newShareServer(t)
	cp := s.pauseControlPlane()
	s.create("/team")

	cp.afterGetShare.armed.Store(true)
	stale := make(chan int, 1)
	go func() { stale <- s.remove("/team") }()
	<-cp.afterGetShare.paused
	resume := sync.OnceFunc(func() { close(cp.afterGetShare.resume) })
	defer resume()

	if code := s.remove("/team"); code != http.StatusNoContent {
		t.Fatalf("remove /team = %d, want 204", code)
	}
	s.create("/team")
	const text = "written to the new share"
	f := s.write("/team", "docs", "new.txt", []byte(text))

	resume()
	if code := <-stale; code != http.StatusNotFound {
		t.Fatalf("the removal that found the old share = %d, want 404", code)
	}
	if _, err := s.rt.GetRootHandle("/team"); err != nil {
		t.Fatalf("the new share is no longer served: %v", err)
	}
	if got := s.read(f, len(text)); got != text {
		t.Fatalf("the new share reads %q, want %q", got, text)
	}
}

// A removal keeps its share's name until the row is deleted, so a second
// removal arriving after the purge is refused rather than claiming the name
// for a purge of its own, and a share created once the first removal is done
// is served instead of being refused by that second removal's claim.
func TestShareRemove_NameHeldUntilRowDeleted(t *testing.T) {
	ps := &pausingStore{paused: make(chan struct{}), resume: make(chan struct{})}
	ps.held.Store(true)
	s := newShareServerWith(t, func(md metadata.Store) metadata.Store {
		ps.Store = md
		return ps
	})
	cp := s.pauseControlPlane()
	s.create("/team")

	cp.beforeDeleteShare.armed.Store(true)
	first := make(chan int, 1)
	go func() { first <- s.remove("/team") }()
	<-cp.beforeDeleteShare.paused
	resumeFirst := sync.OnceFunc(func() { close(cp.beforeDeleteShare.resume) })
	defer resumeFirst()

	// Arm the purge pause for the second removal, should it get as far.
	ps.held.Store(false)
	second := make(chan int, 1)
	go func() { second <- s.remove("/team") }()
	resumeSecond := sync.OnceFunc(func() { close(ps.resume) })
	defer resumeSecond()
	select {
	case code := <-second:
		if code != http.StatusConflict {
			t.Fatalf("a second removal while the first deletes the row = %d, want 409", code)
		}
	case <-ps.paused:
		// The second removal holds the name and is purging.
	}

	resumeFirst()
	if code := <-first; code != http.StatusNoContent {
		t.Fatalf("the first removal = %d, want 204", code)
	}
	s.create("/team")
}

// A share created while a removal of the name has deleted its row but not yet
// let the name go is refused before its row is inserted, rather than stored
// and then refused by the runtime, which would leave it unserved.
func TestShareCreate_RefusedWhileRemovalHoldsName(t *testing.T) {
	s := newShareServer(t)
	cp := s.pauseControlPlane()
	s.create("/team")

	cp.afterDeleteShare.armed.Store(true)
	removed := make(chan int, 1)
	go func() { removed <- s.remove("/team") }()
	<-cp.afterDeleteShare.paused
	resume := sync.OnceFunc(func() { close(cp.afterDeleteShare.resume) })
	defer resume()

	body := `{"name":"/team","metadata_store_id":"md","block_store":"mem","default_permission":"read-write"}`
	w := httptest.NewRecorder()
	s.h.Create(w, httptest.NewRequest(http.MethodPost, "/api/v1/shares", strings.NewReader(body)))
	if w.Code != http.StatusConflict {
		t.Fatalf("create /team while its removal holds the name = %d, want 409", w.Code)
	}
	if _, err := s.cp.GetShare(context.Background(), "/team"); !errors.Is(err, models.ErrShareNotFound) {
		t.Fatalf("a refused create left a row: %v", err)
	}

	resume()
	if code := <-removed; code != http.StatusNoContent {
		t.Fatalf("remove /team = %d, want 204", code)
	}
	s.create("/team")
}
