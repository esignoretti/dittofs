//go:build fdb

package fdbkvtest

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/apple/foundationdb/bindings/go/src/fdb"

	"github.com/marmos91/dittofs/pkg/metadata/kv"
	"github.com/marmos91/dittofs/pkg/metadata/kv/fdbkv"
)

// Two stores Open makes see none of each other's keys, and a store's keys are
// gone once its test ends, so tests sharing one cluster never meet.
func TestOpenIsolatesAndClears(t *testing.T) {
	ctx := context.Background()
	stored := func() int {
		db, err := fdb.OpenDatabase(os.Getenv("DITTOFS_FDB_CLUSTER_FILE"))
		if err != nil {
			t.Fatal(err)
		}
		kvs, err := db.ReadTransact(func(tr fdb.ReadTransaction) (any, error) {
			return tr.GetRange(fdb.KeyRange{Begin: fdb.Key("test/"), End: fdb.Key("test0")}, fdb.RangeOptions{}).GetSliceWithError()
		})
		if err != nil {
			t.Fatal(err)
		}
		return len(kvs.([]fdb.KeyValue))
	}
	var before int
	t.Run("stores", func(t *testing.T) {
		a, b := Open(t, fdbkv.Options{}), Open(t, fdbkv.Options{})
		before = stored()
		if err := a.Update(ctx, func(tx kv.Txn) error { return tx.Set([]byte("k"), []byte("a")) }); err != nil {
			t.Fatal(err)
		}
		if err := b.View(ctx, func(r kv.Reader) error {
			_, err := r.Get([]byte("k"))
			return err
		}); !errors.Is(err, kv.ErrNotFound) {
			t.Errorf("one store read another's key: %v", err)
		}
	})
	if after := stored(); after >= before {
		t.Errorf("%d keys under test/ after the stores' test, %d while it ran: not cleared", after, before)
	}
}
