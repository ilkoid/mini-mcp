// Package anon implements keyword anonymization for the read-only MCP servers.
//
// An Anonymizer is compiled once at startup from a YAML rules file and then
// used in two directions:
//
//   - Apply (mask): original keyword -> alias, applied to EVERY string that
//     leaves the process (cell values, column names, notices, error texts).
//     The session/LLM must never observe an original keyword.
//   - Unmap (reverse): alias -> original keyword, applied to SQL text and bind
//     values BEFORE execution. The agent only knows aliases, so its
//     WHERE name = 'Клиент-7' must be translated to the real value before the
//     database can match it. This happens inside the MCP process — the real
//     value never reaches the session in either direction.
//
// Matching defaults to case-insensitive substring replacement, longest keyword
// first, so overlapping keywords resolve deterministically. Each rule may
// override the defaults with case_sensitive / exact. Numeric-looking keywords
// (INN, OGRN, …) additionally match numeric cells by their canonical string
// form; such cells are returned as the alias STRING (numeric aliases cannot be
// used as bind values — filter on other columns instead).
//
// Derivative guards (test report 2026-09-03, Д-1/Д-2/Д-3): string functions
// computed in the database (left(col,4), md5(col), length(col)) bypass full
// keyword matching, so masking alone leaks derivatives. Two extra layers:
//
//   - CheckBlockedFunctions: the dialect precheck rejects queries containing
//     known leak functions (substring/hash/length families) before execution;
//   - GuardFragments: output-side defense-in-depth that redacts any surviving
//     run of >= fragment_min_len consecutive keyword characters, plus the
//     bytea text guard driven by the callers (encode.go).
//
// Filter completeness (Д-4/Д-5, two-era data): when an autonomous anonymizer
// has written the alias into newer rows, the database holds BOTH the real
// keyword (old era) and the literal alias (new era). Rules may carry eq_any —
// every stored spelling of the value — and ExpandAliasComparisons rewrites a
// full-literal =/LIKE/ILIKE filter into a multi-variant comparison
// (= ANY(ARRAY[...]) on PG, IN (...) on MSSQL) so the filter hits all eras
// and case variants instead of silently dropping rows.
//
// SECURITY: the rules file contains the very keywords being hidden. Treat it
// like a password: keep it out of git, restrict its ACL (icacls), never log
// rule values. Validation errors below deliberately report rule indexes, not
// rule contents.
package anon

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"
)

// defaultFragmentMinLen is the default fragment-guard threshold: runs of >= 4
// consecutive keyword characters that survived Apply get redacted ("…"). It
// matches the independent test report's threshold (Д-7) and catches
// left(col,4)-style 4-char leaks without exploding false positives.
const defaultFragmentMinLen = 4

// Rule is one YAML replacement rule. Field names follow the established
// anonymizer convention: read_substring is what is searched for (matched
// CASE-INSENSITIVELY by default), write_substring is inserted VERBATIM,
// exactly as written, and must not be empty. eq_any optionally lists every
// stored spelling of the value (case variants, and the literal alias itself
// when an upstream anonymizer already wrote it into newer rows) — full-literal
// filters are expanded to this set (see ExpandAliasComparisons).
type Rule struct {
	ReadSubstring  string   `yaml:"read_substring"`  // real value (SECRET), matched case-insensitively
	WriteSubstring string   `yaml:"write_substring"` // alias inserted verbatim; non-empty
	EqAny          []string `yaml:"eq_any,omitempty"`
	CaseSensitive  bool     `yaml:"case_sensitive,omitempty"`
	Exact          bool     `yaml:"exact,omitempty"` // match whole value only
}

// Security holds the tunables of the derivative guards.
type Security struct {
	// FragmentMinLen is the GuardFragments threshold in characters.
	// 0/omitted = default 4; negative disables the fragment guard.
	FragmentMinLen int `yaml:"fragment_min_len,omitempty"`
	// BlockedFunctionsOverride REPLACES the dialect default blocked-function
	// list wholesale (empty = keep defaults). Function names are not secrets.
	BlockedFunctionsOverride []string `yaml:"blocked_functions_override,omitempty"`
}

