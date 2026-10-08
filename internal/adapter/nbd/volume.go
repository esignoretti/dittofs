package nbd

import (
	"context"
	"errors"
	"strings"

	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// errNoRuntime reports an exporter that was built without a runtime. It is a
// programming error rather than a client error, and it is not ErrUnknownExport:
// nothing was looked up, so nothing was found missing.
var errNoRuntime = errors.New("nbd: exporter has no runtime")

// Identity is the configured synthetic identity the adapter operates as.
//
// NBD carries no user identity, so every metadata operation this adapter
// performs runs as this one identity. It is configured rather than fixed
// because the operator, not the protocol, decides who owns the volumes.
type Identity struct {
	UID uint32
	GID uint32
}

// RuntimeExporter resolves export names against the control plane.
//
// A volume is a metadata file at <share>/volumes/<volume>: its size is the
// file's size and its bytes are addressed by the file's payload ID. Nothing
// new is registered for a volume, so a volume is also readable as a plain file
// over the share's other protocols, which is an independent check on the bytes
// when a block write goes wrong.
type RuntimeExporter struct {
	rt *runtime.Runtime
	// id is the synthetic identity the resolution runs as. The handshake has
	// no client identity to take one from, and metadata.Service.Lookup
	// requires one, so the exporter is configured with it.
	id Identity
	// volumesDir is the directory inside a share that holds the volume files.
	volumesDir string
}

// NewRuntimeExporter returns an exporter over rt that resolves volumes as id.
func NewRuntimeExporter(rt *runtime.Runtime, id Identity) *RuntimeExporter {
	return &RuntimeExporter{rt: rt, id: id, volumesDir: "volumes"}
}

// Resolve returns the volume named by an export name of the form
// <share>/<volume>.
//
// Every way the name can fail to name a volume — no such share, no volumes
// directory, no such file — returns ErrUnknownExport. The name is
// client-supplied, and the server must not tell a client which part of its
// guess was wrong.
//
// decision: the export is resolved here, once, and never on the hot path. Its
// size is fixed at volume creation, so a later resolve could only re-read a
// value that cannot change, and holding the file's handle for the life of the
// connection is what lets the write path address the payload without
// re-resolving a name the client could have renamed out from under it.
func (r *RuntimeExporter) Resolve(ctx context.Context, name string) (*Export, error) {
	// An exporter with no runtime can resolve nothing. It is reported as an
	// unknown export rather than as an internal error because the handshake
	// has no reply for an internal error: the client can only be told the
	// export is not available.
	if r.rt == nil {
		return nil, ErrUnknownExport
	}

	// The volume is the last path element; everything before it names the
	// share. A share name may itself contain slashes, so the split is from the
	// right.
	sep := strings.LastIndexByte(name, '/')
	if sep <= 0 || sep == len(name)-1 {
		return nil, ErrUnknownExport
	}
	// Folded the way every other entry point folds a share name, so that a
	// client spelling the name without its leading slash still reaches the
	// share rather than being told it does not exist.
	share := metadata.NormalizeShareName(name[:sep])
	volume := name[sep+1:]

	root, err := r.rt.GetRootHandle(share)
	if err != nil {
		return nil, ErrUnknownExport
	}
	sh, err := r.rt.GetShare(share)
	if err != nil {
		return nil, ErrUnknownExport
	}

	// The handshake has no client identity, so the resolution runs as the
	// configured synthetic identity. ShareReadOnly is deliberately not set:
	// this resolves an export the operator already configured, and the
	// per-user read-only ceiling it applies belongs to the commands that
	// follow, not to whether the export may be named at all.
	uid, gid := r.id.UID, r.id.GID
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}

	metaSvc := r.rt.GetMetadataService()
	volsDir, err := metaSvc.Lookup(authCtx, root, r.volumesDir)
	if err != nil {
		return nil, ErrUnknownExport
	}
	volsDirHandle, err := metadata.EncodeFileHandle(volsDir)
	if err != nil {
		return nil, ErrUnknownExport
	}
	file, err := metaSvc.Lookup(authCtx, volsDirHandle, volume)
	if err != nil {
		return nil, ErrUnknownExport
	}
	// A volume is a regular file. A directory inside volumes/ is not one, and
	// advertising it would hand the client an export whose payload ID is empty
	// and whose every read fails.
	if file.Type != metadata.FileTypeRegular {
		return nil, ErrUnknownExport
	}

	// The file's own handle, not the share root's and not the volumes
	// directory's: the write path derives the payload ID it commits against
	// from this handle, so any other handle would address different bytes.
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, ErrUnknownExport
	}

	return &Export{
		Name:      name,
		Size:      file.Size,
		PayloadID: file.PayloadID,
		Handle:    handle,
		ReadOnly:  sh.ReadOnly,
	}, nil
}

// List returns every export name the server offers, one entry per volume file
// across every share.
//
// A share without a volumes directory offers no exports and is skipped rather
// than reported as a failure: an operator who has created no volume yet has an
// empty list, not a broken server.
func (r *RuntimeExporter) List(ctx context.Context) ([]string, error) {
	if r.rt == nil {
		return nil, errNoRuntime
	}

	uid, gid := r.id.UID, r.id.GID
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &uid, GID: &gid},
	}
	metaSvc := r.rt.GetMetadataService()

	var names []string
	for _, share := range r.rt.ListShares() {
		root, err := r.rt.GetRootHandle(share)
		if err != nil {
			return nil, err
		}
		volsDir, err := metaSvc.Lookup(authCtx, root, r.volumesDir)
		if err != nil {
			if metadata.IsNotFoundError(err) {
				continue
			}
			return nil, err
		}
		volsDirHandle, err := metadata.EncodeFileHandle(volsDir)
		if err != nil {
			return nil, err
		}
		// Paginated rather than read once: a listing that silently stops at the
		// first page would advertise a share as holding fewer volumes than it
		// does, and a client cannot tell a short list from a complete one.
		var cookie uint64
		for {
			page, err := metaSvc.ReadDirectory(authCtx, volsDirHandle, cookie, 0)
			if err != nil {
				return nil, err
			}
			for _, entry := range page.Entries {
				if entry.Attr == nil || entry.Attr.Type != metadata.FileTypeRegular {
					continue
				}
				names = append(names, share+"/"+entry.Name)
			}
			if !page.HasMore {
				break
			}
			cookie = page.NextCookie
		}
	}
	return names, nil
}
