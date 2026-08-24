// SPDX-License-Identifier: MIT

package m4a

import "errors"

// Package-wide sentinel errors. They are returned wrapped (via fmt.Errorf with
// %w) so callers can match with errors.Is while still getting a descriptive
// message. The Reader shares these with the Writer, so they live here rather
// than beside a single implementation; ErrDecodeLimit is here for the same
// reason, shared by the codec bridges rather than by the container code.
var (
	// ErrCorrupt indicates a malformed container: a truncated box, a size field
	// that overflows the stream, a missing required box (moov, stbl, esds), or
	// an inconsistent sample table.
	//
	// The codec bridges also return it for a well-formed container whose codec
	// payload will not decode: a STREAMINFO or dOps that does not build a decoder,
	// or a FLAC frame / Opus packet the codec rejects. Such a file parsed as a
	// container but is corrupt at the codec layer, so the bridges give it the same
	// typed verdict as the demuxer's own rejections.
	ErrCorrupt = errors.New("go-m4a: corrupt container")

	// ErrUnsupported indicates a well-formed MP4 that falls outside the reader's
	// scope: fragmented input, no audio track, a codec other than AAC-LC, Opus or
	// FLAC, or an esds object type that is not AAC. Fragmented input is the
	// asymmetric case: this package writes it (InitSegment, FragmentWriter) but
	// deliberately does not read it back.
	//
	// The codec bridges return it for the narrower scope they accept, which is a
	// subset of the reader's: a track whose codec is not the one that bridge
	// handles, or a channel layout it does not support. Such a file may be
	// perfectly readable through this package and simply belong to another bridge.
	ErrUnsupported = errors.New("go-m4a: unsupported container")

	// ErrClosed is returned by WriteFrame and Close once the Writer has been
	// closed. A second Close, or any WriteFrame after Close, reports this
	// instead of panicking.
	ErrClosed = errors.New("go-m4a: writer is closed")

	// ErrDecodeLimit indicates that a decode was stopped because its output would
	// have exceeded the caller's byte limit: output landing exactly on the limit
	// is a fit, not an excess. It is returned by the codec bridges
	// (flacm4a, opusm4a) rather than by the container code here, which decodes
	// nothing; it lives in this package so both bridges report the same sentinel
	// and a caller handling a mix of codecs matches one error. See
	// DefaultMaxDecodedBytes for why the limit exists.
	//
	// It is a resource limit, not a verdict on the file. A stream that trips it
	// may be perfectly well-formed and merely longer than the caller allowed, so
	// the remedy is a larger limit or a streaming decode, not treating the input
	// as corrupt.
	ErrDecodeLimit = errors.New("go-m4a: decoded size limit exceeded")
)
