// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"syscall"
	"testing"
)

// failingReadSeeker passes reads and seeks through while err and seekErr are
// nil. Once err is set, each Read copies up to n bytes and then returns err;
// once seekErr is set, each Seek returns it. A test sets them after NewReader
// succeeds, so only the first frame read fails.
type failingReadSeeker struct {
	io.ReadSeeker

	n       int
	err     error
	seekErr error
}

func (f *failingReadSeeker) Seek(off int64, whence int) (int64, error) {
	if f.seekErr != nil {
		return 0, f.seekErr
	}
	return f.ReadSeeker.Seek(off, whence)
}

func (f *failingReadSeeker) Read(p []byte) (int, error) {
	if f.err == nil {
		return f.ReadSeeker.Read(p)
	}
	n, err := f.ReadSeeker.Read(p[:min(f.n, len(p))])
	if err != nil {
		return n, err
	}
	return n, f.err
}

// TestReadFrameErrorKeepsCause pins how a frame read that fails after NewReader
// accepted the file is reported, through every public frame path. An error from
// the underlying reader keeps its cause and is not ErrCorrupt; any end of file
// (bare, wrapped, after a partial read, or from the frame Seek) is
// io.ErrUnexpectedEOF. No failure may match io.EOF, which every consumer takes
// as the clean end of frames.
func TestReadFrameErrorKeepsCause(t *testing.T) {
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(3))

	faults := []struct {
		name    string
		n       int
		err     error
		seekErr error
		want    error
	}{
		{name: "io error", err: &fs.PathError{Op: "read", Path: "clip.m4a", Err: syscall.EIO}, want: syscall.EIO},
		{name: "bare EOF", err: io.EOF, want: io.ErrUnexpectedEOF},
		{name: "wrapped EOF", err: fmt.Errorf("source: %w", io.EOF), want: io.ErrUnexpectedEOF},
		// Every synthFrames frame is longer than one byte, so one byte then
		// io.EOF is a partial read that io.ReadFull reports as
		// io.ErrUnexpectedEOF.
		{name: "short read", n: 1, err: io.EOF, want: io.ErrUnexpectedEOF},
		{name: "seek EOF", seekErr: fmt.Errorf("source: %w", io.EOF), want: io.ErrUnexpectedEOF},
	}
	apis := []struct {
		name string
		read func(*Reader) error
	}{
		{name: "ReadFrame", read: func(rd *Reader) error {
			_, err := rd.ReadFrame()
			return err
		}},
		{name: "ReadFrameInto", read: func(rd *Reader) error {
			_, err := rd.ReadFrameInto(make([]byte, 256))
			return err
		}},
		{name: "RawStream", read: func(rd *Reader) error {
			// io.ReadAll treats only io.EOF as success, so a nil error here would
			// mean the failure ended the stream as if it were clean.
			_, err := io.ReadAll(rd.RawStream())
			return err
		}},
	}

	for _, fc := range faults {
		for _, api := range apis {
			t.Run(fc.name+"/"+api.name, func(t *testing.T) {
				src := &failingReadSeeker{ReadSeeker: bytes.NewReader(data), n: fc.n}
				rd, err := NewReader(src)
				if err != nil {
					t.Fatalf("NewReader: %v", err)
				}
				src.err, src.seekErr = fc.err, fc.seekErr

				err = api.read(rd)
				if err == nil {
					t.Fatal("read succeeded, want an error")
				}
				if errors.Is(err, io.EOF) {
					t.Errorf("error %q matches io.EOF, which reads as the clean end of frames", err)
				}
				if errors.Is(err, ErrCorrupt) {
					t.Errorf("error %q matches ErrCorrupt, but the container is not at fault", err)
				}
				if !errors.Is(err, fc.want) {
					t.Errorf("error %q does not match %v", err, fc.want)
				}
				if _, injected := errors.AsType[*fs.PathError](fc.err); injected {
					if _, ok := errors.AsType[*fs.PathError](err); !ok {
						t.Errorf("error %q lost the *fs.PathError cause", err)
					}
				}
			})
		}
	}
}

// shrinkingReadSeeker serves a stream that ends at cut once cut is set, as a
// file truncated after NewReader would.
type shrinkingReadSeeker struct {
	*bytes.Reader

	cut int64
}

func (s *shrinkingReadSeeker) Read(p []byte) (int, error) {
	if s.cut > 0 {
		pos, err := s.Seek(0, io.SeekCurrent)
		if err != nil {
			return 0, err
		}
		if pos >= s.cut {
			return 0, io.EOF
		}
		p = p[:min(int64(len(p)), s.cut-pos)]
	}
	return s.Reader.Read(p)
}

