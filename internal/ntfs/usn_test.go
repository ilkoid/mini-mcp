package ntfs

import (
	"encoding/binary"
	"testing"
	"time"
	"unicode/utf16"
)

// Stage Э-0.2 fixtures: USN_RECORD_V2/V3 buffers built by hand — the parser
// must be verifiable without a volume (PRD §13: parse on fixtures first).

// buildV2 encodes one USN_RECORD_V2 with an 8-byte-aligned length.
func buildV2(t *testing.T, frn, parent, usn, ts int64, reason, attrs uint32, name string) []byte {
	t.Helper()
	uname := utf16.Encode([]rune(name))
	nameLen := len(uname) * 2
	total := 60 + nameLen
	padded := (total + 7) &^ 7
	rec := make([]byte, padded)
	binary.LittleEndian.PutUint32(rec[0:], uint32(padded))
	binary.LittleEndian.PutUint16(rec[4:], 2)
	binary.LittleEndian.PutUint16(rec[6:], 0)
	binary.LittleEndian.PutUint64(rec[8:], uint64(frn))
	binary.LittleEndian.PutUint64(rec[16:], uint64(parent))
	binary.LittleEndian.PutUint64(rec[24:], uint64(usn))
	binary.LittleEndian.PutUint64(rec[32:], uint64(ts))
	binary.LittleEndian.PutUint32(rec[40:], reason)
	binary.LittleEndian.PutUint32(rec[52:], attrs)
	binary.LittleEndian.PutUint16(rec[56:], uint16(nameLen))
	binary.LittleEndian.PutUint16(rec[58:], 60)
	for i, u := range uname {
		binary.LittleEndian.PutUint16(rec[60+i*2:], u)
	}
	return rec
}

