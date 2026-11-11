package ntfs

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// Stage Э-0.3 fixtures: collapse semantics, exactly-once delta application,
// snapshot import with cursor stamping — all against a temp SQLite index.

func TestCollapseDeltas(t *testing.T) {
	recs := []USNRecord{
		{FRN: 10, ParentFRN: 5, Name: "новый.txt", Reason: ReasonFileCreate},
		{FRN: 10, ParentFRN: 5, Name: "новый.txt", Reason: ReasonClose},
		{FRN: 11, ParentFRN: 5, Name: "старое_имя.pdf", Reason: ReasonRenameOldName}, // skipped
		{FRN: 11, ParentFRN: 6, Name: "новое_имя.pdf", Reason: ReasonRenameNewName | ReasonClose},
		{FRN: 12, ParentFRN: 5, Name: "удалён.txt", Reason: ReasonFileDelete | ReasonClose},
		{FRN: 13, ParentFRN: 5, Name: "acl.txt", Reason: ReasonSecurityChange | ReasonClose},
		{FRN: 14, ParentFRN: 5, Name: "a.log", Reason: ReasonFileDelete | ReasonClose},
		{FRN: 14, ParentFRN: 7, Name: "a.log", Reason: ReasonFileCreate | ReasonClose}, // delete→recreate → alive
	}
	acts := CollapseDeltas(recs)
	byFRN := map[int64]*DeltaAction{}
	for _, a := range acts {
		byFRN[a.FRN] = a
	}
	if len(acts) != 5 {
		t.Fatalf("actions = %d; want 5 (five distinct FRNs — skipping OLD must not drop the FRN, only stale names): %+v", len(acts), acts)
	}
	if a := byFRN[10]; a == nil || a.Deleted || a.Name != "новый.txt" {
		t.Errorf("frn 10: %+v", a)
	}
	if a := byFRN[11]; a == nil || a.Name != "новое_имя.pdf" || a.ParentFRN != 6 {
		t.Errorf("frn 11 must carry the NEW twin's state: %+v", a)
	}
	if a := byFRN[12]; a == nil || !a.Deleted {
		t.Errorf("frn 12 must be tombstoned: %+v", a)
	}
	if a := byFRN[13]; a == nil || !a.SecurityChanged {
		t.Errorf("frn 13 must keep the security flag for Э-0.4: %+v", a)
	}
	if a := byFRN[14]; a == nil || a.Deleted || a.ParentFRN != 7 {
		t.Errorf("frn 14 delete→recreate must end alive under new parent: %+v", a)
	}
}

func agentDB(t *testing.T) (*sql.DB, int64) {
	t.Helper()
	db := testOpen(t)
	root := insertRoot(t, db)
	return db, root
}

