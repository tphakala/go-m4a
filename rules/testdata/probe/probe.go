// Package probe plants every old pattern the rules/*.go matchers target, one
// section per matcher, so rules_test.go can check that each matcher still
// fires. It lives under testdata so `./...` never builds, tests or lints it;
// rules_test.go points golangci-lint at it explicitly.
//
// Conventions:
//   - A `// rule: Name` marker starts the section for the matcher Name.
//   - Every planted line carries a want comment (the word want followed by one
//     quoted substring per expected ruleguard finding on that line, in any
//     order). A line with no want comment must produce no finding.
//   - ruleguard reports at most one finding per AST node: when two matchers
//     match the same node, the first in load order wins (files in name order,
//     functions in source order). Plant such overlaps so that the matcher
//     under test is the one that wins, or the one that is left after the
//     other's exclusions.
//   - The code only has to type-check; it is never executed.
package probe

import (
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"math"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"
)

// rule: ErrorsAsType
func errorsAsType(err error) string {
	var pathErr *fs.PathError
	if errors.As(err, &pathErr) { // want "errors.AsType[T](err), where T is pathErr's type"
		return pathErr.Path
	}
	return ""
}

// rule: MinMaxBuiltin
func minMaxBuiltin(a, b int, c, d int64, e, f int32) {
	_ = int(math.Min(float64(a), float64(b)))   // want "use min(a, b)"
	_ = int64(math.Min(float64(c), float64(d))) // want "use min(c, d)"
	_ = int32(math.Min(float64(e), float64(f))) // want "use min(e, f)"
	_ = int(math.Max(float64(a), float64(b)))   // want "use max(a, b)"
	_ = int64(math.Max(float64(c), float64(d))) // want "use max(c, d)"
	_ = int32(math.Max(float64(e), float64(f))) // want "use max(e, f)"
}

// rule: ClearBuiltin
func clearBuiltin(m map[string]int) {
	for k := range m { // want "use clear(m)"
		delete(m, k)
	}
	for k, _ := range m { // want "use clear(m)"
		delete(m, k)
	}
}

// rule: RangeOverInteger
func rangeOverInteger(n int, use func(int)) {
	for i := 0; i < n; i++ { // want "use for i := range n"
		use(i)
	}
	// Not flagged: the bound is re-evaluated by the classic loop.
	s := []int{1, 2, 3}
	for i := 0; i < len(s); i++ {
		use(s[i])
	}
	// Not flagged: the body mutates the index.
	for i := 0; i < n; i++ {
		i++
		use(i)
	}
	// Not flagged: the body advances the index with a compound assignment.
	for i := 0; i < n; i++ {
		i += 2
		use(i)
	}
}

// rule: AppendWithoutValues
func appendWithoutValues(s []int) []int {
	s = append(s) // want "append with single argument has no effect"
	return s
}

// rule: NewWithExpression
func newWithExpression() *int {
	return &[]int{42}[0] // want "use new(42)"
}

// rule: EmbeddedFieldLiteral
type base struct{ Name string }

type config struct {
	base
	Port int
}

type withHeader struct {
	http.Header
	Port int
}

func embeddedFieldLiteral() {
	_ = config{base: base{Name: "x"}, Port: 80}                            // want "initialize the promoted field directly"
	_ = withHeader{Header: http.Header{"Accept": []string{"*"}}, Port: 80} // want "initialize the promoted field directly"
}

// rule: DeprecatedTLSConfigRand
func deprecatedTLSConfigRand(r io.Reader) {
	cfg := &tls.Config{Rand: r} // want "tls.Config.Rand is deprecated"
	cfg.Rand = r                // want "tls.Config.Rand is deprecated"
	var v tls.Config
	v.Rand = r // want "tls.Config.Rand is deprecated"
	_ = cfg
}

// rule: JoinHostPort
func joinHostPort(host string, port int) {
	_ = fmt.Sprintf("%s:%d", host, port) // want "net.JoinHostPort(host, strconv.Itoa(port))"
	_ = fmt.Sprintf("%v:%d", host, port) // want "net.JoinHostPort(host, strconv.Itoa(port))"
}

// rule: FilepathIsLocal
type request struct{ Path string }

