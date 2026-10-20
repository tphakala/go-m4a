// SPDX-License-Identifier: MIT

package m4a

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strings"
	"syscall"
	"testing"
)

// failingReadSeeker passes reads and seeks through while err and seekErr are
// nil. Once err is set, each Read copies up to n bytes and then returns err;
// once seekErr is set, each Seek returns it. A test that sets them after
// NewReader succeeds makes only frame reads meet them; one that sets them at
// construction makes the open-time reads meet them too.
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

// shortSource reports a stream length its data does not back: a Seek from the
// end lands relative to claim, so NewReader sees a stream of claim bytes while a
// Read past the data returns io.EOF, as a file that shrank or was cut short.
type shortSource struct {
	*bytes.Reader

	claim int64
}

func (s *shortSource) Seek(off int64, whence int) (int64, error) {
	if whence == io.SeekEnd {
		return s.Reader.Seek(s.claim+off, io.SeekStart)
	}
	return s.Reader.Seek(off, whence)
}

// seekFailer passes the Seek from the end that NewReader uses to measure the
// stream and fails every other Seek with err, so the failure meets the open
// window's first refill.
type seekFailer struct {
	io.ReadSeeker

	err error
}

func (s *seekFailer) Seek(off int64, whence int) (int64, error) {
	if whence != io.SeekEnd {
		return 0, s.err
	}
	return s.ReadSeeker.Seek(off, whence)
}

// TestOpenErrorKeepsCause pins how NewReader reports a source that fails while it
// reads the first box header: the cause stays in the chain, the failure is not
// ErrCorrupt, and the message names the header read, for a Seek failure and for a
// Read failure alike.
func TestOpenErrorKeepsCause(t *testing.T) {
	eio := &fs.PathError{Op: "read", Path: "clip.m4a", Err: syscall.EIO}
	fixtures := map[string][]byte{
		"plain": plainFixture(t),
		"fragmented": buildFragmentedStream(t, aacFragmentConfig(), [][]fragAU{
			uniformSegment(synthFrames(3)),
		}),
	}
	sources := map[string]func(data []byte) io.ReadSeeker{
		"seek fails": func(data []byte) io.ReadSeeker {
			return &seekFailer{ReadSeeker: bytes.NewReader(data), err: eio}
		},
		"read fails": func(data []byte) io.ReadSeeker {
			return &failingReadSeeker{ReadSeeker: bytes.NewReader(data), err: eio}
		},
	}
	for fname, data := range fixtures {
		for sname, source := range sources {
			t.Run(fname+"/"+sname, func(t *testing.T) {
				_, err := NewReader(source(data))
				if err == nil {
					t.Fatal("NewReader succeeded, want an error")
				}
				if !errors.Is(err, syscall.EIO) {
					t.Errorf("error %q does not match %v", err, syscall.EIO)
				}
				if _, ok := errors.AsType[*fs.PathError](err); !ok {
					t.Errorf("error %q lost the *fs.PathError cause", err)
				}
				if errors.Is(err, ErrCorrupt) {
					t.Errorf("error %q matches ErrCorrupt, but the container is not at fault", err)
				}
				if want := "go-m4a: read box header at 0: "; !strings.HasPrefix(err.Error(), want) {
					t.Errorf("error %q does not start with %q", err, want)
				}
			})
		}
	}
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

// TestFrameRefillFailure pins a source failure that meets the read-ahead
// window. A 120-byte window holds frames 0 to 3, so after a fault injected
// after frame 0, frames 1 to 3 still come from the window and frame 4 is the
// first read that needs the source. It must report the failure with the
// frame-read classification, and once the fault clears, rereading from the start
// must return the stream's bytes: a failed refill may have overwritten the
// window and must not leave it claiming its old range. Frame 5 (72 bytes) is
// larger than half the window, so it is read directly, and a fault injected after
// frame 4 meets that read instead.
func TestFrameRefillFailure(t *testing.T) {
	frames := synthFrames(10) // 7, 20, 33, 46, 59, 72, ... bytes
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, frames)
	const window = 120
	probe := mustReader(t, data)
	if err := probe.SeekSample(4); err != nil {
		t.Fatal(err)
	}
	off4 := probe.curOffset

	eio := &fs.PathError{Op: "read", Path: "clip.m4a", Err: syscall.EIO}
	cases := []struct {
		name string
		// The fault: Reads return n bytes and then err, or the stream ends cut
		// bytes past frame 4's offset (0 leaves it whole).
		n          int
		err        error
		cut        int64
		breakAfter int // frame after whose read the fault starts
		failAt     int
		want       error
	}{
		{name: "io error after a partial refill", n: 5, err: eio, failAt: 4, want: syscall.EIO},
		{name: "EOF at refill", err: io.EOF, failAt: 4, want: io.ErrUnexpectedEOF},
		{name: "stream cut inside frame 4", cut: 10, failAt: 4, want: io.ErrUnexpectedEOF},
		// The refill needs only frame 4's bytes to succeed, not a full window.
		{name: "stream cut at frame 4's end", cut: int64(len(frames[4])), failAt: 5, want: io.ErrUnexpectedEOF},
		{name: "io error on a direct read", n: 5, err: eio, breakAfter: 4, failAt: 5, want: syscall.EIO},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := &failingReadSeeker{ReadSeeker: bytes.NewReader(data), n: tc.n}
			rd, err := NewReader(src)
			if err != nil {
				t.Fatalf("NewReader: %v", err)
			}
			rd.win.buf = make([]byte, 0, window)

			for i := range tc.failAt {
				if _, err := rd.ReadFrame(); err != nil {
					t.Fatalf("frame %d: %v, want it served before the failure", i, err)
				}
				if i == tc.breakAfter {
					src.err = tc.err
					if tc.cut > 0 {
						src.ReadSeeker = bytes.NewReader(data[:off4+tc.cut])
					}
				}
			}
			_, err = rd.ReadFrame()
			if err == nil {
				t.Fatalf("frame %d read succeeded, want an error", tc.failAt)
			}
			if errors.Is(err, io.EOF) || errors.Is(err, ErrCorrupt) {
				t.Errorf("frame %d error %q matches io.EOF or ErrCorrupt", tc.failAt, err)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("frame %d error %q does not match %v", tc.failAt, err, tc.want)
			}
			if _, injected := errors.AsType[*fs.PathError](tc.err); injected {
				if _, ok := errors.AsType[*fs.PathError](err); !ok {
					t.Errorf("frame %d error %q lost the *fs.PathError cause", tc.failAt, err)
				}
			}

			src.err, src.ReadSeeker = nil, bytes.NewReader(data)
			if err := rd.SeekSample(0); err != nil {
				t.Fatal(err)
			}
			drainMatchesStream(t, rd, data, nil)
		})
	}
}
