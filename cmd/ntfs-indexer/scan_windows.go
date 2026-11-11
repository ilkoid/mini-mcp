//go:build windows

package main

import (
	"fmt"
	"log"
	"sort"
	"time"

	"mini-mcp/internal/ntfs"
)

// runScan is the Э-0.2 verification path (PRD §13 DoD): a full MFT pass on
// the volume, include-filtered, with assembled paths printed for a
// byte-for-byte `dir /s /b` comparison. No DB writes — batch persistence
// lands with Э-0.3, when the cursor protocol exists.
func runScan(device, include string, list bool) error {
	// prefix paths with the drive letter when given one, so --list output
	// diffs 1:1 against `dir /s /b` (which prints absolute paths)
	prefix := ""
	if len(device) == 2 && device[1] == ':' {
		prefix = device + `\`
	}
	v, err := ntfs.OpenVolume(device)
	if err != nil {
		return err
	}
	defer v.Close()

	start := time.Now()
	snap, err := v.Snapshot()
	if err != nil {
		return err
	}
	root := ntfs.RootFRN(snap.Entries)
	matched := 0
	out := make([]string, 0, 1024)
	for frn, p := range snap.Paths {
		if frn == root || !ntfs.MatchInclude(p, include) {
			continue
		}
		matched++
		if list || len(out) < 10 {
			out = append(out, prefix+p)
		}
	}
	sort.Strings(out)

	jstate := "journal: absent (deltas impossible until created — П-3)"
	if !snap.Journal.Absent {
		jstate = fmt.Sprintf("journal id=%#x next_usn=%#x (persist as the delta cursor, Э-0.3)",
			snap.Journal.JournalID, snap.Journal.NextUSN)
	}
	log.Printf("volume %s: %d MFT records, %d paths resolved, %d orphans, %d matched include %q, took %s; %s",
		v.Device, snap.Count, len(snap.Paths), len(snap.Orphans), matched, include,
		time.Since(start).Round(time.Millisecond), jstate)
	if len(snap.Orphans) > 0 {
		// one dominant cause = one broken anchor (the wrong-anchor bug
		// showed up here as 393k orphans all dying at one FRN)
		type cause struct {
			frn   int64
			count int
		}
		var top []cause
		for frn, n := range ntfs.OrphanCauses(snap.Entries, snap.Orphans) {
			top = append(top, cause{frn, n})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].count > top[j].count })
		if len(top) > 5 {
			top = top[:5]
		}
		for _, c := range top {
			log.Printf("  orphan cause: parent FRN %#x kills %d chains", c.frn, c.count)
		}
	}
	for _, p := range out {
		if list {
			fmt.Println(p)
		} else {
			log.Printf("  sample: %s", p)
		}
	}
	return nil
}
