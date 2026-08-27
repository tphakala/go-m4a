# go-m4a

[![CI](https://github.com/tphakala/go-m4a/actions/workflows/ci.yml/badge.svg)](https://github.com/tphakala/go-m4a/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/tphakala/go-m4a.svg)](https://pkg.go.dev/github.com/tphakala/go-m4a)
[![codecov](https://codecov.io/gh/tphakala/go-m4a/branch/main/graph/badge.svg)](https://codecov.io/gh/tphakala/go-m4a)
[![Go Version](https://img.shields.io/github/go-mod/go-version/tphakala/go-m4a)](go.mod)
[![Latest tag](https://img.shields.io/github/v/tag/tphakala/go-m4a?sort=semver&label=release)](https://github.com/tphakala/go-m4a/tags)
[![OpenSSF Scorecard](https://api.scorecard.dev/projects/github.com/tphakala/go-m4a/badge)](https://scorecard.dev/viewer/?uri=github.com/tphakala/go-m4a)
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
  pre-skip drives the same edit-list priming trim as AAC. The container timescale is
  fixed at 48 kHz, and the encoder bridge accepts source audio at 8, 12, 16, 24, or
  48 kHz (the true source rate is recorded in `dOps`). The emitted `dOps` is
  byte-identical to ffmpeg's, ffmpeg reads the container, and an encode/decode
  round-trip through go-opus preserves the signal.
- **FLAC** (`fLaC` + `dfLa`): each MP4 sample is one native FLAC frame, with the
  34-byte STREAMINFO in `dfLa`; no edit list (FLAC has no priming). Handles 1 to 8
  channels and sample rates up to 1048575 Hz (the 20-bit STREAMINFO maximum). Reads
  ffmpeg-produced files and round-trips bit-exact (lossless) through go-flac.

Parsing is bounds-checked throughout and never panics on malformed input, and the
accumulating decodes are bounded too. The decoded size of an audio file is not
proportional to the file: a FLAC constant subframe encodes a whole 65535-sample
block in a handful of bytes, so a crafted file of tens of kilobytes decodes to
something on the order of a gigabyte, and a two-byte Opus packet of zero-length
DTX frames decodes to 120 ms. Sixty seconds of ordinary digital silence encodes
to about 14 kB and decodes back to 11.5 MB, a ratio of 831:1, so this is not only
an adversarial concern.

`flacm4a.DecodeInterleaved` and `opusm4a.DecodeInterleaved` therefore stop at
`m4a.DefaultMaxDecodedBytes` (1 GiB, about 101 minutes of CD-quality stereo) and
return an error wrapping `m4a.ErrDecodeLimit`; see that constant for how the
value was chosen. `DecodeInterleavedLimit` takes the ceiling as an argument (zero
or less for none), and `DecodeStream` hands each frame's PCM to a callback
without accumulating at all, so it decodes a stream of any length in memory
proportional to a single frame. What those decodes reserve up front is bounded
separately, by what the container corroborates, by a fixed ceiling and by the
limit itself, so a file that declares an implausible length cannot make the
decoder allocate for the claim before decoding anything.

`aacm4a` needs none of this and gets none of it: it has no accumulating decode,
because `NewDecoder` returns a streaming decoder. A caller that accumulates that
decoder's output itself owns the bound, and should size it against the audio
rather than against the file.

There is also a **fragmented (CMAF) writer** for live HLS or DASH: `InitSegment`
builds the `ftyp`/`moov` initialization segment and a `FragmentWriter` appends
`styp`/`moof`/`mdat` media segments. It never seeks, so it can write straight into
a byte slice or a socket, and it reuses its buffers, so a steady-state segment
allocates nothing. It is codec-generic like the rest of the writer. See
[Fragmented output for HLS](#fragmented-output-for-hls).

Scope: a single audio track. Channel counts are per codec: **AAC-LC** and **Opus**
are mono or stereo, **FLAC** is 1 to 8 channels. The sample rate is also per codec,
because each one constrains it differently: **AAC-LC** takes the eleven MPEG-4
sampling-frequency table rates that the 16-bit sample-entry field can hold (7350,
8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 64000 Hz; 88.2 and
96 kHz are in the table but are rejected rather than written wrong); **Opus** fixes
the container timescale at 48 kHz but accepts source audio at 8, 12, 16, 24, or
48 kHz, recording the true source rate in the `dOps` box; **FLAC** has no rate
table, so any rate up to 1048575 Hz (the 20-bit STREAMINFO maximum). A FLAC rate
above the 16-bit sample-entry field carries a reduced hint there and the true rate
in STREAMINFO, following the Xiph FLAC-in-ISOBMFF fallback. 44.1 and 48 kHz are the
best-trodden paths; every AAC rate above is covered by a round trip rather than
only by the validator letting it past.

One caveat: the `aacm4a` convenience bridge is narrower than the core writer,
because go-aac's encoder accepts only 44100 and 48000 Hz. The reader now handles
both plain files and fragmented (CMAF) input: `NewReader` auto-detects a
`moof`-based stream (an initialization segment followed by `moof`/`mdat` media
segments) and demuxes its access units through the same API. Also out of scope
(again `ErrUnsupported`, never a crash): video or multiple audio tracks, other
codecs, HE-AAC, and writing metadata tags. Surround is partly covered now (FLAC up to 8
channels); more-than-stereo **Opus** is the one tracked codec extension still open
([#5](https://github.com/tphakala/go-m4a/issues/5)), blocked upstream on a go-opus
multistream API.

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

`NewReader` reads a fragmented (CMAF) stream the same way: pass an `io.ReadSeeker`
over the initialization segment concatenated with its `moof`/`mdat` media segments
and `ReadFrame` yields the access units across every fragment in order. Detection
is automatic, so the same code path reads plain and fragmented input alike.

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

The same one-call shape wraps Opus (interleaved S16 at 8, 12, 16, 24, or 48 kHz,
mono or stereo) and FLAC (interleaved 16- or 24-bit, 1 to 8 channels):

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

Both `DecodeInterleaved` calls stop at `m4a.DefaultMaxDecodedBytes`. For a file
the caller did not produce, or one longer than that ceiling, decode it a frame at
a time instead and let nothing accumulate. Both bridges expose the same pair, so
the `flacm4a` calls below have `opusm4a` counterparts that behave the same way:

```go
info, err := flacm4a.DecodeStream(r, func(pcm []byte) error {
    // pcm aliases a reused buffer: copy whatever outlives this call.
    _, werr := w.Write(pcm)
    return werr
})

// Or keep the one-call shape with a ceiling of your own:
pcmOut, info, err := flacm4a.DecodeInterleavedLimit(r, 8<<20)
if errors.Is(err, m4a.ErrDecodeLimit) {
    // The stream decodes to more than 8 MiB.
}
```

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
stream while keeping its buffers, for pooling across sessions; an arena grown to an
outlier size by one pathological segment is released on `Reset` rather than pinning
that peak for the life of the pool.

Those buffers grow on the first segment. For a deployment that churns writers, or
that wants the very first segment allocation-free too, `Grow(samples, bytes)`
pre-reserves the arena for a segment of about `samples` access units totalling
about `bytes` of payload. It is a capacity hint only: it never changes the bytes a
segment emits, over-large values are clamped to the per-segment caps, and a wrong
estimate costs a regrow rather than correctness.

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

## Sponsor

go-m4a is maintained in my own time. If it is useful to you or your project, you
can support continued maintenance through GitHub Sponsors; sponsorship is
entirely optional and never gates any feature.

[![Sponsor on GitHub](https://img.shields.io/github/sponsors/tphakala?logo=githubsponsors&color=ea4aaa&label=Sponsor%20%40tphakala)](https://github.com/sponsors/tphakala)

## License

MIT. See [LICENSE](LICENSE). go-m4a is clean-room container code and does not
include any code ported from FFmpeg, so it is not bound by go-aac's LGPL.