func filepathIsLocal(path, userPath, filename, version string, req request) bool {
	_ = strings.Contains(path, "..")     // want "filepath.IsLocal(path)"
	_ = strings.Contains(userPath, "..") // want "filepath.IsLocal(userPath)"
	_ = strings.Contains(filename, "..") // want "filepath.IsLocal(filename)"
	// Not flagged: the operand does not read like a path, or is a selector.
	_ = strings.Contains(version, "..")
	return strings.Contains(req.Path, "..")
}

// rule: DeprecatedReverseProxyDirector
func deprecatedReverseProxyDirector(director func(*http.Request)) {
	_ = &httputil.ReverseProxy{Director: director} // want "Director is deprecated"
	proxy := &httputil.ReverseProxy{}
	proxy.Director = director // want "Director is deprecated"
	var value httputil.ReverseProxy
	value.Director = director // want "Director is deprecated"
}

// rule: ErrorBeforeUse
func errorBeforeUse(path string) string {
	f, err := os.Open(path) // want "f may be nil if err != nil"
	name := f.Name()
	if err != nil {
		return ""
	}
	return name
}

func errorBeforeUseBareCall(path string) {
	f, err := os.Create(path) // want "f may be nil if err != nil"
	f.Close()
	if err != nil {
		return
	}
}

func errorBeforeUseMultiValue(path string, buf []byte) {
	f, err := os.OpenFile(path, os.O_RDONLY, 0) // want "f may be nil if err != nil"
	n, rerr := f.Read(buf)
	if err != nil {
		return
	}
	_, _ = n, rerr
}

func errorBeforeUseDefer(path string) {
	f, err := os.Open(path) // want "f may be nil if err != nil"
	defer f.Close()
	if err != nil {
		return
	}
}

// rule: URLValuesClone
func urlValuesClone(v url.Values) url.Values {
	return maps.Clone(v) // want "use v.Clone() instead of maps.Clone(v)"
}

// rule: ResponseBodyDrain
func responseBodyDrain(resp *http.Response) {
	io.Copy(io.Discard, resp.Body) // want "io.Copy(io.Discard, resp.Body) before Close is redundant"
	resp.Body.Close()
}

func responseBodyDrainWithTrailer(resp *http.Response) http.Header {
	// Not flagged: the block reads resp.Trailer afterwards.
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.Trailer
}

// rule: PreferAddCleanup
func preferAddCleanup(obj *config, fn func(*config)) {
	runtime.SetFinalizer(obj, fn) // want "runtime.AddCleanup"
}

// rule: GorootDeprecated
func gorootDeprecated() string {
	return runtime.GOROOT() // want "runtime.GOROOT() is deprecated"
}

// rule: SortInts
func sortInts(ints []int, strs []string, floats []float64) {
	sort.Ints(ints)                    // want "slices.Sort(ints)"
	sort.Strings(strs)                 // want "slices.Sort(strs)"
	sort.Float64s(floats)              // want "slices.Sort(floats)"
	_ = sort.IntsAreSorted(ints)       // want "slices.IsSorted(ints)"
	_ = sort.StringsAreSorted(strs)    // want "slices.IsSorted(strs)"
	_ = sort.Float64sAreSorted(floats) // want "slices.IsSorted(floats)"
}

// rule: BytesClone
// SlicesClone excludes []byte, so these lines are BytesClone's alone.
func bytesClone(b []byte) {
	_ = append([]byte(nil), b...) // want "bytes.Clone(b)"
	_ = append([]byte{}, b...)    // want "bytes.Clone(b)"
	_ = append(b[:0:0], b...)     // want "bytes.Clone(b)"
}

// rule: SlicesClone
func slicesClone(s []string) {
	_ = append([]string(nil), s...) // want "slices.Clone(s)"
	_ = append([]string{}, s...)    // want "slices.Clone(s)"
	_ = append(s[:0:0], s...)       // want "slices.Clone(s)"
}

// rule: BackwardIteration
type ints []int

func backwardIteration(s []int, named ints, str string, use func(int), useB func(byte)) {
	for i := len(s) - 1; i >= 0; i-- { // want "slices.Backward(s)"
		use(s[i])
	}
	for i := len(s) - 1; i > -1; i-- { // want "slices.Backward(s)"
		use(s[i])
	}
	for i := len(named) - 1; i >= 0; i-- { // want "slices.Backward(named)"
		use(named[i])
	}
	// Not flagged: slices.Backward does not accept a string.
	for i := len(str) - 1; i >= 0; i-- {
		useB(str[i])
	}
}

