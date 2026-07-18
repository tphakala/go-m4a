// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"testing"
)

// errInjectedWrite and errInjectedSeek are the sentinels the failing
// io.WriteSeekers below return, so a test can match the exact injected failure
// through the Writer's wrapping.
var (
	errInjectedWrite = errors.New("injected write failure")
	errInjectedSeek  = errors.New("injected seek failure")
)

// failWS is an in-memory io.WriteSeeker whose Write returns errInjectedWrite once
// fail is set. NewWriter's header writes run before the test arms it, so the
// failure lands on a WriteFrame and the Writer's sticky-error latch can be
// observed.
type failWS struct {
	buf  []byte
	pos  int64
	fail bool
}

func (f *failWS) Write(p []byte) (int, error) {
	if f.fail {
		return 0, errInjectedWrite
	}
	end := f.pos + int64(len(p))
	if end > int64(len(f.buf)) {
		grown := make([]byte, end)
		copy(grown, f.buf)
		f.buf = grown
	}
	copy(f.buf[f.pos:end], p)
	f.pos = end
	return len(p), nil
}

func (f *failWS) Seek(offset int64, whence int) (int64, error) {
	abs, err := seekAbs(f.pos, int64(len(f.buf)), offset, whence)
	if err != nil {
		return 0, err
	}
	f.pos = abs
	return abs, nil
}

// seekFailWS is an in-memory io.WriteSeeker whose next Seek fails once when
// failNextSeek is set, letting a test drive a transient Close failure and then a
// successful retry.
type seekFailWS struct {
	buf          []byte
	pos          int64
	failNextSeek bool
}

func (s *seekFailWS) Write(p []byte) (int, error) {
	end := s.pos + int64(len(p))
	if end > int64(len(s.buf)) {
		grown := make([]byte, end)
		copy(grown, s.buf)
		s.buf = grown
	}
	copy(s.buf[s.pos:end], p)
	s.pos = end
	return len(p), nil
}

func (s *seekFailWS) Seek(offset int64, whence int) (int64, error) {
	if s.failNextSeek {
		s.failNextSeek = false
		return 0, errInjectedSeek
	}
	abs, err := seekAbs(s.pos, int64(len(s.buf)), offset, whence)
	if err != nil {
		return 0, err
	}
	s.pos = abs
	return abs, nil
}

// seekAbs resolves a Seek target against the current position and length, the
// shared arithmetic behind the two failing writers' Seek methods.
func seekAbs(pos, length, offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = pos + offset
	case io.SeekEnd:
		abs = length + offset
	default:
		return 0, fmt.Errorf("bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("negative position %d", abs)
	}
	return abs, nil
}

