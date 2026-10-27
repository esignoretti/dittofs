package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block/engine"
	"github.com/marmos91/dittofs/pkg/block/journal"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/metadata"
	"github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// shadowJournalCapDivisor sizes a snapshot's hydration cache as a fraction of
// the share's journal ceiling. A shadow journal holds only bytes that are
// recoverable from the remote — it is a cache, not a store — so inheriting a
// share's multi-gigabyte journal ceiling would reserve disk for a boot that
// reads a few hundred megabytes. A sixteenth leaves room for a full boot with
// read-ahead while staying far below production's allocation.
const shadowJournalCapDivisor = 16

// shadowMetaDirName and shadowJournalDirName are the two directories an open
// snapshot creates inside its snapshot directory. Both are removed on close.
//
// Each open gets its own pair, suffixed with the open's ID, and that is
// load-bearing rather than tidy: replaySnapshotMetadata starts with
// os.RemoveAll(dir), so a shared path means a second client connecting to the
// same snapshot deletes the first client's Badger directory while it is being
// read. The damage is silent — the second open succeeds (it removed the lock
// file with everything else), reads still work out of Badger's memtable, and the
// only symptom is that Close never returns, because Badger's shutdown waits for
// a memtable flush retrying "no such file or directory" forever. Two VMs booting
// one golden snapshot is the documented use case, so a shared path is a bug and
// not a saving.
const (
	shadowMetaDirName    = "shadow-meta"
	shadowJournalDirName = "shadow-journal"
)

// OpenSnapshotExport is one snapshot served as a read-only block device.
//
// It owns a private metadata service, a private block store and an open-snapshot
// registration. Close releases all three and is the only way to release them, so
// a caller that opens must always close — a leaked OpenSnapshotExport keeps the
// snapshot undeletable for the process lifetime.
type OpenSnapshotExport struct {
	// ShareName is the PRODUCTION share the snapshot belongs to. The shadow
	// metadata service registers the replayed store under this name, not under
	// the snapshot ID: the dump carries File.ShareName = the production name
	// inside every record, and a handle is "<shareName>:<uuid>" with a 27-byte
	// share-name limit that a 36-character snapshot ID cannot satisfy.
	ShareName string
	// SnapshotID is the snapshot this export serves.
	SnapshotID string
	// RootHandle is the volume's file handle, minted from the REPLAYED store's
	// own records.
	RootHandle metadata.FileHandle
	// PayloadID is the volume's payload, taken from the replayed store.
	PayloadID metadata.PayloadID
	// Size is the volume's size in bytes, taken from the replayed store.
	Size uint64
	// Meta is the private metadata service. Every read goes through it, which is
	// what makes the bytes the snapshot's.
	Meta *metadata.Service

	release func()
	bs      *engine.Store
	// metaStore is the replayed store behind Meta. Held so Close can close it:
	// metadata.Service has no Close of its own, it only holds stores, and
	// leaving the Badger handle open would keep the shadow directory locked.
	metaStore metadata.Store
	// openID makes this open's shadow directories unique. The directories are
	// per-open, not per-snapshot: see the constants' comment for the bug that
	// made a shared path wrong.
	openID  string
	metaDir string
	jrnlDir string

	// closeOnce guards every field Close reads and clears. Close is reachable
	// from two places that do not coordinate — the connection's deferred release
	// and the caller that opened the export — so "idempotent" is not enough:
	// without this both run, both see closed == false, and they race on the
	// fields they clear. The race detector reports it, and in a real run it would
	// double-close the block store.
	closeOnce sync.Once
	closeErr  error
}

