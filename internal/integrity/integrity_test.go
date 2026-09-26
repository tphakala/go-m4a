// SPDX-License-Identifier: MIT

package integrity

import (
	"bytes"
	"crypto/md5" //nolint:gosec // G501: a content fingerprint for the baseline, not a security boundary.
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	aacpcm "github.com/tphakala/go-aac/pcm"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/aacm4a"
	"github.com/tphakala/go-m4a/flacm4a"
	"github.com/tphakala/go-m4a/internal/box"
	"github.com/tphakala/go-m4a/opusm4a"
)

var update = flag.Bool("update", false, "rewrite testdata/baseline.json from the current output")

const baselinePath = "testdata/baseline.json"

// codecName names the inner codec of a corpus case. It is also the value stored
// in the baseline, so renaming one is a baseline change.
type codecName string

const (
	codecAAC  codecName = "aac-lc"
	codecOpus codecName = "opus"
	codecFLAC codecName = "flac"
)

// corpusCase is one deterministic input. Samples is per channel and is chosen
// not to be a multiple of the codec frame size, so the trailing padding the edit
// list has to cut is never zero.
type corpusCase struct {
	name       string
	codec      codecName
	sampleRate int
	channels   int
	bitDepth   int
	samples    int
}

var corpus = []corpusCase{
	{"aac_mono48k", codecAAC, 48000, 1, 16, 30011},
	{"aac_stereo44k", codecAAC, 44100, 2, 16, 22222},
	{"opus_mono48k", codecOpus, 48000, 1, 16, 24007},
	{"opus_stereo48k", codecOpus, 48000, 2, 16, 19201},
	{"flac_mono44k", codecFLAC, 44100, 1, 16, 9001},
	{"flac_stereo48k_24bit", codecFLAC, 48000, 2, 24, 12345},
	{"flac_5.1_48k", codecFLAC, 48000, 6, 16, 4801},
}

// lossless reports whether the round trip must reproduce the source exactly.
func (c corpusCase) lossless() bool { return c.codec == codecFLAC }

// bytesPerSample is the width of one decoded sample. The lossy bridges always
// decode to 16-bit; FLAC decodes at the source depth.
func (c corpusCase) bytesPerSample() int { return c.bitDepth / 8 }

// frameBytes is the size of one interleaved sample frame (all channels).
func (c corpusCase) frameBytes() int { return c.channels * c.bytesPerSample() }

// isin approximates sin(2*pi*phase/2^32) scaled to +-amp with the parabola
// 4x(1-x) on each half cycle. It is pure integer arithmetic on purpose: the
// source must be bit-identical on every architecture, and a float sine is not
// guaranteed to be (the compiler may fuse multiply-adds on arm64), which would
// move the FLAC fingerprint in the baseline.
func isin(phase uint32, amp int64) int64 {
	x := int64(phase >> 16) // 0..65535, one full cycle
	neg := x >= 1<<15
	if neg {
		x -= 1 << 15
	}
	// 4*x*(2^15-x) peaks at 2^30 at x = 2^14, so the shift normalizes to [0, amp].
	y := 4 * x * (1<<15 - x) * amp >> 30
	if neg {
		return -y
	}
	return y
}

// sourcePCM builds the case's interleaved little-endian PCM: a linear chirp per
// channel, each channel sweeping its own band so a channel swap or deinterleave
// bug decorrelates the decode instead of slipping past.
func sourcePCM(c corpusCase) []byte {
	amp := int64(16000) << (c.bitDepth - 16)
	bps := c.bytesPerSample()
	out := make([]byte, 0, c.samples*c.frameBytes())
	phases := make([]uint32, c.channels)
	n := int64(c.samples)
	rate := int64(c.sampleRate)
	for i := range n {
		for ch := range c.channels {
			f0 := int64(300 + 200*ch)
			f1 := rate / int64(8+2*ch)
			// Phase increment in 2^-32 cycles for the instantaneous frequency
			// f0 + (f1-f0)*i/n. The numerator stays below 2^62 for the corpus.
			inc := ((f0*n + (f1-f0)*i) << 32) / (rate * n)
			v := isin(phases[ch], amp)
			phases[ch] += uint32(inc) // wraps by design: a phase accumulator
			switch bps {
			case 2:
				out = binary.LittleEndian.AppendUint16(out, uint16(v))
			case 3:
				u := uint32(v)
				out = append(out, byte(u), byte(u>>8), byte(u>>16))
			}
		}
	}
	return out
}

