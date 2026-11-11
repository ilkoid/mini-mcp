package ntfs

import (
	"encoding/binary"
	"fmt"
	"time"
	"unicode/utf16"
)

// USN-record parsing (PRD §4.1/Э-0.2). Both the MFT snapshot
// (FSCTL_ENUM_USN_DATA) and the journal deltas (FSCTL_READ_USN_JOURNAL,
// Э-0.3) hand back the same packed USN_RECORD structures, so the parser is
// shared by the two scan paths. Parsing is pure byte-decoding — testable on
// fixtures without a volume.

// Reason flags (winnt.h USN_REASON_*), the subset the agent reacts to.
const (
	ReasonDataOverwrite     = 0x00000001
	ReasonDataExtend        = 0x00000002
	ReasonDataTruncation    = 0x00000004
	ReasonFileCreate        = 0x00000100
	ReasonFileDelete        = 0x00000200
	ReasonSecurityChange    = 0x00000800 // ACL changed → refresh stored SD (PRD §4.2)
	ReasonRenameOldName     = 0x00001000
	ReasonRenameNewName     = 0x00002000
	ReasonBasicInfoChange   = 0x00008000 // attributes / timestamps
	ReasonClose             = 0x80000000 // final record for the operation
	ReasonAll               = 0xFFFFFFFF

	FileAttributeDirectory  = 0x00000010
	FileAttributeReparsePt  = 0x00000400 // mount points / symlinks (PRD §4.3)
)

// USNRecord is the parsed view of one USN_RECORD_V2 (NTFS) or V3 (ReFS).
// FRN values fit int64: FRN = segment<<16|sequence, and the segment is
// bounded by MFT size (2^47 unreachable), so the sign bit is never used.
type USNRecord struct {
	FRN, ParentFRN int64
	USN            int64
	TimeStamp      int64 // FILETIME: 100ns ticks since 1601-01-01
	Reason         uint32
	FileAttributes uint32
	Name           string
}

func (r USNRecord) IsDir() bool { return r.FileAttributes&FileAttributeDirectory != 0 }

// ParseUSNRecord decodes one record from buf at off, returning the record
// and its RecordLength in bytes. Major versions other than 2/3 are an error
// (V4 persistent records are only returned when explicitly requested — Э-0.3
// never does).
func ParseUSNRecord(buf []byte, off int) (USNRecord, int, error) {
	if off+8 > len(buf) {
		return USNRecord{}, 0, fmt.Errorf("usn record at %d: buffer too short", off)
	}
	recLen := int(binary.LittleEndian.Uint32(buf[off:]))
	major := binary.LittleEndian.Uint16(buf[off+4:])
	if recLen == 0 {
		return USNRecord{}, 0, fmt.Errorf("usn record at %d: zero length", off)
	}
	if off+recLen > len(buf) {
		return USNRecord{}, 0, fmt.Errorf("usn record at %d: length %d exceeds buffer %d", off, recLen, len(buf)-off)
	}
	var (
		rec            USNRecord
		nameLen, nameOff int
	)
	switch major {
	case 2: // USN_RECORD_V2: 60-byte header
		rec.FRN = int64(binary.LittleEndian.Uint64(buf[off+8:]))
		rec.ParentFRN = int64(binary.LittleEndian.Uint64(buf[off+16:]))
		rec.USN = int64(binary.LittleEndian.Uint64(buf[off+24:]))
		rec.TimeStamp = int64(binary.LittleEndian.Uint64(buf[off+32:]))
		rec.Reason = binary.LittleEndian.Uint32(buf[off+40:])
		rec.FileAttributes = binary.LittleEndian.Uint32(buf[off+52:])
		nameLen = int(binary.LittleEndian.Uint16(buf[off+56:]))
		nameOff = int(binary.LittleEndian.Uint16(buf[off+58:]))
	case 3: // USN_RECORD_V3: 128-bit file IDs, 76-byte header
		rec.FRN = int64(binary.LittleEndian.Uint64(buf[off+8:]))
		rec.ParentFRN = int64(binary.LittleEndian.Uint64(buf[off+24:]))
		rec.USN = int64(binary.LittleEndian.Uint64(buf[off+40:]))
		rec.TimeStamp = int64(binary.LittleEndian.Uint64(buf[off+48:]))
		rec.Reason = binary.LittleEndian.Uint32(buf[off+56:])
		rec.FileAttributes = binary.LittleEndian.Uint32(buf[off+68:])
		nameLen = int(binary.LittleEndian.Uint16(buf[off+72:]))
		nameOff = int(binary.LittleEndian.Uint16(buf[off+74:]))
	default:
		return USNRecord{}, 0, fmt.Errorf("usn record at %d: unsupported major version %d", off, major)
	}
	if nameOff < 0 || nameLen < 0 || off+nameOff+nameLen > off+recLen || nameLen%2 != 0 {
		return USNRecord{}, 0, fmt.Errorf("usn record at %d: bad name span (off=%d len=%d, reclen=%d)", off, nameOff, nameLen, recLen)
	}
	u16 := make([]uint16, nameLen/2)
	for i := range u16 {
		u16[i] = binary.LittleEndian.Uint16(buf[off+nameOff+i*2:])
	}
	rec.Name = string(utf16.Decode(u16))
	return rec, recLen, nil
}

// WalkUSNBuffer iterates the records packed after the 8-byte prefix
// (next-id / next-usn) of an FSCTL_ENUM_USN_DATA or FSCTL_READ_USN_JOURNAL
// output buffer, calling fn for each. Records are LONGLONG-aligned; a zero
// RecordLength or a short tail ends iteration cleanly.
func WalkUSNBuffer(records []byte, fn func(USNRecord) error) error {
	off := 0
	for off+8 <= len(records) {
		recLen := int(binary.LittleEndian.Uint32(records[off:]))
		if recLen == 0 {
			return nil
		}
		rec, _, err := ParseUSNRecord(records, off)
		if err != nil {
			return err
		}
		if err := fn(rec); err != nil {
			return err
		}
		off += (recLen + 7) &^ 7
	}
	return nil
}

// filetimeEpochDelta is 1601-01-01 → 1970-01-01 in 100ns ticks.
const filetimeEpochDelta = 116444736000000000

// filetimeToTime converts FILETIME (100ns ticks since 1601-01-01) to UTC;
// zero maps to the zero Time. Subtract the epoch delta first, then scale —
// ft*100 overflows int64 for present-day values.
func filetimeToTime(ft int64) time.Time {
	if ft == 0 {
		return time.Time{}
	}
	return time.Unix(0, (ft-filetimeEpochDelta)*100).UTC()
}
