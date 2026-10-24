package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/marmos91/dittofs/internal/logger"
	"github.com/marmos91/dittofs/pkg/block"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/metadata"
)

// Rehydrate states. Terminal states are Done and Failed; a job in any other
// state is still running or waiting to.
const (
	RehydrateStatePending   = "pending"
	RehydrateStateReplaying = "replaying"
	RehydrateStateCopying   = "copying"
	RehydrateStateDone      = "done"
	RehydrateStateFailed    = "failed"
)

// RehydrateJob is one snapshot-to-volume rehydration.
//
// It is a job rather than a synchronous call because two of its steps are
// proportional to the volume: replaying the metadata dump and writing one
// FileChunk row per block. For a large volume that is seconds to minutes, and
// the operator wants a status to poll rather than a request that blocks.
type RehydrateJob struct {
	ID         string `json:"id"`
	State      string `json:"state"`
	Share      string `json:"share"`
	SnapshotID string `json:"snapshot_id"`
	// SourceVolume is the volume inside the snapshot that was copied.
	SourceVolume string `json:"source_volume"`
	// Volume is the new volume's name inside Share.
	Volume string `json:"volume"`
	// Blocks is the number of chunk references the copy placed, which is the
	// useful measure of the result: it is the volume's size in chunks, and no
	// bytes moved to produce it.
	Blocks int `json:"blocks"`
	// Bytes is the volume's size after rehydration.
	Bytes uint64 `json:"bytes"`

	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	Err        string    `json:"error,omitempty"`

	// mu guards State, Err and FinishedAt, which the job goroutine writes while a
	// poller reads them through clone. A *sync.Mutex rather than a value, so the
	// struct stays copyable for clone — vet rejects copying a struct containing a
	// value mutex, and it is right to: that would copy the lock, not share it.
	mu *sync.Mutex
}

// clone returns a copy safe to hand outside the registry lock.
func (j *RehydrateJob) clone() *RehydrateJob {
	j.mu.Lock()
	defer j.mu.Unlock()
	cp := *j
	return &cp
}

// newRehydrateJob builds a job with its lock initialised.
func newRehydrateJob(id, share, snapID, srcVolume, dstVolume string) *RehydrateJob {
	return &RehydrateJob{
		ID:           id,
		State:        RehydrateStatePending,
		Share:        share,
		SnapshotID:   snapID,
		SourceVolume: srcVolume,
		Volume:       dstVolume,
		StartedAt:    time.Now(),
		mu:           &sync.Mutex{},
	}
}

// setState records progress under the job's own lock, so a poller never sees a
// torn state/error pair.
func (j *RehydrateJob) setState(state string, err error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.State = state
	if err != nil {
		j.Err = err.Error()
	}
	if state == RehydrateStateDone || state == RehydrateStateFailed {
		j.FinishedAt = time.Now()
	}
}

// rehydrateRegistry tracks in-flight and recently finished rehydrations, so a
// caller can poll one by id. Bounded so a long-lived process does not accumulate
// job records.
type rehydrateRegistry struct {
	mu      sync.Mutex
	jobs    map[string]*RehydrateJob
	order   []string
	maxKeep int
}

func newRehydrateRegistry(maxKeep int) *rehydrateRegistry {
	return &rehydrateRegistry{jobs: make(map[string]*RehydrateJob), maxKeep: maxKeep}
}

func (r *rehydrateRegistry) add(j *RehydrateJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.jobs[j.ID] = j
	r.order = append(r.order, j.ID)
	for len(r.order) > r.maxKeep {
		oldest := r.order[0]
		r.order = r.order[1:]
		if old, ok := r.jobs[oldest]; ok && (old.State == RehydrateStateDone || old.State == RehydrateStateFailed) {
			delete(r.jobs, oldest)
		} else if ok {
			// Still running: keep the record and put it back at the front, so a
			// running job is never dropped in favour of a finished one.
			r.order = append([]string{oldest}, r.order...)
			break
		}
	}
}

func (r *rehydrateRegistry) get(id string) (*RehydrateJob, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	j, ok := r.jobs[id]
	if !ok {
		return nil, false
	}
	return j.clone(), true
}

