// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestWriteReadOpus(t *testing.T) {
	frames := synthFrames(5)
	// Opus at 48 kHz mono: four 20 ms packets (960 samples) and a short final one.
	durations := []uint32{960, 960, 960, 960, 480}
	var content int64
	for _, d := range durations {
		content += int64(d)
	}

	var buf memWS
	w, err := NewWriter(&buf, WriterConfig{
		Codec:               CodecOpus,
		SampleRate:          48000,
		Channels:            1,
		OpusPreSkip:         312,
		OpusInputSampleRate: 48000,
		MediaLength:         content,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, f := range frames {
		if err := w.WriteFrameDuration(f, durations[i]); err != nil {
			t.Fatalf("WriteFrameDuration %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.buf))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	info := r.Info()
	if info.Codec != CodecOpus {
		t.Errorf("Codec = %v, want Opus", info.Codec)
	}
	if info.SampleRate != 48000 || info.Channels != 1 {
		t.Errorf("format = %d Hz / %d ch, want 48000/1", info.SampleRate, info.Channels)
	}
	if info.FrameCount != len(frames) {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, len(frames))
	}
	if info.EncoderDelay != 312 {
		t.Errorf("EncoderDelay = %d, want 312 (the pre-skip)", info.EncoderDelay)
	}
	// CodecConfig is the dOps body: version 0, channels 1, pre-skip 312.
	if len(info.CodecConfig) < 11 || info.CodecConfig[1] != 1 ||
		int(info.CodecConfig[2])<<8|int(info.CodecConfig[3]) != 312 {
		t.Errorf("dOps CodecConfig = %x, want channels 1 pre-skip 312", info.CodecConfig)
	}
	assertFramesRoundTrip(t, r, frames)
}

func TestWriteReadFLAC(t *testing.T) {
	frames := synthFrames(4)
	// FLAC at 44100 mono: three 4096-sample blocks and a short final one.
	durations := []uint32{4096, 4096, 4096, 1000}

	streamInfo := make([]byte, 34)
	for i := range streamInfo {
		streamInfo[i] = byte(0x10 + i)
	}

	var buf memWS
	w, err := NewWriter(&buf, WriterConfig{
		Codec:      CodecFLAC,
		SampleRate: 44100,
		Channels:   1,
		STREAMINFO: streamInfo,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, f := range frames {
		if err := w.WriteFrameDuration(f, durations[i]); err != nil {
			t.Fatalf("WriteFrameDuration %d: %v", i, err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	r, err := NewReader(bytes.NewReader(buf.buf))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	info := r.Info()
	if info.Codec != CodecFLAC {
		t.Errorf("Codec = %v, want FLAC", info.Codec)
	}
	if info.SampleRate != 44100 || info.Channels != 1 {
		t.Errorf("format = %d Hz / %d ch, want 44100/1", info.SampleRate, info.Channels)
	}
	if info.FrameCount != len(frames) {
		t.Errorf("FrameCount = %d, want %d", info.FrameCount, len(frames))
	}
	// FLAC has no encoder priming, so the edit list media_time is 0.
	if info.EncoderDelay != 0 {
		t.Errorf("EncoderDelay = %d, want 0", info.EncoderDelay)
	}
	if !bytes.Equal(info.CodecConfig, streamInfo) {
		t.Errorf("CodecConfig = %x, want the STREAMINFO %x", info.CodecConfig, streamInfo)
	}
	assertFramesRoundTrip(t, r, frames)
}

// TestWriteFrameRejectsZeroDurationCodec confirms WriteFrame (the fixed-duration
// form) refuses Opus and FLAC, whose frames need explicit durations.
func TestWriteFrameRejectsZeroDurationCodec(t *testing.T) {
	var buf memWS
	w, err := NewWriter(&buf, WriterConfig{
		Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: 312,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrame([]byte{1, 2, 3}); err == nil {
		t.Error("WriteFrame on an Opus writer should error; use WriteFrameDuration")
	}
}

// TestFFprobeOpus confirms ffmpeg recognizes the Opus container the writer emits:
// codec, sample rate, and channel count are read from the Opus sample entry and
// dOps box in the moov, so the opaque synthetic packets do not affect stream
// identification.
func TestFFprobeOpus(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH")
	}

	frames := synthFrames(6)
	var buf memWS
	w, err := NewWriter(&buf, WriterConfig{
		Codec: CodecOpus, SampleRate: 48000, Channels: 2, OpusPreSkip: 312, OpusInputSampleRate: 48000,
	})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, f := range frames {
		if err := w.WriteFrameDuration(f, 960); err != nil {
			t.Fatalf("WriteFrameDuration: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	path := filepath.Join(t.TempDir(), "synth_opus.mp4")
	if err := os.WriteFile(path, buf.buf, 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe,
		"-v", "error", "-show_streams", "-print_format", "json", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v\n%s", err, stderr.Bytes())
	}
	var probe struct {
		Streams []struct {
			CodecName  string `json:"codec_name"`
			SampleRate string `json:"sample_rate"`
			Channels   int    `json:"channels"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parse ffprobe json: %v\n%s", err, out)
	}
	if len(probe.Streams) == 0 {
		t.Fatalf("ffprobe reported no streams:\n%s", out)
	}
	s := probe.Streams[0]
	if s.CodecName != "opus" {
		t.Errorf("codec_name = %q, want opus", s.CodecName)
	}
	if s.SampleRate != "48000" {
		t.Errorf("sample_rate = %q, want 48000", s.SampleRate)
	}
	if s.Channels != 2 {
		t.Errorf("channels = %d, want 2", s.Channels)
	}
	t.Logf("ffprobe: codec_name=%s sample_rate=%s channels=%d", s.CodecName, s.SampleRate, s.Channels)
}

// TestWriterCodecConfigValidation drives NewWriter into each codec-specific
// validation error the generalized validateConfig added.
func TestWriterCodecConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  WriterConfig
	}{
		{"opus wrong rate", WriterConfig{Codec: CodecOpus, SampleRate: 44100, Channels: 1}},
		{"opus negative preskip", WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: -1}},
		{"opus preskip overflows u16", WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusPreSkip: 70000}},
		{"opus input rate negative", WriterConfig{Codec: CodecOpus, SampleRate: 48000, Channels: 1, OpusInputSampleRate: -1}},
		{"flac missing streaminfo", WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1}},
		{"flac short streaminfo", WriterConfig{Codec: CodecFLAC, SampleRate: 44100, Channels: 1, STREAMINFO: make([]byte, 20)}},
		{"unknown codec", WriterConfig{Codec: Codec(99), SampleRate: 48000, Channels: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf memWS
			if _, err := NewWriter(&buf, tc.cfg); err == nil {
				t.Errorf("NewWriter(%+v) returned nil error, want a validation error", tc.cfg)
			}
		})
	}
}

func assertFramesRoundTrip(t *testing.T, r *Reader, want [][]byte) {
	t.Helper()
	got := collectFrames(t, r)
	if len(got) != len(want) {
		t.Fatalf("read %d frames, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("frame %d mismatch: got %x, want %x", i, got[i], want[i])
		}
	}
}
