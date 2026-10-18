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

// TestQueryDescriptionFmt_InterpolatesRole guards the psql-bypass hint: the
// pg_readonly_query description must name the role and password env the entry
// actually runs as (mcp_ro/PG_RO_PWD locally, arm_ai_ro/PG_USER_PWD on the
// poncho-ai prod instance). A hardcoded role name would send the agent after
// credentials that don't exist on that host.
func TestQueryDescriptionFmt_InterpolatesRole(t *testing.T) {
	if strings.Contains(queryDescriptionFmt, "mcp_ro") || strings.Contains(queryDescriptionFmt, "PG_RO_PWD") {
		t.Fatal("queryDescriptionFmt hardcodes a role/env name — keep the parameterized placeholders")
	}
	for _, tc := range []struct{ role, pwdEnv string }{
		{"mcp_ro", "PG_RO_PWD"},
		{"arm_ai_ro", "PG_USER_PWD"},
	} {
		desc := buildQueryDescription(tc.role, tc.pwdEnv, "wb_data_test", "localhost", "5432")
		if !strings.Contains(desc, "role "+tc.role+", READ ONLY") {
			t.Fatalf("role %s: description does not name it", tc.role)
		}
		if !strings.Contains(desc, "PGPASSWORD=$"+tc.pwdEnv) {
			t.Fatalf("role %s: description does not name password env %s", tc.role, tc.pwdEnv)
		}
	}
}

// TestQueryDescription_NamesDatabaseHostAndProd is the anti-"which DB am I on?"
// guard. Two entries — `pg-readonly` (wb_data_prod) and `pg-readonly-test`
// (wb_data_test) — run this same binary against the same host:port and differ
// ONLY by --database, so the description is the agent's only way to tell prod
// from test. It must carry the database name and the configured host:port, and
// must flag *_prod loudly (and only *_prod). No DB needed.
func TestQueryDescription_NamesDatabaseHostAndProd(t *testing.T) {
	const host, port = "localhost", "5432"
	addr := host + ":" + port
	prod := buildQueryDescription("mcp_ro", "PG_RO_PWD", "wb_data_prod", host, port)
	test := buildQueryDescription("mcp_ro", "PG_RO_PWD", "wb_data_test", host, port)

	// Printed on failure (and under -v) so the actual text is inspectable.
	t.Logf("wb_data_prod description:\n%s", prod)
	t.Logf("wb_data_test description:\n%s", test)

	for _, want := range []string{"wb_data_prod", addr, "PRODUCTION"} {
		if !strings.Contains(prod, want) {
			t.Errorf("prod description missing %q:\n%s", want, prod)
		}
	}
	for _, want := range []string{"wb_data_test", addr} {
		if !strings.Contains(test, want) {
			t.Errorf("test description missing %q:\n%s", want, test)
		}
	}
	// The prod flag must be the first thing the agent reads, and it must be
	// per-database — a *_test entry sharing the host must NOT inherit it.
	if !strings.HasPrefix(prod, prodMarker) {
		t.Errorf("prod description does not lead with %q:\n%s", prodMarker, prod)
	}
	if strings.Contains(test, "PRODUCTION") {
		t.Errorf("non-prod database flagged as PRODUCTION:\n%s", test)
	}
	if strings.Contains(prod, "wb_data_test") {
		t.Errorf("prod description mentions the test database:\n%s", prod)
	}
	// host:port and database are interpolated, not baked into the format.
	if strings.Contains(queryDescriptionFmt, host) || strings.Contains(queryDescriptionFmt, "wb_data") {
		t.Error("queryDescriptionFmt hardcodes host/database — keep the placeholders")
	}
}

// TestIsProdDatabase pins the *_prod predicate (case-insensitive).
func TestIsProdDatabase(t *testing.T) {
	for db, want := range map[string]bool{
		"wb_data_prod":      true,
		"ozon_data_prod":    true,
		"WB_DATA_PROD":      true,
		"wb_data_test":      false,
		"wb_data_prod_test": false,
		"production":        false,
		"prod":              false,
		"":                  false,
	} {
		if got := isProdDatabase(db); got != want {
			t.Errorf("isProdDatabase(%q) = %v, want %v", db, got, want)
		}
	}
}
