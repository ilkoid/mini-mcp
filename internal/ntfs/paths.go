package ntfs

import "strings"

// Snapshot path assembly (PRD Э-0.2): MFT enumeration yields
// (FRN, parentFRN, name) per file — never a full path — so the snapshot
// builds a volume-wide FRN map and resolves paths by walking parents.
// Pure data manipulation, fixture-testable without a volume.

// Entry is one volume-tree node used for path assembly.
type Entry struct {
	FRN, ParentFRN int64
	Name           string
	IsDir          bool
}

// RootFRN returns the volume root: the self-parented entry (MFT record 5 on
// NTFS, named "."). 0 when absent.
func RootFRN(entries map[int64]Entry) int64 {
	for frn, e := range entries {
		if e.ParentFRN == frn {
			return frn
		}
	}
	return 0
}

// FRN bit layout per Microsoft docs (FILE_REFERENCE): the MFT segment
// number occupies the LOW 48 bits, the sequence number the HIGH 16. The
// root directory is MFT segment 5, so its FRN has low-48 == 5 — e.g.
// 0x0005000000000005, the exact value observed live (orphan-cause
// diagnostic 2026-09-20: 393k chains died at that FRN after an inverted
// >>16 check refused to recognize it).
const frnSegmentMask = int64(0x0000FFFFFFFFFFFF)

func frnSegment(frn int64) int64 { return frn & frnSegmentMask }

// ensureRootAnchor guarantees a self-parented anchor entry. Live Э-0.2
// (Server 2022): FSCTL_ENUM_USN_DATA returns no record for the root
// directory at all. NTFS canon closes this without heuristics: the root IS
// MFT segment 5 (records 0–15 are fixed metadata), so the anchor is the FRN
// with segment 5 — taken from children's parent pointers, sequence bits
// included. The most-referenced-parent heuristic is a FALLBACK only:
// it already misfired live (picked a system directory — thousands of direct
// children vs the real root's dozens — orphaning 65% of the volume,
// 2026-09-20). Returns the anchor FRN, 0 if the map is empty.
func ensureRootAnchor(entries map[int64]Entry) int64 {
	// 1) self-parented anchor already present
	if root := RootFRN(entries); root != 0 {
		return root
	}
	// 2) a segment-5 entry exists but is not self-parented → normalize
	for frn, e := range entries {
		if frnSegment(frn) == 5 && e.ParentFRN != frn {
			e.ParentFRN = frn
			entries[frn] = e
			return frn
		}
	}
	// 3) segment-5 anchor referenced by children but absent → synthesize
	for _, e := range entries {
		if e.ParentFRN != e.FRN && frnSegment(e.ParentFRN) == 5 {
			anchor := e.ParentFRN
			entries[anchor] = Entry{FRN: anchor, ParentFRN: anchor, Name: "", IsDir: true}
			return anchor
		}
	}
	// 4) fallback for exotic layouts: most-referenced non-zero parent
	counts := make(map[int64]int, 256)
	for _, e := range entries {
		// 0 is the "no parent" marker, never a real directory FRN
		if e.ParentFRN != e.FRN && e.ParentFRN != 0 {
			counts[e.ParentFRN]++
		}
	}
	best, bestN := int64(0), 0
	for frn, n := range counts {
		if n > bestN {
			best, bestN = frn, n
		}
	}
	if best == 0 {
		return 0
	}
	if e, ok := entries[best]; ok {
		e.ParentFRN = best
		entries[best] = e
	} else {
		entries[best] = Entry{FRN: best, ParentFRN: best, Name: "", IsDir: true}
	}
	return best
}

// OrphanCauses histograms the FRN where each orphan's parent chain dies
// (missing entry, or cycle). One dominant cause = one broken anchor — the
// diagnostic that pinpointed the wrong-anchor bug live (393k orphans all
// terminating at the real root's FRN).
func OrphanCauses(entries map[int64]Entry, orphans map[int64]bool) map[int64]int {
	causes := make(map[int64]int, 8)
	for frn := range orphans {
		seen := make(map[int64]bool, 8)
		cur := frn
		for {
			if seen[cur] {
				causes[cur]++ // cycle
				break
			}
			seen[cur] = true
			e, ok := entries[cur]
			if !ok {
				causes[cur]++ // missing parent — the chain dies here
				break
			}
			cur = e.ParentFRN
		}
	}
	return causes
}

// DropSystemChildren removes the root's direct $-prefixed children
// ($Mft, $LogFile, $UsnJrnl:…) from the map. The root itself stays — it is
// needed as the resolution anchor — but callers skip it when emitting rows
// (its assembled path is ""). In-place, on the given map.
func DropSystemChildren(entries map[int64]Entry, root int64) {
	for frn, e := range entries {
		if e.ParentFRN == root && len(e.Name) > 0 && e.Name[0] == '$' {
			delete(entries, frn)
		}
	}
}

// BuildPaths assembles volume-rooted backslash paths from the FRN map.
// Entries whose parent chain leaves the map (filtered system files,
// hardlink alternates) or cycles are reported as orphans — not resolved,
// not an error (Э-0.2 rake list: hardlinks surface here harmlessly).
func BuildPaths(entries map[int64]Entry) (paths map[int64]string, orphans map[int64]bool) {
	paths = make(map[int64]string, len(entries))
	orphans = make(map[int64]bool)

	var resolve func(frn int64, seen map[int64]bool) (string, bool)
	resolve = func(frn int64, seen map[int64]bool) (string, bool) {
		if p, ok := paths[frn]; ok {
			return p, true
		}
		if seen[frn] {
			return "", false
		}
		e, ok := entries[frn]
		if !ok {
			return "", false
		}
		if e.ParentFRN == frn {
			// volume root: empty anchor path
			paths[frn] = ""
			return "", true
		}
		seen[frn] = true
		parent, ok := resolve(e.ParentFRN, seen)
		if !ok {
			return "", false
		}
		p := e.Name
		if parent != "" {
			p = parent + "\\" + e.Name
		}
		paths[frn] = p
		return p, true
	}

	for frn := range entries {
		if _, ok := paths[frn]; ok {
			continue
		}
		if _, ok := resolve(frn, map[int64]bool{}); !ok {
			orphans[frn] = true
		}
	}
	return paths, orphans
}

// MatchInclude reports whether a volume-rooted path falls under the include
// prefix (Э-0.2: the pilot's blast-radius control — MFT is read volume-wide,
// rows are limited to the test folder). Both sides Go-lowercased, forward
// slashes normalized to backslashes, trailing separators trimmed.
func MatchInclude(path, include string) bool {
	norm := func(s string) string {
		s = strings.ReplaceAll(lowerName(s), "/", "\\")
		return strings.TrimRight(s, "\\")
	}
	inc := norm(include)
	if inc == "" {
		return true
	}
	p := norm(path)
	return p == inc || len(p) > len(inc) && p[len(inc)] == '\\' && p[:len(inc)] == inc
}
