// SPDX-License-Identifier: MIT

package m4a

import "errors"

// Package-wide sentinel errors. They are returned wrapped (via fmt.Errorf with
// %w) so callers can match with errors.Is while still getting a descriptive
// message. The Reader shares these with the Writer, so they live here rather
// than beside a single implementation.
var (
	// ErrCorrupt indicates a malformed container: a truncated box, a size field
	// that overflows the stream, a missing required box (moov, stbl, esds), or
	// an inconsistent sample table.
	ErrCorrupt = errors.New("go-m4a: corrupt container")

	// ErrUnsupported indicates a well-formed MP4 that falls outside the reader's
	// scope: fragmented input, no audio track, a codec other than AAC-LC, Opus or
	// FLAC, or an esds object type that is not AAC. Fragmented input is the
	// asymmetric case: this package writes it (InitSegment, FragmentWriter) but
	// deliberately does not read it back.
	ErrUnsupported = errors.New("go-m4a: unsupported container")

	// ErrClosed is returned by WriteFrame and Close once the Writer has been
	// closed. A second Close, or any WriteFrame after Close, reports this
	// instead of panicking.
	ErrClosed = errors.New("go-m4a: writer is closed")
)
