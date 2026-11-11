//go:build windows

package ntfs

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Volume-side primitives (PRD §4, Э-0.2): open \\.\X:, query the USN
// journal state, walk the MFT via FSCTL_ENUM_USN_DATA. The FSCTL codes are
// not exposed by x/sys/windows and are computed here per winioctl.h
// CTL_CODE(FILE_DEVICE_FILE_SYSTEM, fn, METHOD_NEITHER, FILE_ANY_ACCESS):
// ENUM=44→0x000900b3, READ_JOURNAL=46→0x000900bb, QUERY_JOURNAL=61→0x000900f4.
const (
	fsctlEnumUSNData     = 0x000900b3
	fsctlReadUSNJournal  = 0x000900bb
	fsctlQueryUSNJournal = 0x000900f4
)

// Volume is an open handle to \\.\X:. Opening requires an elevated token or
// LocalSystem (Э-0.0 П-2: a UAC-filtered admin token gets
// ERROR_ACCESS_DENIED — expected, not a code bug); MFT/journal reads go
// volume-wide by construction, include-filtering happens on rows (PRD §13).
type Volume struct {
	Handle windows.Handle
	Device string
}

// OpenVolume accepts "E:" or `\\.\E:` (Volume GUID paths work too).
func OpenVolume(device string) (*Volume, error) {
	path := device
	if len(path) == 2 && path[1] == ':' {
		path = `\\.\` + path
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := windows.CreateFile(p,
		windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_ATTRIBUTE_NORMAL,
		0)
	if err != nil {
		return nil, fmt.Errorf("open volume %s: %w (unelevated token? Э-0.0 П-2)", path, err)
	}
	return &Volume{Handle: h, Device: path}, nil
}

func (v *Volume) Close() error { return windows.CloseHandle(v.Handle) }

// JournalInfo mirrors the leading fields of USN_JOURNAL_DATA_V0/V1 (they
// share a prefix; extra V1/V2 fields are ignored).
type JournalInfo struct {
	Absent    bool
	JournalID int64 // changes when the journal is recreated → full rescan (wrap)
	FirstUSN  int64
	NextUSN   int64
}

// QueryJournal reads the journal state. An absent journal is NOT an error —
// the MFT snapshot stays valid; deltas just can't run until the journal is
// created (Э-0.0 П-3, E: today).
func (v *Volume) QueryJournal() (JournalInfo, error) {
	var out [2 << 6]byte // USN_JOURNAL_DATA_V2 is 64 bytes; V0 is 48
	var returned uint32
	err := windows.DeviceIoControl(v.Handle, fsctlQueryUSNJournal, nil, 0,
		&out[0], uint32(len(out)), &returned, nil)
	if err == windows.ERROR_JOURNAL_NOT_ACTIVE || err == windows.ERROR_JOURNAL_DELETE_IN_PROGRESS {
		return JournalInfo{Absent: true}, nil
	}
	if err != nil {
		return JournalInfo{}, fmt.Errorf("query usn journal: %w", err)
	}
	if returned < 24 {
		return JournalInfo{}, fmt.Errorf("query usn journal: short output %d bytes", returned)
	}
	return JournalInfo{
		JournalID: int64(binary.LittleEndian.Uint64(out[0:])),
		FirstUSN:  int64(binary.LittleEndian.Uint64(out[8:])),
		NextUSN:   int64(binary.LittleEndian.Uint64(out[16:])),
	}, nil
}

// mftEnumDataV0 is MFT_ENUM_DATA_V0 — the FSCTL_ENUM_USN_DATA input.
type mftEnumDataV0 struct {
	StartFRN uint64
	LowUSN   int64
	HighUSN  int64
}

// EnumMFT walks one USN record per file id on the volume (PRD §4.1) and
// feeds it to fn. Paging: each DeviceIoControl returns [next StartFRN
// uint64][records…]; ERROR_HANDLE_EOF ends the walk. Records are fed in MFT
// order, NOT path order — parents may arrive after children (Э-0.2 rake:
// path assembly happens after the full pass, not inside fn).
func (v *Volume) EnumMFT(fn func(USNRecord) error) (int, error) {
	in := mftEnumDataV0{HighUSN: 1<<63 - 1}
	buf := make([]byte, 64*1024)
	count := 0
	for {
		var returned uint32
		err := windows.DeviceIoControl(v.Handle, fsctlEnumUSNData,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			&buf[0], uint32(len(buf)), &returned, nil)
		if err == windows.ERROR_HANDLE_EOF {
			return count, nil
		}
		if err != nil {
			return count, fmt.Errorf("enum usn data: %w", err)
		}
		if returned < 8 {
			return count, nil
		}
		in.StartFRN = binary.LittleEndian.Uint64(buf[0:])
		if in.StartFRN == 0 {
			return count, nil
		}
		n := 0
		err = WalkUSNBuffer(buf[8:returned], func(r USNRecord) error {
			n++
			return fn(r)
		})
		count += n
		if err != nil {
			return count, err
		}
		if n == 0 {
			// next StartFRN without records would loop forever; bail out
			return count, fmt.Errorf("enum usn data: empty page at FRN %#x", in.StartFRN)
		}
	}
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

// Snapshot runs the full MFT pass and assembles paths.
func (v *Volume) Snapshot() (*Snapshot, error) {
	ji, err := v.QueryJournal()
	if err != nil {
		return nil, err
	}
	entries := make(map[int64]Entry, 1<<16)
	s := &Snapshot{Journal: ji, Entries: entries}
	s.Count, err = v.EnumMFT(func(r USNRecord) error {
		// one record per file id: hardlink alternates overwrite in place —
		// first name wins, deliberate (PRD Э-0.2: dedup by FRN, one path)
		entries[r.FRN] = Entry{FRN: r.FRN, ParentFRN: r.ParentFRN, Name: r.Name, IsDir: r.IsDir()}
		return nil
	})
	if err != nil {
		return nil, err
	}
	root := ensureRootAnchor(entries) // live check Э-0.2: ENUM omits the root record
	DropSystemChildren(entries, root)
	s.Paths, s.Orphans = BuildPaths(entries)
	return s, nil
}
