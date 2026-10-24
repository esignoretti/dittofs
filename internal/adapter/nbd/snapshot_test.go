package nbd

import (
	"context"
	"errors"
	"testing"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// TestParseSnapshotExportName pins the name grammar, including the cases that
// must fall through to the production resolver.
//
// The fall-through cases matter more than the positive ones: a parser that
// claimed too much would shadow a real share called "snapshots" and break every
// volume under it, while a parser that claimed too little would report an
// unknown export for a snapshot the operator asked for by its correct name.
func TestParseSnapshotExportName(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		ok     bool
		share  string
		snapID string
		volume string
	}{
		{
			name: "plain snapshot export",
			in:   "/export/snapshots/abc-123/boot", ok: true,
			share: "/export", snapID: "abc-123", volume: "boot",
		},
		{
			// A leading slash is optional, folded the way every other entry point
			// folds a share name.
			name: "no leading slash",
			in:   "export/snapshots/abc-123/boot", ok: true,
			share: "/export", snapID: "abc-123", volume: "boot",
		},
		{
			// A share name may itself contain slashes, and the marker is what
			// identifies the shape, so the share is everything before it.
			name: "nested share name",
			in:   "/a/b/c/snapshots/abc-123/boot", ok: true,
			share: "/a/b/c", snapID: "abc-123", volume: "boot",
		},
		{
			name: "a real volume is not claimed",
			in:   "/export/boot", ok: false,
		},
		{
			// A volume legitimately named "snapshots" is <share>/volumes/snapshots,
			// which the marker position does not match.
			name: "a volume named snapshots",
			in:   "/export/volumes/snapshots", ok: false,
		},
		{
			name: "marker in the wrong position",
			in:   "/export/boot/snapshots/abc", ok: false,
		},
		{
			name: "missing volume",
			in:   "/export/snapshots/abc-123", ok: false,
		},
		{
			name: "empty snapshot id",
			in:   "/export/snapshots//boot", ok: false,
		},
		{
			name: "empty name",
			in:   "", ok: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			share, snapID, volume, ok := parseSnapshotExportName(tc.in)
			if ok != tc.ok {
				t.Fatalf("ok = %v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if share != tc.share || snapID != tc.snapID || volume != tc.volume {
				t.Fatalf("got (%q, %q, %q), want (%q, %q, %q)",
					share, snapID, volume, tc.share, tc.snapID, tc.volume)
			}
		})
	}
}