// OpenSnapshotAsExport opens a snapshot as a read-only block-device export.
//
// volume is the volume name inside the share (for a share "/export" and a
// volume "boot", it is "boot"). The returned export is independent of
// production: its own metadata service, its own block store, its own journal
// directory, sharing only the immutable remote chunks.
//
// id is the identity the volume is resolved under, and it must be the identity
// the caller will serve reads as — the production exporter resolves under the
// adapter's synthetic identity for the same reason. nil means root, which is
// what a test wants and what a server configured with a root identity gets.
//
// The snapshot stays registered as open until Close, so a concurrent delete is
// refused rather than wiping the manifest mid-boot.
func (r *Runtime) OpenSnapshotAsExport(ctx context.Context, shareName, snapID, volume string, id *metadata.Identity) (*OpenSnapshotExport, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("runtime: nil store")
	}
	// Preconditions first, all of them cheap. Everything below allocates, and an
	// unwind that has to undo a replay is much harder to get right than one that
	// never started.
	snap, err := r.store.GetSnapshot(ctx, shareName, snapID)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %q: %w", snapID, err)
	}
	if snap.State != models.StateReady {
		return nil, fmt.Errorf("open snapshot %q: state %q cannot be exported: %w",
			snapID, snap.State, models.ErrSnapshotStateConflict)
	}
	// Degraded is a flag on a READY row, not a state: the snapshot completed
	// without capturing every metadata row, so its manifest is short by
	// DegradedEntries entries and a read touching one of them would return zeros
	// instead of data. Refusing is the whole point of recording the flag — a boot
	// that silently reads wrong bytes is worse than one that does not start.
	if snap.Degraded {
		return nil, fmt.Errorf("open snapshot %q: snapshot is degraded (%d entries not captured): %w",
			snapID, snap.DegradedEntries, models.ErrSnapshotStateConflict)
	}

	share, err := r.sharesSvc.GetShare(shareName)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %q: %w", snapID, err)
	}
	// A share with no block store, or one with no remote, has nothing to hydrate
	// from: the shadow's journal is empty by construction, so every read would
	// return zeros for data that exists only in the snapshot's manifest with no
	// remote copy behind it. Refusing here costs nothing to unwind.
	if err := checkShareCanHydrate(snapID, shareName, share); err != nil {
		return nil, err
	}

	localStoreDir, err := r.sharesSvc.LocalStoreDir(shareName)
	if err != nil {
		return nil, err
	}
	if localStoreDir == "" {
		return nil, fmt.Errorf("open snapshot %q: share %q has no on-disk data directory: %w",
			snapID, shareName, models.ErrSnapshotLocalStoreUnsupported)
	}

	// Register as open BEFORE reading anything, so a delete that arrives during
	// the replay is refused instead of removing the dump we are about to read.
	release := r.OpenSnapshot(shareName, snapID)

	// A fresh ID per open, so two clients on one snapshot get two independent
	// shadow stores instead of racing over one directory pair.
	openID := uuid.NewString()

	exp := &OpenSnapshotExport{
		ShareName:  shareName,
		SnapshotID: snapID,
		openID:     openID,
		release:    release,
		metaDir:    filepath.Join(snap.SnapshotDir(localStoreDir), shadowMetaDirName+"-"+openID),
		jrnlDir:    filepath.Join(snap.SnapshotDir(localStoreDir), shadowJournalDirName+"-"+openID),
	}
	// One teardown for every acquired resource, so the failure paths below can
	// simply return and the ordered release happens once.
	ok := false
	defer func() {
		if !ok {
			_ = exp.Close()
		}
	}()

	metaStore, err := r.replaySnapshotMetadata(ctx, snap, localStoreDir, exp.metaDir)
	if err != nil {
		return nil, err
	}

	// A private metadata service holding ONLY the replayed store, registered
	// under the production share name. Nothing else is in it, so there is nothing
	// to collide with, and the dump's File.ShareName already matches the key.
	svc := metadata.New()
	if err := svc.RegisterStoreForShare(shareName, metaStore); err != nil {
		_ = metaStore.Close()
		return nil, fmt.Errorf("open snapshot %q: register replayed store: %w", snapID, err)
	}
	exp.Meta = svc
	exp.metaStore = metaStore

	// Resolve the volume from the REPLAYED store, not from production: the
	// handle must belong to the shadow, and minting it against the replayed file
	// is what makes it so.
	file, err := r.resolveVolumeInShadow(ctx, svc, shareName, volume, id)
	if err != nil {
		return nil, err
	}
	// Encoded against the REPLAYED file, not copied from production. Same UUID and
	// same share name, so production's handle would work by accident — and would
	// then point at production's service if the two were ever swapped.
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %q: encode volume handle: %w", snapID, err)
	}
	exp.RootHandle = handle
	exp.PayloadID = file.PayloadID
	exp.Size = file.Size

	bs, err := r.openShadowBlockStore(shareName, share.BlockStore, metaStore, exp.jrnlDir)
	if err != nil {
		return nil, err
	}
	exp.bs = bs

	ok = true
	return exp, nil
}

// checkShareCanHydrate refuses a share whose block store cannot supply bytes to a
// shadow journal.
//
// A shadow store starts with an empty journal, so every read is a hydrate: the
// bytes must come from the remote. A share with no remote (or no block store at
// all) would hydrate nothing, and the read path returns zeros with a nil error —
// a snapshot that boots as a blank disk with nothing in the log. That is the one
// failure mode worth a precondition, because it is silent.
//
// Split out as a function so the guard is testable: no share can be constructed
// without a block store, and every store type wires a remote, so the branch is
// unreachable through OpenSnapshotAsExport. See the design's note on this test.
func checkShareCanHydrate(snapID, shareName string, share *shares.Share) error {
	if share == nil || share.BlockStore == nil {
		return fmt.Errorf("open snapshot %q: share %q has no block store: %w",
			snapID, shareName, models.ErrSnapshotLocalStoreUnsupported)
	}
	if share.BlockStore.RemoteStore() == nil {
		return fmt.Errorf("open snapshot %q: share %q has no remote store, so there is nothing to hydrate from: %w",
			snapID, shareName, models.ErrSnapshotLocalStoreUnsupported)
	}
	return nil
}

