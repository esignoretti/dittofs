package nbd

import (
	"context"
	"testing"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// TestHandleRead_ChecksReadPermission pins the authorization gap: the write path
// funnels through PrepareWrite, which calls checkWritePermission, but the read
// path built no auth context at all — so a volume file whose mode forbids reads
// was served in full to every client.
//
// NBD carries no identity, so the adapter's single synthetic identity either may
// read or may not; the check is still required, because that identity's group and
// the file's mode are the operator's configuration and honouring it is not
// optional. The alternative — serve every READ regardless of mode — means a
// read-only share and a mode-0000 volume are both fiction.
//
// The assertion is on the wire: a denied READ answers NBD_EPERM with no payload.
// That is what a client acts on, and it is the same answer the write path already
// gives (mapError maps merrs.ErrPermissionDenied and merrs.ErrAccessDenied to
// ErrEPERM at connection.go:402).
//
// It needs the real-metadata fixture: newTestHandler passes nil for meta, so a
// handler built that way has no permission check to exercise.
//
// The chmod runs as integrationIdentity because SetFileAttributes gates a mode
// change on ownership (pkg/metadata/file_modify.go:421 isOwner, :564
// "case isOwner || isRoot:"). Any other identity fails the chmod with a
// permission error and this test would then fail for the wrong reason.
func TestHandleRead_ChecksReadPermission(t *testing.T) {
	f := newWriteVolumeFixture(t)
	metaSvc := f.rt.GetMetadataService()

	// There is no f.authCtx(): the fixture builds its AuthContext inline (see
	// newIntegrationFixtureAsSized, integration_test.go:131), so build one here
	// the same way rather than adding a helper for one test.
	authCtx := &metadata.AuthContext{
		Context: context.Background(),
		Identity: &metadata.Identity{
			UID: ptr(integrationIdentity.UID),
			GID: ptr(integrationIdentity.GID),
		},
	}
	// 0o200 leaves write and removes read, for the owner.
	mode := uint32(0o200)
	if _, err := metaSvc.SetFileAttributes(authCtx, f.exp.Handle, &metadata.SetAttrs{Mode: &mode}); err != nil {
		t.Fatalf("chmod: %v", err)
	}

	st := newStoreFake()
	// Keyed by the fixture's real payload ID, not testPayloadID: this export comes
	// from a real volume file, so its ID is whatever the metadata layer assigned.
	st.data[string(f.exp.PayloadID)] = make([]byte, 4096)
	h := f.newHandler(t, st)
	client, _ := serveHandler(t, h, f.exp.Name)

	if rep, _ := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 3, Offset: 0, Length: 4096,
	}); rep.Error != ErrEPERM {
		t.Fatalf("READ on a file with no read bit reply error = %d, want ErrEPERM (%d)", rep.Error, uint32(ErrEPERM))
	}
}

// TestHandleRead_ShortReadIsEIO pins that a store which fills only part of the
// buffer is reported as a fault. The range was already checked against exp.Size,
// so the bytes exist; replying success with a partly-filled buffer hands the
// client zeros in place of its data and reports success, which is the one failure
// mode a filesystem cannot detect — ext4 checksums cover metadata, not file data.
//
// It does not need the metadata fixture: it is a store-fault test, and
// newTestHandler's nil meta is fine because the permission check is guarded.
//
// The fake embeds storeFake by pointer and delegates, so the fake's own data map
// and counters stay live — embedding the value would give this store its own
// zero-valued map and the ReadAt below would never be reached for real data.
func TestHandleRead_ShortReadIsEIO(t *testing.T) {
	st := &shortReadStore{storeFake: newStoreFake()}
	st.data[testPayloadID] = make([]byte, 4096)
	h := newTestHandler(st, testExport(8192), testTimeouts())
	client, _ := serveHandler(t, h, testExportName)

	if rep, _ := readReplyAndPayload(t, client, &Request{
		Type: CmdRead, Cookie: 5, Offset: 0, Length: 4096,
	}); rep.Error != ErrEIO {
		t.Fatalf("short READ reply error = %d, want ErrEIO (%d)", rep.Error, uint32(ErrEIO))
	}
}

// shortReadStore reports one byte fewer than it was asked for, with a nil error.
// A short count and a nil error is legal for io.Reader and a bug for a block
// store, which is exactly the case the handler must not treat as success.
type shortReadStore struct {
	*storeFake
}

func (s *shortReadStore) ReadAt(ctx context.Context, id string, buf []byte, off uint64) (int, error) {
	return len(buf) - 1, nil
}
