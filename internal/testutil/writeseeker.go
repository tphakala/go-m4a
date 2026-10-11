// SPDX-License-Identifier: MIT

package testutil

import (
	"fmt"
	"io"
)

// WriteSeeker is an in-memory io.WriteSeeker. It grows a byte slice on write
// and supports seeking anywhere, which is what the Writer needs to patch the
// mdat size and append moov. Its zero value is ready to use.
//
// Write reallocates and copies whenever it extends the buffer, so a benchmark
// warms the sink up once and calls Rewind between iterations; later writes of
// the same length then allocate nothing in the sink.
type WriteSeeker struct {
	buf []byte
	pos int64
}

// Write implements io.Writer. It overwrites in place and extends the buffer
// when the write passes the end; a gap left by an earlier seek past the end is
// zero-filled.
func (m *WriteSeeker) Write(p []byte) (int, error) {
	end := m.pos + int64(len(p))
	if end > int64(len(m.buf)) {
		grown := make([]byte, end)
		copy(grown, m.buf)
		m.buf = grown
	}
	copy(m.buf[m.pos:end], p)
	m.pos = end
	return len(p), nil
}

// Seek implements io.Seeker. A bad whence or a negative resulting position is
// an error and leaves the position unchanged.
func (m *WriteSeeker) Seek(offset int64, whence int) (int64, error) {
	var abs int64
	switch whence {
	case io.SeekStart:
		abs = offset
	case io.SeekCurrent:
		abs = m.pos + offset
	case io.SeekEnd:
		abs = int64(len(m.buf)) + offset
	default:
		return 0, fmt.Errorf("testutil: bad whence %d", whence)
	}
	if abs < 0 {
		return 0, fmt.Errorf("testutil: negative position %d", abs)
	}
	m.pos = abs
	return abs, nil
}

// Bytes returns the whole backing slice, not just the bytes before the
// position. The slice aliases the buffer and is valid until the next Write;
// callers may modify it in place.
func (m *WriteSeeker) Bytes() []byte { return m.buf }

// Rewind moves the position to 0 and keeps the buffer, so rewriting the same
// length allocates nothing in the sink.
func (m *WriteSeeker) Rewind() { m.pos = 0 }
