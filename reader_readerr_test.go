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

// failingReadSeeker passes Seek and Read through to r until armed. Once armed,
// each Read copies up to n bytes from r and then returns err, so a test can let
// NewReader succeed and fail the first frame read.
type failingReadSeeker struct {
	r     io.ReadSeeker
	armed bool
	n     int
	err   error
}

func (f *failingReadSeeker) Seek(off int64, whence int) (int64, error) {
	return f.r.Seek(off, whence)
}

func (f *failingReadSeeker) Read(p []byte) (int, error) {
	if !f.armed {
		return f.r.Read(p)
	}
	n := 0
	if f.n > 0 {
		var err error
		if n, err = f.r.Read(p[:min(f.n, len(p))]); err != nil {
			return n, err
		}
	}
	return n, f.err
}

// TestReadFrameErrorKeepsCause pins how a frame read that fails after NewReader
// accepted the file is reported, through every public frame path. An error from
// the underlying reader keeps its cause and is not ErrCorrupt; any end of file
// (bare, wrapped, or after a partial read) is io.ErrUnexpectedEOF. No failure may
// match io.EOF, which every consumer takes as the clean end of frames.
func TestReadFrameErrorKeepsCause(t *testing.T) {
	data := writeM4A(t, WriterConfig{SampleRate: 48000, Channels: 1, ASC: ascMono48k}, synthFrames(3))

	faults := []struct {
		name   string
		n      int
		err    error
		want   error
		asPath bool
	}{
		{name: "io error", err: &fs.PathError{Op: "read", Path: "clip.m4a", Err: syscall.EIO}, want: syscall.EIO, asPath: true},
		{name: "bare EOF", err: io.EOF, want: io.ErrUnexpectedEOF},
		{name: "wrapped EOF", err: fmt.Errorf("source: %w", io.EOF), want: io.ErrUnexpectedEOF},
		// The first frame is 7 bytes, so one byte then io.EOF is a partial read
		// that io.ReadFull reports as io.ErrUnexpectedEOF.
		{name: "short read", n: 1, err: io.EOF, want: io.ErrUnexpectedEOF},
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
				src := &failingReadSeeker{r: bytes.NewReader(data), n: fc.n, err: fc.err}
				rd, err := NewReader(src)
				if err != nil {
					t.Fatalf("NewReader: %v", err)
				}
				src.armed = true

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
				if _, ok := errors.AsType[*fs.PathError](err); fc.asPath && !ok {
					t.Errorf("error %q lost the *fs.PathError cause", err)
				}
			})
		}
	}
}
