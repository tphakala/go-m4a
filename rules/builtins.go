//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// MinMaxBuiltin detects the math.Min/math.Max float64-conversion form wrapped
// in an integer conversion and suggests the built-in min/max functions.
//
// Old pattern (only this form is detected; an if/else min is not matched):
//
//	result := int(math.Min(float64(a), float64(b)))
//
// New pattern (Go 1.21+):
//
//	result := min(a, b)
//	result := max(a, b)
//
// Benefits:
//   - Cleaner, more readable code
//   - Works with any ordered type
//   - No type conversion needed
//
// Report-only, no autofix. The rewrite drops the outer integer conversion that
// makes the expression type-check: min/max return the operand type, so when
// the operands are narrower or wider than the conversion target the result no
// longer compiles, and removing the only math.Min/Max call orphans the "math"
// import, which --fix does not prune.
//
// See: https://pkg.go.dev/builtin#min
// See: https://pkg.go.dev/builtin#max
func MinMaxBuiltin(m dsl.Matcher) {
	// math.Min with float64 conversion for integers
	m.Match(
		`int(math.Min(float64($a), float64($b)))`,
	).
		Report("use min($a, $b) instead of int(math.Min(float64(...))) (Go 1.21+)")

	m.Match(
		`int64(math.Min(float64($a), float64($b)))`,
	).
		Report("use min($a, $b) instead of int64(math.Min(float64(...))) (Go 1.21+)")

	m.Match(
		`int32(math.Min(float64($a), float64($b)))`,
	).
		Report("use min($a, $b) instead of int32(math.Min(float64(...))) (Go 1.21+)")

	// math.Max with float64 conversion for integers
	m.Match(
		`int(math.Max(float64($a), float64($b)))`,
	).
		Report("use max($a, $b) instead of int(math.Max(float64(...))) (Go 1.21+)")

	m.Match(
		`int64(math.Max(float64($a), float64($b)))`,
	).
		Report("use max($a, $b) instead of int64(math.Max(float64(...))) (Go 1.21+)")

	m.Match(
		`int32(math.Max(float64($a), float64($b)))`,
	).
		Report("use max($a, $b) instead of int32(math.Max(float64(...))) (Go 1.21+)")
}

// ClearBuiltin detects loop-based map/slice clearing patterns and suggests
// using the built-in clear() function.
//
// Old pattern (only map clearing is detected; a slice-zeroing loop is not
// matched):
//
//	for k := range m {
//	    delete(m, k)
//	}
//
// New pattern (Go 1.21+):
//
//	clear(m)  // Deletes all map entries
//
// Benefits:
//   - Cleaner, more readable code
//   - More efficient (optimized implementation)
//   - Works with maps and slices
//
// See: https://pkg.go.dev/builtin#clear
func ClearBuiltin(m dsl.Matcher) {
	// Map clearing pattern: for k := range m { delete(m, k) }
	m.Match(
		`for $k := range $m { delete($m, $k) }`,
	).
		Report("use clear($m) instead of loop-based map clearing (Go 1.21+)").
		Suggest("clear($m)")

	// Map clearing with underscore value: for k, _ := range m { delete(m, k) }
	m.Match(
		`for $k, _ := range $m { delete($m, $k) }`,
	).
		Report("use clear($m) instead of loop-based map clearing (Go 1.21+)").
		Suggest("clear($m)")
}

// RangeOverInteger detects traditional for loops that iterate from 0 to n
// and suggests using the Go 1.22+ range-over-integer syntax.
//
// Old pattern:
//
//	for i := 0; i < n; i++ {
//	    process(i)
//	}
//
// New pattern (Go 1.22+):
//
//	for i := range n {
//	    process(i)
//	}
//
// Benefits:
//   - More concise and readable
//   - Intent is clearer (iterate n times)
//   - Less error-prone (no off-by-one mistakes)
//
// Note: Only matches loops starting from 0 with < comparison and i++.
// Loops with different starting values, comparisons, or increments
// are intentionally not flagged. The bound $n must also be a plain
// identifier or dotted selector (x, obj.field): a call or len(...) bound
// is not flagged, because range evaluates its operand once whereas the
// classic loop re-evaluates the bound each iteration, so the rewrite is
// not always equivalent. The body must also not reassign $n or mutate the
// index $i or take its address, since range owns the index and evaluates
// its operand once. This also excludes b.N (see the .N filter below).
//
// See: https://go.dev/doc/go1.22#language
func RangeOverInteger(m dsl.Matcher) {
	// Pattern: for i := 0; i < n; i++
	// Exclude benchmark loops (b.N) which should use b.Loop() instead
	m.Match(
		`for $i := 0; $i < $n; $i++ { $*body }`,
	).
		Where(
			m["n"].Text.Matches(`^[A-Za-z_]\w*(\.[A-Za-z_]\w*)*$`) &&
				!m["n"].Text.Matches(`.*\.N$`) &&
				!m["body"].Contains(`$n = $_`) &&
				!m["body"].Contains(`$n += $_`) &&
				!m["body"].Contains(`$n -= $_`) &&
				!m["body"].Contains(`$n++`) &&
				!m["body"].Contains(`$n--`) &&
				!m["body"].Contains(`$i = $_`) &&
				!m["body"].Contains(`$i += $_`) &&
				!m["body"].Contains(`$i -= $_`) &&
				!m["body"].Contains(`$i++`) &&
				!m["body"].Contains(`$i--`) &&
				!m["body"].Contains(`&$i`),
		).
		Report("use for $i := range $n instead of for $i := 0; $i < $n; $i++ (Go 1.22+)").
		// The replacement names the body capture as $body (no star): the $*body
		// spelling belongs to the match pattern and is written out literally by
		// --fix. MEASURED against golangci-lint 2.13.2: --fix rewrites a
		// multi-statement body into a compiling range loop. The Where clauses
		// above keep the rewrite semantics-preserving.
		Suggest("for $i := range $n { $body }")
}

