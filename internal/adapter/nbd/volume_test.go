package nbd

import (
	"bytes"
	"context"
	"errors"
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
	// Enabled: true because AddShare copies the flag verbatim and nothing
	// defaults it; a share built without it is disabled, which the exporter now
	// refuses to resolve.
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "nbd-meta", BlockStoreID: bsID,
		Enabled:           true,
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
	// explicitly afterwards.
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
	// The handle must be the volume file's own, not the share root's and not
	// the volumes directory's: PrepareWrite/CommitWrite derive the payload ID
	// from it, so either of the other two would address different bytes.
	if !bytes.Equal(got.Handle, createdHandle) {
		t.Fatalf("export handle = %q, want the volume file's handle %q", got.Handle, createdHandle)
	}
}

// TestRuntimeExporter_DisabledShareIsRefusedAndUnlisted pins both halves of the
// disabled-share gate: the handshake is the only point at which NBD admits a
// client, so a share being quiesced must be neither resolvable nor advertised.
func TestRuntimeExporter_DisabledShareIsRefusedAndUnlisted(t *testing.T) {
	rt, bsID := testruntime.NewWithShareStore(t)
	store := badgertest.NewInMemory(t)
	if err := rt.RegisterMetadataStore("nbd-meta", store); err != nil {
		t.Fatal(err)
	}
	const share = "/export"
	if err := rt.AddShare(context.Background(), &runtime.ShareConfig{
		Name: share, MetadataStore: "nbd-meta", BlockStoreID: bsID,
		Enabled:           true,
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
	if _, _, err := metaSvc.CreateFile(authCtx, volsHandle, "boot", &metadata.FileAttr{Mode: 0o644}); err != nil {
		t.Fatal(err)
	}

	ex := NewRuntimeExporter(rt, Identity{UID: 65534, GID: 65534})
	if _, err := ex.Resolve(context.Background(), "/export/boot"); err != nil {
		t.Fatalf("Resolve on an enabled share: %v", err)
	}
	if names, err := ex.List(context.Background()); err != nil {
		t.Fatalf("List on an enabled share: %v", err)
	} else if len(names) != 1 || names[0] != "/export/boot" {
		t.Fatalf("List on an enabled share = %v, want [/export/boot]", names)
	}

	if err := rt.SetEnabledForTesting(share, false); err != nil {
		t.Fatal(err)
	}

	if _, err := ex.Resolve(context.Background(), "/export/boot"); !errors.Is(err, ErrUnknownExport) {
		t.Fatalf("Resolve on a disabled share: got %v, want ErrUnknownExport", err)
	}
	if names, err := ex.List(context.Background()); err != nil {
		t.Fatalf("List on a disabled share: %v", err)
	} else if len(names) != 0 {
		t.Fatalf("List on a disabled share = %v, want no exports", names)
	}
}

// TestExport_SizeIsReReadAfterATruncate pins that a live connection sees a
// truncate.
//
// A volume is an ordinary regular file in a share. The operator can shrink it
// over NFS or SMB while an NBD client is connected, and the adapter held the
// handshake-time size for the life of the connection — so a READ past the new
// EOF was served as zeroes and a WRITE past it was refused against a size that
// no longer existed.
//
// It asserts on the wire, because the reply is where the staleness shows.
func TestExport_SizeIsReReadAfterATruncate(t *testing.T) {
	f := newWriteVolumeFixture(t)
	if f.exp.Size != 1<<20 {
		t.Fatalf("fixture export size = %d, want %d", f.exp.Size, uint64(1<<20))
	}

	// Shrink the volume file behind the connection's back, as an operator would
	// over NFS or SMB. The identity must be the file's owner: SetFileAttributes
	// resolves ownership at pkg/metadata/file_modify.go:421 and only an owner or
	// root clears the gate at :560. newWriteVolumeFixture creates the volume as
	// integrationIdentity, so this is the owner. A size change takes the
	// onlySettingSize branch (:465), not the mode branch Task 3's test uses.
	smaller := uint64(4096)
	authCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: ptr(integrationIdentity.UID),
			GID: ptr(integrationIdentity.GID),
		},
	}
	if _, err := f.rt.GetMetadataService().SetFileAttributes(
		authCtx, f.exp.Handle, &metadata.SetAttrs{Size: &smaller},
	); err != nil {
		t.Fatalf("truncate: %v", err)
	}

	st := newStoreFake()
	// Keyed by the fixture's real payload ID, not testPayloadID: this export
	// comes from a real volume file, so its ID is whatever the metadata layer
	// assigned.
	st.data[string(f.exp.PayloadID)] = make([]byte, 1<<20)
	// Built with the REAL exporter, not f.newHandler: that helper wraps
	// exporterFor(f.exp), a fakeExporter pinned to one snapshot, so a refresh
	// would hand back the same stale export and the test would pass or fail for
	// reasons that have nothing to do with the refresh. The real exporter re-reads
	// the volume file's metadata, which is the behaviour under test.
	h := NewHandler(NewRuntimeExporter(f.rt, integrationIdentity), resolverFor(st),
		f.rt.GetMetadataService(), integrationIdentity, testTimeouts(), func() bool { return false })
	// 0 means "refresh before every command", which is what makes this test
	// deterministic instead of a race against the one-second window.
	h.sizeRefresh = 0
	client, _ := serveHandler(t, h, f.exp.Name)

	// Past the new EOF. Refused, not served as zeroes.
	if rep, _ := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 4, Offset: 4096, Length: 4096,
	}); rep.Error == 0 {
		t.Fatal("READ past the truncated size was served, so the connection still holds the handshake-time size")
	}
}