// Config is the YAML file shape.
type Config struct {
	Rules    []Rule    `yaml:"rules"`
	Security *Security `yaml:"security,omitempty"`
}

// compiled is a preprocessed rule for one direction.
type compiled struct {
	from  string // search text in this direction
	to    string // replacement in this direction
	ci    bool   // case-insensitive
	exact bool   // whole-value match only
	// preserveCase mirrors the all-uppercase shape of the matched text into
	// the replacement on the FORWARD direction only (test report Д-6):
	// upper(brand) is computed in the DB before masking, so an all-upper
	// token must yield the all-upper alias. Unmap never preserves case — the
	// restored keyword keeps the yaml spelling verbatim, which is what makes
	// exact-match filters predictable.
	preserveCase bool
}

// Anonymizer is an immutable compiled ruleset. All methods are nil-safe: a nil
// *Anonymizer is a pass-through (unit tests and tools built without masking
// rely on this).
type Anonymizer struct {
	forward []compiled        // keyword -> alias, longest search first
	reverse []compiled        // alias -> keyword, longest search first
	numeric map[string]string // canonical numeric keyword -> alias (exact)
	eqAny   map[string][]string
	// keyword -> [keyword + eq_any variants]; only rules WITH eq_any.
	fragmentMin   int      // GuardFragments threshold; <= 0 disables the guard
	blockedFnOvrd []string // security.blocked_functions_override; nil = dialect defaults
}

// Load reads and compiles a YAML rules file. Fails closed on any problem.
func Load(path string) (*Anonymizer, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read anonymize config: %w", err)
	}
	var cfg Config
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("parse anonymize config %s: %w", path, err)
	}
	a, err := NewWithConfig(&cfg)
	if err != nil {
		return nil, fmt.Errorf("anonymize config %s: %w", path, err)
	}
	return a, nil
}

// New validates and compiles rules. Rule indexes in errors are 1-based to
// match the YAML file the operator edits. Values are never echoed.
func New(rules []Rule) (*Anonymizer, error) {
	return NewWithConfig(&Config{Rules: rules})
}

