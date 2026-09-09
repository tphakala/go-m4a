//go:build ruleguard

// Package gorules defines custom ruleguard matchers for Go modernization and
// for a handful of correctness and security hazards the stock linters miss.
//
// The matchers are loaded by golangci-lint's gocritic ruleguard checker (see
// linters.settings.gocritic.settings.ruleguard in .golangci.yaml). Every file
// carries the //go:build ruleguard tag so the normal Go toolchain ignores
// them; `go vet -tags ruleguard ./rules/` compiles them as a canary, and the
// config sets ruleguard's failOn so a rule file that does not parse fails the
// lint run instead of being dropped silently.
//
// # Verification
//
// rulestest/rules_test.go runs golangci-lint (gocritic only) over rules/testdata/probe,
// a package that plants each old pattern, and checks the ruleguard findings
// against the `// want "..."` annotations on the planted lines: every want must
// be matched by a finding and every finding by a want. A second test checks
// that every matcher function in this package has a `// rule: Name` section in
// the probe, so a new matcher without a probe fails the suite. The test needs
// the golangci-lint binary; it skips when the binary is absent unless
// GOLANGCI_LINT_REQUIRED is set, which CI does.
//
// # Adding a matcher
//
//  1. Add the function to the file that matches its subject (or a new file with
//     the same build tag and package clause).
//  2. Add a `// rule: Name` section to rules/testdata/probe with at least one
//     line per Match group, each carrying a want annotation.
//  3. Run `task rules` (go vet -tags ruleguard ./rules/, then the probe test).
//
// Ruleguard behaviours to keep in mind while writing one, all MEASURED
// against golangci-lint 2.13.2 (ruleguard 0.4.5) unless noted:
//
//   - One finding per AST node. When two matchers match the same node, only
//     the first in load order (files in name order, functions in source order)
//     is reported. A new matcher that overlaps an existing one may therefore
//     never show its message on the overlapping shape.
//   - golangci-lint's analysis cache is keyed on the linted sources and the
//     config, not on the rule files. After editing a matcher, run
//     `golangci-lint cache clean` before trusting a lint run; the probe test
//     uses its own throwaway cache for this reason.
//   - A pattern's package qualifier is resolved through the file's imports,
//     so `rand.Intn($n)` also matches a call through an aliased import.
//   - A $* capture (for example $*body) is written into a Report as the
//     verbatim source of every captured node, newlines included, so messages
//     say "{ ... }" instead. In a Suggest the capture is named without the
//     star ($body) and --fix writes the captured statements out; the starred
//     spelling in a Suggest is emitted literally.
//   - Suggest only when the rewrite is a compiling drop-in. A rewrite that
//     changes the result type (MinMaxBuiltin, NewWithExpression) or removes
//     the last use of an import (SynctestSleep) is Report-only, because --fix
//     does not prune imports. Every $*-captured loop body must be covered by
//     a Where clause that rules out a semantics change (RangeOverInteger).
//
// # Project-specific matchers
//
// This package holds only generic rules. A project that needs its own (a
// dependency boundary such as "no libm transcendentals inside the bit-exact
// encoder", or a house error-constructor idiom) adds a separate file here
// under the same build tag, gates it with m.File().PkgPath or m.Import, and
// gives it a probe section like any other matcher.
//
// # Relationship to the modernize linter
//
// modernize is disabled in .golangci.yaml because these rules carry the
// Go 1.20 through Go 1.27 modernization patterns, several with richer messages
// or safety exclusions than the stock analyzers. Running both would make two
// linters report the same line, and under issues.uniq-by-line only one message
// survives, chosen by run order rather than by quality.
package gorules
