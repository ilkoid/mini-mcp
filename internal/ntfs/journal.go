//go:build windows

package ntfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// USN journal reading (PRD §4.2, Э-0.3). The journal is the volume-wide
// change stream; the include filter belongs to the DB layer (delta.go), not
// here.

var (
	// ErrJournalInactive: the volume has no USN journal (E: before
	// createjournal, П-3). Deltas impossible until one is created.
	ErrJournalInactive = errors.New("usn journal not active on volume")
	// ErrJournalReset: records the cursor still points at were overwritten
	// (journal wrapped or recreated) — the only honest answer is a full
	// rescan, which the agent treats as a normal branch (PRD §4.2).
	ErrJournalReset = errors.New("usn journal wrapped or recreated — full rescan required")
)

// readUSNJournalDataV1 is READ_USN_JOURNAL_DATA_V1. The V0 shape is
// rejected by Server 2022 with ERROR_INVALID_USER_BUFFER when the journal
// negotiates record versions above 2 (live Э-0.3, 2026-09-20 — the journal
// reported min v2 / max v4): the driver expects the version-range fields.
// We pin [2,2]: plain USN_RECORD_V2, exactly what the shared parser and
// ENUM use; V4 persistent records are never requested.
type readUSNJournalDataV1 struct {
	StartUsn                 int64
	ReasonMask               uint32
	ReturnOnlyOnClose        uint32
	Timeout                  uint32
	BytesToWaitFor           uint32
	JournalID                int64
	MinSupportedMajorVersion uint32
	MaxSupportedMajorVersion uint32
}

// ReadJournal feeds all records from startUSN (inclusive) up to the current
// journal tail to fn and returns the next unread USN — the value to persist
// as the root's cursor, but ONLY after the batch is applied (ApplyDeltas
// commits data+cursor together). journalID must be the identity the cursor
// was taken under. minVer/maxVer come from NegotiateRecordVersions.
func (v *Volume) ReadJournal(startUSN, journalID int64, minVer, maxVer uint32, fn func(USNRecord) error) (nextUSN int64, count int, err error) {
	in := readUSNJournalDataV1{
		StartUsn:                 startUSN,
		ReasonMask:               0, // 0 = all reasons per docs; avoids the undefined bits of 0xFFFFFFFF
		JournalID:                journalID,
		MinSupportedMajorVersion: minVer,
		MaxSupportedMajorVersion: maxVer,
	}
	buf := make([]byte, 64*1024)
	for {
		var returned uint32
		err = windows.DeviceIoControl(v.Handle, fsctlReadUSNJournal,
			(*byte)(unsafe.Pointer(&in)), uint32(unsafe.Sizeof(in)),
			&buf[0], uint32(len(buf)), &returned, nil)
		switch {
		case err == nil:
		case errors.Is(err, windows.ERROR_JOURNAL_NOT_ACTIVE):
			return nextUSN, count, ErrJournalInactive
		case errors.Is(err, windows.ERROR_JOURNAL_ENTRY_DELETED):
			return nextUSN, count, ErrJournalReset
		case errors.Is(err, windows.ERROR_HANDLE_EOF):
			// poll hit the tail with nothing new (observed shape varies by
			// driver version): identical to an empty-records page — return
			// the unchanged start, the caller's cursor must not regress
			return in.StartUsn, count, nil
		default:
			return nextUSN, count, fmt.Errorf("read usn journal: %w", err)
		}
		if returned < 8 {
			return in.StartUsn, count, nil
		}
		nextUSN = int64(binary.LittleEndian.Uint64(buf[0:]))
		records := buf[8:returned]
		if len(records) == 0 {
			// caught up with the tail
			return nextUSN, count, nil
		}
		n := 0
		if err := WalkUSNBuffer(records, func(r USNRecord) error {
			n++
			return fn(r)
		}); err != nil {
			return nextUSN, count, err
		}
		count += n
		if nextUSN <= in.StartUsn {
			return nextUSN, count, nil // no-progress guard
		}
		in.StartUsn = nextUSN
	}
}

// NegotiateRecordVersions probes the caller-accepted record-version range
// with cheap tail reads and adopts the first the driver accepts. Live Э-0.3
// (Server 2022, 2026-09-21): a flat [2,2] is rejected with
// ERROR_INVALID_PARAMETER ("The parameter is incorrect") on a journal that
// advertises [2,4] — the accepted upper bound is not guessable from the
// docs, so we probe instead. Records still arrive as V2 (range tracking is
// off); the parser handles V2/V3 and fails loudly if a V4 ever shows up.
func (v *Volume) NegotiateRecordVersions(journalID int64) (minv, maxv uint32, err error) {
	ji, err := v.QueryJournal()
	if err != nil {
		return 0, 0, err
	}
	for _, cand := range [][2]uint32{{2, 2}, {2, 3}, {2, 4}} {
		_, _, perr := v.ReadJournal(ji.NextUSN, journalID, cand[0], cand[1], func(USNRecord) error { return nil })
		switch {
		case perr == nil:
			return cand[0], cand[1], nil
		case errors.Is(perr, windows.ERROR_INVALID_PARAMETER):
			continue
		default:
			return 0, 0, perr // inactive/reset/unexpected — real errors, not negotiation
		}
	}
	return 0, 0, errors.New("usn journal driver rejected every caller version range [2,2]/[2,3]/[2,4]")
}
