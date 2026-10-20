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
