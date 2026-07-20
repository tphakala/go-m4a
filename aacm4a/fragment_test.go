// SPDX-License-Identifier: MIT

package aacm4a

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	aacpcm "github.com/tphakala/go-aac/pcm"
	m4a "github.com/tphakala/go-m4a"
)

// splitADTS returns the raw AAC-LC access units of a CRC-less ADTS stream, which
// is what the fragmented writer takes. It is the test-side equivalent of the
// package's own writeADTSFrames, kept separate so a bug in one cannot hide a bug
// in the other.
func splitADTS(t *testing.T, adts []byte) [][]byte {
	t.Helper()
	var aus [][]byte
	for i := 0; i < len(adts); {
		if i+adtsHeaderLen > len(adts) {
			t.Fatalf("truncated ADTS header at offset %d of %d", i, len(adts))
		}
		if adts[i] != 0xff || adts[i+1]&0xf6 != 0xf0 {
			t.Fatalf("no ADTS syncword at offset %d", i)
		}
		frameLen := (int(adts[i+3]&0x03) << 11) | (int(adts[i+4]) << 3) | (int(adts[i+5]) >> 5)
		if frameLen < adtsHeaderLen || i+frameLen > len(adts) {
			t.Fatalf("ADTS frame length %d at offset %d overruns %d bytes", frameLen, i, len(adts))
		}
		aus = append(aus, adts[i+adtsHeaderLen:i+frameLen])
		i += frameLen
	}
	return aus
}

// encodeAUs encodes pcm and returns the raw access units plus the config the
// container needs to describe them.
func encodeAUs(t *testing.T, cfg aacpcm.Config, pcm []byte) ([][]byte, m4a.WriterConfig) {
	t.Helper()
	var adts bytes.Buffer
	if err := aacpcm.EncodeInterleaved(&adts, cfg, pcm); err != nil {
		t.Fatalf("EncodeInterleaved: %v", err)
	}
	asc, err := audioSpecificConfig(cfg.SampleRate, cfg.Channels)
	if err != nil {
		t.Fatalf("audioSpecificConfig: %v", err)
	}
	return splitADTS(t, adts.Bytes()), m4a.WriterConfig{
		SampleRate: cfg.SampleRate,
		Channels:   cfg.Channels,
		ASC:        asc,
	}
}

// decodePCM runs ffmpeg over path and returns the decoded interleaved S16 PCM.
func decodePCM(t *testing.T, path string, channels int) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	out := filepath.Join(t.TempDir(), "out.raw")
	cmd := exec.CommandContext(ctx, "ffmpeg", "-v", "error",
		"-i", path, "-f", "s16le", "-ac", strconv.Itoa(channels), "-y", out)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg decode of %s failed: %v\n%s", path, err, stderr.Bytes())
	}
	pcm, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read decoded PCM: %v", err)
	}
	return pcm
}