// memWS is an in-memory io.WriteSeeker, what the writer needs to patch the mdat
// size and append moov.
type memWS struct {
	buf []byte
	pos int64
}

func (m *memWS) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

func (m *memWS) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("memWS: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("memWS: negative position %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// encode runs the case's bridge encoder over pcm and returns the file bytes.
func encode(t *testing.T, c corpusCase, pcm []byte) []byte {
	t.Helper()
	var ws memWS
	var err error
	switch c.codec {
	case codecAAC:
		err = aacm4a.EncodeInterleaved(&ws, aacpcm.Config{SampleRate: c.sampleRate, BitDepth: c.bitDepth, Channels: c.channels}, pcm)
	case codecOpus:
		err = opusm4a.EncodeInterleaved(&ws, opusm4a.Config{SampleRate: c.sampleRate, Channels: c.channels, Bitrate: 64000 * c.channels}, pcm)
	case codecFLAC:
		err = flacm4a.EncodeInterleaved(&ws, flacm4a.Config{SampleRate: c.sampleRate, Channels: c.channels, BitDepth: c.bitDepth, CompressionLevel: 5}, pcm)
	}
	if err != nil {
		t.Fatalf("encode %s: %v", c.name, err)
	}
	return ws.buf
}

// decode runs the case's bridge decoder over a file and returns the full decoded
// PCM, before any edit-list trim, together with the reader's Info.
func decode(t *testing.T, c corpusCase, data []byte) ([]byte, m4a.Info) {
	t.Helper()
	var (
		pcm  []byte
		info m4a.Info
		err  error
	)
	switch c.codec {
	case codecAAC:
		var d *aacpcm.Decoder
		d, info, err = aacm4a.NewDecoder(bytes.NewReader(data))
		if err == nil {
			pcm, err = io.ReadAll(d)
		}
	case codecOpus:
		pcm, info, err = opusm4a.DecodeInterleaved(bytes.NewReader(data))
	case codecFLAC:
		pcm, info, err = flacm4a.DecodeInterleaved(bytes.NewReader(data))
	}
	if err != nil {
		t.Fatalf("decode %s: %v", c.name, err)
	}
	return pcm, info
}

// facts is what the container says about itself, read straight from the boxes
// rather than through the Reader under test.
type facts struct {
	brand          string
	topLevel       []string
	movieTimescale uint32
	mediaTimescale uint32
	mediaDuration  uint64
	hasEdit        bool
	editMediaTime  int64
	editSegment    uint64 // segment_duration, in the movie timescale
	sampleCount    uint32 // stsz sample_count: the number of access units
}

var (
	fourccMvhd = box.NewFourCC("mvhd")
	fourccTrak = box.NewFourCC("trak")
	fourccEdts = box.NewFourCC("edts")
	fourccElst = box.NewFourCC("elst")
	fourccMdia = box.NewFourCC("mdia")
	fourccMdhd = box.NewFourCC("mdhd")
	fourccMinf = box.NewFourCC("minf")
	fourccStbl = box.NewFourCC("stbl")
	fourccStsz = box.NewFourCC("stsz")
)

// inspect walks the top-level boxes and reads the single track's timing boxes
// and its stsz sample count.
func inspect(t *testing.T, data []byte) facts {
	t.Helper()
	var f facts
	var moov []byte
	for off := int64(0); off < int64(len(data)); {
		h, err := box.ParseHeader(data[off:])
		if err != nil {
			t.Fatalf("top-level box at %d: %v", off, err)
		}
		total := h.Total
		if h.ToEnd {
			total = int64(len(data)) - off
		}
		if total < h.HeaderLen || off+total > int64(len(data)) {
			t.Fatalf("top-level %q at %d: size %d overruns the %d-byte file", h.Type[:], off, total, len(data))
		}
		body := data[off+h.HeaderLen : off+total]
		f.topLevel = append(f.topLevel, string(h.Type[:]))
		switch string(h.Type[:]) {
		case "ftyp":
			if len(body) >= 4 {
				f.brand = string(body[:4])
			}
		case "moov":
			moov = body
		}
		off += total
	}
	if moov == nil {
		t.Fatal("no moov box")
	}

	mvhd := child(t, moov, fourccMvhd)
	ts, _, err := box.ParseMvhd(mvhd)
	if err != nil {
		t.Fatalf("mvhd: %v", err)
	}
	f.movieTimescale = ts

	trak := child(t, moov, fourccTrak)
	if edts, ok, err := box.FindChild(trak, fourccEdts); err != nil {
		t.Fatalf("edts: %v", err)
	} else if ok {
		elst := child(t, edts, fourccElst)
		seg, mt, has, err := box.ParseElst(elst)
		if err != nil {
			t.Fatalf("elst: %v", err)
		}
		f.hasEdit, f.editSegment, f.editMediaTime = has, seg, mt
	}
	mdia := child(t, trak, fourccMdia)
	f.mediaTimescale, f.mediaDuration, err = box.ParseMdhd(child(t, mdia, fourccMdhd))
	if err != nil {
		t.Fatalf("mdhd: %v", err)
	}
	stsz := child(t, child(t, child(t, mdia, fourccMinf), fourccStbl), fourccStsz)
	if _, f.sampleCount, _, err = box.ParseStsz(stsz); err != nil {
		t.Fatalf("stsz: %v", err)
	}
	return f
}

func child(t *testing.T, payload []byte, typ box.FourCC) []byte {
	t.Helper()
	body, ok, err := box.FindChild(payload, typ)
	if err != nil {
		t.Fatalf("find %q: %v", typ[:], err)
	}
	if !ok {
		t.Fatalf("missing %q box", typ[:])
	}
	return body
}

// presentation returns the edit list's priming trim and presented length, both
// in media-timescale samples. Without an edit list the whole media is presented.
func (f facts) presentation() (start, length int64) {
	if !f.hasEdit || f.editMediaTime < 0 {
		return 0, int64(f.mediaDuration)
	}
	seg := int64(f.editSegment) * int64(f.mediaTimescale) / int64(f.movieTimescale)
	return f.editMediaTime, seg
}

// record is the per-case baseline entry. Every field is an integer or a string
// derived from integer data, so it is identical on every architecture: lossy
// encoders may round differently per platform, but none of these facts depend
// on the bits they produce.
type record struct {
	Codec            codecName `json:"codec"`
	SampleRate       int       `json:"sample_rate"`
	Channels         int       `json:"channels"`
	BitDepth         int       `json:"bit_depth"`
	SourceSamples    int       `json:"source_samples"`
	Brand            string    `json:"brand"`
	TopLevelBoxes    []string  `json:"top_level_boxes"`
	Timescale        uint32    `json:"timescale"`
	MediaDuration    uint64    `json:"media_duration"`
	HasEditList      bool      `json:"has_edit_list"`
	EditMediaTime    int64     `json:"edit_media_time"`
	EditSegment      uint64    `json:"edit_segment_duration"`
	FrameCount       int       `json:"frame_count"`
	DecodedSamples   int       `json:"decoded_samples"`
	PresentedSamples int       `json:"presented_samples"`
	PCMMD5           string    `json:"pcm_md5,omitempty"`
}

// roundTrip is one case taken through encode, inspect, and decode, with the
// source and the edit-list-trimmed decode kept for the signal checks.
type roundTrip struct {
	src     []byte
	file    []byte
	trimmed []byte
	rec     record
}

func runCase(t *testing.T, c corpusCase) roundTrip {
	t.Helper()
	src := sourcePCM(c)
	file := encode(t, c, src)
	f := inspect(t, file)
	pcm, info := decode(t, c, file)

	fb := c.frameBytes()
	if len(pcm)%fb != 0 {
		t.Fatalf("decoded %d bytes, not a whole number of %d-byte frames", len(pcm), fb)
	}
	decoded := len(pcm) / fb
	start, length := f.presentation()

	rt := roundTrip{src: src, file: file}
	if end := (start + length) * int64(fb); start >= 0 && end <= int64(len(pcm)) {
		rt.trimmed = pcm[start*int64(fb) : end]
	}
	rt.rec = record{
		Codec:            c.codec,
		SampleRate:       c.sampleRate,
		Channels:         c.channels,
		BitDepth:         c.bitDepth,
		SourceSamples:    c.samples,
		Brand:            f.brand,
		TopLevelBoxes:    f.topLevel,
		Timescale:        f.mediaTimescale,
		MediaDuration:    f.mediaDuration,
		HasEditList:      f.hasEdit,
		EditMediaTime:    f.editMediaTime,
		EditSegment:      f.editSegment,
		FrameCount:       info.FrameCount,
		DecodedSamples:   decoded,
		PresentedSamples: len(rt.trimmed) / fb,
	}
	if c.lossless() {
		sum := md5.Sum(rt.trimmed) //nolint:gosec // G401: fingerprint, not security.
		rt.rec.PCMMD5 = hex.EncodeToString(sum[:])
	}

	checkStructure(t, c, f, info)
	// mdhd's duration is the sum of the sample-table durations, and each decoder
	// emits exactly its packet's duration, so the two must agree: an mdhd that
	// disagrees with the sample table, or a demux that reads too few or too many
	// frames, breaks this even when the edit list still fits inside the decode.
	// Info.FrameCount is reported rather than used to bound the decode, so it is
	// checked against the stsz sample count directly.
	if uint64(decoded) != f.mediaDuration {
		t.Errorf("decoded %d samples per channel, but mdhd declares %d", decoded, f.mediaDuration)
	}
	if info.FrameCount != int(f.sampleCount) {
		t.Errorf("Info.FrameCount %d, but stsz holds %d samples", info.FrameCount, f.sampleCount)
	}
	if rt.trimmed == nil {
		t.Fatalf("edit list [%d, +%d) samples falls outside the %d decoded samples", start, length, decoded)
	}
	return rt
}

// checkStructure asserts the container invariants that hold for any correct
// writer, independent of the baseline.
func checkStructure(t *testing.T, c corpusCase, f facts, info m4a.Info) {
	t.Helper()
	if len(f.topLevel) == 0 || f.topLevel[0] != "ftyp" {
		t.Errorf("top-level boxes %v: ftyp must come first", f.topLevel)
	}
	for _, want := range []string{"moov", "mdat"} {
		found := false
		for _, got := range f.topLevel {
			found = found || got == want
		}
		if !found {
			t.Errorf("top-level boxes %v: missing %s", f.topLevel, want)
		}
	}
	if f.movieTimescale != f.mediaTimescale {
		t.Errorf("movie timescale %d != media timescale %d", f.movieTimescale, f.mediaTimescale)
	}
	if int(f.mediaTimescale) != c.sampleRate {
		t.Errorf("media timescale %d, want the %d Hz sample rate", f.mediaTimescale, c.sampleRate)
	}

	start, length := f.presentation()
	switch {
	case c.lossless() && f.hasEdit:
		t.Errorf("FLAC file carries an edit list (media_time %d); FLAC has no priming to trim", f.editMediaTime)
	case !c.lossless() && !f.hasEdit:
		t.Error("lossy file has no edit list, so its priming is presented as audio")
	}
	if length != int64(c.samples) {
		t.Errorf("presented length %d samples, want the %d-sample source", length, c.samples)
	}
	if start != info.EncoderDelay {
		t.Errorf("edit media_time %d != Info.EncoderDelay %d", start, info.EncoderDelay)
	}
	if int64(f.mediaDuration) < start+length {
		t.Errorf("media duration %d cannot cover the edit [%d, +%d)", f.mediaDuration, start, length)
	}
	wantDur := time.Duration(length) * time.Second / time.Duration(c.sampleRate)
	if d := info.Duration - wantDur; d < -time.Microsecond || d > time.Microsecond {
		t.Errorf("Info.Duration %v, want %v", info.Duration, wantDur)
	}
}

// channel0 returns channel 0 of 16-bit interleaved PCM as float64 samples.
func channel0(pcm []byte, channels int) []float64 {
	stride := channels * 2
	out := make([]float64, 0, len(pcm)/stride)
	for off := 0; off+2 <= len(pcm); off += stride {
		out = append(out, float64(int16(binary.LittleEndian.Uint16(pcm[off:]))))
	}
	return out
}

// normXCorr is the normalized cross-correlation of a[n] against b[n+lag], in
// [-1, 1] and invariant to a constant gain, which a lossy codec need not keep.
func normXCorr(a, b []float64, lag int) float64 {
	var dot, na, nb float64
	for n := range a {
		m := n + lag
		if m < 0 || m >= len(b) {
			continue
		}
		dot += a[n] * b[m]
		na += a[n] * a[n]
		nb += b[m] * b[m]
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / math.Sqrt(na*nb)
}

func rms(x []float64) float64 {
	if len(x) == 0 {
		return 0
	}
	var sum float64
	for _, v := range x {
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(x)))
}

// minCorrelation is the floor for a lossy decode's peak correlation with the
// source. It is loose on purpose: this is a gross-breakage and alignment probe,
// and perceptual quality is gated in the codec repos.
const minCorrelation = 0.9

// checkSignal compares a trimmed decode with the source: bit-exact for FLAC, and
// for the lossy codecs aligned at lag 0 (a mistrimmed edit list shifts the peak)
// with energy within a factor of two.
func checkSignal(t *testing.T, c corpusCase, src, trimmed []byte) {
	t.Helper()
	if c.lossless() {
		if !bytes.Equal(trimmed, src) {
			t.Errorf("lossless round trip differs from the source (%d vs %d bytes)", len(trimmed), len(src))
		}
		return
	}
	a, b := channel0(src, c.channels), channel0(trimmed, c.channels)
	bestLag, best := 0, math.Inf(-1)
	for lag := -16; lag <= 16; lag++ {
		if v := normXCorr(a, b, lag); v > best {
			best, bestLag = v, lag
		}
	}
	if bestLag != 0 {
		t.Errorf("decode is shifted %d samples against the source; the edit-list trim is off", bestLag)
	}
	if best < minCorrelation {
		t.Errorf("peak correlation %.4f < %.2f: the decode no longer resembles the source", best, minCorrelation)
	}
	if in, out := rms(a), rms(b); out < 0.5*in || out > 2*in {
		t.Errorf("decoded RMS %.0f outside [0.5, 2] x source RMS %.0f", out, in)
	}
}

func TestContainerIntegrity(t *testing.T) {
	got := make(map[string]record, len(corpus))
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			rt := runCase(t, c)
			checkSignal(t, c, rt.src, rt.trimmed)
			got[c.name] = rt.rec
		})
	}
	if t.Failed() {
		return
	}
	// A -run filter selects only some cases. Comparing that subset would report
	// every other case as missing, and writing it would drop them from the file.
	if len(got) != len(corpus) {
		if *update {
			t.Fatalf("-update needs the whole corpus (%d of %d cases ran); drop the subtest filter", len(got), len(corpus))
		}
		t.Logf("%d of %d cases ran; skipping the baseline comparison", len(got), len(corpus))
		return
	}

	if *update {
		out, err := json.MarshalIndent(got, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(baselinePath), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(baselinePath, append(out, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Logf("wrote %s", baselinePath)
		return
	}

	raw, err := os.ReadFile(baselinePath)
	if err != nil {
		t.Fatalf("read baseline (regenerate with -update): %v", err)
	}
	var want map[string]record
	if err := json.Unmarshal(raw, &want); err != nil {
		t.Fatalf("parse %s: %v", baselinePath, err)
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("baseline case %s is no longer in the corpus; regenerate with -update", name)
		}
	}
	for name, g := range got {
		w, ok := want[name]
		if !ok {
			t.Errorf("case %s has no baseline entry; regenerate with -update", name)
			continue
		}
		gj, gerr := json.MarshalIndent(g, "", "  ")
		wj, werr := json.MarshalIndent(w, "", "  ")
		if gerr != nil || werr != nil {
			t.Fatalf("marshal %s: %v, %v", name, gerr, werr)
		}
		if !bytes.Equal(gj, wj) {
			t.Errorf("%s drifted from the baseline.\ngot:  %s\nwant: %s\nIf the change is intended, regenerate with -update and review the diff.", name, gj, wj)
		}
	}
}
