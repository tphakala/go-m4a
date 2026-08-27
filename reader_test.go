// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// readerFromWriter writes cfg + frames with the Writer and returns a Reader over
// the produced bytes, so the round-trip tests exercise the exact byte layout the
// Writer emits.
func readerFromWriter(t *testing.T, cfg WriterConfig, frames [][]byte) *Reader {
	t.Helper()
	data := writeM4A(t, cfg, frames)
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	return r
}

// collectFrames drains ReadFrame until io.EOF and returns the access units.
func collectFrames(t *testing.T, r *Reader) [][]byte {
	t.Helper()
	var got [][]byte
	for {
		au, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			return got
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		got = append(got, au)
	}
}

func TestRoundTripReadFrame(t *testing.T) {
	for _, n := range []int{1, 2, 5, 30} {
		frames := synthFrames(n)
		cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}
		r := readerFromWriter(t, cfg, frames)

		info := r.Info()
		if info.SampleRate != 48000 {
			t.Errorf("n=%d SampleRate = %d, want 48000", n, info.SampleRate)
		}
		if info.Channels != 1 {
			t.Errorf("n=%d Channels = %d, want 1", n, info.Channels)
		}
		if info.FrameCount != n {
			t.Errorf("n=%d FrameCount = %d, want %d", n, info.FrameCount, n)
		}
		if info.EncoderDelay != DefaultEncoderDelay {
			t.Errorf("n=%d EncoderDelay = %d, want %d", n, info.EncoderDelay, DefaultEncoderDelay)
		}
		if info.Brand != "M4A " {
			t.Errorf("n=%d Brand = %q, want %q", n, info.Brand, "M4A ")
		}
		if !bytes.Equal(info.ASC, ascMono48k) {
			t.Errorf("n=%d ASC = % x, want % x", n, info.ASC, ascMono48k)
		}

		got := collectFrames(t, r)
		if len(got) != n {
			t.Fatalf("n=%d read %d frames, want %d", n, len(got), n)
		}
		for i := range frames {
			if !bytes.Equal(got[i], frames[i]) {
				t.Errorf("n=%d frame %d = % x, want % x", n, i, got[i], frames[i])
			}
		}
		// A further read is a clean EOF.
		if _, err := r.ReadFrame(); !errors.Is(err, io.EOF) {
			t.Errorf("n=%d read past end = %v, want io.EOF", n, err)
		}
	}
}

func TestRoundTripStereo(t *testing.T) {
	frames := synthFrames(7)
	r := readerFromWriter(t, WriterConfig{SampleRate: 44100, Channels: 2, ASC: ascStereo44k}, frames)
	info := r.Info()
	if info.SampleRate != 44100 || info.Channels != 2 {
		t.Errorf("format = %d Hz / %d ch, want 44100 / 2", info.SampleRate, info.Channels)
	}
	if !bytes.Equal(info.ASC, ascStereo44k) {
		t.Errorf("ASC = % x, want % x", info.ASC, ascStereo44k)
	}
	got := collectFrames(t, r)
	if len(got) != len(frames) {
		t.Fatalf("read %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("frame %d mismatch", i)
		}
	}
}

func TestRoundTripEditListVariants(t *testing.T) {
	const n = 10
	frames := synthFrames(n)
	tests := []struct {
		name      string
		cfg       WriterConfig
		wantDelay int64
		wantSeg   uint64 // presentation media samples at 48 kHz timescale
	}{
		{
			name:      "default delay",
			cfg:       WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k},
			wantDelay: DefaultEncoderDelay,
			wantSeg:   n*samplesPerFrame - DefaultEncoderDelay,
		},
		{
			name:      "explicit delay",
			cfg:       WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: 2112},
			wantDelay: 2112,
			wantSeg:   n*samplesPerFrame - 2112,
		},
		{
			name:      "media length",
			cfg:       WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, MediaLength: 5000},
			wantDelay: DefaultEncoderDelay,
			wantSeg:   5000,
		},
		{
			name:      "no edit",
			cfg:       WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: NoEdit},
			wantDelay: 0,
			wantSeg:   n * samplesPerFrame, // full media presented
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := readerFromWriter(t, tc.cfg, frames)
			info := r.Info()
			if info.EncoderDelay != tc.wantDelay {
				t.Errorf("EncoderDelay = %d, want %d", info.EncoderDelay, tc.wantDelay)
			}
			want := time.Duration(float64(tc.wantSeg) / 48000 * float64(time.Second))
			if info.Duration != want {
				t.Errorf("Duration = %v, want %v", info.Duration, want)
			}
		})
	}
}

func TestASCReturnsCopy(t *testing.T) {
	r := readerFromWriter(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(3))
	a := r.ASC()
	if !bytes.Equal(a, ascMono48k) {
		t.Fatalf("ASC = % x, want % x", a, ascMono48k)
	}
	a[0] ^= 0xff // mutate the returned copy
	if b := r.ASC(); !bytes.Equal(b, ascMono48k) {
		t.Errorf("mutating the returned ASC changed the Reader: got % x", b)
	}
	// Info().ASC is likewise a copy.
	info := r.Info()
	info.ASC[0] ^= 0xff
	if b := r.Info().ASC; !bytes.Equal(b, ascMono48k) {
		t.Errorf("mutating Info().ASC changed the Reader: got % x", b)
	}
}

