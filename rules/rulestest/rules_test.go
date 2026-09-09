package rulestest

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// requiredEnv, when set, turns a missing or mismatched golangci-lint binary
// into a failure instead of a skip. CI sets it so the probe can never be
// skipped there.
const requiredEnv = "GOLANGCI_LINT_REQUIRED"

// probeDir is the package that plants every old pattern, relative to the
// repository root. It sits under testdata so `./...` never builds, tests or
// lints it.
const probeDir = "rules/testdata/probe"

// rulesNotProbed lists matchers that cannot be exercised from the probe with a
// written reason. Keep it short.
var rulesNotProbed = map[string]string{
	// The match is gated on the file importing github.com/google/uuid, and
	// adding that module as a dependency just to prove the rule fires is not
	// worth it. The rule is exercised in any project that imports it.
	"StdlibUUID": "gated on a github.com/google/uuid import this module does not have",
}

// wantRE captures the quoted strings of a `// want "a" "b"` annotation.
var wantRE = regexp.MustCompile(`//\s*want((?:\s+"(?:[^"\\]|\\.)*")+)`)

// quotedRE splits the capture of wantRE into its quoted strings.
var quotedRE = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)

// ruleMarkerRE captures the name of a `// rule: Name` section marker.
var ruleMarkerRE = regexp.MustCompile(`^\s*//\s*rule:\s*([A-Za-z_]\w*)\s*$`)

// finding is one ruleguard issue reported by golangci-lint.
type finding struct {
	file string // base name of the file
	line int
	text string // message with the "ruleguard: " prefix stripped
}

// lintOutput is the subset of golangci-lint's JSON output the test reads.
type lintOutput struct {
	Issues []struct {
		FromLinter string `json:"FromLinter"`
		Text       string `json:"Text"`
		Pos        struct {
			Filename string `json:"Filename"`
			Line     int    `json:"Line"`
		} `json:"Pos"`
	} `json:"Issues"`
}

// TestRulesFireOnProbe runs golangci-lint with only gocritic enabled over the
// probe package and checks the ruleguard findings against the `// want`
// annotations, both ways: a want with no finding means the matcher regressed,
// a finding with no want means a matcher fires where the probe did not
// expect it (a false positive, or a probe line that needs a second want).
func TestRulesFireOnProbe(t *testing.T) {
	root := repoRoot(t)
	bin := golangciLint(t, root)

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "run",
		"--enable-only", "gocritic",
		"--uniq-by-line=false",
		"--max-issues-per-linter=0",
		"--max-same-issues=0",
		"--show-stats=false",
		"--output.json.path=stdout",
		"./"+probeDir+"/...",
	)
	cmd.Dir = root
	// A throwaway cache. golangci-lint's analysis cache is keyed on the probe
	// sources and the config, not on the rule files, so with the shared cache
	// an edited matcher can keep serving the previous run's findings and a
	// regression passes. MEASURED against golangci-lint 2.13.2: sabotaging a
	// Match pattern left this test green until the cache was cleared.
	cmd.Env = append(os.Environ(), "GOLANGCI_LINT_CACHE="+t.TempDir())
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	// Exit status 1 means issues were found, which is the whole point.
	if ee, ok := errors.AsType[*exec.ExitError](err); err != nil && (!ok || ee.ExitCode() != 1) {
		t.Fatalf("golangci-lint failed: %v\nstderr:\n%s\nstdout:\n%s", err, stderr.String(), out)
	}

	got := parseFindings(t, out)
	if len(got) == 0 {
		t.Fatalf("golangci-lint reported no ruleguard findings on %s; the rules are not being loaded\nstderr:\n%s", probeDir, stderr.String())
	}
	want := readWants(t, filepath.Join(root, probeDir))

	// Match each finding to the first still-unmatched want on the same
	// file:line whose text is a substring of the message.
	matched := make([]bool, len(want))
	for _, f := range got {
		idx := -1
		for i, w := range want {
			if !matched[i] && w.file == f.file && w.line == f.line && strings.Contains(f.text, w.text) {
				idx = i
				break
			}
		}
		if idx < 0 {
			t.Errorf("%s:%d: unexpected finding %q", f.file, f.line, f.text)
			continue
		}
		matched[idx] = true
	}
	for i, w := range want {
		if !matched[i] {
			t.Errorf("%s:%d: want %q, no matching finding", w.file, w.line, w.text)
		}
	}
}

// TestEveryRuleHasAProbe parses rules/*.go and checks that every matcher
// function has a `// rule: Name` section in the probe package, or an entry in
// rulesNotProbed. It also fails when rulesNotProbed names a matcher that no
// longer exists, so the list cannot go stale.
func TestEveryRuleHasAProbe(t *testing.T) {
	root := repoRoot(t)
	rules := ruleFunctions(t, filepath.Join(root, "rules"))
	if len(rules) == 0 {
		t.Fatal("no matcher functions found in rules/*.go")
	}
	markers := ruleMarkers(t, filepath.Join(root, probeDir))

	for _, name := range rules {
		if _, ok := markers[name]; ok {
			continue
		}
		if reason, ok := rulesNotProbed[name]; ok {
			t.Logf("%s: not probed: %s", name, reason)
			continue
		}
		t.Errorf("matcher %s has no `// rule: %s` section in %s", name, name, probeDir)
	}
	for name := range markers {
		if !slices.Contains(rules, name) {
			t.Errorf("probe marker `// rule: %s` names a matcher that does not exist", name)
		}
	}
	for name := range rulesNotProbed {
		if !slices.Contains(rules, name) {
			t.Errorf("rulesNotProbed names %s, which no longer exists", name)
		}
		if _, ok := markers[name]; ok {
			t.Errorf("rulesNotProbed names %s, but the probe has a section for it; drop the entry", name)
		}
	}
}