// NewWithConfig is New plus the security tunables.
func NewWithConfig(cfg *Config) (*Anonymizer, error) {
	rules := cfg.Rules
	a := &Anonymizer{
		numeric:     map[string]string{},
		eqAny:       map[string][]string{},
		fragmentMin: defaultFragmentMinLen,
	}
	if cfg.Security != nil {
		if cfg.Security.FragmentMinLen > 0 {
			a.fragmentMin = cfg.Security.FragmentMinLen
		}
		if cfg.Security.FragmentMinLen < 0 {
			a.fragmentMin = 0 // explicit disable
		}
		a.blockedFnOvrd = cfg.Security.BlockedFunctionsOverride
	}

	for i, r := range rules {
		n := i + 1
		if strings.TrimSpace(r.ReadSubstring) == "" {
			return nil, fmt.Errorf("rule #%d: empty read_substring", n)
		}
		if strings.TrimSpace(r.WriteSubstring) == "" {
			return nil, fmt.Errorf("rule #%d: empty write_substring (must not be empty; redaction is not supported)", n)
		}
		if equalFoldOrCS(r.ReadSubstring, r.WriteSubstring, !r.CaseSensitive) {
			return nil, fmt.Errorf("rule #%d: read_substring equals write_substring (no-op rule)", n)
		}
		for j := 0; j < i; j++ {
			p := rules[j]
			if equalFoldOrCS(p.ReadSubstring, r.ReadSubstring, !r.CaseSensitive || !p.CaseSensitive) {
				return nil, fmt.Errorf("rule #%d: duplicate read_substring with rule #%d", n, j+1)
			}
			if equalFoldOrCS(p.WriteSubstring, r.WriteSubstring, !r.CaseSensitive || !p.CaseSensitive) {
				return nil, fmt.Errorf("rule #%d: duplicate write_substring with rule #%d (Unmap would be ambiguous)", n, j+1)
			}
		}
		// Chained rules would let one replacement be re-matched by a later
		// rule (worst case A->B, B->A ping-pong). Reject at load time.
		for j := 0; j < len(rules); j++ {
			if j == i {
				continue
			}
			other := rules[j]
			ci := !r.CaseSensitive || !other.CaseSensitive
			if other.ReadSubstring != "" && containsFoldOrCS(r.WriteSubstring, other.ReadSubstring, ci) {
				return nil, fmt.Errorf("rule #%d: write_substring contains the read_substring of rule #%d (chained rules are not supported)", n, j+1)
			}
		}

		fwd := compiled{from: r.ReadSubstring, to: r.WriteSubstring, ci: !r.CaseSensitive, exact: r.Exact, preserveCase: true}
		a.forward = append(a.forward, fwd)
		a.reverse = append(a.reverse, compiled{from: r.WriteSubstring, to: r.ReadSubstring, ci: !r.CaseSensitive, exact: r.Exact})
		if canonicalNumeric(r.ReadSubstring) {
			a.numeric[strings.TrimSpace(r.ReadSubstring)] = r.WriteSubstring
		}

		// eq_any: dedupe EXACTLY (not case-insensitively!) against the keyword
		// and each other — variants differing only in case are precisely the
		// point: PG '=' is case-sensitive, so every stored spelling must be
		// its own array element. An entry equal to write_substring is LEGAL
		// and expected — that is the literal-alias era of the data. No usable
		// variants -> no expansion.
		if len(r.EqAny) > 0 {
			variants := []string{r.ReadSubstring}
			for _, v := range r.EqAny {
				v = strings.TrimSpace(v)
				if v == "" {
					continue
				}
				dup := false
				for _, ex := range variants {
					if ex == v {
						dup = true
						break
					}
				}
				if !dup {
					variants = append(variants, v)
				}
			}
			if len(variants) > 1 {
				a.eqAny[r.ReadSubstring] = variants
			}
		}
	}

	sort.SliceStable(a.forward, func(i, j int) bool { return len(a.forward[i].from) > len(a.forward[j].from) })
	sort.SliceStable(a.reverse, func(i, j int) bool { return len(a.reverse[i].from) > len(a.reverse[j].from) })
	return a, nil
}

// Len returns the number of loaded rules (safe for logs/selftest output).
func (a *Anonymizer) Len() int {
	if a == nil {
		return 0
	}
	return len(a.forward)
}

// Apply masks every keyword occurrence in s (nil-safe pass-through).
func (a *Anonymizer) Apply(s string) string {
	if a == nil {
		return s
	}
	for _, r := range a.forward {
		s = applyOne(s, r)
	}
	return s
}

// Unmap restores original keywords from aliases in s (nil-safe pass-through).
func (a *Anonymizer) Unmap(s string) string {
	if a == nil {
		return s
	}
	for _, r := range a.reverse {
		s = applyOne(s, r)
	}
	return s
}

// ApplyNumericString reports the alias for a canonical numeric keyword (exact
// string form, e.g. "7701234567"), or "", false.
func (a *Anonymizer) ApplyNumericString(canonical string) (string, bool) {
	if a == nil || a.numeric == nil {
		return "", false
	}
	alias, ok := a.numeric[canonical]
	return alias, ok
}

// HasNumericRules reports whether numeric-cell masking is active.
func (a *Anonymizer) HasNumericRules() bool {
	return a != nil && len(a.numeric) > 0
}

// guardedApply masks keywords and then redacts surviving keyword fragments.
// Used on strings that already left the data layer (error texts, JSON values,
// bytea-decoded text); plain Apply stays non-guarding because it also runs on
// column names, where fragment false positives would break introspection.
func (a *Anonymizer) guardedApply(s string) string {
	s = a.Apply(s)
	s, _ = a.GuardFragments(s)
	return s
}