// TestFragmentedMatchesNonFragmented is the end-to-end check that the fragmented
// writer describes the same audio as the non-fragmented one. Both mux the very
// same access units with the same priming trim, so ffmpeg must decode both to
// byte-identical PCM. It catches a wrong sample duration, a wrong data offset, a
// dropped or duplicated sample, and a mis-signalled edit list in one assertion.
func TestFragmentedMatchesNonFragmented(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
	}

	const (
		sampleRate = 48000
		channels   = 1
		seconds    = 3
	)
	cfg := aacpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Bitrate: 96000}
	pcm := chirpS16(sampleRate*seconds, channels, sampleRate)
	aus, wcfg := encodeAUs(t, cfg, pcm)
	if len(aus) < 100 {
		t.Fatalf("expected a few hundred access units, got %d", len(aus))
	}
	dir := t.TempDir()

	// The non-fragmented reference. No MediaLength, so it trims the priming and
	// nothing else, exactly like the fragmented path.
	plainPath := filepath.Join(dir, "plain.m4a")
	plain, err := os.Create(plainPath)
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Close on the way out as well as explicitly below. Several t.Fatalf sites sit
	// between here and that close, and on Windows t.TempDir's cleanup fails while a
	// handle is open, which would bury the real failure under an unrelated one.
	t.Cleanup(func() { _ = plain.Close() })
	w, err := m4a.NewWriter(plain, wcfg)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for _, au := range aus {
		if err := w.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := plain.Close(); err != nil {
		t.Fatalf("close plain: %v", err)
	}

	// The fragmented stream: an init segment plus roughly two-second segments,
	// cut on access-unit boundaries as an HLS packager would.
	init, err := m4a.InitSegment(wcfg)
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	fw, err := m4a.NewFragmentWriter(wcfg)
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	const segmentTarget = 2 * sampleRate // two seconds in the media timescale
	stream := append([]byte(nil), init...)
	var segmentDurations []uint64
	flush := func() {
		if fw.PendingSamples() == 0 {
			return
		}
		segmentDurations = append(segmentDurations, fw.PendingDuration())
		var err error
		stream, err = fw.AppendSegment(stream)
		if err != nil {
			t.Fatalf("AppendSegment: %v", err)
		}
	}
	for _, au := range aus {
		if err := fw.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		if fw.PendingDuration() >= segmentTarget {
			flush()
		}
	}
	flush()
	if len(segmentDurations) < 2 {
		t.Fatalf("expected several segments, got %d", len(segmentDurations))
	}

	fragPath := filepath.Join(dir, "fragmented.mp4")
	if err := os.WriteFile(fragPath, stream, 0o600); err != nil {
		t.Fatalf("write fragmented: %v", err)
	}

	// The decisive comparison.
	plainPCM := decodePCM(t, plainPath, channels)
	fragPCM := decodePCM(t, fragPath, channels)
	if len(plainPCM) == 0 {
		t.Fatal("non-fragmented file decoded to nothing")
	}
	if !bytes.Equal(plainPCM, fragPCM) {
		t.Fatalf("fragmented and non-fragmented decode differently: %d vs %d bytes",
			len(plainPCM), len(fragPCM))
	}

	// The durations reported for EXTINF must add up to the whole stream.
	var total uint64
	for _, d := range segmentDurations {
		total += d
	}
	if want := uint64(len(aus)) * 1024; total != want {
		t.Errorf("segment durations sum to %d, want %d", total, want)
	}
	if got := fw.BaseMediaDecodeTime(); got != total {
		t.Errorf("BaseMediaDecodeTime = %d, want %d", got, total)
	}
}

