package nbd

import (
	"context"
	"fmt"

	"github.com/marmos91/dittofs/pkg/controlplane/models"
	"github.com/marmos91/dittofs/pkg/controlplane/runtime"
)

// persistMetadataStoreRow writes the control-plane row a metadata store needs to
// be addressable by snapshot orchestration.
//
// The NBD integration fixture registers a metadata store directly on the runtime
// (rt.RegisterMetadataStore) and adds a share by name, which is all the adapter
// needs. Snapshot creation is the one path that resolves the store through the
// control-plane store instead — it has to, because a snapshot outlives the
// process that created it and its record names the store that produced the dump.
// A registered-but-unpersisted store therefore fails with
// `resolve metadata store config "nbd-meta": store not found`, which names the
// row rather than the fixture and is easy to misread as a snapshot bug.
func persistMetadataStoreRow(t interface {
	Helper()
	Fatalf(string, ...any)
}, rt *runtime.Runtime, name string) error {
	t.Helper()
	cfg := &models.MetadataStoreConfig{Name: name, Type: "badger"}
	if err := cfg.SetConfig(map[string]any{"path": ""}); err != nil {
		return fmt.Errorf("SetConfig: %w", err)
	}
	if _, err := rt.Store().CreateMetadataStore(context.Background(), cfg); err != nil {
		return err
	}
	return nil
}
