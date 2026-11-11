//go:build windows

package main

import (
	"errors"
	"fmt"
	"log"
	"time"

	"golang.org/x/sys/windows"

	"mini-mcp/internal/ntfs"
)

// runAgent is the Э-0.3 loop: snapshot import on first run (or after a
// journal reset), then USN delta passes from the persisted cursor, each
// committed atomically with its cursor (ApplyDeltas). watch==0 runs a single
// pass — the pilot verification mode; watch>0 polls forever (Ctrl+C stops).
func runAgent(dbPath, device, include, agentName, uncBase string, watch time.Duration) error {
	db, err := ntfs.Open(dbPath)
	if err != nil {
		return err
	}
	defer db.Close()

	v, err := ntfs.OpenVolume(device)
	if err != nil {
		return err
	}
	defer v.Close()

	ji, err := v.QueryJournal()
	if err != nil {
		return err
	}
	if ji.Absent {
		return fmt.Errorf("volume %s has no USN journal; create it first (elevated console): "+
			`fsutil usn createjournal m=33554432 c=8388608 %s — П-3`, device, device)
	}
	minVer, maxVer, err := v.NegotiateRecordVersions(ji.JournalID)
	if err != nil {
		return err
	}
	log.Printf("journal %s: id %#x, caller record versions [%d,%d]", device, ji.JournalID, minVer, maxVer)

	volGUID := volumeGUID(device)
	rootID, err := ntfs.EnsureRoot(db, agentName, volGUID, uncBase)
	if err != nil {
		return err
	}
	cursor, storedJournalID, err := ntfs.RootCursor(db, rootID)
	if err != nil {
		return err
	}
	if cursor == 0 || storedJournalID != ji.JournalID {
		if cursor != 0 {
			log.Printf("journal id changed (%#x → %#x): full rescan branch", storedJournalID, ji.JournalID)
		}
		start := time.Now()
		snap, err := v.Snapshot() // journal captured first: deltas replay from NextUSN
		if err != nil {
			return err
		}
		imported, tombstoned, err := ntfs.ImportSnapshot(db, rootID, include, snap, ji.JournalID, time.Now())
		if err != nil {
			return err
		}
		cursor = snap.Journal.NextUSN
		log.Printf("snapshot: %d MFT records, %d imported into include %q, %d tombstoned, took %s",
			snap.Count, imported, include, tombstoned, time.Since(start).Round(time.Millisecond))
	}

	if watch > 0 {
		log.Printf("watching %s every pass for changes on %s (include %q) — Ctrl+C to stop", watch, device, include)
	} else {
		log.Printf("single pass from cursor %#x", cursor)
	}

	for pass := 1; ; pass++ {
		var recs []ntfs.USNRecord
		next, _, err := v.ReadJournal(cursor, ji.JournalID, minVer, maxVer, func(r ntfs.USNRecord) error {
			recs = append(recs, r)
			return nil
		})
		switch {
		case errors.Is(err, ntfs.ErrJournalInactive):
			return err
		case errors.Is(err, ntfs.ErrJournalReset):
			log.Printf("journal reset detected at cursor %#x — next start will rescan", cursor)
			if _, derr := db.Exec(`UPDATE roots SET usn_cursor = 0 WHERE id = ?1`, rootID); derr != nil {
				return derr
			}
			return err
		case err != nil:
			return err
		}
		if len(recs) > 0 {
			actions := ntfs.CollapseDeltas(recs)
			events, skipped, err := ntfs.ApplyDeltas(db, rootID, include, actions, next, time.Now())
			if err != nil {
				return err
			}
			log.Printf("pass %d: %d journal records → %d actions, applied %d, skipped %d (outside include), cursor → %#x",
				pass, len(recs), len(actions), len(events), skipped, next)
			for i, ev := range events {
				if i >= 50 {
					log.Printf("  … and %d more", len(events)-i)
					break
				}
				log.Printf("  %s", ev)
			}
		} else if watch == 0 {
			log.Printf("pass %d: no changes (cursor %#x)", pass, cursor)
		}
		cursor = next
		if watch == 0 {
			return nil
		}
		time.Sleep(watch)
	}
}

// volumeGUID resolves "C:" to the stable \\?\Volume{...}\ identity used as
// the root key (PRD §6); falls back to the device string when the mount
// point query fails.
func volumeGUID(device string) string {
	if len(device) != 2 || device[1] != ':' {
		return device
	}
	mount, err := windows.UTF16PtrFromString(device + `\`)
	if err != nil {
		return device
	}
	buf := make([]uint16, 1024)
	if err := windows.GetVolumeNameForVolumeMountPoint(mount, &buf[0], uint32(len(buf))); err != nil {
		return device
	}
	if s := windows.UTF16ToString(buf); s != "" {
		return s
	}
	return device
}
