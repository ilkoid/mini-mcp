// Package ntfs — plain data types shared across build tags. These structs
// carry no syscalls, so they live untagged: the storage core (store.go, db.go,
// paths.go, usn.go) compiles on every platform, while the syscall side
// (volume.go, journal.go) stays windows-only. Moving a struct here is the fix
// whenever untagged code needs a type that happened to be born in a tagged
// file (build broke on darwin: `undefined: Snapshot`).
package ntfs

// JournalInfo mirrors the leading fields of USN_JOURNAL_DATA_V0/V1 (they
// share a prefix; extra V1/V2 fields are ignored).
type JournalInfo struct {
	Absent    bool
	JournalID int64 // changes when the journal is recreated → full rescan (wrap)
	FirstUSN  int64
	NextUSN   int64
}

// Snapshot is the Э-0.2 full-scan result. Journal is captured FIRST so the
// delta stream (Э-0.3) can start exactly at NextUSN — changes during the
// scan are then replayed on top instead of being lost (Э-0.3 rake).
type Snapshot struct {
	Journal JournalInfo
	Entries map[int64]Entry // $-filtered, includes dirs and the root anchor
	Paths   map[int64]string
	Orphans map[int64]bool
	Count   int // raw records seen (pre-filter)
}
