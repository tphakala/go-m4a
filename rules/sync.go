//go:build ruleguard

package gorules

import "github.com/quasilyte/go-ruleguard/dsl"

// WaitGroupGo detects the old sync.WaitGroup pattern and suggests using Go 1.25's wg.Go().
//
// The old pattern:
//
//	wg.Add(1)
//	go func() {
//	    defer wg.Done()
//	    doSomething()
//	}()
//
// Can be simplified to:
//
//	wg.Go(func() {
//	    doSomething()
//	})
//
// Benefits:
//   - Cleaner, less error-prone (no Add/Done mismatch)
//   - Single function call
//
// wg.Go still calls Done on panic (it is deferred), but it does not recover
// the panic; a panicking task crashes the program just as before.
//
// The Report texts use a literal "{ ... }" rather than $body: the body is a
// $* capture, and interpolating it dumps the whole closure body into the
// message, multi-line for anything but a one-statement body. The Suggest
// templates do interpolate $body, which is what a rewrite needs. MEASURED
// against golangci-lint 2.13.2: --fix turns a multi-statement Add/Done
// goroutine into a compiling wg.Go call.
//
// See: https://pkg.go.dev/sync#WaitGroup.Go
func WaitGroupGo(m dsl.Matcher) {
	// Pattern 1: wg.Add(1) followed by go func() with defer wg.Done()
	// This matches when the defer is the first statement
	m.Match(
		`$wg.Add(1); go func() { defer $wg.Done(); $*body }()`,
	).
		Where(m["wg"].Type.Is("*sync.WaitGroup") || m["wg"].Type.Is("sync.WaitGroup")).
		Report("use $wg.Go(func() { ... }) instead of manual Add/Done pattern (Go 1.25+)").
		Suggest("$wg.Go(func() { $body })")

	// Pattern 2: Same for a named type whose underlying type is sync.WaitGroup
	m.Match(
		`$wg.Add(1); go func() { defer $wg.Done(); $*body }()`,
	).
		Where(m["wg"].Type.Underlying().Is("sync.WaitGroup")).
		Report("use $wg.Go(func() { ... }) instead of manual Add/Done pattern (Go 1.25+)").
		Suggest("$wg.Go(func() { $body })")

	// Pattern 3: When wg is passed by reference to the closure. No Suggest: the
	// body refers to the closure parameter, which the rewrite would remove.
	m.Match(
		`$wg.Add(1); go func($param $typ) { defer $param.Done(); $*body }($wg)`,
		`$wg.Add(1); go func($param $typ) { defer $param.Done(); $*body }(&$wg)`,
	).
		Where(m["wg"].Type.Is("*sync.WaitGroup") || m["wg"].Type.Is("sync.WaitGroup")).
		Report("use $wg.Go(func() { ... }) instead of manual Add/Done pattern (Go 1.25+)")
}

// AtomicTypes detects the primitive sync/atomic functions on plain integers and
// pointers and suggests the typed atomic wrappers (atomic.Int64, atomic.Pointer,
// and so on). This mirrors the atomictypes modernizer that go fix gained in Go 1.27.
//
// Old pattern:
//
//	var hits int64
//	atomic.AddInt64(&hits, 1)
//	n := atomic.LoadInt64(&hits)
//
// New pattern (types available since Go 1.19):
//
//	var hits atomic.Int64
//	hits.Add(1)
//	n := hits.Load()
//
// Benefits:
//   - The compiler rejects non-atomic reads and writes of the field
//   - 64-bit alignment is guaranteed on 32-bit platforms
//   - The intent is visible in the type, not only at each call site
//
// See: https://pkg.go.dev/sync/atomic#Int64
// See: https://pkg.go.dev/sync/atomic#Pointer
func AtomicTypes(m dsl.Matcher) {
	m.Match(
		`atomic.$fn(&$x, $*_)`,
	).
		Where(m["fn"].Text.Matches(`^(Add|Load|Store|Swap|CompareAndSwap|And|Or)(Int32|Int64|Uint32|Uint64|Uintptr)$`)).
		Report("use a typed atomic (atomic.Int64, atomic.Uint32, ...) for $x and call its methods instead of the atomic.$fn function on &$x; typed atomics forbid non-atomic access and fix 32-bit alignment; when $x is a slice/array element or struct field, change the element or field type rather than the call site; not applicable if $x is marshaled or crosses an ABI boundary")

	m.Match(
		`atomic.$fn(&$x, $*_)`,
	).
		Where(m["fn"].Text.Matches(`^(Load|Store|Swap|CompareAndSwap)Pointer$`)).
		Report("use atomic.Pointer[T] for $x and call its methods instead of the atomic.$fn function on &$x; the generic wrapper removes the unsafe.Pointer casts")
}
