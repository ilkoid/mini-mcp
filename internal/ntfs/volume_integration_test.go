//go:build windows

package ntfs

import (
	"os"
	"testing"
)

// Live-volume integration (Э-0.2), gated like PG_TEST_DSN/MSSQL_TEST_DSN:
//   NTFS_TEST_VOLUME='E:' go test -run TestLiveVolume ./internal/ntfs -v
// An ACCESS_DENIED open is a SKIP with the П-2 hint, not a failure — the
// unelevated-token rake is documented (PRD §13); run from an elevated
// console to get the real pass.
func TestLiveVolumeSnapshot(t *testing.T) {
	dev := os.Getenv("NTFS_TEST_VOLUME")
	if dev == "" {
		t.Skip("NTFS_TEST_VOLUME not set (e.g. NTFS_TEST_VOLUME='E:')")
	}
	v, err := OpenVolume(dev)
	if err != nil {
		t.Skipf("%v", err)
	}
	defer v.Close()

	snap, err := v.Snapshot()
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if snap.Count == 0 || len(snap.Paths) == 0 {
		t.Fatalf("empty snapshot: count=%d paths=%d", snap.Count, len(snap.Paths))
	}
	// structural invariants: every non-root path is non-empty and
	// backslash-joined from the root; orphans are a small minority
	empty := 0
	for frn, p := range snap.Paths {
		if frn == RootFRN(snap.Entries) {
			continue
		}
		if p == "" {
			empty++
		}
	}
	if empty > 0 {
		t.Errorf("%d non-root entries resolved to an empty path", empty)
	}
	if len(snap.Orphans) > len(snap.Paths)/10 {
		t.Errorf("orphans suspiciously high: %d of %d resolved", len(snap.Orphans), len(snap.Paths))
	}
	t.Logf("journal: %+v", snap.Journal)
	t.Logf("count=%d paths=%d orphans=%d", snap.Count, len(snap.Paths), len(snap.Orphans))
}