// TestFragmentedFFprobe checks ffprobe reads the stream as the track it claims to
// be, and finds every access unit.
func TestFragmentedFFprobe(t *testing.T) {
	ffprobe, err := exec.LookPath("ffprobe")
	if err != nil {
		t.Skip("ffprobe not found on PATH")
	}

	const sampleRate, channels = 48000, 2
	cfg := aacpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Bitrate: 128000}
	aus, wcfg := encodeAUs(t, cfg, chirpS16(sampleRate*2, channels, sampleRate))

	init, err := m4a.InitSegment(wcfg)
	if err != nil {
		t.Fatalf("InitSegment: %v", err)
	}
	fw, err := m4a.NewFragmentWriter(wcfg)
	if err != nil {
		t.Fatalf("NewFragmentWriter: %v", err)
	}
	stream := append([]byte(nil), init...)
	for i, au := range aus {
		if err := fw.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
		// Deliberately uneven segments, to prove the decode time keeps up.
		if (i+1)%37 == 0 {
			if stream, err = fw.AppendSegment(stream); err != nil {
				t.Fatalf("AppendSegment: %v", err)
			}
		}
	}
	if fw.PendingSamples() > 0 {
		if stream, err = fw.AppendSegment(stream); err != nil {
			t.Fatalf("AppendSegment: %v", err)
		}
	}

	path := filepath.Join(t.TempDir(), "fragmented.mp4")
	if err := os.WriteFile(path, stream, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, ffprobe, "-v", "error",
		"-show_format", "-show_streams", "-count_packets",
		"-print_format", "json", path)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("ffprobe failed: %v\n%s", err, stderr.Bytes())
	}

	var probe struct {
		Streams []struct {
			CodecName     string `json:"codec_name"`
			SampleRate    string `json:"sample_rate"`
			Channels      int    `json:"channels"`
			NbReadPackets string `json:"nb_read_packets"`
		} `json:"streams"`
		Format struct {
			FormatName string `json:"format_name"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("parse ffprobe JSON: %v\n%s", err, out)
	}
	if len(probe.Streams) != 1 {
		t.Fatalf("ffprobe found %d streams, want 1", len(probe.Streams))
	}
	s := probe.Streams[0]
	if s.CodecName != "aac" {
		t.Errorf("codec_name = %q, want %q", s.CodecName, "aac")
	}
	if s.SampleRate != strconv.Itoa(sampleRate) {
		t.Errorf("sample_rate = %q, want %d", s.SampleRate, sampleRate)
	}
	if s.Channels != channels {
		t.Errorf("channels = %d, want %d", s.Channels, channels)
	}
	if got := s.NbReadPackets; got != strconv.Itoa(len(aus)) {
		t.Errorf("nb_read_packets = %q, want %d", got, len(aus))
	}
	if !strings.Contains(probe.Format.FormatName, "mp4") {
		t.Errorf("format_name = %q, want an mp4 variant", probe.Format.FormatName)
	}
}

// TestFragmentedEditListTrimsPriming pins the behaviour the edit list exists for:
// with it, the decoded stream is shorter by exactly the encoder priming. ffmpeg's
// HLS fMP4 packager writes an edit list that trims nothing (media_time 0) and its
// -movflags +empty_moov path writes none at all, so this is the difference that
// would go unnoticed without an explicit check.
func TestFragmentedEditListTrimsPriming(t *testing.T) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not found on PATH")
	}

	const sampleRate, channels = 48000, 1
	cfg := aacpcm.Config{SampleRate: sampleRate, BitDepth: 16, Channels: channels, Bitrate: 96000}
	aus, wcfg := encodeAUs(t, cfg, chirpS16(sampleRate, channels, sampleRate))

	build := func(t *testing.T, encoderDelay int) []byte {
		t.Helper()
		c := wcfg
		c.EncoderDelay = encoderDelay
		init, err := m4a.InitSegment(c)
		if err != nil {
			t.Fatalf("InitSegment: %v", err)
		}
		fw, err := m4a.NewFragmentWriter(c)
		if err != nil {
			t.Fatalf("NewFragmentWriter: %v", err)
		}
		for _, au := range aus {
			if err := fw.WriteFrame(au); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
		}
		stream, err := fw.AppendSegment(append([]byte(nil), init...))
		if err != nil {
			t.Fatalf("AppendSegment: %v", err)
		}
		path := filepath.Join(t.TempDir(), fmt.Sprintf("delay%d.mp4", encoderDelay))
		if err := os.WriteFile(path, stream, 0o600); err != nil {
			t.Fatalf("write: %v", err)
		}
		return decodePCM(t, path, channels)
	}

	trimmed := build(t, 0)            // default AAC-LC priming
	untrimmed := build(t, m4a.NoEdit) // no edit list at all

	const bytesPerSample = 2 // S16 mono
	wantDelta := m4a.DefaultEncoderDelay * bytesPerSample
	if got := len(untrimmed) - len(trimmed); got != wantDelta {
		t.Fatalf("edit list trimmed %d bytes, want %d (%d priming samples)",
			got, wantDelta, m4a.DefaultEncoderDelay)
	}
	// And what remains is the tail of the untrimmed decode, not a shifted copy.
	if !bytes.Equal(trimmed, untrimmed[wantDelta:]) {
		t.Error("the trimmed decode is not the untrimmed one with the priming removed")
	}
}