// RehydrateSnapshot creates a new writable volume whose content is the
// snapshot's, by copying the snapshot's manifest onto the target share's own
// block store. No bytes move: chunks are immutable and content-addressed, so the
// copy writes one reference per block and the new volume shares every chunk with
// the snapshot it came from.
//
// It returns a job to poll. The job runs in the background; its terminal state is
// RehydrateStateDone or RehydrateStateFailed.
//
// The snapshot is untouched and stays bootable: this is a copy out of it, never a
// promotion of it. That is why a snapshot can be rehydrated any number of times.
func (r *Runtime) RehydrateSnapshot(ctx context.Context, share, snapID, srcVolume, dstVolume string) (*RehydrateJob, error) {
	if r == nil || r.store == nil {
		return nil, errors.New("runtime: nil store")
	}
	// Validation reads run detached from the caller's cancellation for the same
	// reason the job does: a caller that hung up mid-request must not turn a
	// valid request into a confusing "context canceled" error, and the job it
	// asked for is going to outlive the request anyway.
	ctx = context.WithoutCancel(ctx)
	if srcVolume == "" || dstVolume == "" {
		return nil, errors.New("rehydrate: both the snapshot's volume name and the destination volume name are required")
	}
	if srcVolume == dstVolume {
		// Not a limitation so much as a sign the caller mixed up the arguments:
		// the destination is created, so it cannot be the volume being read.
		return nil, fmt.Errorf("rehydrate: the destination volume %q would overwrite the snapshot's own volume of the same name",
			dstVolume)
	}

	// Validate everything that can fail cheaply BEFORE creating the job, so a
	// request that cannot work is rejected synchronously and the operator does
	// not have to poll to find out.
	snap, err := r.store.GetSnapshot(ctx, share, snapID)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: %w", err)
	}
	if snap.State != models.StateReady {
		return nil, fmt.Errorf("rehydrate snapshot %q: state %q cannot be read: %w",
			snapID, snap.State, models.ErrSnapshotStateConflict)
	}
	target, err := r.sharesSvc.GetShare(share)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: %w", err)
	}
	if target.ReadOnly {
		return nil, fmt.Errorf("rehydrate into share %q: the share is read-only", share)
	}
	if target.BlockStore == nil || target.BlockStore.RemoteStore() == nil {
		// Chunks are addressed by hash inside one remote store. Without one there
		// is nothing for the new volume's references to resolve to, and the copy
		// would produce a volume that reads as zeros.
		return nil, fmt.Errorf("rehydrate into share %q: the share has no remote store: %w",
			share, errRehydrateNoRemote)
	}
	localStoreDir, err := r.sharesSvc.LocalStoreDir(share)
	if err != nil {
		return nil, err
	}
	if localStoreDir == "" {
		return nil, fmt.Errorf("rehydrate into share %q: no on-disk data directory: %w",
			share, errRehydrateNoLocalDir)
	}

	// The destination must not already exist. Checked here rather than in the job
	// because "that name is taken" is a request error, and the operator should not
	// have to poll to learn it.
	exists, err := r.volumeExists(ctx, share, dstVolume)
	if err != nil {
		return nil, err
	}
	if exists {
		return nil, fmt.Errorf("rehydrate into share %q: volume %q already exists", share, dstVolume)
	}

	job := newRehydrateJob(uuid.NewString(), share, snapID, srcVolume, dstVolume)
	r.rehydrates.add(job)

	// Registered as open for the whole job, so a delete cannot remove the dump
	// between the replay and the copy. Released when the job ends, on every path.
	release := r.OpenSnapshot(share, snapID)

	// Detached from the request's cancellation on purpose: the caller polls for
	// the result, and a client that hangs up mid-rehydrate must not leave a
	// half-copied volume behind. The job's own timeout is the bound.
	go func() {
		defer release()
		jobCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		r.runRehydrate(jobCtx, job, snap, share, srcVolume, dstVolume, localStoreDir)
	}()

	return job.clone(), nil
}

// GetRehydrateJob returns a job's current state.
func (r *Runtime) GetRehydrateJob(id string) (*RehydrateJob, bool) {
	return r.rehydrates.get(id)
}

