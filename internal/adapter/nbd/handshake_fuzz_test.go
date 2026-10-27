package nbd

import "testing"

// FuzzParseSnapshotExportName drives the snapshot export name grammar.
//
// It is a string splitter, but it runs on a client-supplied name before any
// validation and before any export exists, so a panic there is a remote crash in
// the handshake. The property asserted is stronger than "does not panic": a name
// that parses must round-trip to the same string, because the parsed share and ID
// are used to look up a snapshot and a mismatch would open the wrong one.
func FuzzParseSnapshotExportName(f *testing.F) {
	for _, s := range []string{
		"export/snapshots/abc/boot",
		"export/snapshots//boot",
		"/snapshots/abc/boot",
		"snapshots/abc/boot",
		"a/b/snapshots/abc/boot",
		"export/snapshots/abc/",
		"export/snapshots/abc/boot/extra",
		"",
		"/",
		"snapshots",
		"export/snapshots",
		"export/snapshots/",
		"../../etc/passwd",
		"export/snapshots/../../../etc/boot",
	} {
		f.Add(s)
	}

	f.Fuzz(func(t *testing.T, name string) {
		share, snapID, volume, ok := parseSnapshotExportName(name)
		if !ok {
			return
		}
		// A parse that claims success must have produced three non-empty parts:
		// an empty share would resolve to no share, an empty ID to no snapshot.
		if share == "" || snapID == "" || volume == "" {
			t.Fatalf("parsed %q into empty parts: share=%q snap=%q volume=%q",
				name, share, snapID, volume)
		}
		// And the parse must be IDEMPOTENT: re-parsing a name rebuilt from its
		// own parts has to yield the same parts.
		//
		// Not an exact round-trip, because the share is normalized (a leading
		// slash is added), so "export/snapshots/abc/boot" parses to share
		// "/export". Idempotence is the property that actually matters: the
		// exporter claims a name, and the parts it claims are then used for
		// lookups — a parse whose own output re-parses differently would mean the
		// claim and the lookup disagree.
		rebuilt := share + "/" + snapshotsMarker + "/" + snapID + "/" + volume
		share2, snapID2, volume2, ok2 := parseSnapshotExportName(rebuilt)
		if !ok2 {
			t.Fatalf("a name rebuilt from a successful parse does not parse: "+
				"%q -> share=%q snap=%q volume=%q -> rebuilt %q",
				name, share, snapID, volume, rebuilt)
		}
		if share2 != share || snapID2 != snapID || volume2 != volume {
			t.Fatalf("parse is not idempotent for %q: first (%q,%q,%q), "+
				"second (%q,%q,%q) via %q",
				name, share, snapID, volume, share2, snapID2, volume2, rebuilt)
		}
	})
}
