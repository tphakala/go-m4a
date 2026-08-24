// SPDX-License-Identifier: MIT

package opusm4a

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"testing"
	"time"

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

// genSineAt is genSine at an explicit source rate, so the tone stays a real 440 Hz
// sine at input rates other than 48 kHz.
func genSineAt(samplesPerCh, channels, rate int) []byte {
	out := make([]byte, 0, samplesPerCh*channels*2)
	for i := 0; i < samplesPerCh; i++ {
		v := int16(math.Round(18000 * math.Sin(2*math.Pi*440*float64(i)/float64(rate))))
		for c := 0; c < channels; c++ {
			out = binary.LittleEndian.AppendUint16(out, uint16(v))
		}
	}
	return out
}

// TestOpusRoundTripInputRates covers Opus source rates other than 48 kHz (issue
// #3). The container timescale stays 48 kHz, the edit list keeps its 312-sample
// pre-skip, the dOps InputSampleRate records the true source rate, and MediaLength
// (the presented duration) is scaled into the 48 kHz domain. Decode always runs at
// 48 kHz, so parity is checked on signal energy rather than byte length.
func TestOpusRoundTripInputRates(t *testing.T) {
	for _, rate := range []int{8000, 12000, 16000, 24000, 48000} {
		t.Run(fmt.Sprintf("%dHz", rate), func(t *testing.T) {
			const channels = 1
			samplesPerCh := rate / 2 // 0.5 s at the source rate
			pcm := genSineAt(samplesPerCh, channels, rate)

			var buf memWS
			if err := EncodeInterleaved(&buf, Config{SampleRate: rate, Channels: channels, Bitrate: 96000}, pcm); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}
			got, info, err := DecodeInterleaved(bytes.NewReader(buf.buf))
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			// The container timescale is fixed at 48 kHz regardless of input rate.
			if info.SampleRate != 48000 {
				t.Errorf("SampleRate = %d, want 48000 (the Opus container rate)", info.SampleRate)
			}
			if info.EncoderDelay != 312 {
				t.Errorf("EncoderDelay = %d, want 312", info.EncoderDelay)
			}
			// dOps InputSampleRate carries the true source rate (bytes 4..8 of the body).
			if len(info.CodecConfig) < 8 {
				t.Fatalf("dOps body is %d bytes, too short to hold InputSampleRate", len(info.CodecConfig))
			}
			if isr := binary.BigEndian.Uint32(info.CodecConfig[4:8]); isr != uint32(rate) {
				t.Errorf("dOps InputSampleRate = %d, want the source rate %d", isr, rate)
			}
			// The presented duration is the source length. In the 48 kHz timescale that
			// is samplesPerCh*48000/rate ticks; samplesPerCh is rate/2, so it is 24000
			// ticks (0.5 s) at every accepted rate.
			wantDur := time.Duration(samplesPerCh) * time.Second / time.Duration(rate)
			if diff := info.Duration - wantDur; diff < -time.Millisecond || diff > time.Millisecond {
				t.Errorf("Duration = %v, want ~%v (source length presented)", info.Duration, wantDur)
			}

			// Energy parity: trim the 48 kHz pre-skip, then compare RMS. A tone's
			// amplitude is rate-independent, so the source-rate input and the 48 kHz
			// decode carry the same energy even though their lengths differ.
			skip := int(info.EncoderDelay) * channels * 2
			if skip > len(got) {
				t.Fatalf("decoded %d bytes, fewer than the %d-byte pre-skip", len(got), skip)
			}
			content := got[skip:]
			inRMS, outRMS := rms(pcm), rms(content)
			if outRMS < 0.5*inRMS || outRMS > 1.5*inRMS {
				t.Errorf("decoded RMS %.0f not within 50%% of input RMS %.0f", outRMS, inRMS)
			}
			t.Logf("%dHz ch=%d: input RMS %.0f, decoded RMS %.0f, %d frames, dur %v", rate, channels, inRMS, outRMS, info.FrameCount, info.Duration)
		})
	}
}

// TestOpusRejectsUnsupportedInputRate pins that a rate outside the five Opus source
// rates is rejected rather than silently mis-framed.
func TestOpusRejectsUnsupportedInputRate(t *testing.T) {
	var buf memWS
	if err := EncodeInterleaved(&buf, Config{SampleRate: 44100, Channels: 1}, genSineAt(4410, 1, 44100)); err == nil {
		t.Fatal("EncodeInterleaved accepted 44100 Hz, want a rejection")
	}
}