func TestRawStreamFraming(t *testing.T) {
	frames := synthFrames(6)
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}

	raw, err := io.ReadAll(r.RawStream())
	if err != nil {
		t.Fatalf("read RawStream: %v", err)
	}

	// Parse the length-prefixed stream back into access units.
	var got [][]byte
	for off := 0; off < len(raw); {
		if off+2 > len(raw) {
			t.Fatalf("truncated length prefix at %d", off)
		}
		n := int(binary.BigEndian.Uint16(raw[off:]))
		off += 2
		if off+n > len(raw) {
			t.Fatalf("frame at %d claims %d bytes, only %d remain", off, n, len(raw)-off)
		}
		got = append(got, append([]byte(nil), raw[off:off+n]...))
		off += n
	}
	if len(got) != len(frames) {
		t.Fatalf("RawStream yielded %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("RawStream frame %d = % x, want % x", i, got[i], frames[i])
		}
	}
}

func TestNewReaderNil(t *testing.T) {
	if _, err := NewReader(nil); err == nil {
		t.Fatal("NewReader(nil) succeeded, want error")
	}
}

// interopFiles are the ffmpeg- and afconvert-produced fixtures with their
// ffprobe-verified expected values. They exercise moov-before and moov-after
// mdat, multi-chunk stsc (afconvert), and elst-present versus none.
var interopFiles = []struct {
	name         string
	sampleRate   int
	channels     int
	frameCount   int
	wantEditList bool // ffmpeg writes an elst; afconvert does not
}{
	{"ffmpeg_mono48k.m4a", 48000, 1, 30, true},
	{"afconvert_mono48k.m4a", 48000, 1, 31, false},
	{"ffmpeg_stereo44k.m4a", 44100, 2, 27, true},
	{"afconvert_stereo44k.m4a", 44100, 2, 28, false},
}

func TestInterop(t *testing.T) {
	for _, tc := range interopFiles {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join("testdata", "interop", tc.name)
			f, err := os.Open(path) //nolint:gosec // fixed test fixture path
			if err != nil {
				t.Skipf("fixture missing: %v", err)
			}
			defer func() { _ = f.Close() }()

			r, err := NewReader(f)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			info := r.Info()
			assertInteropInfo(t, &info, tc.sampleRate, tc.channels, tc.frameCount, tc.wantEditList)

			total := assertInteropFrames(t, r, tc.frameCount, fileSize(t, path))
			t.Logf("%s: %d Hz, %d ch, %d frames, %d AU bytes, EncoderDelay=%d, Duration=%v",
				tc.name, info.SampleRate, info.Channels, info.FrameCount, total, info.EncoderDelay, info.Duration)
		})
	}
}

// assertInteropInfo checks the parsed Info against the ffprobe-derived expected
// values, including ASC consistency and edit-list-driven EncoderDelay.
func assertInteropInfo(t *testing.T, info *Info, sampleRate, channels, frameCount int, wantEditList bool) {
	t.Helper()
	if info.SampleRate != sampleRate {
		t.Errorf("SampleRate = %d, want %d", info.SampleRate, sampleRate)
	}
	if info.Channels != channels {
		t.Errorf("Channels = %d, want %d", info.Channels, channels)
	}
	if info.FrameCount != frameCount {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, frameCount)
	}
	if len(info.ASC) == 0 {
		t.Error("ASC is empty")
	}
	// The ASC must decode to the same rate and channels as Info reports.
	_, sfi, chanCfg := parseASC(info.ASC)
	if int(sfi) >= len(samplingFrequencyTable) || samplingFrequencyTable[sfi] != sampleRate {
		t.Errorf("ASC samplingFrequencyIndex %d inconsistent with %d Hz", sfi, sampleRate)
	}
	if int(chanCfg) != channels {
		t.Errorf("ASC channelConfiguration %d inconsistent with %d channels", chanCfg, channels)
	}
	// EncoderDelay tracks whether the file carries an edit list.
	if wantEditList && info.EncoderDelay == 0 {
		t.Error("EncoderDelay = 0, want > 0 for a file with an edit list")
	}
	if !wantEditList && info.EncoderDelay != 0 {
		t.Errorf("EncoderDelay = %d, want 0 for a file with no edit list", info.EncoderDelay)
	}
	if info.Duration <= 0 {
		t.Errorf("Duration = %v, want positive", info.Duration)
	}
}

// assertInteropFrames drains ReadFrame, asserting exactly frameCount access
// units of plausible size, and returns the total access-unit byte count.
func assertInteropFrames(t *testing.T, r *Reader, frameCount int, maxBytes int64) int {
	t.Helper()
	frames := collectFrames(t, r)
	if len(frames) != frameCount {
		t.Fatalf("read %d frames, want %d", len(frames), frameCount)
	}
	var total int
	for i, au := range frames {
		if len(au) == 0 || len(au) > 0xffff {
			t.Errorf("frame %d has implausible size %d", i, len(au))
		}
		total += len(au)
	}
	if total == 0 || int64(total) > maxBytes {
		t.Errorf("total AU bytes %d is not sane against the file", total)
	}
	return total
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}
	return fi.Size()
}

