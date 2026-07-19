// SPDX-License-Identifier: MIT

package m4a

import (
	"os"
	"path/filepath"
	"testing"
)

// opusFlacFixtures are ffmpeg-produced Opus-in-MP4 and FLAC-in-MP4 files with
// their ffprobe-verified expected values. They exercise the reader's codec
// dispatch on the Opus and fLaC sample entries and the dOps and dfLa boxes.
var opusFlacFixtures = []struct {
	name       string
	codec      Codec
	sampleRate int
	channels   int
	frameCount int
	encDelay   int64 // elst media_time: Opus pre-skip 312; FLAC has none
	cfgLen     int   // expected CodecConfig length: dOps body 11, STREAMINFO 34
}{
	{"opus_mono48k.mp4", CodecOpus, 48000, 1, 51, 312, 11},
	{"opus_stereo48k.mp4", CodecOpus, 48000, 2, 51, 312, 11},
	{"flac_mono44k.mp4", CodecFLAC, 44100, 1, 10, 0, 34},
	{"flac_stereo48k.mp4", CodecFLAC, 48000, 2, 11, 0, 34},
}

func TestInteropOpusFLAC(t *testing.T) {
	for _, tc := range opusFlacFixtures {
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

			if info.Codec != tc.codec {
				t.Errorf("Codec = %v, want %v", info.Codec, tc.codec)
			}
			if info.SampleRate != tc.sampleRate {
				t.Errorf("SampleRate = %d, want %d", info.SampleRate, tc.sampleRate)
			}
			if info.Channels != tc.channels {
				t.Errorf("Channels = %d, want %d", info.Channels, tc.channels)
			}
			if info.FrameCount != tc.frameCount {
				t.Errorf("FrameCount = %d, want %d", info.FrameCount, tc.frameCount)
			}
			if info.EncoderDelay != tc.encDelay {
				t.Errorf("EncoderDelay = %d, want %d", info.EncoderDelay, tc.encDelay)
			}
			if len(info.CodecConfig) != tc.cfgLen {
				t.Errorf("CodecConfig length = %d, want %d", len(info.CodecConfig), tc.cfgLen)
			}
			// ASC is AAC-only; Opus and FLAC must not report one.
			if info.ASC != nil {
				t.Errorf("ASC = %x, want nil for %v", info.ASC, tc.codec)
			}
			if info.Duration <= 0 {
				t.Errorf("Duration = %v, want positive", info.Duration)
			}

			// Every access unit must extract at its recorded (offset, size).
			frames := collectFrames(t, r)
			if len(frames) != tc.frameCount {
				t.Fatalf("read %d frames, want %d", len(frames), tc.frameCount)
			}
			for i, au := range frames {
				if len(au) == 0 {
					t.Errorf("frame %d is empty", i)
				}
			}
		})
	}
}

// TestOpusCodecConfigIsDops confirms the reader hands back the dOps body verbatim,
// with the pre-skip readable from it, so an opusm4a bridge can recover the
// OpusSpecificBox fields.
func TestOpusCodecConfigIsDops(t *testing.T) {
	path := filepath.Join("testdata", "interop", "opus_mono48k.mp4")
	f, err := os.Open(path) //nolint:gosec // fixed test fixture path
	if err != nil {
		t.Skipf("fixture missing: %v", err)
	}
	defer func() { _ = f.Close() }()

	r, err := NewReader(f)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	cfg := r.Info().CodecConfig
	if len(cfg) < 11 {
		t.Fatalf("dOps body length %d, want at least 11", len(cfg))
	}
	// dOps body: version(1) channels(1) preSkip(2 BE) inputSampleRate(4 BE) ...
	if cfg[0] != 0 {
		t.Errorf("dOps version = %d, want 0", cfg[0])
	}
	if cfg[1] != 1 {
		t.Errorf("dOps OutputChannelCount = %d, want 1", cfg[1])
	}
	preSkip := int(cfg[2])<<8 | int(cfg[3])
	if preSkip != 312 {
		t.Errorf("dOps PreSkip = %d, want 312", preSkip)
	}
}