// rule: MapKeysCollection
func mapKeysCollection(m map[string]int, s []string) []string {
	keys := make([]string, 0, len(m))
	for k := range m { // want "slices.Collect(maps.Keys(m))"
		keys = append(keys, k)
	}
	for k, _ := range m { // want "slices.Collect(maps.Keys(m))"
		keys = append(keys, k)
	}
	// Not flagged: ranging a slice yields indexes, not keys.
	var idx []int
	for i := range s {
		idx = append(idx, i)
	}
	return keys
}

// rule: MapValuesCollection
func mapValuesCollection(m map[string]int, s []int) []int {
	values := make([]int, 0, len(m))
	for _, v := range m { // want "slices.Collect(maps.Values(m))"
		values = append(values, v)
	}
	// Not flagged: over a slice this loop is a copy.
	for _, v := range s {
		values = append(values, v)
	}
	return values
}

// rule: SliceRepeat
// With a plain identifier bound the classic-loop form is also a
// RangeOverInteger match, and that rule loads first (builtins.go sorts before
// slices.go), so it takes the node; see the BytesClone note. A len() bound is
// excluded by RangeOverInteger, which leaves the node to SliceRepeat.
func sliceRepeat(s []int, n int, n64 int64, items [][]int) []int {
	var result []int
	for i := 0; i < n; i++ { // want "use for i := range n"
		result = append(result, s...)
	}
	for i := 0; i < len(s); i++ { // want "slices.Repeat(s, len(s))"
		result = append(result, s...)
	}
	// Not flagged: s[i:] depends on the loop variable, so slices.Repeat is not
	// equivalent and i is out of scope after the loop.
	for i := range n {
		result = append(result, s[i:]...)
	}
	for range n { // want "slices.Repeat(s, n)"
		result = append(result, s...)
	}
	// Not flagged: slices.Repeat's count is exactly int, and ranging a slice
	// is not a repetition count at all.
	for range n64 {
		result = append(result, s...)
	}
	for i := range items {
		result = append(result, items[i]...)
	}
	return result
}

// rule: ReflectTypeAssert
func reflectTypeAssert(v reflect.Value) string {
	return v.Interface().(string) // want "reflect.TypeAssert[string](v)"
}

// rule: DeprecatedReflectPtrTo
func deprecatedReflectPtrTo(t reflect.Type) reflect.Type {
	return reflect.PtrTo(t) // want "reflect.PointerTo(t)"
}

// rule: ReflectTypeOf
func reflectTypeOf() reflect.Type {
	return reflect.TypeOf((*config)(nil)).Elem() // want "reflect.TypeFor[config]()"
}

// rule: DeprecatedReflectHeaders
func deprecatedReflectHeaders(s []byte, str string) {
	_ = reflect.SliceHeader{}                         // want "reflect.SliceHeader is deprecated"
	_ = reflect.SliceHeader{Len: 1}                   // want "reflect.SliceHeader is deprecated"
	_ = reflect.StringHeader{}                        // want "reflect.StringHeader is deprecated"
	_ = reflect.StringHeader{Len: 1}                  // want "reflect.StringHeader is deprecated"
	_ = (*reflect.SliceHeader)(unsafe.Pointer(&s))    // want "reflect.SliceHeader is deprecated"
	_ = (*reflect.StringHeader)(unsafe.Pointer(&str)) // want "reflect.StringHeader is deprecated"
}

// rule: ReflectFieldsIterator
func reflectFieldsIterator(t reflect.Type, v reflect.Value) {
	for i := 0; i < t.NumField(); i++ { // want "range t.Fields()"
		_ = t.Field(i)
	}
	for i := 0; i < v.NumField(); i++ { // want "range v.Fields()"
		_ = v.Field(i)
	}
	for i := range t.NumField() { // want "range t.Fields()"
		_ = t.Field(i)
	}
	for i := range v.NumField() { // want "range v.Fields()"
		_ = v.Field(i)
	}
}

// rule: ReflectMethodsIterator
func reflectMethodsIterator(t reflect.Type, v reflect.Value) {
	for i := 0; i < t.NumMethod(); i++ { // want "range t.Methods()"
		_ = t.Method(i)
	}
	for i := 0; i < v.NumMethod(); i++ { // want "range v.Methods()"
		_ = v.Method(i)
	}
	for i := range t.NumMethod() { // want "range t.Methods()"
		_ = t.Method(i)
	}
	for i := range v.NumMethod() { // want "range v.Methods()"
		_ = v.Method(i)
	}
}

