# go-m4a

[![CI](https://github.com/tphakala/go-m4a/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-m4a/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-m4a.svg)](https://pkg.go.dev/github.com/tphakala/go-m4a)
[![Go Report Card](https://goreportcard.com/badge/github.com/tphakala/go-m4a)](https://goreportcard.com/report/github.com/tphakala/go-m4a)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-m4a)](go.mod)
[![License: MIT](https://img.shields.io/badge/License-MIT-blue.svg)](LICENSE)
[![Sponsor](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor)](https://github.com/sponsors/tphakala)

Pure-Go MP4/M4A muxer and demuxer for AAC-LC, Opus, and FLAC audio. No cgo and no
external binaries. It is the container half that codecs like
[go-aac](https://github.com/tphakala/go-aac) deliberately leave to an external
muxer: go-aac's `pcm` package notes that sample-accurate trimming of the encoder
priming "requires a container with an edit list (MP4)", and points at an external
muxer as the escape hatch. go-m4a is that muxer, plus the matching demuxer.

Write an `.m4a`/`.mp4` from AAC-LC, Opus, or FLAC access units, with an edit list
that trims the encoder priming so playback is gapless and sample-accurate. Read
one back into access units that decode with go-aac, go-opus, go-flac, ffmpeg,
QuickTime, or any compatible decoder. The core `m4a` package is codec-agnostic and
stdlib-only; optional `aacm4a`, `opusm4a`, and `flacm4a` subpackages bridge the
matching codec libraries, so a consumer that only muxes or demuxes pulls no codec.

> Note on interop: Opus-in-MP4 is a niche encapsulation (browsers and hardware
> players prefer Ogg Opus or WebM), and FLAC-in-MP4 buys nothing over native
> `.flac`. Their value here is a pure-Go alternative to `ffmpeg -c:a libopus/flac`
> and API symmetry with the AAC path, not broad playback compatibility.

## Status

The writer and reader dispatch on the codec, so all three share the same
`ftyp | mdat | moov` plumbing, sample tables, and edit list; only the sample entry
and its codec-specific box differ.

- **AAC-LC** (`mp4a` + `esds`): the original path. Output is validated with ffprobe
  and decodes byte-for-byte back to the source through ffmpeg (a go-aac-encoded
  chirp round-trips at cross-correlation 1.0 at lag 0). The reader tolerates the
  extra boxes real encoders emit (`free`, `udta`, `meta`, `sgpd`, `sbgp`), expands
  arbitrary multi-chunk `stsc` tables, and reads ffmpeg and Apple `afconvert` files.
- **Opus** (`Opus` + `dOps`): each MP4 sample is one Opus packet; the `dOps`
  pre-skip drives the same edit-list priming trim as AAC. The emitted `dOps` is
  byte-identical to ffmpeg's, ffmpeg reads the container, and an encode/decode
  round-trip through go-opus preserves the signal.
- **FLAC** (`fLaC` + `dfLa`): each MP4 sample is one native FLAC frame, with the
  34-byte STREAMINFO in `dfLa`; no edit list (FLAC has no priming). Reads
  ffmpeg-produced files and round-trips bit-exact (lossless) through go-flac.

Parsing is bounds-checked throughout and never panics on malformed input. A
`flacm4a` decode also bounds the buffer it reserves up front, so a file that
declares an implausible length cannot make the decoder allocate proportionally to
the claim; longer genuine streams are fully supported and simply grow past the
bound. The bound is a ceiling rather than a guarantee of proportionality, though:
a small crafted file can still reach it, and it bounds the reservation rather
than the decode, so the PCM a heavily compressed stream returns can still be many
times the size of the file.

There is also a **fragmented (CMAF) writer** for live HLS or DASH: `InitSegment`
builds the `ftyp`/`moov` initialization segment and a `FragmentWriter` appends
`styp`/`moof`/`mdat` media segments. It never seeks, so it can write straight into
a byte slice or a socket, and it reuses its buffers, so a steady-state segment
allocates nothing. It is codec-generic like the rest of the writer. See
[Fragmented output for HLS](#fragmented-output-for-hls).

Scope: a single audio track, mono or stereo, 44.1/48 kHz (Opus is always 48 kHz).
Fragmented MP4 is write-only: the reader is for plain files and returns a typed
`ErrUnsupported` for fragmented input. Also out of scope (again `ErrUnsupported`,
never a crash): video or multiple audio tracks, other codecs, HE-AAC, surround,
and writing metadata tags. See the open issues for the tracked extensions
(non-48 kHz Opus input, high sample rates, more than two channels).

## Install

```sh
go get github.com/tphakala/go-m4a
```

The core `m4a` package is stdlib-only. The optional `aacm4a`, `opusm4a`, and
`flacm4a` subpackages wire the container to go-aac, go-opus, and go-flac; each
pulls in its codec module, so import only the ones you use.

## Usage

### m4a: the container

`WriterConfig.Codec` selects the codec (the zero value is `CodecAACLC`, so existing
AAC callers are unchanged). AAC-LC access units are a fixed 1024 samples each, so
`WriteFrame` suffices; Opus packets and FLAC frames vary in length, so those use
`WriteFrameDuration(au, sampleDuration)`. The reader reports `Info.Codec` and a
generic `Info.CodecConfig` (the ASC, the `dOps` body, or the STREAMINFO).

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
no glue, allocation-free per frame. For callers that want the bytes directly
without the `io.Reader` framing, `Reader.ReadFrameInto(dst)` fills a reused
buffer instead of allocating one per frame like `ReadFrame` does (it returns the
required length with `io.ErrShortBuffer` if `dst` is too small).

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

### opusm4a and flacm4a: the go-opus and go-flac bridges

The same one-call shape wraps Opus (48 kHz interleaved S16) and FLAC (interleaved
16- or 24-bit):

```go
import (
    "github.com/tphakala/go-m4a/opusm4a"
    "github.com/tphakala/go-m4a/flacm4a"
)

// Opus: gapless (the pre-skip and trailing padding are trimmed by the edit list).
err := opusm4a.EncodeInterleaved(f, opusm4a.Config{SampleRate: 48000, Channels: 2, Bitrate: 96000}, pcm)
pcmOut, info, err := opusm4a.DecodeInterleaved(r) // []byte, m4a.Info, error

// FLAC: lossless, so the decode is bit-identical to the input.
err = flacm4a.EncodeInterleaved(f, flacm4a.Config{SampleRate: 44100, Channels: 1, BitDepth: 16, CompressionLevel: 5}, pcm)
pcmOut, info, err = flacm4a.DecodeInterleaved(r)
```

As with `aacm4a`, `opusm4a.DecodeInterleaved` returns every decoded sample,
including the Opus pre-skip priming and the trailing padding, so trim
`info.EncoderDelay` leading samples then keep `info.Duration`-worth for
sample-accurate output. `flacm4a` needs no trimming (FLAC has no priming).

### Fragmented output for HLS

`Writer` produces `ftyp | mdat | moov` and needs an `io.WriteSeeker`, because the
`mdat` size is patched at `Close`. A live HLS or DASH stream cannot seek, so it uses
the fragmented path instead: one initialization segment, then a media segment every
couple of seconds, each appended to a buffer the caller owns.

```go
cfg := m4a.WriterConfig{SampleRate: 48000, Channels: 1, ASC: asc}

// Built once, served as the playlist's EXT-X-MAP target.
initSeg, err := m4a.InitSegment(cfg)
if err != nil {
    return err
}
publishInit(initSeg)

fw, err := m4a.NewFragmentWriter(cfg)
if err != nil {
    return err
}

var segment []byte // reused across segments
for _, au := range accessUnits {
    if err := fw.WriteFrame(au); err != nil { // copies au, so reuse it freely
        return err
    }
    // Cut on an access-unit boundary once the target duration is reached.
    if fw.PendingDuration() >= 2*48000 {
        extinf := float64(fw.PendingDuration()) / 48000 // seconds, for the playlist
        segment, err = fw.AppendSegment(segment[:0])
        if err != nil {
            return err
        }
        publish(segment, extinf)
    }
}
```

`WriteFrame` copies each access unit into an internal buffer, so an encoder may keep
reusing one scratch slice. Both that buffer and the segment assembly are retained
across segments: once they have grown, a segment allocates nothing at all. Opus and
FLAC use `WriteFrameDuration` here too, and a segment whose frames do not share a
duration automatically carries per-sample durations.

The segments use 64-bit `tfdt` decode times, because a 32-bit one at 48 kHz wraps
after about 24.8 hours of continuous streaming. `Reset` rebinds a writer to a new
stream while keeping its buffers, for pooling across sessions.

## Gapless playback and the edit list

A lossy encoder emits priming before the first real sample and pads the final
frame: AAC-LC primes 1024 samples (go-aac), Opus 312 at 48 kHz (`dOps` pre-skip).
go-m4a writes an `elst` edit list whose `media_time` skips the priming and whose
`segment_duration` (set from `WriterConfig.MediaLength`) excludes the trailing
padding, so a compliant player presents exactly the original audio. The reader
surfaces the edit list as `Info.EncoderDelay` and `Info.Duration`. FLAC is lossless
and has no priming, so no edit list is written.

The fragmented writer trims the priming the same way, with `segment_duration` 0,
the open-ended form used by fragmented-MP4 packagers for a stream whose length is
not yet known (`MediaLength` has no meaning there and is rejected). A codec with no
priming gets no edit list on the fragmented path, so FLAC never carries one there.
The non-fragmented `Writer` still writes one unless the caller passes `NoEdit`,
which is what `flacm4a` does.

This was verified rather than assumed. `TestFragmentedEditListTrimsPriming` writes
the same access units with and without the edit list and decodes both with ffmpeg,
asserting the difference is exactly the 1024 priming samples; the same comparison
in hls.js on Chromium reports media durations 0.021332 s apart, which is that same
frame. Worth knowing, because ffmpeg's HLS fMP4 packager emits an edit list that
trims nothing (`media_time` 0) and its `-movflags +empty_moov` path emits none at
all, so players differ in how much they exercise this. Set `EncoderDelay:
m4a.NoEdit` to opt out and accept the priming as a constant offset of about 21 ms.

## License

MIT. See [LICENSE](LICENSE). go-m4a is clean-room container code and does not
include any code ported from FFmpeg, so it is not bound by go-aac's LGPL.
