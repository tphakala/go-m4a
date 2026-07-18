// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// memWriteSeeker is an in-memory io.WriteSeeker: it grows a byte slice on write
// and supports seeking anywhere, exactly what the Writer needs to patch the mdat
// size and append moov. The round-trip tests encode into it and then decode the
// captured bytes through a bytes.Reader.
type memWriteSeeker struct {
	buf []byte
	pos int64
}

func (m *memWriteSeeker) Write(p []byte) (int, error) {
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

func (m *memWriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("memWriteSeeker: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("memWriteSeeker: negative position %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// chirpS16 synthesizes samplesPerChannel of a linear-frequency chirp per channel
// as interleaved little-endian S16. Each channel sweeps a distinct frequency band
// so that a deinterleave bug (reading the wrong channel) would drop the
// cross-correlation, not slip past the test.
func chirpS16(samplesPerChannel, channels, sampleRate int) []byte {
	const amp = 0.5 * math.MaxInt16
	dur := float64(samplesPerChannel) / float64(sampleRate)
	pcm := make([]byte, samplesPerChannel*channels*2)
	for n := 0; n < samplesPerChannel; n++ {
		t := float64(n) / float64(sampleRate)
		for c := 0; c < channels; c++ {
			// Channel 0: 300 Hz -> sampleRate/4. Channel 1: 500 Hz -> sampleRate/6.
			f0 := 300.0 + float64(c)*200.0
			f1 := float64(sampleRate) / (4.0 + float64(c)*2.0)
			// Instantaneous phase of a linear chirp: integral of the swept
			// frequency, i.e. f0*t + (f1-f0)/(2*dur)*t^2, times 2*pi.
			phase := 2 * math.Pi * (f0*t + 0.5*(f1-f0)/dur*t*t)
			s := int16(amp * math.Sin(phase))
			off := (n*channels + c) * 2
			binary.LittleEndian.PutUint16(pcm[off:], uint16(s))
		}
	}
	return pcm
}

// deinterleaveCh0 pulls channel 0 out of an interleaved S16 byte buffer as
// float64 samples, taking at most samplesPerChannel of them.
func deinterleaveCh0(pcm []byte, channels, samplesPerChannel int) []float64 {
	out := make([]float64, 0, samplesPerChannel)
	stride := channels * 2
	for n := 0; n < samplesPerChannel; n++ {
		off := n * stride
		if off+2 > len(pcm) {
			break
		}
		out = append(out, float64(int16(binary.LittleEndian.Uint16(pcm[off:]))))
	}
	return out
}

// normXCorr is the normalized cross-correlation of a against b at the given lag,
// aligning a[n] with b[n+lag]. The result is in [-1, 1] and is invariant to a
// constant amplitude scale, which is what makes it a clean alignment probe for a
// lossy codec that need not preserve absolute level.
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

// encodeToBytes runs EncodeInterleaved into an in-memory writer and returns the
// captured .m4a bytes, failing the test on any encode error.
func encodeToBytes(t *testing.T, cfg aacpcm.Config, pcm []byte) []byte {
	t.Helper()
	var ws memWriteSeeker
	if err := EncodeInterleaved(&ws, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	return ws.buf
}

func TestRoundTripGapless(t *testing.T) {
	cases := []struct {
		name              string
		cfg               aacpcm.Config
		samplesPerChannel int
	}{
		{
			name:              "mono48k",
			cfg:               aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1},
			samplesPerChannel: 32768,
		},
		{
			name:              "stereo44k",
			cfg:               aacpcm.Config{SampleRate: 44100, BitDepth: 16, Channels: 2},
			samplesPerChannel: 24000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := chirpS16(tc.samplesPerChannel, tc.cfg.Channels, tc.cfg.SampleRate)
			data := encodeToBytes(t, tc.cfg, src)

			d, info, err := NewDecoder(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if info.SampleRate != tc.cfg.SampleRate || info.Channels != tc.cfg.Channels {
				t.Fatalf("Info = %d Hz / %d ch, want %d / %d",
					info.SampleRate, info.Channels, tc.cfg.SampleRate, tc.cfg.Channels)
			}

			var decoded bytes.Buffer
			if _, err := io.Copy(&decoded, d); err != nil {
				t.Fatalf("decode copy: %v", err)
			}

			// Trim info.EncoderDelay leading samples per channel, then keep the
			// source length, exactly the gapless-playback recipe the API documents.
			trim := int(info.EncoderDelay) * tc.cfg.Channels * 2
			if trim > decoded.Len() {
				t.Fatalf("decoded %d bytes shorter than %d-byte priming trim", decoded.Len(), trim)
			}
			body := decoded.Bytes()[trim:]

			decCh0 := deinterleaveCh0(body, tc.cfg.Channels, tc.samplesPerChannel)
			if len(decCh0) < tc.samplesPerChannel {
				t.Fatalf("decoded channel 0 has %d samples, need %d", len(decCh0), tc.samplesPerChannel)
			}
			srcCh0 := deinterleaveCh0(src, tc.cfg.Channels, tc.samplesPerChannel)

			bestLag, bestCorr := -100, math.Inf(-1)
			for lag := -4; lag <= 4; lag++ {
				c := normXCorr(srcCh0, decCh0, lag)
				if c > bestCorr {
					bestCorr, bestLag = c, lag
				}
			}
			if bestLag != 0 {
				t.Errorf("cross-correlation peaks at lag %d, want 0 (edit-list trim is off by %d samples)", bestLag, bestLag)
			}
			if bestCorr < 0.99 {
				t.Errorf("peak normalized correlation %.4f at lag %d, want > 0.99", bestCorr, bestLag)
			}
			t.Logf("%s: peak lag %d, corr %.4f", tc.name, bestLag, bestCorr)
		})
	}
}

func TestAudioSpecificConfig(t *testing.T) {
	cases := []struct {
		name     string
		rate     int
		channels int
		want     []byte
	}{
		// 48000 mono: object type 2, samplingFrequencyIndex 3, channels 1.
		{"mono48k", 48000, 1, []byte{0x11, 0x88}},
		// 44100 stereo: object type 2, samplingFrequencyIndex 4, channels 2. The
		// value matches the core writer's ascStereo44k and go-aac's asc.go.
		{"stereo44k", 44100, 2, []byte{0x12, 0x10}},
		// 48000 stereo, included because {0x11,0x90} is a value easy to confuse
		// with 44100 stereo: it is samplingFrequencyIndex 3 (48000), not 4.
		{"stereo48k", 48000, 2, []byte{0x11, 0x90}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := audioSpecificConfig(tc.rate, tc.channels)
			if err != nil {
				t.Fatalf("audioSpecificConfig: %v", err)
			}
			if !bytes.Equal(got, tc.want) {
				t.Errorf("ASC(%d, %d) = % x, want % x", tc.rate, tc.channels, got, tc.want)
			}
		})
	}
}

func TestInteropDecode(t *testing.T) {
	cases := []struct {
		file       string
		sampleRate int
		channels   int
		frameCount int
		delay      int64
		wantBytes  int64
	}{
		{"ffmpeg_mono48k.m4a", 48000, 1, 30, m4a.DefaultEncoderDelay, 61440},
		{"afconvert_mono48k.m4a", 48000, 1, 31, 0, 63488},
		{"ffmpeg_stereo44k.m4a", 44100, 2, 27, m4a.DefaultEncoderDelay, 110592},
		{"afconvert_stereo44k.m4a", 44100, 2, 28, 0, 114688},
	}
	for _, tc := range cases {
		t.Run(tc.file, func(t *testing.T) {
			path := filepath.Join("..", "testdata", "interop", tc.file)
			f, err := os.Open(path)
			if err != nil {
				t.Fatalf("open %s: %v", path, err)
			}
			defer func() { _ = f.Close() }()

			d, info, err := NewDecoder(f)
			if err != nil {
				t.Fatalf("NewDecoder: %v", err)
			}
			if info.SampleRate != tc.sampleRate || info.Channels != tc.channels {
				t.Errorf("Info = %d Hz / %d ch, want %d / %d",
					info.SampleRate, info.Channels, tc.sampleRate, tc.channels)
			}
			if info.FrameCount != tc.frameCount {
				t.Errorf("FrameCount = %d, want %d", info.FrameCount, tc.frameCount)
			}
			if info.EncoderDelay != tc.delay {
				t.Errorf("EncoderDelay = %d, want %d", info.EncoderDelay, tc.delay)
			}

			n, err := io.Copy(io.Discard, d)
			if err != nil {
				t.Fatalf("decode copy: %v", err)
			}
			if n != tc.wantBytes {
				t.Errorf("decoded %d bytes, want %d", n, tc.wantBytes)
			}
		})
	}
}

// TestFFprobeValidates encodes a real file and asks ffprobe to demux it, proving
// the bridge output is valid to a third-party tool. It is skipped when ffprobe is
// not on PATH.
func TestFFprobeValidates(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not on PATH")
	}

	cfg := aacpcm.Config{SampleRate: 48000, BitDepth: 16, Channels: 1}
	src := chirpS16(16000, cfg.Channels, cfg.SampleRate)

	path := filepath.Join(t.TempDir(), "probe.m4a")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := EncodeInterleaved(f, cfg, src); err != nil {
		_ = f.Close()
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// CombinedOutput captures stderr alongside stdout so a nonzero exit is
	// diagnosable; "-v error" keeps stderr empty on success, leaving pure JSON.
	out, err := exec.CommandContext(ctx, ffprobe, "-v", "error", "-show_streams",
		"-print_format", "json", path).CombinedOutput()
	if err != nil {
		t.Fatalf("ffprobe: %v\n%s", err, out)
	}

	var probe struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			CodecType  string `json:"codec_type"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parse ffprobe json: %v", err)
	}
	if len(probe.Streams) != 1 {
		t.Fatalf("ffprobe reported %d streams, want 1", len(probe.Streams))
	}
	s := probe.Streams[0]
	if s.CodecName != "aac" {
		t.Errorf("codec_name = %q, want aac", s.CodecName)
	}
	if s.SampleRate != "48000" {
		t.Errorf("sample_rate = %q, want 48000", s.SampleRate)
	}
	if s.Channels != 1 {
		t.Errorf("channels = %d, want 1", s.Channels)
	}
}