// patch returns a copy of data with sub's first occurrence replaced by repl.
func patch(t *testing.T, data []byte, sub, repl string) []byte {
	t.Helper()
	i := bytes.Index(data, []byte(sub))
	if i < 0 {
		t.Fatalf("substring %q not found", sub)
	}
	if len(sub) != len(repl) {
		t.Fatalf("patch length mismatch: %q vs %q", sub, repl)
	}
	out := append([]byte(nil), data...)
	copy(out[i:], repl)
	return out
}

func TestMalformedInput(t *testing.T) {
	valid := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(5))

	// A minimal valid ftyp box (size 8, no payload) for the missing-moov case.
	ftypOnly := func() []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, 8)
		copy(b[4:], "ftyp")
		return b
	}()

	// ftyp whose 32-bit size claims far more than the stream holds.
	overflow := func() []byte {
		b := make([]byte, 8)
		binary.BigEndian.PutUint32(b, 0x7fffffff)
		copy(b[4:], "ftyp")
		return b
	}()

	// A copy of the valid file with the ftyp size field zeroed: the extends-to-EOF
	// box swallows moov, which then cannot be found.
	sizeZero := append([]byte(nil), valid...)
	binary.BigEndian.PutUint32(sizeZero, 0)

	// A copy with a wildly inflated stsz sample_count. From the "stsz" type
	// bytes: version/flags(4) + sample_size(4), then sample_count at +12.
	lyingStsz := append([]byte(nil), valid...)
	if i := bytes.Index(lyingStsz, []byte("stsz")); i >= 0 {
		binary.BigEndian.PutUint32(lyingStsz[i+12:], 0x7fffffff)
	}

	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, ErrCorrupt},
		{"truncated header", []byte{0x00, 0x00, 0x00}, ErrCorrupt},
		{"size overflows stream", overflow, ErrCorrupt},
		{"missing moov", ftypOnly, ErrCorrupt},
		{"size zero non-terminal", sizeZero, ErrCorrupt},
		{"no soun track", patch(t, valid, "soun", "vide"), ErrUnsupported},
		{"codec not mp4a", patch(t, valid, "mp4a", "ac-3"), ErrUnsupported},
		{"lying stsz count", lyingStsz, ErrCorrupt},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := noPanicNewReader(t, tc.data)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

// noPanicNewReader runs NewReader and, on success, drains ReadFrame, recovering
// any panic into a test failure. It returns the first error observed.
func noPanicNewReader(t *testing.T, data []byte) (err error) {
	t.Helper()
	defer func() {
		if p := recover(); p != nil {
			t.Fatalf("panic on malformed input: %v", p)
		}
	}()
	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	for {
		_, ferr := r.ReadFrame()
		if ferr != nil {
			if errors.Is(ferr, io.EOF) {
				return nil
			}
			return ferr
		}
	}
}

// TestResolveFormatFLACChannelsFromStreamInfo pins that a FLAC track reports the
// channel count from its dfLa STREAMINFO, not the AudioSampleEntry. A foreign muxer
// may cap the sample-entry channelcount at 2 for a multichannel FLAC stream while
// STREAMINFO carries the true count, so the block is authoritative for channels
// exactly as it already is for the sample rate.
func TestResolveFormatFLACChannelsFromStreamInfo(t *testing.T) {
	t.Parallel()

	// STREAMINFO declares 6 channels; the sample entry is capped at 2.
	tr := &track{
		codec:        fourccFlac,
		codecConfig:  flacStreamInfo(48000, 6),
		seChannels:   2,
		seSampleRate: 48000,
	}
	if rate, ch := resolveFormat(tr); ch != 6 || rate != 48000 {
		t.Errorf("resolveFormat = (%d Hz, %d ch), want (48000, 6) from STREAMINFO", rate, ch)
	}

	// With no STREAMINFO (absent/truncated), STREAMINFOChannels yields 0 and the
	// sample-entry channel count stands.
	trNoSI := &track{codec: fourccFlac, codecConfig: nil, seChannels: 2, seSampleRate: 48000}
	if _, ch := resolveFormat(trNoSI); ch != 2 {
		t.Errorf("resolveFormat channels with no STREAMINFO = %d, want 2 (sample-entry fallback)", ch)
	}

	// An all-zero placeholder block declares rate 0 (so it is not "real") but its
	// channel field decodes as 1. The override is gated on a nonzero rate, so the
	// sample-entry count must stand rather than being clobbered to mono.
	trPlaceholder := &track{codec: fourccFlac, codecConfig: make([]byte, flacStreamInfoLen), seChannels: 6, seSampleRate: 48000}
	if _, ch := resolveFormat(trPlaceholder); ch != 6 {
		t.Errorf("resolveFormat channels with all-zero placeholder STREAMINFO = %d, want 6 (sample-entry, not overridden to 1)", ch)
	}
}
