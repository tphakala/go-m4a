// SPDX-License-Identifier: MIT

package m4a

import (
	"errors"
	"io"
)

// window is a read-ahead buffer keyed by absolute stream offset: buf holds the
// stream bytes [start, start+len(buf)), and cap(buf) is the window size. A refill
// empties buf before overwriting it, so a failed read never leaves buf claiming
// bytes it no longer holds. The frame path and NewReader's box reads each own one.
type window struct {
	buf   []byte
	start int64
}

// newWindow returns an empty window of min(size, end) bytes. The size is capped
// only by the stream length end, never by a claim in the file.
func newWindow(size int, end int64) window {
	return window{buf: make([]byte, 0, int(min(int64(size), end)))}
}

// lookup copies stream[off, off+len(dst)) into dst and reports true when the range
// lies wholly inside the window. It is small enough to inline, so the frame path
// pays no call for a hit.
func (w *window) lookup(off int64, dst []byte) bool {
	if off < w.start || off+int64(len(dst)) > w.start+int64(len(w.buf)) {
		return false
	}
	copy(dst, w.buf[off-w.start:])
	return true
}

// read copies stream[off, off+len(dst)) into dst. A range wholly inside the window
// is copied from it. Otherwise read seeks to off, and a range up to half the
// window refills the window from off, reading until the window is full or the
// stream stops returning bytes, so a source whose Read returns a little at a
// time (a network chunk) still costs one Seek per window. A larger range is read
// straight into dst and leaves the window as it was, since a refill would fetch
// a whole window to serve one or two ranges. end is the stream length. Callers keep
// off+len(dst) within it, and an empty dst does no I/O.
//
// On failure op is "seek" or "read", for callers that label the failing step.
func (w *window) read(r io.ReadSeeker, off, end int64, dst []byte) (op string, err error) {
	if len(dst) == 0 {
		return "", nil
	}
	if w.lookup(off, dst) {
		return "", nil
	}
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return "seek", err
	}
	if len(dst) > cap(w.buf)/2 {
		if _, err := io.ReadFull(r, dst); err != nil {
			return "read", err
		}
		return "", nil
	}
	// The max keeps want >= len(dst) if a caller breaks the end bound. An error after
	// the range's bytes arrived is dropped; the next refill meets it again.
	want := max(len(dst), int(min(int64(cap(w.buf)), end-off)))
	w.buf = w.buf[:0]
	got, err := io.ReadFull(r, w.buf[:want])
	if got < len(dst) {
		return "read", err
	}
	w.buf, w.start = w.buf[:got], off
	copy(dst, w.buf)
	return "", nil
}

// openWindowSize is the size, in bytes, of the window NewReader reads box headers
// and box bodies through. Reading each box on its own cost a Seek and a Read per
// header and per moof body: a 59 MB stream of 1800 segments (94 AAC frames of
// 346 bytes each) opened with 7205 Seek and 7204 Read calls, 0.9 MB read. Through
// a 4 KiB window it opens with 3601 and 3600, 14.7 MB read, since a segment's
// styp, moof and mdat header share one refill when the moof is small, and the
// moof is read again from the window or from one more refill. A larger window
// transfers more than it saves: 64 KiB makes 1801 and 1800 calls but reads 118
// MB, twice the stream, because each refill at a segment header also pulls the
// segment's media payload. A source in memory pays one 4 KiB copy per refill
// instead of a 16-byte read. The window lives only while NewReader runs, and it
// is capped only by the stream length, never by a claim in the file.
const openWindowSize = 4 << 10

// openReader serves NewReader's reads (box headers, ftyp and moov bodies, moof
// bodies) through a window of its own, so the stream's position and the frame
// window are not involved. The open-time error messages do not distinguish a Seek
// from a Read, so read drops the op.
type openReader struct {
	r   io.ReadSeeker
	end int64
	win window
}

// read copies stream[off, off+len(dst)) into dst, with any EOF mapped by
// unexpectedEOF.
func (o *openReader) read(off int64, dst []byte) error {
	_, err := o.win.read(o.r, off, o.end, dst)
	return unexpectedEOF(err)
}

// unexpectedEOF maps any EOF to io.ErrUnexpectedEOF and returns other errors
// unchanged. Every Reader read covers a range already bounded against the
// stream length measured at open, so running out of bytes means the stream is
// shorter than that, never a clean end; io.ReadFull passes a reader's wrapped
// io.EOF through, and a Seek may report one.
func unexpectedEOF(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}
