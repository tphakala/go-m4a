# AGENTS.md

Orientation for coding agents and automated reviewers working on this repository. README.md is the user-facing reference (usage examples, codec coverage, decode limits); CONTRIBUTING.md has the contribution rules. This file is the map for changing and reviewing the code.

## What this is

`github.com/tphakala/go-m4a` (root package `m4a`) is a pure-Go MP4/M4A muxer and demuxer for AAC-LC, Opus and FLAC audio. It is the container half that codec libraries (go-aac, go-opus, go-flac) leave to an external muxer: it writes access units into `ftyp | mdat | moov` (or CMAF fragments), with an edit list that trims encoder priming for gapless playback, and reads them back. It aims to:

1. **Build anywhere Go builds.** No cgo, no external binaries. The root `m4a` package is stdlib-only; each codec comes in through an optional bridge subpackage, so a consumer that only muxes or demuxes pulls no codec.
2. **Be a container, not a codec.** The core never parses a codec bitstream. Audio quality belongs to the inner codec repos; what this library guarantees is container correctness: sample-accurate round trips, correct sample tables and edit lists, bit-exact FLAC.
3. **Survive hostile input.** The reader and the bridges parse untrusted bytes. Parsing is bounds-checked and total, never panics, and every allocation driven by a file's own claims is bounded. Consumers are long-running unattended services (BirdNET-Go is the main one, shipping on 32-bit ARM and 386 as well).

Packages:

| Package | Role |
|---|---|
| `m4a` (root) | Codec-agnostic `Writer`, `FragmentWriter`/`InitSegment` (CMAF for live HLS/DASH), `Reader` (plain and fragmented input), `Info`, `Codec`, sentinel errors |
| `aacm4a` | go-aac bridge: `EncodeInterleaved`, `NewDecoder`, `NewSeekableDecoder` (`Decoder` with `SeekSample` and `SeekTime`) |
| `opusm4a` | go-opus bridge: `EncodeInterleaved`, `DecodeInterleaved[Limit]`, `DecodeStream` |
| `flacm4a` | go-flac bridge: same shape as `opusm4a` |
| `internal/box` | ISO BMFF byte primitives: big-endian append-style marshalers and total, size-capped parsers |
| `internal/reservation` | Shared buffer-reservation policy for the accumulating decodes |
| `internal/integrity` | Round-trip integrity gate (tests only, pinned baseline, ffmpeg cross-checks) |
| `internal/testutil` | Test-only helpers: in-memory `WriteSeeker`, signal helpers, `LookTool`; imported only from `_test.go` files |
| `rules` | gocritic ruleguard matchers (build tag `ruleguard`, lint-only) |