// AppendWithoutValues detects append calls with no values which have no effect.
//
// Broken pattern:
//
//	slice = append(slice)  // No effect
//
// See: https://pkg.go.dev/builtin#append
// Note: Go 1.22 vet tool also warns about this pattern.
func AppendWithoutValues(m dsl.Matcher) {
	m.Match(
		`append($s)`,
	).
		Report("append with single argument has no effect; did you forget the values to append?")
}

// NewWithExpression detects the slice-literal hack for getting a pointer to a value
// and suggests using Go 1.26's enhanced new() built-in.
//
// Old pattern (slice hack):
//
//	field := &[]string{"hello"}[0]
//	field := &[]int{42}[0]
//	field := &[]time.Duration{5 * time.Second}[0]
//
// New pattern (Go 1.26+):
//
//	field := new("hello")
//	field := new(42)
//	field := new(5 * time.Second)
//
// Benefits:
//   - Eliminates the obscure slice-literal-index hack
//   - Clearer intent: "pointer to this value"
//   - No intermediate slice allocation
//   - Works with any expression, including function calls
//
// Report-only, no autofix: new($val) takes the type of $val, so for an untyped
// constant whose default type is not $typ (&[]int64{42}[0] yields *int64,
// new(42) yields *int) the rewrite has to be new($typ($val)), and the rule
// cannot tell the two cases apart.
//
// See: https://go.dev/doc/go1.26#language
func NewWithExpression(m dsl.Matcher) {
	// Pattern: &[]T{v}[0] - the well-known slice hack for pointer-to-value
	m.Match(
		`&[]$typ{$val}[0]`,
	).
		Report("use new($val) instead of &[]$typ{$val}[0] (Go 1.26+); write new($typ($val)) when $val is an untyped constant whose default type is not $typ")
}

// EmbeddedFieldLiteral detects a struct literal that initializes a promoted
// field through a nested literal of the embedded type, and suggests keying the
// promoted field directly, which Go 1.27 allows.
//
// Old pattern:
//
//	type Base struct{ Name string }
//	type Config struct {
//	    Base
//	    Port int
//	}
//	cfg := Config{Base: Base{Name: "x"}, Port: 80}
//
// New pattern (Go 1.27+):
//
//	cfg := Config{Name: "x", Port: 80}
//
// The spec now allows a struct literal key to be any valid field selector for
// a (possibly promoted) field of the struct, as long as no embedded field on the
// path is a pointer. This mirrors the embedlit modernizer in go fix.
//
// The rule only fires when the key and the nested literal's type share the same
// name, which is what an embedded value field looks like in a literal. A regular
// field that happens to be named after its type (Base Base) also matches, and a
// promoted name that is shadowed or ambiguous in the outer struct cannot be
// keyed directly, so this is a report rather than a rewrite.
//
// See: https://go.dev/doc/go1.27#language
func EmbeddedFieldLiteral(m dsl.Matcher) {
	m.Match(
		`$T{$*_, $emb: $embT{$k: $v, $*_}, $*_}`,
	).
		Where(m["emb"].Text == m["embT"].Text).
		Report("initialize the promoted field directly: $T{$k: $v, ...} instead of $emb: $embT{$k: $v, ...} (Go 1.27+); not applicable if $k is shadowed or ambiguous in $T, or if $emb is a regular field named after its type")

	// Embedded type from another package: field name is the bare type name.
	m.Match(
		`$T{$*_, $emb: $pkg.$embT{$k: $v, $*_}, $*_}`,
	).
		Where(m["emb"].Text == m["embT"].Text).
		Report("initialize the promoted field directly: $T{$k: $v, ...} instead of $emb: $pkg.$embT{$k: $v, ...} (Go 1.27+); not applicable if $k is shadowed or ambiguous in $T, or if $emb is a regular field named after its type")
}
