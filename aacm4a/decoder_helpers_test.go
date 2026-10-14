// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"sync"
	"testing"
	"time"

	aac "github.com/tphakala/go-aac"

	"github.com/tphakala/go-m4a/internal/testutil"

	m4a "github.com/tphakala/go-m4a"
)

// fixture is a go-aac encode muxed into an in-memory .m4a, with the pieces the
// seek tests need.
type fixture struct {
	data     []byte
	aus      [][]byte
	wcfg     m4a.WriterConfig
	stats    aac.Stats
	rate     int
	channels int
}

func (f *fixture) stride() int { return f.channels * 2 }

// synthSignal returns seconds of planar float32 audio: two tones per channel,
// 200-sample noise bursts every 9000 samples (they force short windows and TNS),
// and broadband noise when noisy is set (it makes the encoder use PNS).
func synthSignal(rate, channels, seconds int, noisy bool) [][]float32 {
	n := rate * seconds
	rng := rand.New(rand.NewPCG(7, 11)) //nolint:gosec // G404: a fixed-seed test signal, not a security boundary.
	out := make([][]float32, channels)
	for c := range channels {
		ch := make([]float32, n)
		f1 := 440.0 * (1 + 0.5*float64(c))
		f2 := 1870.0 * (1 + 0.25*float64(c))
		for i := range ch {
			ts := float64(i) / float64(rate)
			v := 0.25*math.Sin(2*math.Pi*f1*ts) + 0.15*math.Sin(2*math.Pi*f2*ts)
			if i%9000 < 200 {
				v += 0.7 * (rng.Float64()*2 - 1)
			}
			if noisy {
				v += 0.15 * (rng.Float64()*2 - 1)
			}
			ch[i] = float32(max(-1, min(1, v)))
		}
		out[c] = ch
	}
	return out
}

