package ntfs

import (
	"database/sql"
	"fmt"
	"time"
)

// Root and snapshot persistence (PRD §5/Э-0.3). The hub is the sole writer;
// these helpers run inside its connection.

// EnsureRoot finds or creates the (agent, volume_guid) row. The agent name
// IS the source marking — data in the index always carries its server
// (PRD §5, question "from which server did the data arrive").
func EnsureRoot(db *sql.DB, agent, volumeGUID, uncBase string) (int64, error) {
	var id int64
	err := db.QueryRow(`SELECT id FROM roots WHERE agent = ?1 AND volume_guid = ?2`, agent, volumeGUID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if err != sql.ErrNoRows {
		return 0, err
	}
	res, err := db.Exec(`INSERT INTO roots(agent, volume_guid, unc_base) VALUES(?1, ?2, ?3)`,
		agent, volumeGUID, uncBase)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// RootCursor returns the persisted delta position: usn_cursor plus the
// journal identity it was taken under (0 = no snapshot yet).
func RootCursor(db *sql.DB, rootID int64) (cursor, journalID int64, err error) {
	err = db.QueryRow(`SELECT usn_cursor, COALESCE(journal_id, 0) FROM roots WHERE id = ?1`, rootID).
		Scan(&cursor, &journalID)
	return
}

// ImportSnapshot persists an include-filtered MFT snapshot: upserts matched
// entries in chunked transactions (10⁴ rows, Э-0.9 rake), tombstones live
// rows that vanished since the previous snapshot, and stamps the root with
// the journal identity + the snapshot-time NextUSN cursor. Because
// Snapshot() captures JournalInfo BEFORE the MFT walk, changes that happen
// during the walk are replayed from that cursor by the delta loop — nothing
// is lost (PRD Э-0.3 rake).
func ImportSnapshot(db *sql.DB, rootID int64, include string, snap *Snapshot, journalID int64, now time.Time) (imported, tombstoned int, err error) {
	root := RootFRN(snap.Entries)
	nowStr := now.Format(time.RFC3339)

	live := make(map[int64]bool)
	rows, err := db.Query(`SELECT frn FROM files WHERE root_id = ?1 AND deleted_at IS NULL`, rootID)
	if err != nil {
		return 0, 0, err
	}
	for rows.Next() {
		var frn int64
		if err := rows.Scan(&frn); err != nil {
			rows.Close()
			return 0, 0, err
		}
		live[frn] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, 0, err
	}

	var (
		tx   *sql.Tx
		stmt *sql.Stmt
		n    int
	)
	beginChunk := func() error {
		var err error
		tx, err = db.Begin()
		if err != nil {
			return err
		}
		stmt, err = tx.Prepare(upsertFileSQL)
		return err
	}
	endChunk := func() error {
		if stmt != nil {
			stmt.Close()
			stmt = nil
		}
		if tx != nil {
			return tx.Commit()
		}
		return nil
	}
	if err := beginChunk(); err != nil {
		return 0, 0, err
	}
	for frn, e := range snap.Entries {
		if frn == root {
			continue
		}
		p := snap.Paths[frn]
		if p == "" || !MatchInclude(p, include) {
			continue
		}
		if _, err := stmt.Exec(rootID, frn, e.ParentFRN, p, e.Name,
			lowerName(e.Name), extOf(e.Name), e.IsDir, nowStr); err != nil {
			endChunk()
			return imported, tombstoned, fmt.Errorf("import %q: %w", p, err)
		}
		delete(live, frn)
		imported++
		if n++; n%10000 == 0 {
			if err := endChunk(); err != nil {
				return imported, tombstoned, err
			}
			if err := beginChunk(); err != nil {
				return imported, tombstoned, err
			}
		}
	}
	// tombstone rows that disappeared from the snapshot
	for frn := range live {
		if _, err := tx.Exec(`UPDATE files SET deleted_at = ?1 WHERE root_id = ?2 AND frn = ?3 AND deleted_at IS NULL`,
			nowStr, rootID, frn); err != nil {
			endChunk()
			return imported, tombstoned, err
		}
		tombstoned++
	}
	if _, err := tx.Exec(`UPDATE roots SET usn_cursor = ?1, journal_id = ?2, last_full_scan = ?3, file_count = ?4
		WHERE id = ?5`, snap.Journal.NextUSN, journalID, nowStr, imported, rootID); err != nil {
		endChunk()
		return imported, tombstoned, err
	}
	if err := endChunk(); err != nil {
		return imported, tombstoned, err
	}
	return imported, tombstoned, nil
}
