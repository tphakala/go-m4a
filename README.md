# go-m4a

[![CI](https://github.com/tphakala/go-m4a/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-m4a/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-m4a.svg)](https://pkg.go.dev/github.com/tphakala/go-m4a)
[![Go Report Card](https://goreportcard.com/badge/github.com/tphakala/go-m4a)](https://goreportcard.com/report/github.com/tphakala/go-m4a)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-m4a)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

Pure-Go MP4/M4A muxer and demuxer for AAC-LC audio. No cgo and no external
binaries. It is the container half that
[go-aac](https://github.com/tphakala/go-aac) deliberately leaves to an external
muxer: go-aac's `pcm` package notes that sample-accurate trimming of the encoder
priming "requires a container with an edit list (MP4)", and points at an external
muxer as the escape hatch. go-m4a is that muxer, plus the matching demuxer.

Write an `.m4a` from AAC-LC access units, with an edit list that trims the
encoder priming so playback is gapless and sample-accurate. Read an `.m4a` back
into access units that decode with go-aac, ffmpeg, QuickTime, or any AAC decoder.

## Status

- **Writer: complete for AAC-LC.** Streams `ftyp | mdat | moov` to an
  `io.WriteSeeker`, with a proper `esds` (including the mandatory
  `SLConfigDescriptor`), a single-track sample table, and an `edts`/`elst` edit
  list. Output is validated with ffprobe and decodes byte-for-byte back to the
  source through ffmpeg: a go-aac-encoded chirp muxed by go-m4a and decoded by
  ffmpeg reproduces the input with a cross-correlation peak of 1.0 at lag 0.
- **Reader: complete for AAC-LC.** Locates `moov` whether it precedes or follows
  `mdat`, tolerates the extra boxes real encoders emit (`free`, `udta`, `meta`,
  `sgpd`, `sbgp`), expands arbitrary multi-chunk `stsc` tables, and extracts the
  ASC from `esds`. It reads files produced by ffmpeg and Apple's `afconvert`
  (mono and stereo, 44.1 and 48 kHz, single and multi chunk, edit list or
  `iTunSMPB`), yielding access units that decode identically to the reference
  extraction. Parsing is bounds-checked throughout and never panics on malformed
  input.

Scope in v1: non-fragmented MP4, a single AAC-LC audio track, mono or stereo,
44.1 or 48 kHz, matching what go-aac encodes and decodes. Out of scope (the
reader returns a typed `ErrUnsupported`, never crashes): fragmented MP4, video or
multiple audio tracks, codecs other than AAC-LC, HE-AAC, and writing metadata
tags. Apple `iTunSMPB` gapless tags are not parsed; the reader reports the edit
list, so an Apple file with no `elst` reports an encoder delay of 0.

## Install

```sh
go get github.com/tphakala/go-m4a
```

The core `m4a` package is stdlib-only. The optional `aacm4a` subpackage wires the
container to go-aac and pulls that module in; import it only if you use it.

## Usage

### m4a: the container

Write AAC-LC access units (from any source) into an `.m4a`:

```go
import "github.com/tphakala/go-m4a"

w, err := m4a.NewWriter(f, m4a.WriterConfig{
    SampleRate:  48000,
    Channels:    1,
    ASC:         asc,   // 2-byte AudioSpecificConfig, e.g. aac.Encoder.AudioSpecificConfig()
    MediaLength: nSamples, // source samples per channel, for a sample-accurate edit list
})
for _, au := range accessUnits {
    if err := w.WriteFrame(au); err != nil { /* ... */ }
}
err = w.Close() // patches the mdat size and writes moov
```

`NewWriter` needs an `io.WriteSeeker` because the streamed `mdat` size is patched
once at `Close`. The edit list trims `WriterConfig.EncoderDelay` priming samples
(default 1024, go-aac's measured value); set `EncoderDelay: m4a.NoEdit` to omit
the edit list.

Read an `.m4a` back into access units:

```go
r, err := m4a.NewReader(f) // io.ReadSeeker
info := r.Info()           // SampleRate, Channels, ASC, FrameCount, EncoderDelay, Duration
for {
    au, err := r.ReadFrame()
    if err == io.EOF { break }
    if err != nil { /* ... */ }
    // hand au to your AAC decoder
}
```

`Reader.RawStream()` returns an `io.Reader` that frames each access unit exactly
as go-aac's `pcm.WithRawStream` expects, so the two libraries plug together with
no glue.

### aacm4a: the go-aac bridge

The `aacm4a` subpackage is the one-call path for callers that have interleaved
integer PCM and want an `.m4a`, or have an `.m4a` and want PCM:

```go
import (
    aacpcm "github.com/tphakala/go-aac/pcm"
    "github.com/tphakala/go-m4a/aacm4a"
)

// Encode interleaved little-endian PCM to a gapless AAC-LC .m4a.
err := aacm4a.EncodeInterleaved(f, aacpcm.Config{
    SampleRate: 48000, BitDepth: 16, Channels: 1, Bitrate: 96000,
}, pcmBytes)

// Decode an .m4a to interleaved S16 PCM with go-aac.
dec, info, err := aacm4a.NewDecoder(f) // *aacpcm.Decoder, m4a.Info, error
_, err = io.Copy(pcmOut, dec)
```

The go-aac decoder is not edit-list aware: `NewDecoder` emits every decoded
sample, including both the leading priming and the trailing final-frame padding.
For sample-accurate output, skip `info.EncoderDelay` leading samples per channel,
then keep only `info.Duration`-worth of samples (`Duration * SampleRate` per
channel) and discard the rest.

## Gapless playback and the edit list

An AAC-LC encoder emits a frame of priming (1024 samples for go-aac) before the
first real sample, and pads the final frame. ADTS cannot signal either, so raw
AAC streams decode with roughly 1024 extra leading samples. go-m4a writes an
`elst` edit list whose `media_time` skips the priming and whose
`segment_duration` (set from `WriterConfig.MediaLength`) excludes the trailing
padding, so a compliant player presents exactly the original audio. The reader
surfaces the edit list as `Info.EncoderDelay` and `Info.Duration`.

## License

MIT. See [LICENSE](LICENSE). go-m4a is clean-room container code and does not
include any code ported from FFmpeg, so it is not bound by go-aac's LGPL.
