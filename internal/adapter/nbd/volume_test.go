package nbd

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/internal/adapter/testruntime"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger/badgertest"
)

// ptr returns a pointer to v, for the metadata setters that take one to
// distinguish "unset" from a zero value.
func ptr[T any](v T) *T { return &v }

func TestRuntimeExporter_ResolvesVolumeFile(t *testing.T) {
	rt, bsID := testruntime.NewWithShareStore(t)
	store := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("nbd-meta", store); err != nil {
		t.Fatal(err)
	}
	const share = "/export"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "nbd-meta", BlockStoreID: bsID,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{UID: 1000, GID: 1000},
	}); err != nil {
		t.Fatal(err)
	}
	root, err := rt.GetRootHandle(share)
	if err != nil {
		t.Fatal(err)
	}
	metaSvc := rt.GetMetadataService()
	authCtx := &metadata.AuthContext{
		Context:  context.Background(),
		Identity: &metadata.Identity{UID: ptr(uint32(1000)), GID: ptr(uint32(1000))},
	}
	vols, _, err := metaSvc.CreateDirectory(authCtx, root, "volumes", &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		t.Fatal(err)
	}
	volsHandle, err := metadata.EncodeFileHandle(vols)
	if err != nil {
		t.Fatal(err)
	}
	created, _, err := metaSvc.CreateFile(authCtx, volsHandle, "boot", &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		t.Fatal(err)
	}
	createdHandle, err := metadata.EncodeFileHandle(created)
	if err != nil {
		t.Fatal(err)
	}
	// Creation zeroes the size (ApplyCreateDefaults), so the export size is set
	// explicitly afterwards — this is the "size is fixed at creation" step.
	if _, err := metaSvc.SetFileAttributes(authCtx, createdHandle, &metadata.SetAttrs{Size: ptr(uint64(1 << 30))}); err != nil {
		t.Fatal(err)
	}

	ex := NewRuntimeExporter(rt, Identity{UID: 65534, GID: 65534})
	got, err := ex.Resolve(context.Background(), "/export/boot")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.Size != 1<<30 {
		t.Fatalf("size = %d, want %d", got.Size, 1<<30)
	}
	if got.PayloadID == "" {
		t.Fatal("resolved export has an empty payload ID")
	}
	if got.ReadOnly {
		t.Fatal("share is read-write but the export reports read-only")
	}
	if got.Handle == nil {
		t.Fatal("resolved export has no file handle; the write path cannot call PrepareWrite without one")
	}
}
