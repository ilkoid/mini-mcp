package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestAnnotations_WireSerialization is the load-bearing check the design doc
// demands (§5.0, §3.2): an explicit "destructiveHint": false MUST survive
// marshalling on the wire, because the ZCode plan-mode predicate is
// `isMcp && !destructive`. With a plain bool + omitempty it would be dropped;
// the *bool field in the official SDK is what makes ptr(false) emit it.
func TestAnnotations_WireSerialization(t *testing.T) {
	a := readOnlyAnnotations
	b, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	s := string(b)
	for _, want := range []string{
		`"readOnlyHint":true`,
		`"destructiveHint":false`,
		`"idempotentHint":true`,
		`"openWorldHint":false`,
	} {
		if !strings.Contains(s, want) {
			t.Fatalf("wire JSON missing %q\nfull: %s", want, s)
		}
	}
	// destructiveHint must NOT be omitted (the whole point of *bool here).
	if strings.Contains(s, `"destructiveHint":true`) {
		t.Fatalf("destructiveHint serialized as true: %s", s)
	}
	t.Logf("annotations wire: %s", s)
}

// TestReadOnlyAnnotations_PointerFields asserts the *bool fields are non-nil
// and carry the explicit value, so they serialize (nil would be omitted).
func TestReadOnlyAnnotations_PointerFields(t *testing.T) {
	if readOnlyAnnotations.DestructiveHint == nil {
		t.Fatal("DestructiveHint is nil — would be omitted on the wire")
	}
	if *readOnlyAnnotations.DestructiveHint != false {
		t.Fatalf("DestructiveHint = %v, want false", *readOnlyAnnotations.DestructiveHint)
	}
	if !readOnlyAnnotations.ReadOnlyHint {
		t.Fatal("ReadOnlyHint must be true")
	}
	if !readOnlyAnnotations.IdempotentHint {
		t.Fatal("IdempotentHint must be true")
	}
}

// TestPtrHelper confirms ptr(false) returns a *bool with value false.
func TestPtrHelper(t *testing.T) {
	p := ptr(false)
	if p == nil || *p != false {
		t.Fatalf("ptr(false) = %v, want non-nil pointer to false", p)
	}
}

// TestToolAnnotations_SDKTypeShape guards against SDK upgrades silently
// changing the field back to a non-pointer bool (which would re-introduce
// the omission bug). If this test breaks, re-audit §5.0.
func TestToolAnnotations_SDKTypeShape(t *testing.T) {
	// We can't reflect on unexported fields, but we can round-trip a value
	// through JSON and confirm the false survives.
	in := &mcp.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: ptr(false),
		IdempotentHint:  true,
	}
	b, _ := json.Marshal(in)
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if out["destructiveHint"] != false {
		t.Fatalf("destructiveHint did not round-trip as false: %v (raw=%s)", out["destructiveHint"], b)
	}
	if out["readOnlyHint"] != true {
		t.Fatalf("readOnlyHint did not round-trip as true: %s", b)
	}
}
