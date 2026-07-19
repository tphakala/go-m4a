// SPDX-License-Identifier: MIT

package flacm4a

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

// genS16 builds interleaved little-endian 16-bit PCM: a per-channel sine at a
// distinct frequency.
func genS16(samplesPerCh, channels int) []byte {
	out := make([]byte, 0, samplesPerCh*channels*2)
	for i := 0; i < samplesPerCh; i++ {
		for c := 0; c < channels; c++ {
			v := int16(math.Round(20000 * math.Sin(float64(i)*(0.02+0.005*float64(c)))))
			out = binary.LittleEndian.AppendUint16(out, uint16(v))
		}
	}
	return out
}

func TestFLACRoundTrip(t *testing.T) {
	cases := []struct {
		name         string
		sampleRate   int
		channels     int
		samplesPerCh int
	}{
		{"mono44k", 44100, 1, 9000},
		{"stereo48k", 48000, 2, 12000},
		{"mono48k_short", 48000, 1, 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := genS16(tc.samplesPerCh, tc.channels)

			var buf memWS
			cfg := Config{SampleRate: tc.sampleRate, Channels: tc.channels, BitDepth: 16, CompressionLevel: 5}
			if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}

			got, info, err := DecodeInterleaved(bytes.NewReader(buf.buf))
			if err != nil {
				t.Fatalf("DecodeInterleaved: %v", err)
			}
			if info.Codec != m4a.CodecFLAC {
				t.Errorf("Codec = %v, want FLAC", info.Codec)
			}
			if info.SampleRate != tc.sampleRate || info.Channels != tc.channels {
				t.Errorf("format = %d Hz / %d ch, want %d/%d", info.SampleRate, info.Channels, tc.sampleRate, tc.channels)
			}
			// FLAC is lossless, so the decode must be byte-identical to the input.
			if !bytes.Equal(got, pcm) {
				t.Errorf("round-trip PCM mismatch: got %d bytes, want %d bytes", len(got), len(pcm))
			}
		})
	}
}

// TestFLACEncodeErrors covers the config guards that must reject bad input before
// go-flac sees it.
func TestFLACEncodeErrors(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		pcm  []byte
	}{
		{"bad channels", Config{SampleRate: 44100, Channels: 3, BitDepth: 16}, make([]byte, 12)},
		// A non-byte-aligned bit depth would compute the wrong stride and mis-parse.
		{"non-byte-aligned bit depth", Config{SampleRate: 44100, Channels: 1, BitDepth: 20}, make([]byte, 12)},
		{"partial trailing sample", Config{SampleRate: 44100, Channels: 2, BitDepth: 16}, make([]byte, 6)}, // stride 4, 6 bytes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf memWS
			if err := EncodeInterleaved(&buf, tc.cfg, tc.pcm); err == nil {
				t.Errorf("EncodeInterleaved(%+v) returned nil error, want a validation error", tc.cfg)
			}
		})
	}
}