// rule: ReflectInsOutsIterator
func reflectInsOutsIterator(t reflect.Type) {
	for i := 0; i < t.NumIn(); i++ { // want "range t.Ins()"
		_ = t.In(i)
	}
	for i := 0; i < t.NumOut(); i++ { // want "range t.Outs()"
		_ = t.Out(i)
	}
	for i := range t.NumIn() { // want "range t.Ins()"
		_ = t.In(i)
	}
	for i := range t.NumOut() { // want "range t.Outs()"
		_ = t.Out(i)
	}
}

// rule: StringsLinesIteration
func stringsLinesIteration(s string, b []byte, use func(string), useB func([]byte)) {
	for _, line := range strings.Split(s, "\n") { // want "strings.Lines(s)"
		use(line)
	}
	for _, line := range strings.Split(s, "\r\n") { // want "strings.Lines(s)"
		use(line)
	}
	for _, line := range bytes.Split(b, []byte("\n")) { // want "bytes.Lines(b)"
		useB(line)
	}
	for _, line := range bytes.Split(b, []byte{'\n'}) { // want "bytes.Lines(b)"
		useB(line)
	}
}

// rule: StringsSplitIteration
func stringsSplitIteration(s string, b []byte, use func(string), useB func([]byte)) {
	for _, part := range strings.Split(s, ",") { // want "strings.SplitSeq(s, \",\")"
		use(part)
	}
	for _, part := range bytes.Split(b, []byte(",")) { // want "bytes.SplitSeq(b, []byte(\",\"))"
		useB(part)
	}
}

// rule: StringsFieldsIteration
func stringsFieldsIteration(s string, b []byte, use func(string), useB func([]byte)) {
	for _, field := range strings.Fields(s) { // want "strings.FieldsSeq(s)"
		use(field)
	}
	for _, field := range bytes.Fields(b) { // want "bytes.FieldsSeq(b)"
		useB(field)
	}
}

// rule: StringsFieldsFuncIteration
func stringsFieldsFuncIteration(s string, b []byte, f func(rune) bool, use func(string), useB func([]byte)) {
	for _, field := range strings.FieldsFunc(s, f) { // want "strings.FieldsFuncSeq(s, f)"
		use(field)
	}
	for _, field := range bytes.FieldsFunc(b, f) { // want "bytes.FieldsFuncSeq(b, f)"
		useB(field)
	}
}

// rule: StringsCutLast
func stringsCutLast(s, sep string, b, bsep []byte) {
	_ = s[:strings.LastIndex(s, sep)]          // want "strings.CutLast(s, sep) instead of slicing before"
	_ = s[strings.LastIndex(s, sep)+len(sep):] // want "strings.CutLast(s, sep) instead of slicing after"
	_ = b[:bytes.LastIndex(b, bsep)]           // want "bytes.CutLast(b, bsep) instead of slicing before"
	_ = b[bytes.LastIndex(b, bsep)+len(bsep):] // want "bytes.CutLast(b, bsep) instead of slicing after"
	_ = s[strings.LastIndex(s, "/")+1:]        // want "strings.CutLast(s, \"/\") instead of slicing around"
	_ = s[strings.LastIndex(s, "\n")+1:]       // want "strings.CutLast(s, \"\\n\") instead of slicing around"
	_ = b[bytes.LastIndex(b, []byte("/"))+1:]  // want "bytes.CutLast(b, []byte(\"/\")) instead of slicing around"
	_ = b[bytes.LastIndex(b, []byte{'/'})+1:]  // want "bytes.CutLast(b, []byte{'/'}) instead of slicing around"
	// Not flagged: with the +1 form a multi-character separator keeps part of
	// itself, and a multi-byte rune would be cut in the middle.
	_ = s[strings.LastIndex(s, "::")+1:]
	_ = s[strings.LastIndex(s, "ä")+1:]

	if i := strings.LastIndex(s, sep); i >= 0 { // want "strings.CutLast(s, sep) instead of checking"
		_ = s[:i]
	}
	if i := strings.LastIndex(s, sep); i != -1 { // want "strings.CutLast(s, sep) instead of checking"
		_ = s[i+len(sep):]
	}
	if i := bytes.LastIndex(b, bsep); i >= 0 { // want "bytes.CutLast(b, bsep) instead of checking"
		_ = b[:i]
	}
	if i := bytes.LastIndex(b, bsep); i != -1 { // want "bytes.CutLast(b, bsep) instead of checking"
		_ = b[i+len(bsep):]
	}
	// Not flagged: the index is not used to slice.
	if i := strings.LastIndex(s, sep); i >= 0 {
		_ = i
	}
}

