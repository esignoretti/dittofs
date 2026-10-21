// Command nbd-fixture serves one volume through the real NBD adapter so a kernel
// client can be crashed against it.
//
// It exists because every NBD durability claim needs a server that can be
// SIGKILLed, and the production `dfs` server cannot be: it needs a provisioned
// volume, and creating one needs a share mount, which needs root. This builds the
// same runtime the integration tests build, from an ordinary program, and then
// holds still so a crash rig can kill it.
//
//	# on the server host, after cross-compiling:
//	GOOS=linux GOARCH=amd64 go build -o /tmp/nbd-fixture ./test/nbd/fixture
//	/tmp/nbd-fixture -port 10950 -size 2147483648 -journal /tmp/fx-j -meta /tmp/fx-m
//
// Two things here are load-bearing and were both learned by getting them wrong:
//
// The metadata store is ON DISK. An in-memory store loses the file's size and its
// chunk manifest when the process dies, so after a restart every read returns
// zeros — which a verifying client reports as a corruption bug in the adapter.
// Whatever holds the file's size and manifest must survive the kill.
//
// The store is durable ACROSS RUNS, so provisioning is idempotent. A crash test
// restarts the server by definition; a fixture that re-creates its volume on every
// start would erase the data it is meant to have survived.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	pkgadapternbd "github.com/marmos91/dittofs/pkg/adapter/nbd"
	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime/shares"
	"github.com/marmos91/dittofs/pkg/controlplane/store"
	"github.com/marmos91/dittofs/pkg/metadata"
	badgermeta "github.com/marmos91/dittofs/pkg/metadata/store/badger"
)

// shareName is the share the export lives in, and exportName is what a client
// passes to `nbd-client -N`. Both are fixed so a crash rig can hardcode them.
const (
	shareName  = "/export"
	exportName = "export/boot"
	volumeName = "boot"
	volumeDir  = "volumes"

	// fixtureIdentity is the identity every metadata operation runs as, matching
	// pkg/adapter/nbd's DefaultUID/DefaultGID. NBD carries no user identity, so
	// the adapter uses this one for everything.
	fixtureIdentity = uint32(65534)
)

func main() {
	port := flag.Int("port", 10900, "NBD listen port")
	size := flag.Int64("size", 1<<30, "volume size in bytes")
	journal := flag.String("journal", "/tmp/nbd-fixture-journal", "journal root (must survive a crash)")
	metaPath := flag.String("meta", "/tmp/nbd-fixture-meta", "metadata store path (must survive a crash)")
	inFlight := flag.Int("in-flight", 0, "max commands in flight per connection (0 = adapter default)")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	rt, handle := provision(ctx, *journal, *metaPath, uint64(*size))

	cfg := pkgadapternbd.Config{
		Enabled:         true,
		Port:            *port,
		BindAddress:     "127.0.0.1",
		MaxConnections:  64,
		MaxInFlight:     *inFlight,
		AllowRemoteBind: false,
		Timeouts: pkgadapternbd.TimeoutsConfig{
			Read:     5 * time.Minute,
			Write:    30 * time.Second,
			Idle:     5 * time.Minute,
			Shutdown: 30 * time.Second,
		},
	}
	a := pkgadapternbd.New(cfg)
	a.SetRuntime(rt)

	serveCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	serveErr := make(chan error, 1)
	go func() { serveErr <- a.Serve(serveCtx) }()

	// Announce readiness only once the port actually accepts a connection, and
	// fail loudly if it does not. A fixture that printed READY while Serve had
	// already died on "address already in use" let an older server on the same
	// port answer the clients, so a run silently measured the wrong process.
	waitListening(*port, serveErr)

	fmt.Printf("READY port=%d export=%s size=%d journal=%s meta=%s in_flight=%d handle=%x\n",
		*port, exportName, *size, *journal, *metaPath, cfg.MaxInFlight, handle)

	select {
	case err := <-serveErr:
		log.Fatalf("Serve exited: %v", err)
	case <-ctx.Done():
	}
	_ = a.Stop(serveCtx)
}

