package ntfs

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// USN delta pipeline (PRD §4.2, Э-0.3). The journal stream is VOLUME-WIDE
// (live Q 2026-09-20: C: deltas arrive for every file on the disk), so raw
// records are first collapsed per FRN, and only actions whose resolved path
// falls under the include filter ever reach the database.

// DeltaAction is the collapsed last-known state of one file within a batch.
type DeltaAction struct {
	FRN       int64
	ParentFRN int64
	Name      string
	IsDir     bool
	Deleted   bool
	// SecurityChanged records a USN_REASON_SECURITY_CHANGE — Э-0.4 consumes
	// it to refresh the stored SD (wired through now so the flag survives
	// collapse).
	SecurityChanged bool
}

// CollapseDeltas reduces raw USN records, in journal order, to one action
// per FRN (last state wins: delete→recreate resolves to alive). Pure
// function over the shared USNRecord parser — fixture-testable.
//
// RENAME_OLD_NAME records are skipped: their RENAME_NEW_NAME twin carries
// the move. A trailing OLD without NEW means the cursor cut an operation in
// half — the next batch completes it (PRD Э-0.3 rake list).
func CollapseDeltas(recs []USNRecord) []*DeltaAction {
	m := make(map[int64]*DeltaAction, len(recs))
	for _, r := range recs {
		isOld := r.Reason&ReasonRenameOldName != 0
		isNew := r.Reason&ReasonRenameNewName != 0
		if isOld && !isNew && r.Reason&(ReasonFileCreate|ReasonFileDelete) == 0 {
			continue
		}
		a := m[r.FRN]
		if a == nil {
			a = &DeltaAction{FRN: r.FRN}
			m[r.FRN] = a
		}
		a.Deleted = r.Reason&ReasonFileDelete != 0 && r.Reason&ReasonFileCreate == 0
		if r.Reason&ReasonSecurityChange != 0 {
			a.SecurityChanged = true
		}
		a.ParentFRN, a.Name, a.IsDir = r.ParentFRN, r.Name, r.IsDir()
	}
	out := make([]*DeltaAction, 0, len(m))
	for _, a := range m {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FRN < out[j].FRN })
	return out
}

const upsertFileSQL = `INSERT INTO files(root_id, frn, parent_frn, path, name, name_lower, ext, is_dir, size, first_seen, last_seen)
VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, ?8, 0, ?9, ?9)
ON CONFLICT(root_id, frn) DO UPDATE SET
	parent_frn = excluded.parent_frn, path = excluded.path,
	name = excluded.name, name_lower = excluded.name_lower, ext = excluded.ext,
	is_dir = excluded.is_dir, last_seen = excluded.last_seen, deleted_at = NULL`

// ApplyDeltas writes one collapsed batch and advances the root's USN cursor
// IN THE SAME TRANSACTION — exactly-once across agent restarts: a crash
// before commit re-reads the same journal range and replays idempotent
// upserts (PRD Э-0.3). Returns human-readable events ("+ path" new/updated,
// "- path" tombstoned) for the agent log plus the skipped count.
//
// Paths are resolved from the batch itself, then live DB rows (parent dir
// path + name); actions whose chain resolves outside the map — i.e. files
// outside the include scope — are skipped. A file that MOVED out of include
// is tombstoned (it left the indexed view); a delete of an unknown FRN is
// skipped (it was never stored).
func ApplyDeltas(db *sql.DB, rootID int64, include string, actions []*DeltaAction, nextUSN int64, now time.Time) (events []string, skipped int, err error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()

	batch := make(map[int64]*DeltaAction, len(actions))
	for _, a := range actions {
		if !a.Deleted {
			batch[a.FRN] = a
		}
	}
	dbPaths := make(map[int64]string)
	missing := make(map[int64]bool)
	var dbErr error
	getDBPath := func(frn int64) (string, bool) {
		if p, ok := dbPaths[frn]; ok {
			return p, true
		}
		if missing[frn] {
			return "", false
		}
		var p string
		switch err := tx.QueryRow(`SELECT path FROM files WHERE root_id = ?1 AND frn = ?2 AND deleted_at IS NULL`,
			rootID, frn).Scan(&p); {
		case err == nil:
			dbPaths[frn] = p
			return p, true
		case err == sql.ErrNoRows:
			missing[frn] = true
			return "", false
		default:
			dbErr = fmt.Errorf("parent lookup %#x: %w", frn, err)
			return "", false
		}
	}
	var resolve func(frn int64, seen map[int64]bool) (string, bool)
	resolve = func(frn int64, seen map[int64]bool) (string, bool) {
		if seen[frn] {
			return "", false
		}
		if frnSegment(frn) == 5 {
			return "", true // volume-root anchor
		}
		if a, ok := batch[frn]; ok {
			seen[frn] = true
			parent, ok := resolve(a.ParentFRN, seen)
			if !ok {
				return "", false
			}
			if parent == "" {
				return a.Name, true
			}
			return parent + `\` + a.Name, true
		}
		return getDBPath(frn)
	}

	tombstone := func(frn int64) error {
		res, err := tx.Exec(`UPDATE files SET deleted_at = ?1 WHERE root_id = ?2 AND frn = ?3 AND deleted_at IS NULL`,
			now.Format(time.RFC3339), rootID, frn)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n > 0 {
			var p string
			if err := tx.QueryRow(`SELECT path FROM files WHERE root_id = ?1 AND frn = ?2`, rootID, frn).Scan(&p); err == nil {
				events = append(events, "- "+p)
			} else {
				events = append(events, fmt.Sprintf("- frn %#x", frn))
			}
		} else {
			skipped++
		}
		return nil
	}

	for _, a := range actions {
		if a.Deleted {
			if err := tombstone(a.FRN); err != nil {
				return events, skipped, err
			}
			continue
		}
		path, ok := resolve(a.FRN, map[int64]bool{})
		if dbErr != nil {
			return events, skipped, dbErr
		}
		if !ok || !MatchInclude(path, include) {
			// outside the indexed scope: drop if never stored, tombstone if
			// it just moved out of the include subtree
			if ok {
				if err := tombstone(a.FRN); err != nil {
					return events, skipped, err
				}
			} else {
				skipped++
			}
			continue
		}
		if _, err := tx.Exec(upsertFileSQL, rootID, a.FRN, a.ParentFRN, path, a.Name,
			lowerName(a.Name), extOf(a.Name), a.IsDir, now.Format(time.RFC3339)); err != nil {
			return events, skipped, fmt.Errorf("upsert %q: %w", path, err)
		}
		events = append(events, "+ "+path)
	}

	if _, err := tx.Exec(`UPDATE roots SET usn_cursor = ?1 WHERE id = ?2`, nextUSN, rootID); err != nil {
		return events, skipped, err
	}
	if err := tx.Commit(); err != nil {
		return events, skipped, err
	}
	return events, skipped, nil
}