// rule: WaitGroupGo
func waitGroupGo(work func()) {
	var wg sync.WaitGroup
	wg.Add(1) // want "wg.Go(func() { ... })"
	go func() {
		defer wg.Done()
		work()
	}()

	pwg := &sync.WaitGroup{}
	pwg.Add(1) // want "pwg.Go(func() { ... })"
	go func() {
		defer pwg.Done()
		work()
	}()

	wg.Add(1) // want "wg.Go(func() { ... })"
	go func(w *sync.WaitGroup) {
		defer w.Done()
		work()
	}(&wg)
	wg.Wait()
	pwg.Wait()
}

// rule: AtomicTypes
func atomicTypes(hits *int64, flag *uint32, p *unsafe.Pointer) {
	var counter int64
	atomic.AddInt64(&counter, 1) // want "use a typed atomic (atomic.Int64, atomic.Uint32, ...) for counter"
	_ = atomic.LoadInt64(hits)   // not flagged: no address-of on a plain pointer
	atomic.StoreUint32(flag, 1)  // not flagged: no address-of on a plain pointer
	var ready uint32
	_ = atomic.CompareAndSwapUint32(&ready, 0, 1) // want "use a typed atomic (atomic.Int64, atomic.Uint32, ...) for ready"
	var ptr unsafe.Pointer
	_ = atomic.LoadPointer(&ptr)  // want "use atomic.Pointer[T] for ptr"
	atomic.StorePointer(&ptr, *p) // want "use atomic.Pointer[T] for ptr"
}

// rule: TimeDateTimeConstants
func timeDateTimeConstants(t time.Time, pt *time.Time, s string) {
	_ = t.Format("2006-01-02 15:04:05")         // want "t.Format(time.DateTime)"
	_ = pt.Format("2006-01-02 15:04:05")        // want "pt.Format(time.DateTime)"
	_, _ = time.Parse("2006-01-02 15:04:05", s) // want "time.Parse(time.DateTime, s)"
	_ = t.Format("2006-01-02")                  // want "t.Format(time.DateOnly)"
	_, _ = time.Parse("2006-01-02", s)          // want "time.Parse(time.DateOnly, s)"
	_ = t.Format("15:04:05")                    // want "t.Format(time.TimeOnly)"
	_, _ = time.Parse("15:04:05", s)            // want "time.Parse(time.TimeOnly, s)"
}

type customFormatter struct{}

func (customFormatter) Format(string) string { return "" }

func timeDateTimeConstantsNotTime(f customFormatter) {
	// Not flagged: Format on a value that is not a time.Time.
	_ = f.Format("2006-01-02")
}

// rule: TimerChannelLen
func timerChannelLen(timer *time.Timer, ticker *time.Ticker) {
	_ = len(timer.C)  // want "len() on timer channel is always 0"
	_ = len(ticker.C) // want "len() on ticker channel is always 0"
	_ = cap(timer.C)  // want "cap() on timer channel is always 0"
	_ = cap(ticker.C) // want "cap() on ticker channel is always 0"
}

// rule: DeferredTimeSince
func deferredTimeSince(start time.Time) {
	defer log.Println(time.Since(start))                // want "time.Since(start) is evaluated at defer time"
	defer log.Println(time.Since(start), "done")        // want "time.Since(start) is evaluated at defer time"
	defer log.Println("took", time.Since(start))        // want "time.Since(start) is evaluated at defer time"
	defer log.Println("a", "b", time.Since(start))      // want "time.Since(start) is evaluated at defer time"
	defer log.Println("took", time.Since(start), "ms")  // want "time.Since(start) is evaluated at defer time"
	defer log.Println("a", "b", "c", time.Since(start)) // want "time.Since(start) is evaluated at defer time"
}

// rule: DeferredTimeNow
func deferredTimeNow() {
	defer log.Println(time.Now())             // want "time.Now() is evaluated at defer time"
	defer log.Println("finished", time.Now()) // want "time.Now() is evaluated at defer time"
}