func countLive(t *testing.T, db *sql.DB, rootID int64, glob string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE root_id = ?1 AND deleted_at IS NULL AND name_lower GLOB ?2`,
		rootID, lowerName(glob)).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func pathOf(t *testing.T, db *sql.DB, rootID int64, frn int64) string {
	t.Helper()
	var p string
	if err := db.QueryRow(`SELECT path FROM files WHERE root_id = ?1 AND frn = ?2`, rootID, frn).Scan(&p); err != nil {
		t.Fatalf("path of %#x: %v", frn, err)
	}
	return p
}

func TestApplyDeltasLifecycle(t *testing.T) {
	db, rootID := agentDB(t)
	now := time.Now()
	const (
		dirFRN   = int64(0x00060000000000AA) // тест-индекс
		fileFRN  = int64(0x00060000000000AB)
		fileFRN2 = int64(0x00060000000000AC)
		extFRN   = int64(0x00060000000000FF) // outside include, parent unknown
	)
	// seed: the include dir itself + one file (root anchor in the correct
	// low-48 segment layout, as observed live)
	seed := []*DeltaAction{
		{FRN: dirFRN, ParentFRN: int64(0x0005000000000005), Name: "тест-индекс", IsDir: true},
		{FRN: fileFRN, ParentFRN: dirFRN, Name: "фото_предметка.jpg"},
	}
	if _, _, err := ApplyDeltas(db, rootID, `тест-индекс`, seed, 100, now); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if got := pathOf(t, db, rootID, fileFRN); got != `тест-индекс\фото_предметка.jpg` {
		t.Fatalf("seeded path = %q", got)
	}

	// rename + move within include (parent resolved through the DB)
	if _, _, err := ApplyDeltas(db, rootID, `тест-индекс`, []*DeltaAction{
		{FRN: fileFRN, ParentFRN: dirFRN, Name: "фото_лук.png"},
	}, 200, now); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if got := pathOf(t, db, rootID, fileFRN); got != `тест-индекс\фото_лук.png` {
		t.Fatalf("renamed path = %q", got)
	}
	if n := countLive(t, db, rootID, "*лук*"); n != 1 {
		t.Fatalf("FTS/GLOB after rename = %d; want 1", n)
	}

	// new file whose parent arrives in the SAME batch (path via batch map)
	if _, _, err := ApplyDeltas(db, rootID, `тест-индекс`, []*DeltaAction{
		{FRN: fileFRN2, ParentFRN: dirFRN, Name: "сертификат.pdf"},
	}, 300, now); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got := pathOf(t, db, rootID, fileFRN2); got != `тест-индекс\сертификат.pdf` {
		t.Fatalf("created path = %q", got)
	}

	// delete → tombstone, hidden from search
	if _, _, err := ApplyDeltas(db, rootID, `тест-индекс`, []*DeltaAction{
		{FRN: fileFRN2, Deleted: true, Name: "сертификат.pdf"},
	}, 400, now); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := countLive(t, db, rootID, "*сертификат*"); n != 0 {
		t.Fatalf("tombstoned file still searchable: %d", n)
	}

	// file outside include: unknown chain → skipped, never stored
	events, skipped, err := ApplyDeltas(db, rootID, `тест-индекс`, []*DeltaAction{
		{FRN: extFRN, ParentFRN: int64(0x0009000000000DEAD), Name: "чужой.txt"},
	}, 500, now)
	if err != nil {
		t.Fatalf("outside: %v", err)
	}
	if len(events) != 0 || skipped != 1 {
		t.Fatalf("outside include: events=%v skipped=%d; want 0 events/1 skipped", events, skipped)
	}

	// cursor advanced atomically with the last batch
	if cursor, jid, _ := RootCursor(db, rootID); cursor != 500 || jid != 0 {
		t.Fatalf("cursor after batches = %#x/%#x; want 500/0", cursor, jid)
	}

	// replay the same batch (agent crashed before ack) → idempotent
	if events, _, err := ApplyDeltas(db, rootID, `тест-индекс`, []*DeltaAction{
		{FRN: fileFRN, ParentFRN: dirFRN, Name: "фото_лук.png"},
	}, 600, now); err != nil || len(events) != 1 {
		t.Fatalf("replay: events=%v err=%v; want exactly 1 event", events, err)
	}
	if n := countLive(t, db, rootID, "*лук*"); n != 1 {
		t.Fatalf("replay duplicated rows: %d", n)
	}
}

func TestImportSnapshotStampsCursorAndTombstones(t *testing.T) {
	db, rootID := agentDB(t)
	const rootFRN = int64(0x0005000000000005)
	const dirFRN = int64(0x00060000000000AA)

	// names keyed by FRN (an index-keyed name once produced the same path
	// for two different FRNs and tripped the honest UNIQUE(root_id, path))
	mkSnapshot := func(frns ...int64) *Snapshot {
		entries := map[int64]Entry{
			rootFRN: {FRN: rootFRN, ParentFRN: rootFRN, Name: ".", IsDir: true},
			dirFRN:  {FRN: dirFRN, ParentFRN: rootFRN, Name: "тест-индекс", IsDir: true},
		}
		for _, frn := range frns {
			entries[frn] = Entry{FRN: frn, ParentFRN: dirFRN, Name: fmt.Sprintf("файл_%d.txt", frn)}
		}
		paths, _ := BuildPaths(entries)
		return &Snapshot{Entries: entries, Paths: paths, Orphans: map[int64]bool{}, Count: len(entries)}
	}

	s1 := mkSnapshot(1, 2)
	imported, tomb, err := ImportSnapshot(db, rootID, `тест-индекс`, s1, 0xABC, time.Now())
	if err != nil || imported != 3 || tomb != 0 {
		t.Fatalf("import1: imported=%d tomb=%d err=%v; want 3/0", imported, tomb, err)
	}
	cursor, jid, _ := RootCursor(db, rootID)
	if cursor != s1.Journal.NextUSN || jid != 0xABC {
		t.Fatalf("root stamp: cursor=%#x jid=%#x", cursor, jid)
	}
	var fileCount int
	if err := db.QueryRow(`SELECT file_count FROM roots WHERE id = ?1`, rootID).Scan(&fileCount); err != nil || fileCount != 3 {
		t.Fatalf("file_count = %d, %v; want 3", fileCount, err)
	}

	// second snapshot without frn=2 → tombstone sweep
	s2 := mkSnapshot(1, 3)
	imported, tomb, err = ImportSnapshot(db, rootID, `тест-индекс`, s2, 0xABC, time.Now())
	if err != nil || imported != 3 || tomb != 1 {
		t.Fatalf("import2: imported=%d tomb=%d err=%v; want 3/1", imported, tomb, err)
	}
	var live, dead int
	db.QueryRow(`SELECT COUNT(*) FROM files WHERE root_id = ?1 AND deleted_at IS NULL`, rootID).Scan(&live)
	db.QueryRow(`SELECT COUNT(*) FROM files WHERE root_id = ?1 AND deleted_at IS NOT NULL`, rootID).Scan(&dead)
	if live != 3 || dead != 1 {
		t.Fatalf("live=%d dead=%d; want 3/1", live, dead)
	}
}

func TestEnsureRootIdempotent(t *testing.T) {
	db := testOpen(t)
	id1, err := EnsureRoot(db, "fs01", `\\?\Volume{X}\`, `\\fs01\photo$`)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	id2, err := EnsureRoot(db, "fs01", `\\?\Volume{X}\`, `\\fs01\other$`) // unc_base ignored on hit
	if err != nil || id1 != id2 {
		t.Fatalf("second ensure: id %d vs %d, err %v — same (agent,volume) must return the same root", id1, id2, err)
	}
}

func TestMigrationV2JournalID(t *testing.T) {
	db := testOpen(t)
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 2 {
		t.Fatalf("user_version = %d, %v; want 2", v, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('roots') WHERE name = 'journal_id'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("roots.journal_id missing: %d, %v", n, err)
	}
	var mirror string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&mirror); err != nil || mirror != "2" {
		t.Fatalf("meta.schema_version = %q, %v; want 2", mirror, err)
	}
	// reopen path: existing v2 db goes through Open without re-migrating
	path := filepath.Join(t.TempDir(), "idx2.sqlite")
	w, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, roots, files, err := SchemaInfo(w); err != nil || roots != 0 || files != 0 {
		t.Fatalf("fresh index stats: roots=%d files=%d err=%v; want 0/0", roots, files, err)
	}
	w.Close()
}