Non-goals: video or multiple audio tracks, other codecs, writing metadata tags, HE-AAC (typed as unsupported by the `aacm4a` decoder), more-than-stereo Opus (tracked in issue #5, blocked upstream).

### API stability

The public API is not stable before v1.0.0. Exported types, functions, struct fields and error types may change in any v0.x release when a cleaner design calls for it, and such a change ships as a minor version bump with a note in the release. Prefer the better API over compatibility: do not add deprecated aliases, adapters, compatibility shims or wrapper functions to keep an old signature working, and do not bend a design to avoid a breaking change. A changed signature still needs every caller in this repo updated, plus the README, godoc and a `CHANGELOG.md` entry under Unreleased.

The design rules below are not part of this: they are the library's contract and hold across every release.

## Design rules you must not break

A change that violates one of these is wrong even if tests pass.

1. **The core stays stdlib-only.** Only `aacm4a`, `opusm4a` and `flacm4a` import a codec module. Never import a codec into the root package or `internal/box`. `go-ruleguard/dsl` is lint-only, behind a build tag.
2. **No cgo, ever.** Everything builds with `CGO_ENABLED=0`.
3. **Parsers are total and size-capped.** An entry count or length read from a file is checked against the bytes actually present before it drives a `make`, a loop or an offset. Offset and size arithmetic uses overflow-safe widths and is correct on 32-bit (`int` is 32 bits on 386 and arm; use `int64`/`uint64` for sizes and ticks). Malformed input returns a typed error, never a panic or an index error.
4. **Typed errors with `%w`.** Map conditions to the sentinels in `errors.go` so callers can use `errors.Is`:
   - `ErrCorrupt`: malformed container, or a well-formed container whose codec payload will not decode (the bridges return it for that too).
   - `ErrUnsupported`: well-formed but out of scope (no audio track, other codec, unsupported fragment base offset, init segment with no samples, a time seek on a track with no sample timing or an edit list that does not reduce to one presented window).
   - `ErrClosed`: `WriteFrame` or `Close` after `Close`.
   - `ErrOutOfRange`: `Reader.SeekSample` index outside `[0, Info().FrameCount)`, or a `Reader.SeekTime` time before 0 or at or past the presented end; a caller error, not a verdict on the file.
   - `ErrBoxTooLarge` and `ErrDecodeLimit`: resource limits, not verdicts on the file. Keep them distinct from `ErrCorrupt`.
5. **Bounded decodes.** `DecodeInterleaved` stops at `m4a.DefaultMaxDecodedBytes` and returns `ErrDecodeLimit`; `DecodeInterleavedLimit` takes the ceiling; `DecodeStream` never accumulates. The decoded size of audio is not proportional to the file size (a FLAC constant subframe, DTX Opus packets), so the limit is a defence against amplification and silence-heavy field recordings alike. Reservations derived from file input need both a hard ceiling (`reservation.MaxPCMReservation`) and a cross-check against what the container can hold; reservations derived from caller input need only an overflow guard. The rule and its reasoning live in `internal/reservation/reservation.go`; follow it rather than adding a parallel copy in a bridge.
6. **Reader memory is bounded.** Box bodies larger than the reader's box-buffer limit (`DefaultMaxBoxBuffer`, `WithMaxBoxBuffer`) are refused before allocation, so a sparse file claiming a multi-gigabyte `moov` or `moof` cannot drive a large heap allocation. The frame read-ahead window is at most 64 KiB per Reader (`frameWindowSize`), and NewReader reads box headers and box bodies up to half its size through a separate window of at most 4 KiB (`openWindowSize`) that is dropped when NewReader returns. Both are `min(size, stream length)` (`newWindow`), never sized from box claims, and every refill seeks first, so reads never depend on where the caller's stream was left.
7. **Edit list semantics.** Lossy codecs trim priming through `elst`: `media_time` skips the priming (`EncoderDelay`, default 1024 for AAC, `DefaultOpusPreSkip` 312 for Opus) and `segment_duration` (from `WriterConfig.MediaLength`) excludes trailing padding. FLAC has no priming and gets no edit list. The fragmented writer uses `segment_duration` 0 (open-ended) and rejects `MediaLength`. `NoEdit` opts out. The reader resolves the edit list once (`reader_edit.go`) into the window behind `Info.EncoderDelay`, `Duration`, `StartOffset` and `SeekTime`. The decoders in the bridges are not edit-list aware; callers trim with `Info.EncoderDelay` and `Info.Duration`. `aacm4a.Decoder.SeekTime` starts its output at the presented time but still does not trim the tail.
8. **Fragmented writer discipline.** `FragmentWriter` never seeks, copies each access unit (callers may reuse their scratch slice), reuses its buffers, and a steady-state segment allocates nothing. Segments use 64-bit `tfdt` decode times (a 32-bit one at 48 kHz wraps after about 24.8 hours). `Reset` releases an arena that one outlier segment grew; `Grow` is a capacity hint only and never changes emitted bytes. Per-segment caps (`maxSamplesPerSegment`, `maxSegmentBytes`) are enforced.
9. **Codec-specific limits are validated, not silently adapted.** AAC-LC takes the eleven MPEG-4 table rates the 16-bit sample-entry field holds (88.2 and 96 kHz are rejected rather than written wrong), mono or stereo. Opus fixes the container timescale at 48 kHz, accepts 8/12/16/24/48 kHz source audio and records the true rate in `dOps`, mono or stereo. FLAC takes 1 to 8 channels and rates up to 1048575 Hz (20-bit STREAMINFO maximum), with a reduced hint in the sample entry above 16 bits. A config outside these limits is an error from `NewWriter`, never a quietly wrong file.
10. **Interop is the oracle.** Output must be readable by ffmpeg, and the `dOps` bytes match ffmpeg's. The reader must accept files from ffmpeg and Apple `afconvert` (extra boxes such as `free`, `udta`, `meta`, `sgpd`, `sbgp`, multi-chunk `stsc`, constant `stsz`, `co64`).

## Layout

```
codec.go              Codec enum (AACLC is the zero value) and String
errors.go             Sentinel errors, shared by reader, writer and bridges
decode.go             DefaultMaxDecodedBytes and the reasoning behind it

writer.go             WriterConfig, Writer (needs io.WriteSeeker: mdat size is patched at Close),
                      per-codec config validation, moov assembly, edit list
fragment.go           InitSegment, FragmentWriter (styp/moof/mdat), Reset/Grow, segment caps

reader.go             Reader: top-level scan, moov parse, sample-table geometry, ReadFrame,
                      ReadFrameInto, RawStream (framing for go-aac pcm.WithRawStream), Info
reader_edit.go        Edit-list resolver: the one presented window behind EncoderDelay, Duration, StartOffset, SeekTime
reader_time.go        SeekTime and the run-length timing table (stts or trun durations) behind it
reader_fragment.go    Fragmented (CMAF) input: init moov plus moof/mdat accumulation
reader_window.go      Read-ahead window shared by frame reads and NewReader's box reads (window, openReader, openWindowSize)

internal/box/         box.go (FourCC, byte primitives), marshal.go (typed marshalers),
                      fragment.go (moof/traf/trun marshalers), parse.go and parse_fragment.go
                      (table parsers), plus fuzz targets for each parser
internal/reservation/ NewLimit, MaxPCMReservation, MaxRetainedSlack, ShouldTrim
internal/integrity/   Round-trip gate, testdata/baseline.json
internal/testutil/    Test-only helpers shared by the root, bridge and integrity tests (WriteSeeker, MDATPayload, NormXCorr, LookTool)

aacm4a/ opusm4a/ flacm4a/   Codec bridges (one file each plus tests; aacm4a also has decoder.go)
rules/                gocritic ruleguard matchers; rules/rulestest runs the probe test
testdata/interop/     ffmpeg and afconvert produced .m4a/.mp4 files for reader interop tests
```

The writer and reader dispatch on `Codec`, so all three codecs share the `ftyp | mdat | moov` plumbing, sample tables and edit list; only the sample entry and its codec-specific box (`esds`, `dOps`, `dfLa`) differ. Adding a codec touches the `Codec` enum, writer validation and sample entry, the reader's sample-entry parse and `validateCodec`, `internal/box` marshal and parse, a bridge package, the integrity corpus and baseline, the README and the CHANGELOG.

## Testing approach

Tests run without audio hardware and, apart from the ffmpeg cross-checks, without external tools.

- **Regression tests.** New behaviour gets a test. A bug fix gets a regression test that fails before the fix. A test must be able to fail: break the line it pins and confirm it fails, and avoid fixtures where every path gives the same result.
- **Fuzzing.** Root: `FuzzReader`, `FuzzFragmentWriter`. `internal/box`: one target per table parser (`FuzzParseStsz`, `Stsc`, `Stco`, `Co64`, `Stts`, `Dfla`, `Dops`, `Elst`, `Tfhd`, `Trun`). `internal/reservation`: `FuzzShouldTrim`. Bridges: `FuzzDecodeInterleaved` (flacm4a, opusm4a), `FuzzSeekableDecoder` (aacm4a), `FuzzRoundTrip` and `FuzzPCMReservation` (flacm4a), `FuzzPCMReservation` (opusm4a). A `-fuzz` pattern that matches nothing passes without running, so run a new target in the package that defines it. New parsing paths get a fuzz target or a seed under `testdata/fuzz`. Discovered crashers are committed as regression data.
- **Integrity gate.** `internal/integrity` encodes a small deterministic corpus through each bridge and checks box layout, edit list (priming trim and presented length), sample-accurate decode, and bit-exact FLAC. Its integer facts are pinned in `testdata/baseline.json`; regenerate with `task integrity:update` only for an intended container change or a codec dependency bump that moves the lossy frame and duration fields, and explain the diff in the PR. Every ffmpeg and ffprobe interop test finds its tool with `testutil.LookTool`: it skips without the tool and fails in the CI job that installs ffmpeg (the Linux leg of the test matrix), which sets `M4A_REQUIRE_FFMPEG`; the 386 job has no ffmpeg and skips. New interop tests use `LookTool`, never `exec.LookPath` plus `t.Skip`.
- **Decode-limit seams.** The bridges hold their default ceiling as an atomic built by `reservation.NewLimit` so a test can lower it without decoding a gigabyte. Keep that shape.
- **Allocation tests.** Steady-state fragment writing is allocation-free and guarded by allocation tests; a change on that path keeps them green.
- **Architectures.** The suite also runs as `GOARCH=386` binaries, because the size and offset arithmetic must hold on 32-bit.
- **Benchmarks.** `bench_test.go` in the root and each bridge; the PR benchmark workflow compares against the base with benchstat. It is informational, read the p-values, not the percentages.

## Commands

Go version is read from `go.mod` (currently 1.27); the golangci-lint version is pinned once in `.golangci-version` and read by both the Taskfile and CI. The project tracks the latest stable Go, so no shims or build tags for older releases. When a new minor ships, bump `go.mod`, and CI follows through `go-version-file`.

```
task check            # full local gate: build, vet (amd64, arm64, linux/386, ruleguard), go mod tidy -diff,
                      # golangci-lint (config verified first), ruleguard probe test, race tests
task test             # go test -shuffle=on ./...
task test:race        # same with -race
task test:cover       # race, cross-package coverage (-coverpkg=./...), prints the total
task integrity        # integrity gate, -count=1, ffmpeg checks run when ffmpeg is on PATH
task integrity:update # regenerate internal/integrity/testdata/baseline.json
task bench            # all benchmarks with allocation counts
task fuzz             # short bounded smoke run of the primary fuzz targets
task lint | lint:fix | fmt   # golangci-lint, with fixes, gofumpt + goimports
task rules            # compile the ruleguard matchers and check each fires on its probe
task tools            # install the pinned golangci-lint and the latest govulncheck
task vuln             # govulncheck ./...
go test -race -tags noasm ./...     # scalar-path build, covered by its own CI step
```

Run `task check` before pushing. CI additionally runs the 386 test job, the `-tags noasm` test run, the fuzz smoke job, CodeQL, govulncheck and OpenSSF Scorecard. `-tags noasm` drops exactly one package (`simd/f32`); go-flac and go-opus link `github.com/tphakala/simd` unconditionally, so it is not an assembly-free build.

## Conventions

- **Lint.** Config is `.golangci.yaml`: strict by default, a linter is disabled only with a written reason. Notable: gofumpt and goimports (local prefix `github.com/tphakala/go-m4a`), gosec (G104, G115 excluded), gocognit 30, gocyclo 20, dupl 120, errorlint, exhaustive (`default` counts as exhaustive), `gochecknoinits` (no `init` functions), depguard (no `io/ioutil`, `github.com/pkg/errors`, `x/exp` slices, maps or constraints, or `math/rand` v1), revive, godoclint, nolintlint.
- **No `//nolint` without a linter name and a reason.** If a linter is wrong often enough to silence globally, change `.golangci.yaml` with a comment saying why. Fix new findings after a tooling bump in the same change.
- **Modernize.** The `modernize` linter is off on purpose; the ruleguard matchers in `rules/` carry the Go 1.20 to 1.27 patterns. A new matcher needs a `// rule: Name` section with `// want` annotations in `rules/testdata/probe/probe.go`, and after editing one run `golangci-lint cache clean` (`task rules` does) because the analysis cache is not keyed on the rule files.
- **Errors.** Error strings carry the `go-m4a:` prefix in the root package. Wrap with `%w`; use `errors.New` for constant messages.
- **Comments.** Explain why: the box layout and ISO/IEC 14496-12 clause where it is not obvious, the failure being avoided, the measurement behind a constant. Match the existing density; the long rationale comments on `DefaultMaxDecodedBytes` and `MaxPCMReservation` are the model for a limit's documentation.
- **Marshalers** in `internal/box` are append-style (`dst` in, extended `dst` out, allocation-free with spare capacity), each documenting the field layout it writes; tests pin the exact bytes.
- **Files** start with `// SPDX-License-Identifier: MIT` (new files too). `.editorconfig` and `.gitattributes` fix tabs for Go and LF line endings.
- **Changelog.** User-facing changes go in `CHANGELOG.md` under Unreleased (Keep a Changelog format).
- **Commits** use an imperative subject with a type prefix (`feat:`, `fix:`, `docs:`, `build:`, `ci:`, `chore:`, `refactor:`, `test:`), optionally scoped (`fix(reader): ...`).
- **Dependencies.** Dependabot keeps Go modules and GitHub Actions current. Workflow actions that hold write permissions are pinned by commit SHA.

## Review guidance

For CodeRabbit, Copilot and human reviewers. Rate findings on what a caller of the library would see.

Flag these, they are real defects here:

- Any break of the design rules above: a codec import in the core, cgo, a parser that trusts a count or length from the file, a missing overflow guard on 32-bit.
- A panic, index error, or unbounded allocation reachable from file bytes, in the reader, the `internal/box` parsers or a bridge.
- A reservation sized from file claims without the ceiling and container cross-check, or a copy of that policy that drifts from `internal/reservation`.
- A resource limit reported as `ErrCorrupt`, or a corrupt file reported as a limit or as `ErrUnsupported`.
- An edit list that no longer trims exactly the priming or no longer excludes the padding, or that appears for FLAC.
- A change to emitted bytes (box layout, `dOps`, `dfLa`, `tfdt`, `trun` flags) without updated byte-pinning tests, and without a reviewed integrity baseline diff when the baseline moved.
- An allocation on the steady-state `FragmentWriter` path.
- A test that cannot fail on the behaviour it names, or a doc sentence that is false for some codec.

Intentional, do not flag:

- The `internal/reservation` limits (`MaxPCMReservation` 64 MiB, `MaxRetainedSlack`, the proportional `ShouldTrim` rule) and `DefaultMaxDecodedBytes` (1 GiB): they are measured trades, documented where they are defined.
- `DecodeInterleaved` returning priming and trailing padding unchanged; the caller trims with `Info`.
- Atomic package-level decode limits in the bridges, used as test seams.
- `aacm4a` having no accumulating decode and no limit: `NewDecoder` and `NewSeekableDecoder` stream, and a caller that accumulates owns the bound.
- The `aacm4a` convenience path accepting only 44100 and 48000 Hz while the core writer accepts more; that is go-aac's encoder limit.
- The reader labelling an HE-AAC track `CodecAACLC`; the `esds` object type names the MPEG-4 Audio family, and the `aacm4a` decoder rejects HE-AAC with `pcm.ErrUnsupportedSBR` or `pcm.ErrUnsupportedPS`.
- Duplicated shape between `opusm4a` and `flacm4a`; the two bridges deliberately expose the same API.
- Breaking exported-API changes before v1.0.0 (see API stability), and the absence of deprecated aliases, adapters or shims for the old form.

## Things that do not belong in the repo

- Raw captures or scratch media (`/scratch/`, `*.tmp.m4a`). Committed media is limited to small, scrubbed fixtures under `testdata/`.
- Build outputs (`*.test`, `*.out`, `*.exe`) and coverage files.