// replaySnapshotMetadata creates a fresh metadata store at dir and replays the
// snapshot's dump into it.
//
// dir is created fresh on every attempt and removed on failure. RestoreSnapshot
// refuses a non-empty destination (ErrRestoreDestinationNotEmpty), so reusing a
// directory left behind by a failed replay would fail the retry for a different
// reason than the original attempt.
func (r *Runtime) replaySnapshotMetadata(ctx context.Context, snap *models.Snapshot, localStoreDir, dir string) (metadata.Store, error) {
	if err := os.RemoveAll(dir); err != nil {
		return nil, fmt.Errorf("open snapshot %q: clear shadow metadata dir: %w", snap.ID, err)
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return nil, fmt.Errorf("open snapshot %q: create shadow metadata dir: %w", snap.ID, err)
	}

	store, err := badger.NewBadgerMetadataStoreWithDefaults(ctx, dir)
	if err != nil {
		return nil, fmt.Errorf("open snapshot %q: create shadow metadata store: %w", snap.ID, err)
	}
	// Asserted at compile time below rather than type-asserted at runtime: the
	// concrete type is known here, so a rename in the Snapshotable method set
	// should fail the build instead of every open at runtime.
	var restorable metadata.Snapshotable = store

	dumpPath := snap.MetadataDumpPath(localStoreDir)
	f, err := os.Open(dumpPath)
	if err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open snapshot %q: open dump %q: %w", snap.ID, dumpPath, err)
	}
	defer func() { _ = f.Close() }()

	start := time.Now()
	if err := restorable.RestoreSnapshot(ctx, f); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open snapshot %q: replay %q: %w", snap.ID, dumpPath, err)
	}
	logger.Info("snapshot metadata replayed", "snapshot", snap.ID, "dir", dir, "elapsed", time.Since(start))
	return store, nil
}

// resolveVolumeInShadow looks the volume up in the replayed store. It is the
// same path production's exporter walks, so a snapshot whose volume was renamed
// or removed behaves exactly as production would.
func (r *Runtime) resolveVolumeInShadow(ctx context.Context, svc *metadata.Service, shareName, volume string, id *metadata.Identity) (*metadata.File, error) {
	root, err := svc.GetRootHandle(ctx, shareName)
	if err != nil {
		return nil, fmt.Errorf("shadow metadata has no root handle for share %q: %w", shareName, err)
	}
	authCtx := &metadata.AuthContext{Context: ctx, Identity: id}
	// <share>/volumes/<volume> is the layout the NBD adapter provisions, and it is
	// tried first. A share whose files sit at its root is also legal — that is how
	// the snapshot tests and older shares lay them out — so a missing volumes/
	// directory falls back to the root rather than failing. The layout belongs to
	// the share, not to this code.
	if volumes, lerr := svc.Lookup(authCtx, root, "volumes"); lerr == nil {
		if volumesHandle, herr := metadata.EncodeFileHandle(volumes); herr == nil {
			if file, ferr := svc.Lookup(authCtx, volumesHandle, volume); ferr == nil {
				return file, nil
			}
		}
	}
	file, err := svc.Lookup(authCtx, root, volume)
	if err != nil {
		return nil, fmt.Errorf("shadow metadata: look up volume %q: %w", volume, err)
	}
	return file, nil
}