// ApplyErr returns an error whose text is masked and fragment-guarded.
// Wrapping is intentionally dropped: the result is only ever rendered as
// tool-output text.
func (a *Anonymizer) ApplyErr(err error) error {
	if a == nil || err == nil {
		return err
	}
	return errors.New(a.guardedApply(err.Error()))
}

// ApplyJSON masks strings (and map keys, recursively) inside a JSON document
// and returns the re-marshaled bytes. Non-JSON input is returned unchanged —
// callers use it for json/jsonb cells where a raw byte-level replace could
// corrupt the JSON structure.
func (a *Anonymizer) ApplyJSON(raw []byte) []byte {
	if a == nil || len(a.forward) == 0 || len(raw) == 0 {
		return raw
	}
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return raw
	}
	out, err := json.Marshal(a.applyJSONValue(v))
	if err != nil {
		return raw // never let masking break a result
	}
	return out
}

func (a *Anonymizer) applyJSONValue(v any) any {
	switch x := v.(type) {
	case string:
		return a.guardedApply(x)
	case float64:
		// JSON numbers: match numeric keywords by their exact decimal form.
		if a.HasNumericRules() {
			if s, ok := a.ApplyNumericString(strconv.FormatFloat(x, 'f', -1, 64)); ok {
				return s
			}
		}
		return x
	case map[string]any:
		m := make(map[string]any, len(x))
		for k, val := range x {
			m[a.Apply(k)] = a.applyJSONValue(val)
		}
		return m
	case []any:
		s := make([]any, len(x))
		for i, val := range x {
			s[i] = a.applyJSONValue(val)
		}
		return s
	default:
		return v
	}
}

// GuardFragments redacts fragments of real keywords that survived in s after
// Apply — defense-in-depth against derivatives computed in the database
// (left(col,4), reverse(), translate(), …): every case-insensitive run of
// fragmentMin or more consecutive keyword characters is replaced with "…".
// Alias occurrences are shielded first (the alias is public — a keyword
// fragment that also occurs inside the alias must not redact it). Returns the
// guarded string and whether anything was redacted.
func (a *Anonymizer) GuardFragments(s string) (string, bool) {
	if a == nil || len(a.forward) == 0 || a.fragmentMin < 1 {
		return s, false
	}
	// Shield alias occurrences behind sentinels (\x00 can't occur in YAML
	// keywords in practice, and shields are restored before returning).
	shields := map[string]string{}
	for i, r := range a.forward {
		if containsFoldOrCS(s, r.to, true) {
			ph := "\x00" + strconv.Itoa(i) + "\x00"
			s = replaceFold(s, r.to, ph, false)
			shields[ph] = r.to
		}
	}
	if !a.hasMinFragment(s) {
		for ph, alias := range shields {
			s = strings.ReplaceAll(s, ph, alias)
		}
		return s, false
	}
	hit := false
	for _, r := range a.forward {
		kw := []rune(r.from)
		// Longest fragments first, so maximal runs collapse into one "…".
		for L := len(kw); L >= a.fragmentMin; L-- {
			for j := 0; j+L <= len(kw); j++ {
				frag := string(kw[j : j+L])
				guarded := replaceFold(s, frag, "…", false)
				if guarded != s {
					hit = true
					s = guarded
				}
			}
		}
	}
	for ph, alias := range shields {
		s = strings.ReplaceAll(s, ph, alias)
	}
	return s, hit
}

// hasMinFragment is the cheap pre-check for GuardFragments: any redactable
// run of length >= N contains a run of exactly N, so if none of the
// length-N windows of any keyword occur, nothing can ever match.
func (a *Anonymizer) hasMinFragment(s string) bool {
	for _, r := range a.forward {
		kw := []rune(r.from)
		if len(kw) < a.fragmentMin {
			continue
		}
		for j := 0; j+a.fragmentMin <= len(kw); j++ {
			if containsFoldOrCS(s, string(kw[j:j+a.fragmentMin]), true) {
				return true
			}
		}
	}
	return false
}