// TestWriterStickyError proves the Writer latches a failed frame write: once a
// WriteFrame's underlying Write fails, that same error must be returned by every
// later WriteFrame and by Close, so the sample table can never disagree with the
// bytes on disk.
func TestWriterStickyError(t *testing.T) {
	fw := &failWS{}
	w, err := NewWriter(fw, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	fw.fail = true // arm the failure for the first frame write
	frame := []byte{1, 2, 3}

	err1 := w.WriteFrame(frame)
	if !errors.Is(err1, errInjectedWrite) {
		t.Fatalf("first WriteFrame error = %v, want wrapped errInjectedWrite", err1)
	}

	err2 := w.WriteFrame(frame)
	if !errors.Is(err2, errInjectedWrite) {
		t.Fatalf("second WriteFrame error = %v, want the latched error", err2)
	}

	err3 := w.Close()
	if !errors.Is(err3, errInjectedWrite) {
		t.Fatalf("Close error = %v, want the latched error", err3)
	}
}

// TestWriterDoubleCloseAfterSuccess proves a clean Close followed by a second
// Close returns ErrClosed rather than re-finalizing or panicking. It uses failWS
// unarmed so the successful path runs over the same writer the sticky-error test
// exercises.
func TestWriterDoubleCloseAfterSuccess(t *testing.T) {
	fw := &failWS{}
	w, err := NewWriter(fw, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteFrame([]byte{1, 2, 3}); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := w.Close(); !errors.Is(err, ErrClosed) {
		t.Errorf("second Close = %v, want ErrClosed", err)
	}
}

// TestWriterCloseSeekRetry proves a transient Seek failure in Close is
// recoverable: finalized is not set, so a later Close retries and produces a
// valid file. The mdat largesize seek fails once, then the retry patches the
// header and writes moov, and the reparsed file reads back every frame.
func TestWriterCloseSeekRetry(t *testing.T) {
	frames := synthFrames(3)
	s := &seekFailWS{}
	w, err := NewWriter(s, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k})
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	for i, au := range frames {
		if err := w.WriteFrame(au); err != nil {
			t.Fatalf("WriteFrame %d: %v", i, err)
		}
	}

	s.failNextSeek = true
	if err := w.Close(); !errors.Is(err, errInjectedSeek) {
		t.Fatalf("Close with a failing seek = %v, want wrapped errInjectedSeek", err)
	}

	// The retry seeks succeed, so Close finalizes cleanly.
	if err := w.Close(); err != nil {
		t.Fatalf("Close retry: %v", err)
	}

	r, err := NewReader(bytes.NewReader(s.buf))
	if err != nil {
		t.Fatalf("NewReader on the retried file: %v", err)
	}
	if fc := r.Info().FrameCount; fc != len(frames) {
		t.Errorf("FrameCount = %d, want %d", fc, len(frames))
	}
	got := collectFrames(t, r)
	if len(got) != len(frames) {
		t.Fatalf("read %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Errorf("frame %d = % x, want % x", i, got[i], frames[i])
		}
	}
}

// TestWriterEditListClamp covers the delay-larger-than-media clamp: with a single
// frame (1024 media samples) and an EncoderDelay of 5000, the post-priming length
// is negative and must clamp to a zero segment_duration rather than wrap the
// unsigned elst field. The reparsed Duration is a sane zero and EncoderDelay is
// the raw media_time.
func TestWriterEditListClamp(t *testing.T) {
	const delay = 5000
	cfg := WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, EncoderDelay: delay}
	data := writeM4A(t, cfg, synthFrames(1))

	seg, mt, _ := readElst(t, data)
	if seg != 0 {
		t.Errorf("elst segment_duration = %d, want 0 (clamped)", seg)
	}
	if mt != delay {
		t.Errorf("elst media_time = %d, want %d", mt, delay)
	}

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	info := r.Info()
	if info.Duration != 0 {
		t.Errorf("Duration = %v, want 0", info.Duration)
	}
	if info.EncoderDelay != delay {
		t.Errorf("EncoderDelay = %d, want %d", info.EncoderDelay, delay)
	}
}

// TestWriterBrandOverride confirms WriterConfig.Brand overrides the ftyp major
// brand and that the reader surfaces it.
func TestWriterBrandOverride(t *testing.T) {
	const wantBrand = "mp42"
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, Brand: wantBrand}, synthFrames(2))

	ftyp := boxBody(t, data, "ftyp")
	if got := string(ftyp[:4]); got != wantBrand {
		t.Errorf("ftyp major brand = %q, want %q", got, wantBrand)
	}

	r, err := NewReader(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	if b := r.Info().Brand; b != wantBrand {
		t.Errorf("Info().Brand = %q, want %q", b, wantBrand)
	}
}

// TestWriterBrandWrongLength confirms a brand that is not exactly four bytes is
// rejected by NewWriter.
func TestWriterBrandWrongLength(t *testing.T) {
	_, err := NewWriter(&memWS{}, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k, Brand: "qt"})
	if err == nil {
		t.Fatal("NewWriter accepted a 2-byte brand, want error")
	}
}

// TestValidateConfigBranches exercises the config-validation branches the round
// trip tests do not reach: an ASC whose audio object type is not AAC-LC, a
// supported AAC rate that still overflows the mp4a 16.16 samplerate field, and an
// explicit-rate (index 15) ASC.
func TestValidateConfigBranches(t *testing.T) {
	tests := []struct {
		name string
		cfg  WriterConfig
	}{
		{
			// ASC {0x29,0x88}: audio object type 5 (not AAC-LC 2).
			name: "object type not aac-lc",
			cfg:  WriterConfig{SampleRate: 48000, Channels: 1, ASC: []byte{0x29, 0x88}},
		},
		{
			// 96000 Hz is a valid AAC rate but exceeds the 16.16 samplerate field.
			name: "rate overflows mp4a samplerate field",
			cfg:  WriterConfig{SampleRate: 96000, Channels: 1, ASC: ascMono48k},
		},
		{
			// ASC {0x17,0x88}: samplingFrequencyIndex 15 (explicit rate, out of scope).
			name: "sampling frequency index 15",
			cfg:  WriterConfig{SampleRate: 48000, Channels: 1, ASC: []byte{0x17, 0x88}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewWriter(&memWS{}, tc.cfg); err == nil {
				t.Fatal("NewWriter succeeded, want a validation error")
			}
		})
	}
}