// repoRoot resolves the repository root from this file's location
// (rules/rulestest/rules_test.go) so the test works regardless of the go test
// working directory.
func repoRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed; cannot locate the repository root")
	}
	root := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	if _, err := os.Stat(filepath.Join(root, ".golangci.yaml")); err != nil {
		t.Fatalf("%s does not look like the repository root (no .golangci.yaml): %v", root, err)
	}
	return root
}

// golangciLint locates the golangci-lint binary and compares its version with
// the pin in .golangci-version. A missing binary or a version mismatch is a
// skip locally and a failure when requiredEnv is set.
func golangciLint(t *testing.T, root string) string {
	t.Helper()
	required := os.Getenv(requiredEnv) != ""
	bin, err := exec.LookPath("golangci-lint")
	if err != nil {
		if required {
			t.Fatalf("%s is set but golangci-lint is not in PATH", requiredEnv)
		}
		t.Skipf("golangci-lint not in PATH; set %s to make this a failure", requiredEnv)
	}

	pinned, err := os.ReadFile(filepath.Join(root, ".golangci-version"))
	if err != nil {
		t.Fatalf("reading .golangci-version: %v", err)
	}
	pin := strings.TrimPrefix(strings.TrimSpace(string(pinned)), "v")

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version", "--short").Output()
	if err != nil {
		t.Fatalf("golangci-lint version: %v", err)
	}
	have := strings.TrimPrefix(strings.TrimSpace(string(out)), "v")
	if have != pin {
		if required {
			t.Fatalf("golangci-lint %s in PATH, but .golangci-version pins %s", have, pin)
		}
		t.Skipf("golangci-lint %s in PATH, but .golangci-version pins %s; install the pinned version (task tools) to run the probe", have, pin)
	}
	return bin
}

// parseFindings decodes golangci-lint's JSON output and keeps the ruleguard
// findings, keyed by file base name and line.
func parseFindings(t *testing.T, out []byte) []finding {
	t.Helper()
	var lo lintOutput
	if err := json.Unmarshal(out, &lo); err != nil {
		t.Fatalf("decoding golangci-lint JSON output: %v\noutput:\n%s", err, out)
	}
	var fs []finding
	for _, is := range lo.Issues {
		if is.FromLinter != "gocritic" {
			continue
		}
		text, ok := strings.CutPrefix(is.Text, "ruleguard: ")
		if !ok {
			continue
		}
		fs = append(fs, finding{file: filepath.Base(is.Pos.Filename), line: is.Pos.Line, text: text})
	}
	return fs
}

// wantEntry is one expected substring on one probe line.
type wantEntry struct {
	file string
	line int
	text string
}

// readWants scans every .go file in dir for `// want "..."` annotations.
func readWants(t *testing.T, dir string) []wantEntry {
	t.Helper()
	var wants []wantEntry
	for _, path := range goFiles(t, dir) {
		f, err := os.Open(path)
		if err != nil {
			t.Fatalf("opening %s: %v", path, err)
		}
		sc := bufio.NewScanner(f)
		for n := 1; sc.Scan(); n++ {
			m := wantRE.FindStringSubmatch(sc.Text())
			if m == nil {
				continue
			}
			for _, q := range quotedRE.FindAllStringSubmatch(m[1], -1) {
				text, err := strconv.Unquote(`"` + q[1] + `"`)
				if err != nil {
					t.Fatalf("%s:%d: bad want string %q: %v", path, n, q[0], err)
				}
				wants = append(wants, wantEntry{file: filepath.Base(path), line: n, text: text})
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		if err := f.Close(); err != nil {
			t.Fatalf("closing %s: %v", path, err)
		}
	}
	if len(wants) == 0 {
		t.Fatalf("no // want annotations found under %s", dir)
	}
	return wants
}

// ruleMarkers scans the probe files for `// rule: Name` markers and returns
// the set of names.
func ruleMarkers(t *testing.T, dir string) map[string]struct{} {
	t.Helper()
	markers := make(map[string]struct{})
	for _, path := range goFiles(t, dir) {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("reading %s: %v", path, err)
		}
		for line := range strings.Lines(string(data)) {
			if m := ruleMarkerRE.FindStringSubmatch(line); m != nil {
				markers[m[1]] = struct{}{}
			}
		}
	}
	return markers
}

// ruleFunctions parses every rule file in dir (reading them directly, since
// their build tag hides them from the toolchain) and returns the names of the
// top-level functions taking a single dsl.Matcher parameter.
func ruleFunctions(t *testing.T, dir string) []string {
	t.Helper()
	fset := token.NewFileSet()
	var names []string
	for _, path := range goFiles(t, dir) {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Recv != nil || fd.Type.Params.NumFields() != 1 {
				continue
			}
			if isMatcherParam(fd.Type.Params.List[0]) {
				names = append(names, fd.Name.Name)
			}
		}
	}
	slices.Sort(names)
	return names
}

// isMatcherParam reports whether a parameter has type dsl.Matcher.
func isMatcherParam(field *ast.Field) bool {
	sel, ok := field.Type.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "dsl" && sel.Sel.Name == "Matcher"
}

// goFiles lists the .go files directly under dir, sorted.
func goFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var paths []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			paths = append(paths, filepath.Join(dir, e.Name()))
		}
	}
	if len(paths) == 0 {
		t.Fatalf("no .go files under %s", dir)
	}
	return paths
}

// String makes findings readable in failure output.
func (f finding) String() string {
	return fmt.Sprintf("%s:%d: %s", f.file, f.line, f.text)
}
