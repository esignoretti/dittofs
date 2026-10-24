package nbd

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/marmos91/dittofs/pkg/metadata"
)

// snapshotsMarker is the second path segment that marks an export name as
// naming a snapshot rather than a volume: <share>/snapshots/<id>/<volume>.
//
// It cannot collide with a real volume, because the production exporter reads
// only <share>/volumes/<volume> and a volume name cannot contain a slash.
const snapshotsMarker = "snapshots"

// ErrSnapshotExportUnavailable reports a snapshot export name that is
// well-formed but cannot be served. It is deliberately not ErrUnknownExport:
// the client guessed the name correctly, so telling it the export does not
// exist would be a lie it cannot act on. The underlying reason is in the error
// chain for the operator's log.
var ErrSnapshotExportUnavailable = errors.New("nbd: snapshot export unavailable")

// parseSnapshotExportName splits <share>/snapshots/<id>/<volume>.
//
// The share is everything before the marker rather than before the last slash,
// because a share name may itself contain slashes and the marker is what
// identifies the shape. ok is false for any name that is not this shape, which
// is how every other export name reaches the production resolver unchanged.
func parseSnapshotExportName(name string) (share, snapID, volume string, ok bool) {
	trimmed := strings.TrimPrefix(name, "/")
	parts := strings.Split(trimmed, "/")
	// share (at least one segment) + "snapshots" + id + volume.
	if len(parts) < 4 {
		return "", "", "", false
	}
	marker := parts[len(parts)-3]
	if marker != snapshotsMarker {
		return "", "", "", false
	}
	share = "/" + strings.Join(parts[:len(parts)-3], "/")
	snapID = parts[len(parts)-2]
	volume = parts[len(parts)-1]
	if snapID == "" || volume == "" {
		return "", "", "", false
	}
	return metadata.NormalizeShareName(share), snapID, volume, true
}

// resolveSnapshot resolves an export name that names a snapshot.
//
// It opens the snapshot through the runtime, which is where the replay, the
// private metadata service and the private block store live, and wraps the
// result in an Export the wire layer can serve. The export is read-only by
// construction: a snapshot is a claim on chunks the live volume still shares,
// so a write through it would either be lost or corrupt the volume it came
// from.
//
// The caller owns the returned export and must call its closeFn when the
// connection ends — that is what releases the open registration and lets the
// snapshot be deleted again.
func (r *RuntimeExporter) resolveSnapshot(ctx context.Context, name, share, snapID, volume string) (*Export, error) {
	if r.rt == nil {
		return nil, ErrUnknownExport
	}
	uid, gid := r.id.UID, r.id.GID
	open, err := r.rt.OpenSnapshotAsExport(ctx, share, snapID, volume,
		&metadata.Identity{UID: &uid, GID: &gid})
	if err != nil {
		// Every refusal here is an operator-visible condition — a snapshot that
		// is not ready, a share with no remote, a replay that failed — and none
		// of them is "you named the wrong export". Wrapped so the handshake can
		// log the cause while the client still gets a plain refusal.
		return nil, fmt.Errorf("%w: %s/%s/%s: %w", ErrSnapshotExportUnavailable, share, snapID, volume, err)
	}

	return &Export{
		Name:      name,
		Size:      open.Size,
		PayloadID: open.PayloadID,
		Handle:    open.RootHandle,
		// Always: a snapshot is served from chunks the live volume still owns,
		// so it is readable and never writable. Promotion is a rehydrate into a
		// new volume, not a mode change on this export.
		ReadOnly: true,
		Meta:     open.Meta,
		store:    open.BlockStore(),
		closeFn:  open.Close,
	}, nil
}