// encodeFixture encodes three seconds of synthSignal with go-aac's encoder and
// muxes the access units the way EncodeInterleaved does. It uses aac.NewEncoder
// directly because only aac.EncoderConfig can switch PNS off.
func encodeFixture(tb testing.TB, rate, channels, bitrate int, noisy, disablePNS bool) fixture {
	tb.Helper()
	enc, err := aac.NewEncoder(aac.EncoderConfig{
		SampleRate: rate, Channels: channels, Bitrate: bitrate, DisablePNS: disablePNS,
	})
	if err != nil {
		tb.Fatalf("NewEncoder: %v", err)
	}
	sig := synthSignal(rate, channels, 3, noisy)
	total := len(sig[0])
	var aus [][]byte
	emit := func(out []byte) {
		if len(out) > 0 {
			aus = append(aus, bytes.Clone(out))
		}
	}
	for off := 0; off < total; off += aac.FrameSize {
		end := min(off+aac.FrameSize, total)
		frame := make([][]float32, channels)
		for c := range frame {
			frame[c] = sig[c][off:end]
		}
		out, err := enc.EncodeFrame(nil, frame)
		if err != nil {
			tb.Fatalf("EncodeFrame: %v", err)
		}
		emit(out)
	}
	for !enc.Drained() {
		out, err := enc.EncodeFrame(nil, nil)
		if err != nil {
			tb.Fatalf("drain: %v", err)
		}
		emit(out)
	}
	wcfg := m4a.WriterConfig{
		SampleRate:   rate,
		Channels:     channels,
		ASC:          enc.AudioSpecificConfig(),
		EncoderDelay: aac.EncoderDelay,
		MediaLength:  int64(total),
	}
	var ws testutil.WriteSeeker
	w, err := m4a.NewWriter(&ws, wcfg)
	if err != nil {
		tb.Fatalf("NewWriter: %v", err)
	}
	for _, au := range aus {
		if err := w.WriteFrame(au); err != nil {
			tb.Fatalf("WriteFrame: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		tb.Fatalf("Close: %v", err)
	}
	return fixture{data: ws.Bytes(), aus: aus, wcfg: wcfg, stats: enc.Stats(), rate: rate, channels: channels}
}

// fixtureCache holds encoded fixtures by key so tests that share a config encode
// it once. Callers must not modify the cached slices.
var fixtureCache sync.Map

func cachedFixture(tb testing.TB, key string, build func() fixture) fixture {
	tb.Helper()
	if v, ok := fixtureCache.Load(key); ok {
		if f, ok := v.(fixture); ok {
			return f
		}
	}
	f := build()
	fixtureCache.Store(key, f)
	return f
}

// exactFixture is a PNS-free fixture that also exercises short windows.
func exactFixture(tb testing.TB, rate, channels int) fixture {
	tb.Helper()
	return cachedFixture(tb, fmt.Sprintf("exact/%d/%d", rate, channels), func() fixture {
		return buildExactFixture(tb, rate, channels)
	})
}

func buildExactFixture(tb testing.TB, rate, channels int) fixture {
	tb.Helper()
	f := encodeFixture(tb, rate, channels, 64000*channels, false, true)
	if f.stats.PNSBands != 0 {
		tb.Fatalf("exact fixture has %d PNS bands", f.stats.PNSBands)
	}
	if f.stats.ShortFrames == 0 {
		tb.Fatalf("exact fixture has no short windows, so it does not exercise window shape state")
	}
	return f
}

// pnsFixture is a noisy fixture that must contain PNS bands.
func pnsFixture(tb testing.TB, rate, channels, bitrate int) fixture {
	tb.Helper()
	return cachedFixture(tb, fmt.Sprintf("pns/%d/%d/%d", rate, channels, bitrate), func() fixture {
		f := encodeFixture(tb, rate, channels, bitrate, true, false)
		if f.stats.PNSBands == 0 {
			tb.Fatalf("PNS fixture has no PNS bands")
		}
		return f
	})
}

// exactConfigs are the rate and channel combinations the bit-exact tests cover.
var exactConfigs = []struct {
	name     string
	rate     int
	channels int
}{
	{"mono44k", 44100, 1},
	{"stereo44k", 44100, 2},
	{"mono48k", 48000, 1},
	{"stereo48k", 48000, 2},
}

// linear decodes data with NewDecoder and returns every byte.
func linear(tb testing.TB, data []byte) []byte {
	tb.Helper()
	d, _, err := NewDecoder(bytes.NewReader(data))
	if err != nil {
		tb.Fatalf("NewDecoder: %v", err)
	}
	out, err := io.ReadAll(d)
	if err != nil {
		tb.Fatalf("linear decode: %v", err)
	}
	return out
}

// newSeekable opens data with NewSeekableDecoder.
func newSeekable(tb testing.TB, data []byte) (*Decoder, m4a.Info) {
	tb.Helper()
	d, info, err := NewSeekableDecoder(bytes.NewReader(data))
	if err != nil {
		tb.Fatalf("NewSeekableDecoder: %v", err)
	}
	return d, info
}

// presentedSamples is Info.Duration at the sample rate rounded up. Duration is a
// floor in nanoseconds, so a truncating product loses the last sample on many
// 44.1 kHz lengths.
func presentedSamples(info m4a.Info) int64 {
	return (int64(info.Duration)*int64(info.SampleRate) + int64(time.Second) - 1) / int64(time.Second)
}

// sampleTime converts a sample count at rate to a Duration. The truncation stays
// far below the half-tick rounding Reader.SeekTime applies, so it resolves back
// to n.
func sampleTime(n int64, rate int) time.Duration {
	return time.Duration(n * int64(time.Second) / int64(rate))
}

// firstDiff returns the index of the first differing byte, or -1 when equal.
func firstDiff(a, b []byte) int {
	n := min(len(a), len(b))
	for i := range n {
		if a[i] != b[i] {
			return i
		}
	}
	if len(a) != len(b) {
		return n
	}
	return -1
}

// requireSame fails the test when got differs from want.
func requireSame(tb testing.TB, what string, got, want []byte) {
	tb.Helper()
	if i := firstDiff(got, want); i >= 0 {
		tb.Fatalf("%s: output differs at byte %d (got %d bytes, want %d)", what, i, len(got), len(want))
	}
}

// fragmentedStream muxes aus as an init segment plus roughly two-second
// segments, cut on access-unit boundaries, and returns the stream and the number
// of segments. The fragmented edit list is open-ended, so MediaLength is cleared.
func fragmentedStream(tb testing.TB, aus [][]byte, wcfg m4a.WriterConfig) (stream []byte, segments int) {
	tb.Helper()
	wcfg.MediaLength = 0
	init, err := m4a.InitSegment(wcfg)
	if err != nil {
		tb.Fatalf("InitSegment: %v", err)
	}
	fw, err := m4a.NewFragmentWriter(wcfg)
	if err != nil {
		tb.Fatalf("NewFragmentWriter: %v", err)
	}
	stream = bytes.Clone(init)
	flush := func() {
		if fw.PendingSamples() == 0 {
			return
		}
		var err error
		stream, err = fw.AppendSegment(stream)
		if err != nil {
			tb.Fatalf("AppendSegment: %v", err)
		}
		segments++
	}
	for _, au := range aus {
		if err := fw.WriteFrame(au); err != nil {
			tb.Fatalf("WriteFrame: %v", err)
		}
		if fw.PendingDuration() >= uint64(2*wcfg.SampleRate) {
			flush()
		}
	}
	flush()
	return stream, segments
}
