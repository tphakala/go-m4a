// SPDX-License-Identifier: MIT

package flacm4a

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	m4a "github.com/tphakala/go-m4a"
	"github.com/tphakala/go-m4a/internal/testutil"
)

// genS16 builds interleaved little-endian 16-bit PCM: a per-channel sine at a
// distinct frequency.
func genS16(samplesPerCh, channels int) []byte {
	out := make([]byte, 0, samplesPerCh*channels*2)
	for i := range samplesPerCh {
		for c := range channels {
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
		{"5.1_48k", 48000, 6, 8000},
		{"7.1_48k", 48000, 8, 4000},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pcm := genS16(tc.samplesPerCh, tc.channels)

			var buf testutil.WriteSeeker
			cfg := Config{SampleRate: tc.sampleRate, Channels: tc.channels, BitDepth: 16, CompressionLevel: 5}
			if err := EncodeInterleaved(&buf, cfg, pcm); err != nil {
				t.Fatalf("EncodeInterleaved: %v", err)
			}

			got, info, err := DecodeInterleaved(bytes.NewReader(buf.Bytes()))
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
		{"zero channels", Config{SampleRate: 44100, Channels: 0, BitDepth: 16}, make([]byte, 12)},
		{"too many channels", Config{SampleRate: 44100, Channels: 9, BitDepth: 16}, make([]byte, 36)},
		// A non-byte-aligned bit depth would compute the wrong stride and mis-parse.
		{"non-byte-aligned bit depth", Config{SampleRate: 44100, Channels: 1, BitDepth: 20}, make([]byte, 12)},
		{"partial trailing sample", Config{SampleRate: 44100, Channels: 2, BitDepth: 16}, make([]byte, 6)}, // stride 4, 6 bytes
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf testutil.WriteSeeker
			if err := EncodeInterleaved(&buf, tc.cfg, tc.pcm); err == nil {
				t.Errorf("EncodeInterleaved(%+v) returned nil error, want a validation error", tc.cfg)
			}
		})
	}
}
