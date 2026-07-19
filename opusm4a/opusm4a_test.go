// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"testing"

	m4a "github.com/tphakala/go-m4a"
)

// memWS is an in-memory io.WriteSeeker for the round-trip tests.
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

// genSine builds interleaved little-endian 16-bit PCM of a 440 Hz sine at 48 kHz.
func genSine(samplesPerCh, channels int) []byte {
	out := make([]byte, 0, samplesPerCh*channels*2)
	for i := 0; i < samplesPerCh; i++ {
		v := int16(math.Round(18000 * math.Sin(2*math.Pi*440*float64(i)/48000)))
		for c := 0; c < channels; c++ {
			out = binary.LittleEndian.AppendUint16(out, uint16(v))
		}
	}
	return out
}

func rms(pcm []byte) float64 {
	n := len(pcm) / 2
	if n == 0 {
		return 0
	}
	var sum float64
	for i := 0; i < n; i++ {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[2*i:])))
		sum += v * v
	}
	return math.Sqrt(sum / float64(n))
}

func TestOpusRoundTrip(t *testing.T) {
	for _, channels := range []int{1, 2} {
		t.Run(fmt.Sprintf("ch%d", channels), func(t *testing.T) {
			const samplesPerCh = 24000 // 0.5 s at 48 kHz
			pcm := genSine(samplesPerCh, channels)

			var buf memWS
			cfg := Config{SampleRate: 48000, Channels: channels, Bitrate: 96000}
			if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}

			got, info, err := DecodeInterleaved(bytes.NewReader(buf.buf))
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if info.Codec != m4a.CodecOpus {
				t.Errorf("Codec = %v, want Opus", info.Codec)
			}
			if info.SampleRate != 48000 || info.Channels != channels {
				t.Errorf("format = %d Hz / %d ch, want 48000/%d", info.SampleRate, info.Channels, channels)
			}
			if info.EncoderDelay != 312 {
				t.Errorf("EncoderDelay = %d, want 312", info.EncoderDelay)
			}
			// The encoder emits ceil((content + pre-skip) / 960) full 20 ms packets:
			// enough to flush every content sample past the pre-skip delay.
			wantFrames := (samplesPerCh + int(info.EncoderDelay) + frameSamplesPerChannel - 1) / frameSamplesPerChannel
			if info.FrameCount != wantFrames {
				t.Errorf("FrameCount = %d, want %d", info.FrameCount, wantFrames)
			}

			// Opus is lossy, so trim the pre-skip priming and compare signal energy
			// rather than bytes. The decode has EncoderDelay leading priming samples
			// per channel; drop them, then keep the original content length.
			skip := int(info.EncoderDelay) * channels * 2
			if skip > len(got) {
				t.Fatalf("decoded %d bytes, fewer than the %d-byte pre-skip", len(got), skip)
			}
			content := got[skip:]
			wantBytes := samplesPerCh * channels * 2
			if len(content) < wantBytes {
				t.Fatalf("decoded content %d bytes, want at least %d", len(content), wantBytes)
			}
			content = content[:wantBytes]

			inRMS, outRMS := rms(pcm), rms(content)
			if outRMS < 0.5*inRMS || outRMS > 1.5*inRMS {
				t.Errorf("decoded RMS %.0f not within 50%% of input RMS %.0f (lossy but energy-preserving)", outRMS, inRMS)
			}
			t.Logf("ch=%d: input RMS %.0f, decoded RMS %.0f, %d frames", channels, inRMS, outRMS, info.FrameCount)
		})
	}
}