// runRehydrate is the job body. Every failure unwinds to a rollback: the
// destination volume is removed, so there is no partial state to clean up by
// hand. The snapshot is never modified, so the operator just retries.
func (r *Runtime) runRehydrate(ctx context.Context, job *RehydrateJob, snap *models.Snapshot,
	share, srcVolume, dstVolume, localStoreDir string) {

	job.setState(RehydrateStateReplaying, nil)

	// (1) Replay the dump into a throwaway store and read the snapshot volume's
	// manifest from it. This is the only use of the replayed store: rehydration
	// reads the manifest, never the data.
	tmpDir := filepath.Join(snap.SnapshotDir(localStoreDir), "rehydrate-meta-"+job.ID)
	replayed, err := r.replaySnapshotMetadata(ctx, snap, localStoreDir, tmpDir)
	if err != nil {
		job.setState(RehydrateStateFailed, err)
		return
	}
	// The replay directory is a scratch copy; it is removed whatever happens.
	defer func() { _ = os.RemoveAll(tmpDir) }()

	srcFile, err := resolveSnapshotSourceVolume(ctx, r.metadataServiceOver(replayed, share), share, srcVolume, r.rehydrateIdentity())
	if err != nil {
		_ = replayed.Close()
		job.setState(RehydrateStateFailed, err)
		return
	}
	srcBlocks := srcFile.Blocks
	srcSize := srcFile.Size
	_ = replayed.Close()

	// (2) Create the destination volume. It is an ordinary regular file in an
	// ordinary share, so every existing invariant — quota, locks, GC enumeration,
	// the read path — applies to it unchanged.
	job.setState(RehydrateStateCopying, nil)

	created, createdHandle, payloadID, err := r.createRehydrateDestination(ctx, share, dstVolume)
	if err != nil {
		job.setState(RehydrateStateFailed, err)
		return
	}
	// Rollback: the volume is removed on any failure below. Nothing about the
	// snapshot needs undoing.
	committed := false
	defer func() {
		if committed {
			return
		}
		cleanupCtx := context.WithoutCancel(ctx)
		if rerr := r.removeVolumeFile(cleanupCtx, share, dstVolume, createdHandle); rerr != nil {
			logger.Error("rehydrate rollback: could not remove the destination volume",
				"job", job.ID, "share", share, "volume", dstVolume, "error", rerr)
		}
	}()
	_ = created

	target, err := r.sharesSvc.GetShare(share)
	if err != nil {
		job.setState(RehydrateStateFailed, err)
		return
	}
	bs := target.BlockStore
	metaStore, err := r.metadataStoreForShare(share)
	if err != nil {
		job.setState(RehydrateStateFailed, err)
		return
	}

	// (3) Copy the manifest, in one transaction, exactly as the clone path does:
	// the per-file rows the read path resolves through and the file's Blocks
	// manifest then commit or roll back together.
	var copied []block.ChunkRef
	err = metaStore.WithTransaction(ctx, func(tx metadata.Transaction) error {
		copied = nil
		txCtx := metadata.WithTx(ctx, tx)
		// Re-read the destination inside the txn: it was just created, and the
		// manifest write must be against the same row the copy's refcount bumps
		// join.
		dstFile, err := tx.GetFile(ctx, createdHandle)
		if err != nil {
			return fmt.Errorf("re-read destination volume: %w", err)
		}
		newBlocks, err := bs.CopyPayload(txCtx, string(srcFile.PayloadID), string(payloadID), srcBlocks)
		if err != nil {
			return fmt.Errorf("copy the snapshot manifest: %w", err)
		}
		copied = newBlocks
		dstFile.Blocks = newBlocks
		dstFile.Size = srcSize
		dstFile.Mtime = time.Now()
		dstFile.Ctime = dstFile.Mtime
		if err := tx.SetManifest(ctx, dstFile); err != nil {
			return fmt.Errorf("persist the destination manifest: %w", err)
		}
		return nil
	})
	if err != nil {
		job.setState(RehydrateStateFailed, err)
		return
	}

	// (4) After the commit, tell the local tier which ranges the manifest places.
	// The order matters and is the same as the clone path's: the discard takes the
	// destination's replaced ranges back to holes, then the seed accounts for the
	// ones the copy gave it.
	if err := bs.SeedColdRefs(ctx, string(payloadID), copied); err != nil {
		job.setState(RehydrateStateFailed, fmt.Errorf("seed the destination's cold ranges: %w", err))
		return
	}

	committed = true
	job.mu.Lock()
	job.Blocks = len(copied)
	job.Bytes = srcSize
	job.mu.Unlock()
	job.setState(RehydrateStateDone, nil)

	logger.Info("rehydrate complete", "job", job.ID, "share", share,
		"snapshot", job.SnapshotID, "source", srcVolume, "volume", dstVolume,
		"blocks", len(copied), "bytes", srcSize)
}

