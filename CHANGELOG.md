# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.7.0] - 2026-10-14

### Added

- `aacm4a.NewSeekableDecoder` and `aacm4a.Decoder` stream the same S16 PCM as `NewDecoder` and add `SeekSample` and `SeekTime`. The decoder handles the codec pre-roll itself, so the output after a seek starts at the exact requested sample. On a stream that uses perceptual noise substitution the noise bands after a seek differ from a linear decode, which go-aac cannot avoid; the position and length stay exact.

### Changed

- `aacm4a.NewDecoder` accepts `m4a.ReaderOption`s, for example `m4a.WithMaxBoxBuffer`. Existing calls compile unchanged.

## [0.6.0] - 2026-10-13

### Added

- `Reader.SeekSample` positions the reader at any access unit without reading the ones before it, for plain and fragmented input. It does no I/O itself, so a jump costs one seek on the next read. An index outside the track returns the new `ErrOutOfRange` sentinel. By @vst93 (#69).

- `Reader.SeekTime` seeks to a presentation time instead of a sample index. It maps the time (measured from the end of the encoder priming, `Info.EncoderDelay`) to the access unit that holds it using timing the reader now keeps from `stts` or the fragment `trun` durations, moves the cursor there, and returns the number of decoded samples per channel to drop from that unit. A time outside the presented range returns `ErrOutOfRange` with the cursor unchanged; a track with no sample timing, or whose edit list is not one presented window (several non-empty edits, a trailing empty edit, a media rate other than 1.0, no non-empty edit, a `media_time` below -1), returns `ErrUnsupported`.

- `Info.StartOffset` reports the length of the leading empty edits of the edit list, the presentation time of the first presented sample (ffprobe's `start_time`).

### Changed

- `Info.Duration`, `Info.EncoderDelay` and `Reader.SeekTime` now come from one presented window resolved from the edit list. For the same file this changes some reported values: a fragmented stream excludes its priming (a stream of 48 AAC frames at 48 kHz reports 1.002667 s, not 1.024 s); an Opus file without an edit list excludes the `dOps` pre-skip; a file without an edit list uses the exact media length instead of the rounded movie header duration; a segment duration longer than the media is clipped to the media; a leading empty edit no longer zeroes `EncoderDelay` or becomes the `Duration`; an edit list with several non-empty edits reports the summed presented edits; a plain file whose edit has `segment_duration` 0 presents nothing. `Duration` is now computed in integer arithmetic, rounding down to the nanosecond.
- An `elst` whose entry count overruns its body is now rejected as `ErrCorrupt`.
- The `ErrOutOfRange` message is now "go-m4a: seek position out of range", since it covers a seek time as well as a sample index.
- The minimum Go version is now 1.27.
- The `aacm4a` bridge now builds against go-aac v0.7.0.

Releases up to and including v0.5.0 predate this file; see the [GitHub releases](https://github.com/tphakala/go-m4a/releases) and the git tags for their history.

[Unreleased]: https://github.com/tphakala/go-m4a/compare/v0.7.0...HEAD
[0.7.0]: https://github.com/tphakala/go-m4a/compare/v0.6.0...v0.7.0
[0.6.0]: https://github.com/tphakala/go-m4a/compare/v0.5.0...v0.6.0