func TestParseUSNRecordV2(t *testing.T) {
	buf := buildV2(t,
		0x000300000000ABCD, // large FRN pattern: segment 0x30000, seq bytes
		5, 0xDFC29000, 0x01DCA7111A8B5A97,
		ReasonFileCreate|ReasonClose, 0,
		"отчёт_предметка_весна.pdf")

	rec, n, err := ParseUSNRecord(buf, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rec.FRN != 0x000300000000ABCD || rec.ParentFRN != 5 || rec.USN != 0xDFC29000 {
		t.Fatalf("ids: %+v", rec)
	}
	if rec.TimeStamp != 0x01DCA7111A8B5A97 || rec.Reason != ReasonFileCreate|ReasonClose {
		t.Fatalf("stamp/reason: %+v", rec)
	}
	if rec.Name != "отчёт_предметка_весна.pdf" {
		t.Fatalf("name = %q (Cyrillic must survive UTF-16 decode)", rec.Name)
	}
	if rec.IsDir() {
		t.Fatal("attrs=0 reported as dir")
	}
	if n != len(buf) {
		t.Fatalf("record length = %d; want padded %d", n, len(buf))
	}
}

func TestParseUSNRecordV2Directory(t *testing.T) {
	buf := buildV2(t, 100, 5, 1, 1, ReasonBasicInfoChange, FileAttributeDirectory, "Фото")
	rec, _, err := ParseUSNRecord(buf, 0)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rec.IsDir() {
		t.Fatal("FILE_ATTRIBUTE_DIRECTORY not decoded")
	}
}

func TestParseUSNRecordV3(t *testing.T) {
	name := "refs_том.docx"
	uname := utf16.Encode([]rune(name))
	total := (76 + len(uname)*2 + 7) &^ 7
	rec := make([]byte, total)
	binary.LittleEndian.PutUint32(rec[0:], uint32(total))
	binary.LittleEndian.PutUint16(rec[4:], 3)
	binary.LittleEndian.PutUint64(rec[8:], 42)  // low 64 of FILE_ID_128
	binary.LittleEndian.PutUint64(rec[24:], 43) // parent low 64
	binary.LittleEndian.PutUint64(rec[40:], 777)
	binary.LittleEndian.PutUint64(rec[48:], 555)
	binary.LittleEndian.PutUint32(rec[56:], ReasonFileDelete)
	binary.LittleEndian.PutUint32(rec[68:], 0)
	binary.LittleEndian.PutUint16(rec[72:], uint16(len(uname)*2))
	binary.LittleEndian.PutUint16(rec[74:], 76)
	for i, u := range uname {
		binary.LittleEndian.PutUint16(rec[76+i*2:], u)
	}

	got, n, err := ParseUSNRecord(rec, 0)
	if err != nil {
		t.Fatalf("parse v3: %v", err)
	}
	if got.FRN != 42 || got.ParentFRN != 43 || got.USN != 777 || got.Reason != ReasonFileDelete || got.Name != name {
		t.Fatalf("v3 fields: %+v", got)
	}
	if n != total {
		t.Fatalf("v3 length = %d; want %d", n, total)
	}
}

func TestParseUSNRecordRejectsBadInput(t *testing.T) {
	good := buildV2(t, 1, 5, 1, 1, 0, 0, "ok.txt")
	if _, _, err := ParseUSNRecord(good[:len(good)-2], 0); err == nil {
		t.Fatal("truncated buffer unexpectedly parsed")
	}
	zero := make([]byte, 64)
	if _, _, err := ParseUSNRecord(zero, 0); err == nil {
		t.Fatal("zero-length record unexpectedly parsed")
	}
	binary.LittleEndian.PutUint16(good[4:], 9) // unknown major version
	if _, _, err := ParseUSNRecord(good, 0); err == nil {
		t.Fatal("unknown major version unexpectedly parsed")
	}
}

func TestWalkUSNBuffer(t *testing.T) {
	r1 := buildV2(t, 10, 5, 100, 1, ReasonFileCreate, 0, "первый.txt")
	r2 := buildV2(t, 11, 5, 200, 2, ReasonFileDelete, FileAttributeDirectory, "Вторая папка")
	buf := append(append([]byte{}, r1...), r2...)
	buf = append(buf, make([]byte, 8)...) // alignment padding tail < 8 records

	var names []string
	if err := WalkUSNBuffer(buf, func(r USNRecord) error {
		names = append(names, r.Name)
		return nil
	}); err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(names) != 2 || names[0] != "первый.txt" || names[1] != "Вторая папка" {
		t.Fatalf("walked %v", names)
	}
}

func TestFiletimeToTime(t *testing.T) {
	if !filetimeToTime(0).IsZero() {
		t.Fatal("zero FILETIME must map to zero Time")
	}
	if got := filetimeToTime(filetimeEpochDelta); got.Format(time.RFC3339) != "1970-01-01T00:00:00Z" {
		t.Fatalf("epoch: %s", got.Format(time.RFC3339))
	}
	// 2026-09-20T12:00:00Z, computed from the Unix side (round-trip check
	// that the scale/subtract order survives large present-day values)
	want := time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)
	ft := want.UnixNano()/100 + filetimeEpochDelta
	if got := filetimeToTime(ft); !got.Equal(want) {
		t.Fatalf("round-trip: got %s want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestBuildPathsTree(t *testing.T) {
	entries := map[int64]Entry{
		5:  {FRN: 5, ParentFRN: 5, Name: ".", IsDir: true},   // volume root
		10: {FRN: 10, ParentFRN: 5, Name: "Фото", IsDir: true},
		11: {FRN: 11, ParentFRN: 10, Name: "предметка", IsDir: true},
		12: {FRN: 12, ParentFRN: 11, Name: "предметка_12345678.jpg"},
		13: {FRN: 13, ParentFRN: 11, Name: "лук_87654321.png"},
		20: {FRN: 20, ParentFRN: 5, Name: "Регламенты", IsDir: true},
		21: {FRN: 21, ParentFRN: 20, Name: "инструкция по упаковке.pdf"},
	}
	paths, orphans := BuildPaths(entries)
	want := map[int64]string{
		5:  "",
		10: "Фото",
		11: `Фото\предметка`,
		12: `Фото\предметка\предметка_12345678.jpg`,
		13: `Фото\предметка\лук_87654321.png`,
		20: "Регламенты",
		21: `Регламенты\инструкция по упаковке.pdf`,
	}
	for frn, p := range want {
		if paths[frn] != p {
			t.Errorf("path[%d] = %q; want %q", frn, paths[frn], p)
		}
	}
	if len(paths) != len(want) {
		t.Errorf("resolved %d paths; want %d", len(paths), len(want))
	}
	if len(orphans) != 0 {
		t.Errorf("orphans = %v; want none", orphans)
	}
}

func TestBuildPathsOrphansAndCycles(t *testing.T) {
	entries := map[int64]Entry{
		5:  {FRN: 5, ParentFRN: 5, Name: ".", IsDir: true},
		30: {FRN: 30, ParentFRN: 99, Name: "осиротевший.txt"},   // parent missing
		40: {FRN: 40, ParentFRN: 41, Name: "a"},                 // cycle pair
		41: {FRN: 41, ParentFRN: 40, Name: "b"},
	}
	_, orphans := BuildPaths(entries)
	for _, frn := range []int64{30, 40, 41} {
		if !orphans[frn] {
			t.Errorf("frn %d must be an orphan", frn)
		}
	}
}

func TestRootFRNAndDropSystemChildren(t *testing.T) {
	entries := map[int64]Entry{
		5:  {FRN: 5, ParentFRN: 5, Name: "."},
		0:  {FRN: 0, ParentFRN: 5, Name: "$Mft"},
		1:  {FRN: 1, ParentFRN: 5, Name: "$LogFile"},
		10: {FRN: 10, ParentFRN: 5, Name: "Фото", IsDir: true},
		11: {FRN: 11, ParentFRN: 10, Name: "видео$архив.mp4"}, // $ NOT at root depth — keep
	}
	if root := RootFRN(entries); root != 5 {
		t.Fatalf("RootFRN = %d; want 5", root)
	}
	DropSystemChildren(entries, 5)
	for _, frn := range []int64{0, 1} {
		if _, ok := entries[frn]; ok {
			t.Errorf("system entry %d survived the filter", frn)
		}
	}
	for _, frn := range []int64{5, 10, 11} {
		if _, ok := entries[frn]; !ok {
			t.Errorf("entry %d must survive the filter", frn)
		}
	}
}

func TestEnsureRootAnchorSynthesizesMissingRoot(t *testing.T) {
	// the live Э-0.2 case: ENUM returned children of the root but no root
	// record at all; realistic FRNs (segment<<16|seq), root = segment 5
	const rootFRN = int64(0x0005000000000005)
	entries := map[int64]Entry{
		0x00060000000000AA: {FRN: 0x00060000000000AA, ParentFRN: rootFRN, Name: "Фото", IsDir: true},
		0x00060000000000AB: {FRN: 0x00060000000000AB, ParentFRN: 0x00060000000000AA, Name: "предметка_1.jpg"},
		0x00060000000000AC: {FRN: 0x00060000000000AC, ParentFRN: rootFRN, Name: "Регламенты", IsDir: true},
	}
	if RootFRN(entries) != 0 {
		t.Fatal("precondition: no self-parented entry yet")
	}
	if got := ensureRootAnchor(entries); got != rootFRN {
		t.Fatalf("anchor = %#x; want segment-5 %#x", got, rootFRN)
	}
	paths, orphans := BuildPaths(entries)
	if len(orphans) != 0 {
		t.Fatalf("orphans = %v; want none after anchoring", orphans)
	}
	if paths[0x00060000000000AB] != `Фото\предметка_1.jpg` {
		t.Fatalf("paths after synthesis: %v", paths)
	}
}

func TestEnsureRootAnchorIgnoresMostReferencedDir(t *testing.T) {
	// the live WRONG-anchor case: a system directory with thousands of
	// direct children must NOT win over the segment-5 root (the old
	// heuristic orphaned 65% of the volume through exactly this)
	const rootFRN = int64(0x0005000000000005)
	const system32 = int64(0x0007000000000ABC)
	entries := map[int64]Entry{
		0x0006000000000001: {FRN: 0x0006000000000001, ParentFRN: system32, Name: "kernel32.dll"},
		0x0006000000000002: {FRN: 0x0006000000000002, ParentFRN: system32, Name: "shell32.dll"},
		0x0006000000000003: {FRN: 0x0006000000000003, ParentFRN: system32, Name: "ntdll.dll"},
		0x0006000000000004: {FRN: 0x0006000000000004, ParentFRN: rootFRN, Name: "тест-индекс", IsDir: true},
	}
	if got := ensureRootAnchor(entries); got != rootFRN {
		t.Fatalf("anchor = %#x; want segment-5 root despite system32 having 3 refs vs root's 1", got)
	}
	if paths, _ := BuildPaths(entries); paths[0x0006000000000004] != "тест-индекс" {
		t.Fatalf("тест-индекс must resolve under the root, got %v", paths)
	}
}

func TestEnsureRootAnchorNormalizesStrayParent(t *testing.T) {
	// root record present but its parent is 0, not itself
	const rootFRN = int64(0x0005000000000005)
	entries := map[int64]Entry{
		rootFRN:             {FRN: rootFRN, ParentFRN: 0, Name: ".", IsDir: true},
		0x0006000000000001: {FRN: 0x0006000000000001, ParentFRN: rootFRN, Name: "файл.txt"},
	}
	if got := ensureRootAnchor(entries); got != rootFRN {
		t.Fatalf("anchor = %#x; want %#x", got, rootFRN)
	}
	paths, orphans := BuildPaths(entries)
	if len(orphans) != 0 || paths[0x0006000000000001] != "файл.txt" {
		t.Fatalf("paths=%v orphans=%v", paths, orphans)
	}
}

func TestEnsureRootAnchorFallbackExotic(t *testing.T) {
	// no segment-5 anywhere (exotic layout) → most-referenced parent wins
	const fakeRoot = int64(0x0009000000000123)
	entries := map[int64]Entry{
		0x000A000000000001: {FRN: 0x000A000000000001, ParentFRN: fakeRoot, Name: "a.txt"},
		0x000A000000000002: {FRN: 0x000A000000000002, ParentFRN: fakeRoot, Name: "b.txt"},
	}
	if got := ensureRootAnchor(entries); got != fakeRoot {
		t.Fatalf("fallback anchor = %#x; want %#x", got, fakeRoot)
	}
	if paths, orphans := BuildPaths(entries); len(orphans) != 0 || paths[0x000A000000000001] != "a.txt" {
		t.Fatalf("fallback paths=%v orphans=%v", paths, orphans)
	}
}

func TestRootFRNSegmentLayoutLiveObserved(t *testing.T) {
	// regression for the live 2026-09-20 finding: the volume root's FRN on
	// Server 2022 is 0x0005000000000005 — segment 5 in the LOW 48 bits
	// (sequence 5 in the high 16), where an inverted >>16 check saw nothing
	if frnSegment(0x0005000000000005) != 5 {
		t.Fatalf("frnSegment(0x0005000000000005) = %#x; want 5 (low-48 segment layout)", frnSegment(0x0005000000000005))
	}
	// a high-sequence FRN must not be mistaken for segment 5
	if frnSegment(0x0006000000000ABC) == 5 {
		t.Fatal("segment extraction leaks sequence bits")
	}
	// and the anchor must recognize the exact live value via children
	const liveRoot = int64(0x0005000000000005)
	entries := map[int64]Entry{
		0x00060000000000AA: {FRN: 0x00060000000000AA, ParentFRN: liveRoot, Name: "тест-индекс", IsDir: true},
		0x00060000000000AB: {FRN: 0x00060000000000AB, ParentFRN: 0x00060000000000AA, Name: "предметка_1.jpg"},
	}
	if got := ensureRootAnchor(entries); got != liveRoot {
		t.Fatalf("anchor = %#x; want live-observed %#x", got, liveRoot)
	}
	paths, orphans := BuildPaths(entries)
	if len(orphans) != 0 || paths[0x00060000000000AB] != `тест-индекс\предметка_1.jpg` {
		t.Fatalf("paths=%v orphans=%v", paths, orphans)
	}
}

func TestOrphanCauses(t *testing.T) {
	entries := map[int64]Entry{
		5:  {FRN: 5, ParentFRN: 5, Name: "."},
		10: {FRN: 10, ParentFRN: 99, Name: "a.txt"}, // dies at missing 99
		11: {FRN: 11, ParentFRN: 12, Name: "x"},     // cycle 11↔12
		12: {FRN: 12, ParentFRN: 11, Name: "y"},
	}
	_, orphans := BuildPaths(entries)
	causes := OrphanCauses(entries, orphans)
	if causes[99] != 1 {
		t.Errorf("missing parent 99 must be the cause once, got %v", causes)
	}
	// cycle: both 11 and 12 are orphans; the walk from either terminates at
	// the first revisited node — exactly one of them is counted per orphan
	if total := causes[11] + causes[12]; total != 2 {
		t.Errorf("cycle orphans miscounted: %v", causes)
	}
}

func TestMatchInclude(t *testing.T) {
	cases := []struct {
		path, include string
		want          bool
	}{
		{`Фото\предметка\x.jpg`, `Фото`, true},
		{`Фото\предметка\x.jpg`, `фото/предметка/`, true},   // slashes + case + trailing sep
		{`Фотобанк\y.jpg`, `Фото`, false},                    // prefix must cut at separator
		{`Регламенты\a.pdf`, `Фото`, false},
		{`anything`, ``, true},                               // empty include = whole volume
		{`ФОТО\z.jpg`, `фото`, true},
	}
	for _, c := range cases {
		if got := MatchInclude(c.path, c.include); got != c.want {
			t.Errorf("MatchInclude(%q, %q) = %v; want %v", c.path, c.include, got, c.want)
		}
	}
}