// openShadowBlockStore builds the read-only block store a snapshot export reads
// through.
//
// Its local tier is an EMPTY journal and its manifest is the replayed store's,
// so every range reports a hole and hydrates from the snapshot's manifest plus
// the shared remote. That is the state a range with no local copy is supposed to
// be in, which is why no seeding step is needed.
func (r *Runtime) openShadowBlockStore(shareName string, production *engine.Store, shadowMeta metadata.Store, journalDir string) (*engine.Store, error) {
	r.mu.RLock()
	defaults := r.localStoreDefaults
	r.mu.RUnlock()

	var maxLocal int64
	var maxLog int64
	if defaults != nil {
		maxLocal = int64(defaults.MaxSize / shadowJournalCapDivisor)
		maxLog = defaults.MaxLogBytes
	}
	if maxLocal <= 0 {
		// Without a configured ceiling the journal would grow without bound next
		// to the volume production writes to. A fixed floor is still a bound.
		maxLocal = 256 << 20
	}

	if err := os.RemoveAll(journalDir); err != nil {
		return nil, fmt.Errorf("clear shadow journal dir: %w", err)
	}
	if err := os.MkdirAll(journalDir, 0o750); err != nil {
		return nil, fmt.Errorf("create shadow journal dir: %w", err)
	}
	local, err := journal.Open(filepath.Join(journalDir, "journal"), journal.Config{
		MaxLocalBytes: maxLocal,
		MaxLogBytes:   maxLog,
	})
	if err != nil {
		return nil, fmt.Errorf("open shadow journal: %w", err)
	}

	// The replayed store is BOTH the manifest the reads resolve against and the
	// durability witness the syncer consults. Pointing either at production's
	// store would hydrate production's bytes into a snapshot export, and
	// NewRemoteSync panics on a nil fileChunkStore, so this is not optional.
	// Wrapped so closing the shadow does not close the remote production is
	// still using. The wrapper is shares.NonClosing (see shares/blockstore_config.go),
	// which forwards Durable and ReadChunk as well as the block-keyed surface —
	// embedding a bare remote.RemoteStore would silently drop durability
	// reporting and packed-chunk reads.
	engineRemote := shares.NonClosing(production.RemoteStore())
	syncer := engine.NewRemoteSync(local, engineRemote, shadowMeta, engine.RemoteSyncConfig{})

	// SyncedHashStore is what makes hydration possible at all, and it is easy to
	// leave out because nothing fails when it is missing. Without it the syncer's
	// resolveLocator reports every hash as "not synced", dispatchRemoteFetch
	// returns no bytes and no error, and the read zero-fills — a snapshot that
	// reads as a blank disk with no error anywhere. The locators live in the
	// metadata store beside the manifest, so the REPLAYED store is the right
	// source: it carries the snapshot's own "this hash was uploaded" record, which
	// is exactly what decides whether the shared remote holds these bytes.
	cfg := engine.BlockStoreConfig{
		Local:          local,
		Remote:         engineRemote,
		RemoteSync:     syncer,
		FileChunkStore: shadowMeta,
	}
	if shs, ok := shadowMeta.(metadata.SyncedHashStore); ok {
		cfg.SyncedHashStore = shs
	} else {
		_ = syncer.Close()
		_ = local.Close()
		return nil, fmt.Errorf("shadow metadata store %T does not implement metadata.SyncedHashStore: "+
			"without it every hash reads as not-synced and every snapshot read would zero-fill", shadowMeta)
	}

	bs, err := engine.New(cfg)
	if err != nil {
		_ = syncer.Close()
		_ = local.Close()
		return nil, fmt.Errorf("create shadow block store: %w", err)
	}
	if err := bs.Start(context.Background()); err != nil {
		_ = bs.Close()
		return nil, fmt.Errorf("start shadow block store: %w", err)
	}

	// Every byte in a shadow journal is recoverable from the remote, so it is
	// genuinely safe to evict — but Start derived the gate from carve health,
	// which a read-only store never wires, so eviction is off. Without this the
	// capped journal would fill and stall for EvictMaxWait on every read instead
	// of evicting a hydrated chunk that can be fetched again.
	bs.SetEvictionAllowed(true)
	return bs, nil
}

// BlockStore exposes the shadow block store, for a caller that wants to report
// on it. Nil after Close.
func (e *OpenSnapshotExport) BlockStore() *engine.Store { return e.bs }

// Close releases everything the open acquired, in reverse order.
//
// Idempotent, because the open path's deferred teardown and an explicit caller
// Close can both run. The open registration is released LAST: until the block
// store and metadata store are closed, something could still read the snapshot
// directory, and releasing early would let a delete race that read.
func (e *OpenSnapshotExport) Close() error {
	if e == nil {
		return nil
	}
	e.closeOnce.Do(e.close)
	return e.closeErr
}

// close does the work, once.
func (e *OpenSnapshotExport) close() {
	var errs []error
	if e.bs != nil {
		if err := e.bs.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close shadow block store: %w", err))
		}
		e.bs = nil
	}
	// metadata.Service has no Close: it holds stores rather than owning them.
	// Deregister first so nothing in the service can reach the store, then close
	// the store itself — an open Badger handle would keep the shadow directory
	// from being removed.
	if e.Meta != nil {
		e.Meta.RemoveStoreForShare(e.ShareName)
		e.Meta = nil
	}
	if e.metaStore != nil {
		if err := e.metaStore.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close shadow metadata store: %w", err))
		}
		e.metaStore = nil
	}
	for _, dir := range []string{e.jrnlDir, e.metaDir} {
		if dir == "" {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			errs = append(errs, fmt.Errorf("remove %q: %w", dir, err))
		}
	}
	if e.release != nil {
		e.release()
		e.release = nil
	}

	e.closeErr = errors.Join(errs...)
}
