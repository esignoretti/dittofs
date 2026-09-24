package journal

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

func TestSealDiscardsFailedAppendSuffix(t *testing.T) {
	for _, stage := range []string{"payload", "crc"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			s := testStore(t, Config{ShardCount: 1, SegmentSize: 1 << 20, DirtyExpiry: -1})
			prefix := bytes.Repeat([]byte("p"), 256<<10)
			shorter := []byte("successful shorter append")
			rotating := bytes.Repeat([]byte("r"), 900<<10)
			if err := s.WriteAt(ctx, "prefix", 0, prefix); err != nil {
				t.Fatal(err)
			}
			sh := s.shardFor("prefix")
			sh.mu.Lock()
			seg := sh.active
			// Model an append that wrote its header and some body bytes, then
			// failed. Neither the published tail nor the index advances.
			failed := frameRecord("failed", 0, bytes.Repeat([]byte("x"), 128<<10), s.nextVersion(), 0)
			n := recordHeaderSize + len("failed") + 64<<10
			if stage == "crc" {
				n = len(failed) - 2
			}
			_, err := seg.fd.WriteAt(failed[:n], seg.tail.Load())
			sh.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			if err := s.WriteAt(ctx, "shorter", 0, shorter); err != nil {
				t.Fatal(err)
			}
			tail := seg.tail.Load()
			if fileSize(seg.fd) <= tail {
				t.Fatal("fixture left no unowned suffix beyond the shorter append")
			}
			// This accepted append rotates the old segment through the normal
			// writer path. Reopening must retain all three successful writes.
			if err := s.WriteAt(ctx, "rotating", 0, rotating); err != nil {
				t.Fatal(err)
			}
			if !seg.sealed.Load() {
				t.Fatal("fixture did not rotate the segment")
			}
			if got := fileSize(seg.fd); got != tail {
				t.Errorf("sealed size=%d, published tail=%d: failed append suffix survived", got, tail)
			}
			r := reopen(t, s, Config{})
			for id, want := range map[FileID][]byte{"prefix": prefix, "shorter": shorter, "rotating": rotating} {
				if got := readAll(t, r, id, len(want)); !bytes.Equal(got, want) {
					t.Fatalf("successful write %q changed after recovery", id)
				}
			}
			if _, ok := r.FileSize(ctx, "failed"); ok {
				t.Fatal("failed append became a live file after recovery")
			}
		})
	}
}

func TestSealTruncateFailureDoesNotPublishSeal(t *testing.T) {
	s := testStore(t, Config{ShardCount: 1, DirtyExpiry: -1})
	if err := s.WriteAt(context.Background(), "f", 0, []byte("data")); err != nil {
		t.Fatal(err)
	}
	sh := s.shardFor("f")
	sh.mu.Lock()
	defer sh.mu.Unlock()
	seg := sh.active
	if _, err := seg.fd.WriteAt([]byte("unowned suffix"), seg.tail.Load()); err != nil {
		t.Fatal(err)
	}
	if err := seg.fd.Close(); err != nil {
		t.Fatal(err)
	}
	fd, err := os.Open(s.segPath(seg.id))
	if err != nil {
		t.Fatal(err)
	}
	seg.fd = fd
	if err := seg.sealInPlace(); err == nil || !strings.Contains(err.Error(), "truncate before seal") {
		t.Fatalf("want a truncate failure before publishing the seal, got %v", err)
	}
	if seg.sealed.Load() {
		t.Fatal("failed truncation published the in-memory sealed flag")
	}
	var hdr [segHeaderSize]byte
	if _, err := fd.ReadAt(hdr[:], 0); err != nil {
		t.Fatal(err)
	}
	_, _, flags, ok := decodeSegHeader(hdr[:])
	if !ok || flags&segFlagSealed != 0 {
		t.Fatal("failed truncation changed the on-disk sealed header")
	}
}
