package ntfs

// Schema v1 (PRD §6, "Схема данных"). One row = one file or directory;
// roots carry the per-(agent, volume) USN cursor; ad_cache/fetch_audit are
// hub-side enrichment and issuance audit.
//
// files.id is an explicit INTEGER PRIMARY KEY rather than the sketch's
// implicit rowid: files_fts is an external-content FTS5 table keyed on
// content_rowid, and only an explicit alias rowid is guaranteed stable
// across VACUUM — implicit rowids of a table keyed on (root_id, frn) may be
// renumbered, silently corrupting the FTS linkage.
const schemaV1 = `
CREATE TABLE meta(key TEXT PRIMARY KEY, value TEXT);

CREATE TABLE roots(
  id INTEGER PRIMARY KEY,
  agent TEXT NOT NULL,              -- file-server name (source marking, PRD §5)
  volume_guid TEXT NOT NULL,        -- \\?\Volume{...}\
  unc_base TEXT NOT NULL,           -- canonical UNC for issuance: \\fs01\photo$
  usn_cursor INTEGER NOT NULL DEFAULT 0,
  last_full_scan TEXT,
  file_count INTEGER,
  UNIQUE(agent, volume_guid)
);

CREATE TABLE files(
  id INTEGER PRIMARY KEY,
  root_id INTEGER NOT NULL REFERENCES roots,
  frn INTEGER NOT NULL,             -- file reference number (int64-safe: FRN = segment<<16|seq)
  parent_frn INTEGER NOT NULL,
  path TEXT NOT NULL,               -- assembled UNC path (unique within a root)
  name TEXT NOT NULL,
  name_lower TEXT NOT NULL,         -- Unicode-lowercased by Go, NOT SQL LOWER (PRD Э-0.6)
  ext TEXT,
  is_dir INTEGER NOT NULL DEFAULT 0,
  size INTEGER NOT NULL DEFAULT 0,
  ctime TEXT, mtime TEXT,           -- RFC3339
  attrs INTEGER,
  owner_sid TEXT,
  owner_sam TEXT, owner_display TEXT, owner_dept TEXT,  -- denormalized from ad_cache
  tags TEXT,                        -- folder-tag CSV (PRD §5)
  sd BLOB,                          -- self-relative SECURITY_DESCRIPTOR
  first_seen TEXT NOT NULL, last_seen TEXT NOT NULL,
  deleted_at TEXT,                  -- tombstone (PRD §5)
  UNIQUE(root_id, frn)
);
CREATE UNIQUE INDEX files_path ON files(root_id, path) WHERE deleted_at IS NULL;
CREATE INDEX files_parent ON files(root_id, parent_frn);
CREATE INDEX files_name_lower ON files(name_lower);

CREATE VIRTUAL TABLE files_fts USING fts5(
  name, content='files', content_rowid='id', tokenize='unicode61'
);
CREATE TRIGGER files_fts_ai AFTER INSERT ON files BEGIN
  INSERT INTO files_fts(rowid, name) VALUES (new.id, new.name);
END;
CREATE TRIGGER files_fts_ad AFTER DELETE ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name) VALUES ('delete', old.id, old.name);
END;
CREATE TRIGGER files_fts_au AFTER UPDATE OF name ON files BEGIN
  INSERT INTO files_fts(files_fts, rowid, name) VALUES ('delete', old.id, old.name);
  INSERT INTO files_fts(rowid, name) VALUES (new.id, new.name);
END;

CREATE TABLE ad_cache(
  sid TEXT PRIMARY KEY, sam TEXT, display TEXT, dept TEXT, mail TEXT,
  active INTEGER, fetched_at TEXT NOT NULL
);

CREATE TABLE fetch_audit(          -- who/what/when fetched a file (PRD §8)
  ts TEXT NOT NULL, user TEXT NOT NULL, root_id INTEGER NOT NULL,
  path TEXT NOT NULL, bytes INTEGER, status TEXT
);
`

// migrations apply in order, one transaction per step; PRAGMA user_version is
// the source of truth (meta.schema_version mirrors it for human readers).
var migrations = []struct {
	version int
	stmts   string
}{
	{1, schemaV1},
	// v2 (Э-0.3): journal identity per root. A changed JournalID means the
	// USN journal was recreated (wrap / admin deletion) — the delta cursor
	// is void and the agent must full-rescan (PRD §4.2).
	{2, `ALTER TABLE roots ADD COLUMN journal_id INTEGER;`},
}