// TestResolve_SnapshotExportIsServedReadOnly boots a snapshot through the real
// exporter and reads it over the wire, asserting the two properties that make a
// snapshot export a snapshot: it carries the bytes as of the snapshot, and it
// refuses every write.
//
// The volume here is written through the production path first, so the manifest
// the snapshot captures is a real one — a snapshot of a volume with no chunks
// would read as zeros whether the wiring were right or wrong.
func TestResolve_SnapshotExportIsServedReadOnly(t *testing.T) {
	ctx := context.Background()
	rt, _, st := newIntegrationFixture(t)
	payloadID := integrationPayloadID(t, rt)

	// CreateSnapshot reads the metadata store's persisted config, which the NBD
	// fixture registers without a control-plane row (it needs no snapshots).
	// Persisting the row here keeps the fixture unchanged for the other 100-odd
	// tests that share it.
	if err := persistMetadataStoreRow(t, rt, "nbd-meta"); err != nil {
		t.Fatalf("persist metadata store row: %v", err)
	}

	// Populate the volume, then snapshot it, then change it. The export must
	// return the ORIGINAL bytes; a shadow that leaked production's journal would
	// return the new ones.
	const size = 64 << 10
	original := make([]byte, size)
	for i := range original {
		original[i] = byte(i%251) + 1
	}
	if _, err := st.WriteAt(ctx, payloadID, nil, original, 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}

	snapID, err := rt.CreateSnapshot(ctx, integrationShare, runtime.CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := rt.WaitForSnapshot(ctx, integrationShare, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	mutated := make([]byte, size)
	for i := range mutated {
		mutated[i] = 0xEE
	}
	if _, err := st.WriteAt(ctx, payloadID, nil, mutated, 0); err != nil {
		t.Fatalf("WriteAt after snapshot: %v", err)
	}

	// The snapshot export name.
	snapName := integrationShare + "/snapshots/" + snapID + "/" + integrationVolume
	snapExp, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(ctx, snapName)
	if err != nil {
		t.Fatalf("resolve snapshot export %q: %v", snapName, err)
	}
	if snapExp.closeFn == nil {
		t.Fatal("snapshot export has no closeFn: its open registration would leak")
	}
	t.Cleanup(func() { _ = snapExp.closeFn() })

	// (a) Read-only, and the flags the client is handed say so.
	if !snapExp.ReadOnly {
		t.Fatal("snapshot export is not read-only")
	}
	if snapExp.TransmissionFlags()&FlagReadOnly == 0 {
		t.Fatal("snapshot export does not advertise NBD_FLAG_READ_ONLY")
	}

	// (b) Its own metadata service and store, not production's.
	if snapExp.Meta == nil {
		t.Fatal("snapshot export has no metadata service of its own")
	}
	if snapExp.Meta == rt.GetMetadataService() {
		t.Fatal("snapshot export shares production's metadata service")
	}
	if snapExp.store == nil {
		t.Fatal("snapshot export has no block store of its own")
	}

	// (c) The bytes are the snapshot's.
	bs, err := (&Handler{}).storeFor(ctx, snapExp)
	if err != nil {
		t.Fatalf("resolve snapshot store: %v", err)
	}
	got := make([]byte, size)
	n, rerr := bs.ReadAt(ctx, string(snapExp.PayloadID), got, 0)
	if rerr != nil {
		t.Fatalf("read through snapshot store: %v", rerr)
	}
	if n != size {
		t.Fatalf("read %d bytes, want %d", n, size)
	}
	if string(got) != string(original) {
		t.Fatalf("snapshot export returned the wrong bytes: first difference at %d (got %#x, want %#x)",
			firstDiffIndex(original, got), got[firstDiffIndex(original, got)], original[firstDiffIndex(original, got)])
	}
}

// TestResolve_UnknownSnapshotIsUnavailable asserts a well-formed snapshot name
// that names nothing resolves to ErrSnapshotExportUnavailable, not to a
// successful open and not to ErrUnknownExport.
//
// The distinction is what lets the handshake log a cause: "the export does not
// exist" and "the export exists but its snapshot could not be opened" are
// different operator problems.
func TestResolve_UnknownSnapshotIsUnavailable(t *testing.T) {
	ctx := context.Background()
	rt, _, _ := newIntegrationFixture(t)

	_, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(ctx,
		integrationShare+"/snapshots/00000000-0000-0000-0000-000000000000/"+integrationVolume)
	if err == nil {
		t.Fatal("resolving a nonexistent snapshot succeeded")
	}
	if !errors.Is(err, ErrSnapshotExportUnavailable) {
		t.Fatalf("error = %v, want ErrSnapshotExportUnavailable", err)
	}
	if errors.Is(err, ErrUnknownExport) {
		t.Fatal("a well-formed snapshot name reported as an unknown export: the client named it correctly")
	}
}

// TestResolve_VolumeStillResolvesAlongsideSnapshots asserts the snapshot claim
// does not shadow the production resolver — the regression the fall-through is
// for.
func TestResolve_VolumeStillResolvesAlongsideSnapshots(t *testing.T) {
	ctx := context.Background()
	rt, _, _ := newIntegrationFixture(t)

	exp, err := NewRuntimeExporter(rt, integrationIdentity).Resolve(ctx, integrationExportName)
	if err != nil {
		t.Fatalf("resolve production export: %v", err)
	}
	if exp.Meta != nil {
		t.Fatal("a production export carries its own metadata service: it must use the handler's")
	}
	if exp.store != nil {
		t.Fatal("a production export carries its own store: it must resolve the share's")
	}
	if exp.closeFn != nil {
		t.Fatal("a production export has a closeFn: it acquires nothing, so there is nothing to release")
	}
	if exp.ReadOnly {
		t.Fatal("a production export of a read-write share is read-only")
	}
}

// firstDiffIndex returns the first index at which a and b differ, or 0 when they
// are equal.
func firstDiffIndex(a, b []byte) int {
	for i := range a {
		if i >= len(b) || a[i] != b[i] {
			return i
		}
	}
	return 0
}

// TestSnapshotExport_RefusesEveryWrite drives the write commands over the wire
// against a snapshot export and requires each to be refused with EPERM.
//
// The positive control is what makes this test mean anything. A refusal is easy
// to produce for the wrong reason — the fixture's volume is owned by uid 1000
// while the adapter's synthetic identity is 65534, so a write is denied by
// ordinary permission checks whether or not the export is read-only. The test
// therefore first drives the SAME handler identity against the PRODUCTION export
// and requires the write to succeed, proving the identity is authorized; only
// then does it assert that the snapshot export refuses the identical request.
// Without that, this test would have passed with the read-only check deleted.
func TestSnapshotExport_RefusesEveryWrite(t *testing.T) {
	ctx := context.Background()
	rt, _, st := newIntegrationFixture(t)
	payloadID := integrationPayloadID(t, rt)
	if err := persistMetadataStoreRow(t, rt, "nbd-meta"); err != nil {
		t.Fatalf("persist metadata store row: %v", err)
	}

	const size = 16 << 10
	if _, err := st.WriteAt(ctx, payloadID, nil, make([]byte, size), 0); err != nil {
		t.Fatalf("WriteAt: %v", err)
	}
	snapID, err := rt.CreateSnapshot(ctx, integrationShare, runtime.CreateSnapshotOpts{NoVerify: true})
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if _, werr := rt.WaitForSnapshot(ctx, integrationShare, snapID); werr != nil {
		t.Fatalf("WaitForSnapshot: %v", werr)
	}

	// The identity that owns the fixture's volume, so a write is authorized by
	// ownership rather than by being root: the point is to remove permission as
	// a reason for the refusal.
	owner := Identity{UID: 1000, GID: 1000}
	exporter := NewRuntimeExporter(rt, owner)

	prodExp, err := exporter.Resolve(ctx, integrationExportName)
	if err != nil {
		t.Fatalf("resolve production export: %v", err)
	}
	if prodExp.ReadOnly {
		t.Fatal("the fixture's share is read-only, so this test cannot distinguish a refusal cause")
	}

	// POSITIVE CONTROL: the same identity, the same command, a writable export.
	prodHandler := NewHandler(exporterReturning(prodExp), resolverFor(st), rt.GetMetadataService(),
		owner, testTimeouts(), func() bool { return false })
	prodClient, _ := serveHandler(t, prodHandler, integrationExportName)
	if rep := writeThroughClient(t, prodClient, 0, make([]byte, 4096), 0); rep.Error != 0 {
		t.Fatalf("control: WRITE to the production export = %d, want 0 — the identity is not "+
			"authorized, so a refusal below would prove nothing about read-only", rep.Error)
	}

	// The snapshot export, same identity, same commands.
	snapName := integrationShare + "/snapshots/" + snapID + "/" + integrationVolume
	snapExp, err := exporter.Resolve(ctx, snapName)
	if err != nil {
		t.Fatalf("resolve snapshot export: %v", err)
	}
	t.Cleanup(func() { _ = snapExp.closeFn() })
	if !snapExp.ReadOnly {
		t.Fatal("snapshot export is not read-only")
	}
	if snapExp.TransmissionFlags()&FlagReadOnly == 0 {
		t.Fatal("snapshot export does not advertise NBD_FLAG_READ_ONLY")
	}

	snapHandler := NewHandler(exporterReturning(snapExp), resolverFor(st), rt.GetMetadataService(),
		owner, testTimeouts(), func() bool { return false })
	client, _ := serveHandler(t, snapHandler, snapName)

	if rep := writeThroughClient(t, client, 0, make([]byte, 4096), 0); rep.Error != ErrEPERM {
		t.Fatalf("WRITE to a snapshot export = %d, want EPERM (%d)", rep.Error, ErrEPERM)
	}
	if rep := trimThroughClient(t, client, 0, 4096, 0); rep.Error != ErrEPERM {
		t.Fatalf("TRIM to a snapshot export = %d, want EPERM (%d)", rep.Error, ErrEPERM)
	}
	if rep := writeZeroesThroughClient(t, client, 0, 4096, 0); rep.Error != ErrEPERM {
		t.Fatalf("WRITE_ZEROES to a snapshot export = %d, want EPERM (%d)", rep.Error, ErrEPERM)
	}

	// A read still works, so the refusals are not a dead connection.
	rep, data := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 0x5a, Length: 4096,
	})
	if rep.Error != 0 {
		t.Fatalf("READ after refused writes = %d, want 0", rep.Error)
	}
	if len(data) != 4096 {
		t.Fatalf("READ returned %d bytes, want 4096", len(data))
	}
}

// exporterReturning is an Exporter that resolves every name to exp. The
// handshake still resolves by name, so a handler built on it serves the export
// the test chose for any name the client asks for — which is what lets a test
// drive a snapshot export without a runtime-backed exporter re-opening it.
func exporterReturning(exp *Export) Exporter { return fixedExporter{exp: exp} }

type fixedExporter struct{ exp *Export }

func (f fixedExporter) Resolve(context.Context, string) (*Export, error) { return f.exp, nil }
func (f fixedExporter) List(context.Context) ([]string, error)           { return nil, nil }