// resolveSnapshotSourceVolume finds the snapshot's copy of volume, which is
// either <share>/volumes/<volume> (the layout the NBD adapter provisions) or
// <share>/<volume> (a share whose files sit at its root, which the snapshot
// tests and older shares use).
//
// The layout is the share's, not the snapshot's — the dump carries whatever the
// share had — so the resolver asks rather than assumes, and the volumes/ form is
// tried first because that is what the NBD path creates.
func resolveSnapshotSourceVolume(ctx context.Context, svc *metadata.Service, share, volume string,
	id *metadata.Identity) (*metadata.File, error) {

	authCtx := &metadata.AuthContext{Context: ctx, Identity: id}
	root, err := svc.GetRootHandle(ctx, share)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: resolve the snapshot's share root: %w", err)
	}

	// <share>/volumes/<volume> first.
	if volsDir, lerr := svc.Lookup(authCtx, root, "volumes"); lerr == nil {
		if volsHandle, herr := metadata.EncodeFileHandle(volsDir); herr == nil {
			if file, ferr := svc.Lookup(authCtx, volsHandle, volume); ferr == nil {
				return file, nil
			}
		}
	}

	// <share>/<volume>.
	file, err := svc.Lookup(authCtx, root, volume)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: the snapshot holds no volume %q in share %q: %w",
			volume, share, err)
	}
	return file, nil
}

// metadataServiceOver builds a service holding just the given store under the
// production share name, for reading a replayed snapshot. It is the same shape
// OpenSnapshotAsExport uses, factored out because the rehydrate job needs the
// service without the block store.
func (r *Runtime) metadataServiceOver(store metadata.Store, share string) *metadata.Service {
	svc := metadata.New()
	// The error is impossible for a fresh service and a valid name; a caller that
	// somehow hit it would get a service with no store, and the lookup below
	// would report it. Not returned because the only failure mode is a
	// programming error, and the rehydrate job's own error path covers it.
	_ = svc.RegisterStoreForShare(share, store)
	return svc
}

// createRehydrateDestination creates the new volume file and returns it with its
// handle and payload ID.
//
// The payload ID is the NEW file's, never the snapshot's: PayloadID is
// "<share>/<uuid>", so reusing the source's would address the source's rows and
// leave the new file with no manifest of its own.
func (r *Runtime) createRehydrateDestination(ctx context.Context, share, volume string) (*metadata.File, metadata.FileHandle, metadata.PayloadID, error) {
	metaSvc := r.GetMetadataService()
	root, err := metaSvc.GetRootHandle(ctx, share)
	if err != nil {
		return nil, nil, "", fmt.Errorf("rehydrate: resolve the share root: %w", err)
	}
	// The volumes directory is created by whoever provisions the share; a
	// rehydrate into a share that has none creates it, so the operator does not
	// have to know the layout.
	id := r.rehydrateIdentity()
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: id,
	}
	volsDir, _, err := metaSvc.CreateDirectory(authCtx, root, "volumes", &metadata.FileAttr{Mode: 0o755})
	if err != nil && !isAlreadyExists(err) {
		return nil, nil, "", fmt.Errorf("rehydrate: create volumes/: %w", err)
	}
	if volsDir == nil {
		volsDir, err = metaSvc.Lookup(authCtx, root, "volumes")
		if err != nil {
			return nil, nil, "", fmt.Errorf("rehydrate: look up volumes/: %w", err)
		}
	}
	volsHandle, err := metadata.EncodeFileHandle(volsDir)
	if err != nil {
		return nil, nil, "", fmt.Errorf("rehydrate: encode the volumes handle: %w", err)
	}

	file, _, err := metaSvc.CreateFile(authCtx, volsHandle, volume, &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		if isAlreadyExists(err) {
			return nil, nil, "", fmt.Errorf("rehydrate: volume %q already exists in share %q", volume, share)
		}
		return nil, nil, "", fmt.Errorf("rehydrate: create the destination volume: %w", err)
	}
	handle, err := metadata.EncodeFileHandle(file)
	if err != nil {
		return nil, nil, "", fmt.Errorf("rehydrate: encode the destination handle: %w", err)
	}
	return file, handle, file.PayloadID, nil
}

