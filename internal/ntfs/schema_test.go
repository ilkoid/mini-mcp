package ntfs

import (
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Stage Э-0.1 fixtures (PRD §13): Cyrillic names with the shapes the search
// scenarios actually produce — token-separated compound names, 8-digit
// artikuls, mixed case, extensions.

func testOpen(t *testing.T) *sql.DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "idx.sqlite"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertRoot(t *testing.T, db *sql.DB) int64 {
	t.Helper()
	res, err := db.Exec(`INSERT INTO roots(agent, volume_guid, unc_base)
		VALUES('fs01', '\\?\Volume{TEST}\', '\\fs01\photo$')`)
	if err != nil {
		t.Fatalf("insert root: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

func insertFile(t *testing.T, db *sql.DB, rootID int64, frn, parent int64, name string) {
	t.Helper()
	now := time.Now().UTC().Format(time.RFC3339)
	ext := ""
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		ext = strings.ToLower(name[i+1:])
	}
	_, err := db.Exec(`INSERT INTO files(root_id, frn, parent_frn, path, name, name_lower, ext, size, first_seen, last_seen)
		VALUES(?1, ?2, ?3, ?4, ?5, ?6, ?7, 0, ?8, ?8)`,
		rootID, frn, parent, `\fs01\photo$\`+name, name, lowerName(name), ext, now)
	if err != nil {
		t.Fatalf("insert file %q: %v", name, err)
	}
}

func ftsCount(t *testing.T, db *sql.DB, match string) int {
	t.Helper()
	var n int
	// join back to files and respect tombstones, as ntfs_search will (PRD §7.1)
	if err := db.QueryRow(`SELECT COUNT(*) FROM files_fts f JOIN files ON files.id = f.rowid
		WHERE files_fts MATCH ?1 AND files.deleted_at IS NULL`, match).Scan(&n); err != nil {
		t.Fatalf("fts match %q: %v", match, err)
	}
	return n
}

func TestMigrateCreatesSchema(t *testing.T) {
	db := testOpen(t)
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil || v != 2 {
		t.Fatalf("user_version = %d, %v; want 2", v, err)
	}
	for _, obj := range []string{"meta", "roots", "files", "files_fts", "ad_cache", "fetch_audit",
		"files_fts_ai", "files_fts_ad", "files_fts_au"} {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = ?1`, obj).Scan(&n); err != nil || n != 1 {
			t.Fatalf("object %s: count=%d err=%v; want 1", obj, n, err)
		}
	}
	var mirror string
	if err := db.QueryRow(`SELECT value FROM meta WHERE key = 'schema_version'`).Scan(&mirror); err != nil || mirror != "2" {
		t.Fatalf("meta.schema_version = %q, %v; want 2", mirror, err)
	}
}

func TestMigrateIdempotent(t *testing.T) {
	db := testOpen(t)
	if err := migrate(db); err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	v, roots, files, err := SchemaInfo(db)
	if err != nil || v != 2 || roots != 0 || files != 0 {
		t.Fatalf("SchemaInfo after re-migrate = %d/%d/%d, %v; want 2/0/0", v, roots, files, err)
	}
}

func TestFTS5CyrillicTokenization(t *testing.T) {
	db := testOpen(t)
	root := insertRoot(t, db)
	insertFile(t, db, root, 1, 0, "фото_предметка_12345678.jpg")
	insertFile(t, db, root, 2, 0, "Отчёт по весне.pdf")
	insertFile(t, db, root, 3, 0, "АРТИКУЛ 87654321.PNG")

	cases := []struct {
		match string
		want  int
	}{
		{"предметка", 1},    // token inside an underscore-separated compound name
		{"ПРЕДМЕТКА", 1},    // tokenizer folds case, Cyrillic included
		{"предмет*", 1},     // prefix query
		{"весне", 1},        // plain Cyrillic token
		{"отчёт", 1},        // ё survives tokenization
		{"12345678", 1},     // digit run is a token
		{"87654321", 1},
		{"артикул", 1},      // stored upper, matched lower
		{"нетакоготокена", 0},
	}
	for _, c := range cases {
		if n := ftsCount(t, db, c.match); n != c.want {
			t.Errorf("MATCH %q = %d rows; want %d", c.match, n, c.want)
		}
	}
}

func TestFTSRenameResync(t *testing.T) {
	db := testOpen(t)
	root := insertRoot(t, db)
	insertFile(t, db, root, 7, 0, "старое_имя_файла.pdf")

	if n := ftsCount(t, db, "старое"); n != 1 {
		t.Fatalf("before rename: MATCH старое = %d; want 1", n)
	}
	if _, err := db.Exec(`UPDATE files SET name = 'переименован.pdf', name_lower = ?1 WHERE frn = 7`,
		lowerName("переименован.pdf")); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if n := ftsCount(t, db, "старое"); n != 0 {
		t.Errorf("after rename: MATCH старое = %d; want 0 (stale FTS row)", n)
	}
	if n := ftsCount(t, db, "переименован"); n != 1 {
		t.Errorf("after rename: MATCH переименован = %d; want 1", n)
	}
}

func TestUpsertKeepsRowidAndFTS(t *testing.T) {
	db := testOpen(t)
	root := insertRoot(t, db)
	now := time.Now().UTC().Format(time.RFC3339)

	insertFile(t, db, root, 9, 0, "первый_вариант.docx")
	var idBefore int64
	if err := db.QueryRow(`SELECT id FROM files WHERE root_id = ?1 AND frn = 9`, root).Scan(&idBefore); err != nil {
		t.Fatalf("id before: %v", err)
	}
	// the agent's batch shape (PRD §4.2): same (root_id, frn), refreshed name/last_seen
	if _, err := db.Exec(`INSERT INTO files(root_id, frn, parent_frn, path, name, name_lower, ext, size, first_seen, last_seen)
		VALUES(?1, 9, 0, ?2, 'второй_вариант.docx', ?3, 'docx', 0, ?4, ?4)
		ON CONFLICT(root_id, frn) DO UPDATE SET name = excluded.name, name_lower = excluded.name_lower,
			path = excluded.path, last_seen = excluded.last_seen`,
		root, `\fs01\photo$\второй_вариант.docx`, lowerName("второй_вариант.docx"), now); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	var idAfter int64
	if err := db.QueryRow(`SELECT id FROM files WHERE root_id = ?1 AND frn = 9`, root).Scan(&idAfter); err != nil {
		t.Fatalf("id after: %v", err)
	}
	if idBefore != idAfter {
		t.Fatalf("rowid changed on upsert: %d → %d (breaks external-content FTS)", idBefore, idAfter)
	}
	if n := ftsCount(t, db, "вариант"); n != 1 {
		t.Errorf("MATCH вариант = %d; want exactly 1 (no stale or duplicate FTS rows)", n)
	}
	if n := ftsCount(t, db, "первый"); n != 0 {
		t.Errorf("MATCH первый = %d; want 0 (old name leaked in FTS)", n)
	}
}

func TestGLOBCyrillic(t *testing.T) {
	db := testOpen(t)
	root := insertRoot(t, db)
	insertFile(t, db, root, 1, 0, "фото_предметка_12345678.jpg")
	insertFile(t, db, root, 2, 0, "АРТИКУЛ 87654321.PNG")

	// pattern search path (PRD §7.1): GLOB over Go-lowercased name_lower,
	// pattern folded the same way — SQLite GLOB itself is case-sensitive
	globCount := func(pattern string) int {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE name_lower GLOB ?1 AND deleted_at IS NULL`,
			lowerName(pattern)).Scan(&n); err != nil {
			t.Fatalf("glob %q: %v", pattern, err)
		}
		return n
	}
	if n := globCount("*предметка*"); n != 1 {
		t.Errorf("GLOB *предметка* = %d; want 1", n)
	}
	if n := globCount("*8765432?.png"); n != 1 {
		t.Errorf("GLOB *8765432?.png = %d; want 1 (artikul mask)", n)
	}
	if n := globCount("*артикул*"); n != 1 {
		t.Errorf("GLOB *артикул* = %d; want 1 (query fold must match stored fold)", n)
	}
}

func TestTombstoneHidesFromSearch(t *testing.T) {
	db := testOpen(t)
	root := insertRoot(t, db)
	insertFile(t, db, root, 1, 0, "сертификат_скрыт.pdf")
	now := time.Now().UTC().Format(time.RFC3339)
	if _, err := db.Exec(`UPDATE files SET deleted_at = ?1 WHERE frn = 1`, now); err != nil {
		t.Fatalf("tombstone: %v", err)
	}
	if n := ftsCount(t, db, "сертификат"); n != 0 {
		t.Errorf("MATCH сертификат after tombstone = %d; want 0", n)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM files WHERE name_lower GLOB '*сертификат*' AND deleted_at IS NULL`).Scan(&n); err != nil || n != 0 {
		t.Errorf("GLOB after tombstone = %d, %v; want 0", n, err)
	}
}

func TestHardDeletePurgesFTS(t *testing.T) {
	db := testOpen(t)
	root := insertRoot(t, db)
	insertFile(t, db, root, 1, 0, "удаляемый_файл.txt")
	if n := ftsCount(t, db, "удаляемый"); n != 1 {
		t.Fatalf("before delete: %d; want 1", n)
	}
	if _, err := db.Exec(`DELETE FROM files WHERE frn = 1`); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n := ftsCount(t, db, "удаляемый"); n != 0 {
		t.Errorf("after hard delete: %d; want 0 (trigger must purge FTS)", n)
	}
}

func TestReaderReadOnlyAndWALVisibility(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.sqlite")

	// writer stays open, as the hub does (PRD §3)
	w, err := Open(path)
	if err != nil {
		t.Fatalf("writer Open: %v", err)
	}
	defer w.Close()
	root := insertRoot(t, w)
	insertFile(t, w, root, 1, 0, "видимый_в_wal.pdf")

	r, err := OpenReader(path)
	if err != nil {
		t.Fatalf("OpenReader: %v", err)
	}
	defer r.Close()

	// WAL visibility: reader sees the writer's committed rows while the
	// writer connection is still open — the property mcp-ntfs search lives on
	var n int
	if err := r.QueryRow(`SELECT COUNT(*) FROM files WHERE name_lower GLOB '*wal*'`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("reader WAL visibility: %d, %v; want 1", n, err)
	}
	// readers never write: both mode=ro and query_only(1) must refuse
	if _, err := r.Exec(`INSERT INTO files(root_id, frn, parent_frn, path, name, name_lower, first_seen, last_seen)
		VALUES(1, 99, 0, 'x', 'x', 'x', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err == nil {
		t.Fatal("reader INSERT unexpectedly succeeded")
	}
}

func TestOpenReaderRefusesEmptyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.sqlite")
	if _, err := OpenReader(path); err == nil {
		t.Fatal("OpenReader on a missing/empty file unexpectedly succeeded (readers must never create/migrate)")
	}
}