// provision builds the runtime, the share and the volume, and returns the volume's
// file handle. Every creation step tolerates "already there" because this runs
// again on every restart of a crash test.
func provision(ctx context.Context, journalRoot, metaPath string, size uint64) (*runtime.Runtime, metadata.FileHandle) {
	cps, err := store.New(&store.Config{
		Type:   store.DatabaseTypeSQLite,
		SQLite: store.SQLiteConfig{Path: ":memory:"},
	})
	if err != nil {
		log.Fatalf("store.New: %v", err)
	}
	bsID, err := cps.CreateBlockStore(ctx, &models.BlockStoreConfig{Name: "fixture-blocks", Type: "memory"})
	if err != nil {
		log.Fatalf("CreateBlockStore: %v", err)
	}
	for _, dir := range []string{journalRoot, metaPath} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			log.Fatalf("mkdir %s: %v", dir, err)
		}
	}

	rt := runtime.New(cps)
	rt.SetLocalStoreDefaults(&shares.LocalStoreDefaults{
		JournalRoot: journalRoot,
		ChunkSize:   128 << 10,
		ChunkMax:    1 << 20,
	})

	metaStore, err := badgermeta.NewBadgerMetadataStoreWithDefaults(ctx, metaPath)
	if err != nil {
		log.Fatalf("metadata store at %s: %v", metaPath, err)
	}
	if err := rt.RegisterMetadataStore("fixture-meta", metaStore); err != nil {
		log.Fatalf("RegisterMetadataStore: %v", err)
	}
	if err := rt.AddShare(ctx, &runtime.ShareConfig{
		Name:              shareName,
		MetadataStore:     "fixture-meta",
		BlockStoreID:      bsID,
		Enabled:           true,
		DefaultPermission: string(models.PermissionReadWrite),
		RootAttr:          &metadata.FileAttr{UID: fixtureIdentity, GID: fixtureIdentity},
	}); err != nil {
		log.Fatalf("AddShare: %v", err)
	}

	root, err := rt.GetRootHandle(shareName)
	if err != nil {
		log.Fatalf("GetRootHandle: %v", err)
	}
	metaSvc := rt.GetMetadataService()
	id := fixtureIdentity
	authCtx := &metadata.AuthContext{
		Context:  ctx,
		Identity: &metadata.Identity{UID: &id, GID: &id},
	}

	// Lookup-or-create, in that order: on a restart the tree is already there and
	// the point of the restart is that it survived.
	volsDir, _, err := metaSvc.CreateDirectory(authCtx, root, volumeDir, &metadata.FileAttr{Mode: 0o755})
	if err != nil {
		volsDir, err = metaSvc.Lookup(authCtx, root, volumeDir)
		if err != nil {
			log.Fatalf("CreateDirectory(%s) then Lookup: %v", volumeDir, err)
		}
	}
	volsHandle, err := metadata.EncodeFileHandle(volsDir)
	if err != nil {
		log.Fatalf("EncodeFileHandle(%s): %v", volumeDir, err)
	}

	volume, _, err := metaSvc.CreateFile(authCtx, volsHandle, volumeName, &metadata.FileAttr{Mode: 0o644})
	if err != nil {
		volume, err = metaSvc.Lookup(authCtx, volsHandle, volumeName)
		if err != nil {
			log.Fatalf("CreateFile(%s) then Lookup: %v", volumeName, err)
		}
	}
	handle, err := metadata.EncodeFileHandle(volume)
	if err != nil {
		log.Fatalf("EncodeFileHandle(%s): %v", volumeName, err)
	}

	// Creation zeroes the size (ApplyCreateDefaults, pkg/metadata/validation.go),
	// and a zero-length export fails the handshake. Only set it when it is wrong,
	// so a restart does not truncate the data under test.
	if volume.Size != size {
		if _, err := metaSvc.SetFileAttributes(authCtx, handle, &metadata.SetAttrs{Size: &size}); err != nil {
			log.Fatalf("SetFileAttributes(size=%d): %v", size, err)
		}
	}
	return rt, handle
}

// waitListening blocks until the port accepts a connection, or exits if Serve
// fails first. It cannot distinguish its own server from an older one on the same
// port, so it reports the failure rather than pretending to have bound.
func waitListening(port int, serveErr <-chan error) {
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond); err == nil {
			_ = conn.Close()
			return
		}
		select {
		case err := <-serveErr:
			log.Fatalf("Serve failed before the listener came up (is port %d already in use?): %v", port, err)
		default:
		}
		time.Sleep(50 * time.Millisecond)
	}
	log.Fatalf("nothing accepted a connection on %s after 15s", addr)
}
