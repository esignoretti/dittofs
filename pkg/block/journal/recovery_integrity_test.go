package journal

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
)

// A sealed segment has already committed every record. Damage anywhere in its
// stream must not turn that record and the intact suffix into unwritten holes.
func TestRecoveryRejectsDamagedSealedRecords(t *testing.T) {
	for _, position := range []int{0, 1, 2} {
		for _, damage := range []string{"payload", "header", "truncated"} {
			t.Run(fmt.Sprintf("record%d/%s", position, damage), func(t *testing.T) {
				s := testStore(t, Config{ShardCount: 1})
				ids := []FileID{"first", "middle", "last"}
				for _, id := range ids {
					if err := s.WriteAt(context.Background(), id, 0, []byte("durable contents")); err != nil {
						t.Fatal(err)
					}
				}
				iv := firstInterval(t, s, ids[position])
				sh := s.shardFor(ids[0])
				sh.mu.Lock()
				err := s.sealSegment(sh)
				sh.mu.Unlock()
				if err != nil {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				path := s.segPath(iv.loc.SegmentID)
				switch damage {
				case "payload":
					flipSegByte(t, s, iv.loc.SegmentID, iv.recOff+recordHeaderSize+int64(len(ids[position])))
				case "header":
					flipSegByte(t, s, iv.loc.SegmentID, iv.recOff)
				case "truncated":
					if err := os.Truncate(path, iv.recOff+recordHeaderSize+1); err != nil {
						t.Fatal(err)
					}
				}
				before, err := os.ReadFile(path)
				if err != nil {
					t.Fatal(err)
				}
				// Repeat the refusal to cover failed-open cleanup as well as integrity.
				for range 2 {
					r, err := openJournal(s.dir, s.cfg)
					if err == nil {
						_ = r.Close()
						t.Fatal("recovery accepted a damaged sealed record and discarded its suffix")
					}
					if !errors.Is(err, errTornRecord) {
						t.Fatalf("want a record integrity error, got %v", err)
					}
					after, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !bytes.Equal(before, after) {
						t.Fatal("failed recovery changed the damaged segment")
					}
				}
			})
		}
	}
}