// BlockedFunctions returns the effective blocked-function list: the config
// override when set, else the dialect defaults supplied by the caller.
func (a *Anonymizer) BlockedFunctions(dialectDefaults []string) []string {
	if a != nil && len(a.blockedFnOvrd) > 0 {
		return a.blockedFnOvrd
	}
	return dialectDefaults
}

// ProbeAliases exposes the aliases (write_substrings) for selftest probes —
// the aliases are not secrets (the session sees them in every masked result).
func (a *Anonymizer) ProbeAliases() []string {
	if a == nil {
		return nil
	}
	out := make([]string, 0, len(a.forward))
	for _, r := range a.forward {
		out = append(out, r.to)
	}
	return out
}

// CheckBlockedFunctions reports the first name in blocked that occurs in sql
// as a function call — the name as a whole word followed by optional
// whitespace and '('. Pass the literal/comment-masked statement copy so names
// inside string data ('uses md5 internally') don't trigger. Matching is
// case-insensitive. This is a DX guard, not a boundary: it stops accidental
// derivative leaks (left/md5/length over sensitive columns, test report
// Д-1/Д-2/Д-3) with a clear error; anything that slips past is still caught
// output-side by GuardFragments and the bytea text guard.
func CheckBlockedFunctions(sql string, blocked []string) (string, bool) {
	for _, name := range blocked {
		if name == "" {
			continue
		}
		if containsCall(sql, name) {
			return name, true
		}
	}
	return "", false
}

// containsCall reports whether sql contains name as a whole-word call.
func containsCall(s, name string) bool {
	n, m := len(s), len(name)
	for i := 0; i+m <= n; i++ {
		if !asciiEqualFoldByte(s[i], name[0]) {
			continue
		}
		if !asciiEqualFold(s[i:i+m], name) {
			continue
		}
		if i > 0 && isIdentByte(s[i-1]) {
			continue // glued to an identifier: brand_md5, x.left, "md5"
		}
		j := i + m
		for j < n && isSpaceByte(s[j]) {
			j++
		}
		if j < n && s[j] == '(' {
			return true
		}
	}
	return false
}

// comparisonLiteral is one string literal sitting on the right side of an
// equality/pattern comparison, with the span of its operator construct.
type comparisonLiteral struct {
	opStart, litEnd int    // replacement span [opStart, litEnd) in the source SQL
	op              string // "=", "LIKE", "ILIKE"
	negated         bool   // !=, <>, NOT LIKE / NOT ILIKE
	lit             string // unescaped literal text
}