// volumeExists reports whether the share already holds a file of this name, at
// either layout the rehydrate resolver accepts.
func (r *Runtime) volumeExists(ctx context.Context, share, volume string) (bool, error) {
	metaSvc := r.GetMetadataService()
	root, err := metaSvc.GetRootHandle(ctx, share)
	if err != nil {
		return false, fmt.Errorf("rehydrate: resolve the share root: %w", err)
	}
	authCtx := &metadata.AuthContext{Context: ctx, Identity: r.rehydrateIdentity()}
	if volsDir, lerr := metaSvc.Lookup(authCtx, root, "volumes"); lerr == nil {
		if volsHandle, herr := metadata.EncodeFileHandle(volsDir); herr == nil {
			if _, ferr := metaSvc.Lookup(authCtx, volsHandle, volume); ferr == nil {
				return true, nil
			}
		}
	}
	if _, ferr := metaSvc.Lookup(authCtx, root, volume); ferr == nil {
		return true, nil
	}
	return false, nil
}

// removeVolumeFile deletes the destination volume on rollback.
func (r *Runtime) removeVolumeFile(ctx context.Context, share, volume string, handle metadata.FileHandle) error {
	metaSvc := r.GetMetadataService()
	root, err := metaSvc.GetRootHandle(ctx, share)
	if err != nil {
		return err
	}
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: r.rehydrateIdentity(),
	}
	volsDir, err := metaSvc.Lookup(authCtx, root, "volumes")
	if err != nil {
		return err
	}
	volsHandle, err := metadata.EncodeFileHandle(volsDir)
	if err != nil {
		return err
	}
	_, _, err = metaSvc.RemoveFile(authCtx, volsHandle, volume)
	return err
}

// metadataStoreForShare returns the metadata store the production share's
// handles live in.
func (r *Runtime) metadataStoreForShare(share string) (metadata.Store, error) {
	store, err := r.GetMetadataService().GetStoreForShare(share)
	if err != nil {
		return nil, fmt.Errorf("rehydrate: metadata store for share %q: %w", share, err)
	}
	return store, nil
}

// rehydrateIdentity is the identity the rehydrate job runs as: root, so it can
// create the destination volume in a share whose owner it does not know. It is a
// control-plane operation on the operator's behalf, not a client request, which
// is why it is not the adapter's synthetic identity.
func (r *Runtime) rehydrateIdentity() *metadata.Identity {
	uid, gid := uint32(0), uint32(0)
	return &metadata.Identity{UID: &uid, GID: &gid}
}

// The two conditions the cheap validation rejects with, named so a caller can
// distinguish them from a store error.
var (
	errRehydrateNoRemote   = errors.New("rehydrate: the share has no remote store, so the copied references would resolve to nothing")
	errRehydrateNoLocalDir = errors.New("rehydrate: the share has no on-disk data directory to replay the snapshot into")
)

// isAlreadyExists reports whether err says the entry is already there.
//
// The metadata layer returns a *merrs.StoreError whose Code is the discriminator
// and exposes no sentinel for this case, so the check is on the code's string
// form. Kept in one place so it is easy to replace when a sentinel exists.
func isAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	var withCode interface{ CodeString() string }
	if errors.As(err, &withCode) {
		return withCode.CodeString() == "AlreadyExists"
	}
	return strings.Contains(err.Error(), "AlreadyExists")
}
