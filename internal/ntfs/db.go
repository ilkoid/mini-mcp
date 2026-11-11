// Package ntfs — the shared storage core of the MCP NTFS index (PRD
// reports/2026-09-20_PRD_MCP_NTFS.md): SQLite schema (§6), migrations, and
// the two open modes the architecture (§3) is built on — one writer (the
// hub: ntfs-indexer --role hub) and many read-only WAL readers (mcp-ntfs,
// launched per-user by ZCode). Keeping open semantics in one place is what
// lets that split coexist (§10): the hub serializes all writes through
// Open(), the MCP side never migrates and never writes (OpenReader).
package ntfs

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// dsn builds a modernc.org/sqlite connection string. Windows drive letters
// survive the "file:" URI only in slash form, hence ToSlash (E:\x\y →
// E:/x/y). Pragmas ride the DSN so they apply on every (re)connect.
func dsn(path string, readonly bool) string {
	p := filepath.ToSlash(path)
	if readonly {
		return "file:" + p + "?mode=ro&_pragma=busy_timeout(10000)&_pragma=query_only(1)"
	}
	return "file:" + p + "?_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(1)"
}

// Open opens (creating if needed) the index for the single writer and brings
// the schema to the latest version. The hub is the only process that may
// call this. MaxOpenConns=1 keeps SQLite's write lock discipline explicit.
func Open(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return db, nil
}

// OpenReader opens the index read-only for the MCP process. It refuses a
// database with no schema (readers must never create or migrate) and adds
// query_only on top of mode=ro as a second belt. WAL readers additionally
// need the -wal/-shm ACLs of PRD §10 (variant A).
func OpenReader(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		return nil, err
	}
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		db.Close()
		return nil, fmt.Errorf("open index %s: %w", path, err)
	}
	if v == 0 {
		db.Close()
		return nil, fmt.Errorf("open index %s: not an ntfs index (schema_version 0); run the hub/migrator first", path)
	}
	return db, nil
}

// migrate applies pending migration steps. Each step is one transaction;
// PRAGMA user_version inside the transaction makes version and DDL commit
// atomically (a half-applied step is never observed).
func migrate(db *sql.DB) error {
	var v int
	if err := db.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	for _, m := range migrations {
		if m.version <= v {
			continue
		}
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(m.stmts); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
		if _, err := tx.Exec("PRAGMA user_version = " + strconv.Itoa(m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (user_version): %w", m.version, err)
		}
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES('schema_version', ?1)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, strconv.Itoa(m.version)); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d (meta): %w", m.version, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("migration %d: %w", m.version, err)
		}
	}
	return nil
}

// SchemaInfo reports the applied schema version and live row counts
// (--migrate output, selftest, ntfs_status).
func SchemaInfo(db *sql.DB) (version, roots, files int, err error) {
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM roots").Scan(&roots); err != nil {
		return
	}
	err = db.QueryRow("SELECT COUNT(*) FROM files WHERE deleted_at IS NULL").Scan(&files)
	return
}

// lowerName is the canonical case fold for search (PRD §6, Э-0.6): Go's
// Unicode-aware ToLower, NOT SQL LOWER — SQLite's LOWER is ASCII-only and
// would break Cyrillic matching. Store with lowerName, fold query patterns
// with it too.
func lowerName(s string) string {
	return strings.ToLower(s)
}

// extOf returns the lowercased extension without the dot, "" when absent.
func extOf(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 && i < len(name)-1 {
		return lowerName(name[i+1:])
	}
	return ""
}