// TestFrameRefillFailure pins a source failure that meets the read-ahead
// window. A 60-byte window holds frames 0 to 2, so after the fault frames 1 and
// 2 still come from the window and frame 3 is the first read that needs the
// source. It must report the failure with the frame-read classification, and
// once the fault clears, rereading from the start must return the stream's
// bytes: a failed refill may have overwritten the window's buffer and must not
// leave the window claiming its old range. Frame 5 (72 bytes) is larger than the
// window, so a fault injected after frame 4 meets the direct read instead.
func TestFrameRefillFailure(t *testing.T) {
	frames := synthFrames(10) // 7, 20, 33, 46, ... bytes
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	const window = 60 // frames 0 to 2

	cases := []struct {
		name string
		// fault builds the source for frame 3 at off3 and returns it with a
		// func that breaks it and one that repairs it.
		fault func(data []byte, off3 int64) (io.ReadSeeker, func(), func())
		// breakAfter is the frame after whose read the fault is injected.
		breakAfter int
		failAt     int
		want       error
		pathError  bool
	}{
		{
			name: "io error after a partial refill", failAt: 3, want: syscall.EIO, pathError: true,
			fault: func(data []byte, _ int64) (io.ReadSeeker, func(), func()) {
				src := &failingReadSeeker{ReadSeeker: bytes.NewReader(data), n: 5}
				return src,
					func() { src.err = &fs.PathError{Op: "read", Path: "clip.m4a", Err: syscall.EIO} },
					func() { src.err = nil }
			},
		},
		{
			name: "EOF at refill", failAt: 3, want: io.ErrUnexpectedEOF,
			fault: func(data []byte, _ int64) (io.ReadSeeker, func(), func()) {
				src := &failingReadSeeker{ReadSeeker: bytes.NewReader(data)}
				return src, func() { src.err = io.EOF }, func() { src.err = nil }
			},
		},
		{
			name: "stream cut inside frame 3", failAt: 3, want: io.ErrUnexpectedEOF,
			fault: func(data []byte, off3 int64) (io.ReadSeeker, func(), func()) {
				src := &shrinkingReadSeeker{Reader: bytes.NewReader(data)}
				return src, func() { src.cut = off3 + 10 }, func() { src.cut = 0 }
			},
		},
		{
			// The refill needs only frame 3's bytes to succeed, not a full window.
			name: "stream cut at frame 3's end", failAt: 4, want: io.ErrUnexpectedEOF,
			fault: func(data []byte, off3 int64) (io.ReadSeeker, func(), func()) {
				src := &shrinkingReadSeeker{Reader: bytes.NewReader(data)}
				return src, func() { src.cut = off3 + int64(len(frames[3])) }, func() { src.cut = 0 }
			},
		},
		{
			name: "io error on a frame larger than the window", breakAfter: 4, failAt: 5,
			want: syscall.EIO, pathError: true,
			fault: func(data []byte, _ int64) (io.ReadSeeker, func(), func()) {
				src := &failingReadSeeker{ReadSeeker: bytes.NewReader(data), n: 5}
				return src,
					func() { src.err = &fs.PathError{Op: "read", Path: "clip.m4a", Err: syscall.EIO} },
					func() { src.err = nil }
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe := mustReader(t, data)
			if err := probe.SeekSample(3); err != nil {
				t.Fatal(err)
			}
			src, breakSrc, fixSrc := tc.fault(data, probe.curOffset)
			rd, err := NewReader(src)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			rd.winBuf = make([]byte, window)

			for i := range tc.failAt + 1 {
				_, err := rd.ReadFrame()
				if i == tc.breakAfter {
					breakSrc()
				}
				if i < tc.failAt {
					if err != nil {
						t.Fatalf("frame %d: %v, want it served before the failure", i, err)
					}
					continue
				}
				if err == nil {
					t.Fatalf("frame %d read succeeded, want an error", i)
				}
				if errors.Is(err, io.EOF) || errors.Is(err, ErrCorrupt) {
					t.Errorf("frame %d error %q matches io.EOF or ErrCorrupt", i, err)
				}
				if !errors.Is(err, tc.want) {
					t.Errorf("frame %d error %q does not match %v", i, err, tc.want)
				}
				if _, ok := errors.AsType[*fs.PathError](err); tc.pathError && !ok {
					t.Errorf("frame %d error %q lost the *fs.PathError cause", i, err)
				}
			}

			fixSrc()
			if err := rd.SeekSample(0); err != nil {
				t.Fatal(err)
			}
			drainMatchesStream(t, rd, data)
		})
	}
}
