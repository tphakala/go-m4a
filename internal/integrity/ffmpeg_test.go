// SPDX-License-Identifier: MIT

package integrity

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireFFmpegEnv names the variable that, set to any non-empty value, turns a
// missing ffmpeg from a skip into a failure. CI sets it on the Linux test step,
// where ffmpeg is installed, so the cross-checks cannot pass there by never
// running.
const requireFFmpegEnv = "M4A_REQUIRE_FFMPEG"

func lookFFmpeg(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("ffmpeg")
	if err != nil {
		if os.Getenv(requireFFmpegEnv) != "" {
			t.Fatalf("ffmpeg not on PATH but %s is set: %v", requireFFmpegEnv, err)
		}
		t.Skip("ffmpeg not on PATH")
	}
	return path
}

// runFFmpeg runs ffmpeg with args and returns its stdout.
func runFFmpeg(t *testing.T, ffmpeg string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, ffmpeg, append([]string{"-nostdin", "-hide_banner", "-v", "error"}, args...)...)
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("ffmpeg %v: %v\n%s", args, err, stderr.Bytes())
	}
	return stdout.Bytes()
}

func writeTemp(t *testing.T, name string, data []byte) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// pcmFormat is the ffmpeg raw format and codec matching the case's decoded
// sample width.
func (c corpusCase) pcmFormat() (format, codec string) {
	if c.bitDepth == 24 && c.lossless() {
		return "s24le", "pcm_s24le"
	}
	return "s16le", "pcm_s16le"
}

// TestFFmpegDecodesOurFiles has ffmpeg decode each file the writer produced.
// ffmpeg applies the edit list's media_time (the priming trim) itself. Whether it
// also cuts the last frame's trailing padding at the edit's end depends on the
// version (6.1 and 7.1 keep it, 9.0 cuts it), so the length may be anything from
// the source length to media_duration less media_time. The container fact is
// the start: the first source-length samples are bit-exact for FLAC and aligned
// at lag 0 for the lossy codecs.
func TestFFmpegDecodesOurFiles(t *testing.T) {
	ffmpeg := lookFFmpeg(t)
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			ours := runCase(t, c)
			in := writeTemp(t, "in.mp4", ours.file)
			format, codec := c.pcmFormat()
			out := runFFmpeg(t, ffmpeg, "-i", in, "-f", format, "-c:a", codec, "-")

			fb := c.frameBytes()
			most := int64(ours.rec.MediaDuration) - ours.rec.EditMediaTime
			if got := int64(len(out) / fb); len(out)%fb != 0 || got < int64(c.samples) || got > most {
				t.Fatalf("ffmpeg decoded %d bytes (%d samples per channel), want %d to %d: the source length up to media duration %d less media_time %d",
					len(out), got, c.samples, most, ours.rec.MediaDuration, ours.rec.EditMediaTime)
			}
			checkSignal(t, c, ours.src, out[:c.samples*fb])
		})
	}
}

// TestFFmpegRemuxReadsBack has ffmpeg stream-copy each file into its own MP4 and
// reads the result with go-m4a, which exercises the Reader on ffmpeg's layout (a
// 1000 movie timescale, free and udta boxes, an edit list on FLAC). The inner stream
// is untouched, so once each decode is trimmed by its own media_time and cut to
// the source length the two must be identical: any difference is a disagreement
// about where the audio starts. ffmpeg copies our media_time, so this does not
// check the writer's priming value; TestContainerIntegrity does.
func TestFFmpegRemuxReadsBack(t *testing.T) {
	ffmpeg := lookFFmpeg(t)
	for _, c := range corpus {
		t.Run(c.name, func(t *testing.T) {
			ours := runCase(t, c)
			in := writeTemp(t, "in.mp4", ours.file)
			remuxPath := filepath.Join(filepath.Dir(in), "remux.mp4")
			runFFmpeg(t, ffmpeg, "-i", in, "-c", "copy", "-map_metadata", "-1", "-f", "mp4", remuxPath)
			remux, err := os.ReadFile(remuxPath)
			if err != nil {
				t.Fatal(err)
			}

			f := inspect(t, remux)
			pcm, info := decode(t, c, remux)
			if info.EncoderDelay != ours.rec.EditMediaTime {
				t.Errorf("ffmpeg's remux has EncoderDelay %d, ours %d", info.EncoderDelay, ours.rec.EditMediaTime)
			}
			if info.FrameCount != ours.rec.FrameCount {
				t.Errorf("ffmpeg's remux has %d frames, ours %d", info.FrameCount, ours.rec.FrameCount)
			}

			// ffmpeg writes the edit in its own, usually coarser, movie timescale and
			// rounds it to a whole tick in either direction (7.1 turns a 9001-sample
			// FLAC edit into 9040 samples), and some versions (8.0) present the whole
			// media after media_time instead of our end. So the presented length may
			// run from one movie tick short of the source to one tick past the
			// remux's media duration less its start; the trim below uses the source
			// length either way.
			start, length := f.presentation()
			tol := int64(1)
			if f.movieTimescale > 0 {
				tol += (int64(f.mediaTimescale) + int64(f.movieTimescale) - 1) / int64(f.movieTimescale)
			}
			if most := int64(f.mediaDuration) - start; length < int64(c.samples)-tol || length > most+tol {
				t.Errorf("ffmpeg's remux presents %d samples, want %d to %d (each within %d of rounding)", length, c.samples, most, tol)
			}
			fb := int64(c.frameBytes())
			end := (start + int64(c.samples)) * fb
			if start < 0 || end > int64(len(pcm)) {
				t.Fatalf("remux decode has %d samples, too few for trim %d plus %d", int64(len(pcm))/fb, start, c.samples)
			}
			if !bytes.Equal(pcm[start*fb:end], ours.trimmed) {
				t.Error("trimmed decode of ffmpeg's remux differs from the trimmed decode of our file")
			}
		})
	}
}