// ExpandComparisonLiterals rewrites full-value string literals in equality /
// pattern comparisons. onLiteral receives the unescaped literal and returns
// replacement variants (nil/empty = leave untouched); render builds the SQL
// replacing the whole operator+literal span (e.g. "= 'x'" -> "= ANY(ARRAY[…])"
// on PG, "IN (…)" on T-SQL). LIKE/ILIKE literals containing wildcards
// (% _ \) are left alone — partial patterns are not translatable (test report
// Д-7, documented limitation). E'…' literals are never touched.
func ExpandComparisonLiterals(sql string, onLiteral func(lit string) []string, render func(op string, negated bool, variants []string) string) string {
	var out strings.Builder
	last := 0
	scanComparisonLiterals(sql, func(c comparisonLiteral) bool {
		if c.op != "=" && strings.ContainsAny(c.lit, `%_\`) {
			return false
		}
		variants := onLiteral(c.lit)
		if len(variants) == 0 {
			return false
		}
		out.WriteString(sql[last:c.opStart])
		out.WriteString(render(c.op, c.negated, variants))
		last = c.litEnd
		return false
	})
	out.WriteString(sql[last:])
	return out.String()
}

// ExpandAliasComparisons rewrites full-literal comparisons whose literal
// matches a rule keyword (or one of its eq_any spellings) into a multi-variant
// comparison via the dialect render function, so the filter hits every stored
// spelling of the value — both data eras (pre-migration keyword rows and
// post-migration literal-alias rows) and case variants. No-op unless the rule
// carries eq_any. Run AFTER Unmap on the SQL text (Unmap has already restored
// the agent's alias to the yaml keyword spelling by then).
func (a *Anonymizer) ExpandAliasComparisons(sql string, render func(op string, negated bool, variants []string) string) string {
	if a == nil || len(a.eqAny) == 0 {
		return sql
	}
	return ExpandComparisonLiterals(sql, a.variantsForLiteral, render)
}

func (a *Anonymizer) variantsForLiteral(lit string) []string {
	for kw, variants := range a.eqAny {
		if equalFoldOrCS(lit, kw, true) {
			return variants
		}
		for _, v := range variants {
			if equalFoldOrCS(lit, v, true) {
				return variants
			}
		}
	}
	return nil
}

// HasAliasEquality reports whether sql contains an '=' comparison — or any
// string bind arg — whose literal case-insensitively equals one of the
// aliases. That is the pattern that silently returns 0 rows when the restored
// keyword spelling's case doesn't match the stored data (test report Д-5);
// callers attach a notice when the result set came back empty.
func (a *Anonymizer) HasAliasEquality(sql string, args []any) bool {
	if a == nil {
		return false
	}
	found := false
	scanComparisonLiterals(sql, func(c comparisonLiteral) bool {
		if c.op == "=" && !c.negated && a.isAlias(c.lit) {
			found = true
			return true
		}
		return false
	})
	if found {
		return true
	}
	for _, arg := range args {
		if s, ok := arg.(string); ok && a.isAlias(s) {
			return true
		}
	}
	return false
}

// HasPartialPattern reports whether sql contains a LIKE/ILIKE pattern that
// includes a run of >= fragmentMin consecutive alias characters WITHOUT the
// full alias inside — those patterns are NOT translated by Unmap and may
// match unexpectedly (test report Д-7). Patterns containing the whole alias
// are fine (Unmap replaces it in place, wildcards or not).
func (a *Anonymizer) HasPartialPattern(sql string) bool {
	if a == nil || len(a.reverse) == 0 {
		return false
	}
	found := false
	scanComparisonLiterals(sql, func(c comparisonLiteral) bool {
		if c.op != "LIKE" && c.op != "ILIKE" {
			return false
		}
		for _, r := range a.reverse {
			if containsFoldOrCS(c.lit, r.from, true) {
				continue // full alias inside: translated in place
			}
			if hasAliasRun(c.lit, r.from, a.hintRunLen()) {
				found = true
				return true
			}
		}
		return false
	})
	return found
}

// hintRunLen is the partial-pattern notice threshold (>= fragmentMin).
func (a *Anonymizer) hintRunLen() int {
	if a.fragmentMin > 0 {
		return a.fragmentMin
	}
	return defaultFragmentMinLen
}

// hasAliasRun reports whether pattern contains >= min consecutive characters
// of alias (case-insensitive).
func hasAliasRun(pattern, alias string, min int) bool {
	ar := []rune(alias)
	if min < 1 {
		min = 1
	}
	for L := len(ar); L >= min; L-- {
		for j := 0; j+L <= len(ar); j++ {
			if containsFoldOrCS(pattern, string(ar[j:j+L]), true) {
				return true
			}
		}
	}
	return false
}

// isAlias reports whether s equals (CI) one of the aliases.
func (a *Anonymizer) isAlias(s string) bool {
	for _, r := range a.reverse {
		if equalFoldOrCS(s, r.from, true) {
			return true
		}
	}
	return false
}

// scanComparisonLiterals visits every standard '..' string literal that sits
// directly on the right side of an equality/pattern comparison (=, !=, <>,
// [NOT] LIKE, [NOT] ILIKE). E'…' escape literals are skipped entirely
// (backslash semantics differ; they are rare in filters). visit returns true
// to stop scanning.
func scanComparisonLiterals(sql string, visit func(c comparisonLiteral) bool) {
	b := []byte(sql)
	n := len(b)
	for i := 0; i < n; i++ {
		if b[i] != '\'' {
			continue
		}
		escape := i > 0 && (b[i-1] == 'e' || b[i-1] == 'E') &&
			(i-1 == 0 || !isIdentByte(b[i-2]))
		end, ok := scanStringLiteral(b, i, escape)
		if !ok {
			return // unterminated literal: the DB rejects the statement anyway
		}
		if !escape {
			if opStart, op, negated, found := comparisonBefore(b, i); found {
				lit := strings.ReplaceAll(string(b[i+1:end-1]), "''", "'")
				if visit(comparisonLiteral{opStart: opStart, litEnd: end, op: op, negated: negated, lit: lit}) {
					return
				}
			}
		}
		i = end - 1
	}
}

// scanStringLiteral returns the index just past the closing quote of the
// literal opening at b[i]; ok=false when unterminated.
func scanStringLiteral(b []byte, i int, escape bool) (int, bool) {
	n := len(b)
	j := i + 1
	for j < n {
		if escape && b[j] == '\\' {
			j += 2
			continue
		}
		if b[j] == '\'' {
			if j+1 < n && b[j+1] == '\'' {
				j += 2 // doubled quote is data
				continue
			}
			return j + 1, true
		}
		j++
	}
	return n, false
}

// comparisonBefore finds the comparison operator construct that ends just
// before position litStart (skipping whitespace). found=false when the
// literal is not the right-hand side of an equality/pattern comparison.
func comparisonBefore(b []byte, litStart int) (opStart int, op string, negated, found bool) {
	j := litStart - 1
	for j >= 0 && isSpaceByte(b[j]) {
		j--
	}
	if j < 0 {
		return 0, "", false, false
	}
	// National-string prefix (SQL standard / T-SQL N'…'): in `= N'x'` the N
	// is glued to the literal, not an operator. Skip it when it starts its
	// own token and classify by what precedes — the replaced span starts at
	// the operator, so the rewrite absorbs the N (needed for MSSQL, whose
	// agents write unicode literals as N'…').
	if j == litStart-1 && (b[j] == 'N' || b[j] == 'n') && (j == 0 || !isIdentByte(b[j-1])) {
		j--
		for j >= 0 && isSpaceByte(b[j]) {
			j--
		}
		if j < 0 {
			return 0, "", false, false
		}
	}
	switch b[j] {
	case '=':
		if j > 0 && b[j-1] == '!' {
			return j - 1, "=", true, true
		}
		if j > 0 && (b[j-1] == '<' || b[j-1] == '>' || b[j-1] == '=' || b[j-1] == '|') {
			return 0, "", false, false // <=, >=, ==, ||= are not value equality
		}
		return j, "=", false, true
	case '>':
		if j > 0 && b[j-1] == '<' {
			return j - 1, "=", true, true // <>
		}
		return 0, "", false, false
	}
	// Word operators: [NOT] LIKE / [NOT] ILIKE.
	k := j
	for k >= 0 && !isSpaceByte(b[k]) && b[k] != '(' && b[k] != ',' {
		k--
	}
	word := string(b[k+1 : j+1])
	if !asciiEqualFold(word, "LIKE") && !asciiEqualFold(word, "ILIKE") {
		return 0, "", false, false
	}
	op = strings.ToUpper(word)
	m := k
	for m >= 0 && isSpaceByte(b[m]) {
		m--
	}
	if m >= 2 {
		k2 := m
		for k2 >= 0 && !isSpaceByte(b[k2]) && b[k2] != '(' {
			k2--
		}
		if asciiEqualFold(string(b[k2+1:m+1]), "NOT") {
			return k2 + 1, op, true, true
		}
	}
	return k + 1, op, false, true
}

// applyOne applies a single compiled rule to s.
func applyOne(s string, r compiled) string {
	if r.exact {
		if equalFoldOrCS(s, r.from, r.ci) {
			if r.preserveCase {
				return upperIfUpper(s, r.to)
			}
			return r.to
		}
		return s
	}
	if r.ci {
		return replaceFold(s, r.from, r.to, r.preserveCase)
	}
	rep := r.to
	if r.preserveCase {
		rep = upperIfUpper(r.from, rep) // CS: the matched text IS from, verbatim
	}
	return strings.ReplaceAll(s, r.from, rep)
}

// upperIfUpper mirrors the all-uppercase shape of src into the replacement
// (test report Д-6): upper(brand) is computed in the DB before masking, so an
// all-upper token must yield the all-upper alias — otherwise the two data
// eras (masked keyword rows vs literal-alias rows run through upper()) show
// two different spellings of one alias. Case-less script letters (CJK and
// friends — upper() leaves them alone) are neutral: only cased letters vote.
// Everything else stays canonical.
func upperIfUpper(src, to string) string {
	hasCased := false
	allUpper := true
	for _, r := range src {
		if !unicode.IsLetter(r) {
			continue
		}
		if !unicode.IsUpper(r) && !unicode.IsLower(r) {
			continue // case-less letter (e.g. CJK): neutral
		}
		if unicode.IsLower(r) {
			allUpper = false
			break
		}
		hasCased = true
	}
	if hasCased && allUpper {
		return strings.ToUpper(to)
	}
	return to
}

// replaceFold replaces every case-insensitive occurrence of from in s with to.
// It scans with EqualFold over byte windows of len(from) instead of searching
// in a lowercased copy: ToLower can change byte length for some runes, which
// would misalign indexes into the original. When preserve is set, an
// all-uppercase matched window writes the uppercase replacement (Д-6).
func replaceFold(s, from, to string, preserve bool) string {
	if len(from) == 0 || len(s) < len(from) {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	i := 0
	for i+len(from) <= len(s) {
		if strings.EqualFold(s[i:i+len(from)], from) {
			rep := to
			if preserve {
				rep = upperIfUpper(s[i:i+len(from)], to)
			}
			b.WriteString(rep)
			i += len(from)
			continue
		}
		// Advance one rune, not one byte, to keep multibyte text cheap.
		_, size := decodeRune(s[i:])
		b.WriteString(s[i : i+size])
		i += size
	}
	b.WriteString(s[i:])
	return b.String()
}

// decodeRune is strings.RuneCount-free decode of the first rune (size only).
func decodeRune(s string) (rune, int) {
	for i, r := range s {
		_ = i
		return r, len(string(r))
	}
	return 0, 0
}

func equalFoldOrCS(a, b string, ci bool) bool {
	if ci {
		return strings.EqualFold(a, b)
	}
	return a == b
}

// containsFoldOrCS reports whether s contains sub (fold-aware per ci).
func containsFoldOrCS(s, sub string, ci bool) bool {
	if sub == "" {
		return true
	}
	if !ci {
		return strings.Contains(s, sub)
	}
	if len(s) < len(sub) {
		return false
	}
	for i := 0; i+len(sub) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(sub)], sub) {
			return true
		}
	}
	return false
}

// --- ASCII helpers shared by the call/word scanners (SQL identifiers and
// function names are ASCII in practice; non-ASCII bytes simply never match). ---

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if !asciiEqualFoldByte(a[i], b[i]) {
			return false
		}
	}
	return true
}

func asciiEqualFoldByte(a, b byte) bool {
	if a >= 'A' && a <= 'Z' {
		a += 'a' - 'A'
	}
	if b >= 'A' && b <= 'Z' {
		b += 'a' - 'A'
	}
	return a == b
}

func isIdentByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
		c >= '0' && c <= '9' || c == '_' || c == '$' || c == '"'
}

func isSpaceByte(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// canonicalNumeric reports whether s is a plain (optionally signed) decimal
// integer, the canonical rendering of PG int8 / MSSQL bigint cells. Leading
// zeros are rejected: PG/MSSQL never emit them.
func canonicalNumeric(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	if s[0] == '-' || s[0] == '+' {
		s = s[1:]
	}
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
