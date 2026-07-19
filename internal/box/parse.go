// SPDX-License-Identifier: MIT

package box

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// errParse is the base error every parser in this file returns on malformed
// input. The public m4a.Reader wraps it as m4a.ErrCorrupt, so callers match on
// the public sentinel while the descriptive message survives via %v. Keeping a
// package-local base error lets the box unit tests assert malformed input is
// rejected without importing the m4a package.
var errParse = errors.New("box: malformed")

// Header describes a parsed ISO-BMFF box header: its four-character type, the
// header length in bytes (8 for the 32-bit form, 16 for the 64-bit largesize
// form), and the total box length including the header. ToEnd reports the
// size==0 form, whose total length is "everything to the end of the enclosing
// stream or buffer" and is resolved by the caller (valid only for the last box).
type Header struct {
	Type      FourCC
	HeaderLen int64
	Total     int64 // total box length including header; 0 when ToEnd
	ToEnd     bool
}

// ParseHeader decodes a box header from the front of b. It handles the 32-bit
// size form, the 64-bit largesize form (size==1), and the extends-to-EOF form
// (size==0). It rejects a truncated header, a size in 2..7 (smaller than the
// 8-byte header), and a largesize below 16. It does NOT validate Total against
// the caller's available bytes: the caller knows whether b is a whole stream or
// a single container payload and bounds-checks Total itself.
func ParseHeader(b []byte) (Header, error) {
	if len(b) < 8 {
		return Header{}, fmt.Errorf("box header: %d bytes, need 8: %w", len(b), errParse)
	}
	size := binary.BigEndian.Uint32(b)
	var typ FourCC
	copy(typ[:], b[4:8])

	switch size {
	case 1:
		if len(b) < 16 {
			return Header{}, fmt.Errorf("box %q largesize: %d bytes, need 16: %w", typ, len(b), errParse)
		}
		large := binary.BigEndian.Uint64(b[8:16])
		if large < 16 {
			return Header{}, fmt.Errorf("box %q largesize %d below 16: %w", typ, large, errParse)
		}
		// A largesize above the int64 range cannot address bytes in a real
		// stream; reject it rather than let the signed conversion wrap.
		if large > 1<<62 {
			return Header{}, fmt.Errorf("box %q largesize %d too large: %w", typ, large, errParse)
		}
		return Header{Type: typ, HeaderLen: 16, Total: int64(large)}, nil
	case 0:
		return Header{Type: typ, HeaderLen: 8, ToEnd: true}, nil
	default:
		if size < 8 {
			return Header{}, fmt.Errorf("box %q size %d below 8: %w", typ, size, errParse)
		}
		return Header{Type: typ, HeaderLen: 8, Total: int64(size)}, nil
	}
}

// WalkChildren iterates the child boxes packed in payload, calling fn with each
// child's type and body (the bytes after that child's header). A size==0 child
// consumes the rest of payload. It stops and returns the first error from fn or
// the first malformed header, and returns nil once payload is fully consumed.
// Every child advances the cursor by at least eight bytes, so a hostile table
// cannot spin: ParseHeader rejects any smaller size.
func WalkChildren(payload []byte, fn func(typ FourCC, body []byte) error) error {
	// All arithmetic is int64 and every bound is compared against
	// int64(len(payload)) before any slice. A largesize can be as large as
	// 1<<62, so narrowing to int (32-bit on a 386/arm build) before the bound
	// check would truncate a hostile size and defeat the guard, slicing out of
	// range. The int conversion happens only after the bound is proven.
	n := int64(len(payload))
	for off := int64(0); off < n; {
		h, err := ParseHeader(payload[off:])
		if err != nil {
			return err
		}
		total := h.Total
		if h.ToEnd {
			total = n - off
		}
		if total < h.HeaderLen || off+total > n {
			return fmt.Errorf("child box %q runs past parent: %w", h.Type, errParse)
		}
		body := payload[off+h.HeaderLen : off+total]
		if err := fn(h.Type, body); err != nil {
			return err
		}
		off += total
	}
	return nil
}

// readDescriptorSize decodes an ISO/IEC 14496-1 expandable size at the front of
// b: seven bits per byte, most significant group first, high bit set on every
// byte except the last. It accepts up to four continuation bytes (a five-byte
// encoding, as ffmpeg emits a zero-padded four-byte form) and reports ok=false
// on a truncated buffer or a fifth continuation byte. n is the number of bytes
// consumed.
func readDescriptorSize(b []byte) (size uint32, n int, ok bool) {
	var v uint32
	for i := 0; i < 5; i++ {
		if i >= len(b) {
			return 0, 0, false
		}
		v = (v << 7) | uint32(b[i]&0x7f)
		if b[i]&0x80 == 0 {
			return v, i + 1, true
		}
	}
	// The fifth byte still had its continuation bit set: the encoding is longer
	// than the four continuation bytes we tolerate.
	return 0, 0, false
}

// nextDescriptor decodes one descriptor (tag, expandable size, payload) from the
// front of b. It returns the tag, the descriptor body of exactly the decoded
// size, and the remaining bytes after it. A truncated tag/size or a size that
// overruns b is malformed.
func nextDescriptor(b []byte) (tag byte, body, rest []byte, err error) {
	if len(b) < 2 {
		return 0, nil, nil, fmt.Errorf("descriptor header: %d bytes: %w", len(b), errParse)
	}
	tag = b[0]
	size, n, ok := readDescriptorSize(b[1:])
	if !ok {
		return 0, nil, nil, fmt.Errorf("descriptor %#x size encoding: %w", tag, errParse)
	}
	start := 1 + n
	if int64(start)+int64(size) > int64(len(b)) {
		return 0, nil, nil, fmt.Errorf("descriptor %#x size %d overruns %d bytes: %w", tag, size, len(b)-start, errParse)
	}
	return tag, b[start : start+int(size)], b[start+int(size):], nil
}

// ParseEsds extracts the AudioSpecificConfig and the DecoderConfigDescriptor
// objectTypeIndication from an esds box body (the bytes after the 8-byte box
// header). It walks ES_Descriptor(0x03) > DecoderConfigDescriptor(0x04) >
// DecoderSpecificInfo(0x05), tolerating the optional ES_Descriptor dependency,
// URL, and OCR fields and any sibling descriptors (for example the mandatory
// SLConfigDescriptor). The returned asc aliases payload; the caller copies it.
func ParseEsds(payload []byte) (asc []byte, objectType byte, err error) {
	if len(payload) < 4 {
		return nil, 0, fmt.Errorf("esds: %d bytes, need version/flags: %w", len(payload), errParse)
	}
	// Skip the FullBox version and 24-bit flags.
	tag, esBody, _, err := nextDescriptor(payload[4:])
	if err != nil {
		return nil, 0, err
	}
	if tag != tagESDescriptor {
		return nil, 0, fmt.Errorf("esds: first descriptor %#x is not ES_Descriptor: %w", tag, errParse)
	}

	// ES_Descriptor: ES_ID(2) + flags(1), then flag-gated optional fields.
	if len(esBody) < 3 {
		return nil, 0, fmt.Errorf("esds ES_Descriptor: %d bytes: %w", len(esBody), errParse)
	}
	flags := esBody[2]
	p := 3
	if flags&0x80 != 0 { // streamDependenceFlag: dependsOn_ES_ID
		p += 2
	}
	if flags&0x40 != 0 { // URL_Flag: URLlength then URLstring
		if p >= len(esBody) {
			return nil, 0, fmt.Errorf("esds URL length truncated: %w", errParse)
		}
		p += 1 + int(esBody[p])
	}
	if flags&0x20 != 0 { // OCRstreamFlag: OCR_ES_ID
		p += 2
	}
	if p > len(esBody) {
		return nil, 0, fmt.Errorf("esds ES_Descriptor optional fields overrun: %w", errParse)
	}

	dcd, err := findDescriptor(esBody[p:], tagDecoderConfig)
	if err != nil {
		return nil, 0, fmt.Errorf("esds DecoderConfigDescriptor: %w", err)
	}
	// DecoderConfigDescriptor fixed fields: objectTypeIndication(1),
	// streamType/upstream(1), bufferSizeDB(3), maxBitrate(4), avgBitrate(4).
	const dcdFixed = 13
	if len(dcd) < dcdFixed {
		return nil, 0, fmt.Errorf("esds DecoderConfigDescriptor: %d bytes: %w", len(dcd), errParse)
	}
	objectType = dcd[0]

	asc, err = findDescriptor(dcd[dcdFixed:], tagDecoderSpecificInfo)
	if err != nil {
		return nil, 0, fmt.Errorf("esds DecoderSpecificInfo: %w", err)
	}
	return asc, objectType, nil
}

// findDescriptor scans a run of sibling descriptors for the first one whose tag
// matches want and returns its body.
func findDescriptor(b []byte, want byte) ([]byte, error) {
	for len(b) > 0 {
		tag, body, rest, err := nextDescriptor(b)
		if err != nil {
			return nil, err
		}
		if tag == want {
			return body, nil
		}
		b = rest
	}
	return nil, fmt.Errorf("descriptor %#x not found: %w", want, errParse)
}

// ParseAudioSampleEntry reads the fixed AudioSampleEntry fields from an mp4a box
// body (the bytes after the 8-byte box header) and returns the channel count,
// the integer sample rate (the 16.16 samplerate field truncated to Hz), and the
// offset within payload where the child boxes (esds and any siblings) begin. It
// honors the QuickTime version 1 and version 2 sound entries, which insert extra
// fixed fields before the child boxes.
func ParseAudioSampleEntry(payload []byte) (channels uint16, sampleRate uint32, childOffset int, err error) {
	// SampleEntry(8) + reserved(8) + channelcount(2) + samplesize(2) +
	// pre_defined(2) + reserved(2) + samplerate(4) = 28 bytes.
	const base = 28
	if len(payload) < base {
		return 0, 0, 0, fmt.Errorf("mp4a: %d bytes, need %d: %w", len(payload), base, errParse)
	}
	version := binary.BigEndian.Uint16(payload[8:])
	channels = binary.BigEndian.Uint16(payload[16:])
	sampleRate = binary.BigEndian.Uint32(payload[24:]) >> 16
	childOffset = base
	switch version {
	case 1:
		childOffset += 16 // samplesPerPacket, bytesPerPacket, bytesPerFrame, bytesPerSample
	case 2:
		childOffset += 36 // sizeOfStructOnly + 5 fields of the v2 layout
	}
	if childOffset > len(payload) {
		childOffset = len(payload)
	}
	return channels, sampleRate, childOffset, nil
}

// ParseDops decodes the OpusSpecificBox (dOps) body carried by an Opus sample
// entry, per the Encapsulation of Opus in ISOBMFF. It reads the fixed fields:
// Version(1) OutputChannelCount(1) PreSkip(2) InputSampleRate(4) OutputGain(2)
// ChannelMappingFamily(1). A non-zero mapping family is followed by a channel
// mapping table, which the reader does not need for frame extraction and ignores.
// It rejects a version other than 0.
func ParseDops(payload []byte) (channels uint8, preSkip uint16, inputSampleRate uint32, err error) {
	const fixed = 11
	if len(payload) < fixed {
		return 0, 0, 0, fmt.Errorf("dOps: %d bytes, need %d: %w", len(payload), fixed, errParse)
	}
	if payload[0] != 0 {
		return 0, 0, 0, fmt.Errorf("dOps version %d unsupported: %w", payload[0], errParse)
	}
	channels = payload[1]
	preSkip = binary.BigEndian.Uint16(payload[2:])
	inputSampleRate = binary.BigEndian.Uint32(payload[4:])
	return channels, preSkip, inputSampleRate, nil
}

// ParseDfla extracts the raw STREAMINFO metadata block body from a FLACSpecificBox
// (dfLa) body, per the Encapsulation of FLAC in ISOBMFF. Layout: a FullBox
// version/flags(4), then one or more FLAC metadata blocks (a 1-byte last-flag+type,
// a 24-bit length, then the body). The first block must be STREAMINFO (type 0). The
// returned slice aliases payload; the caller copies it.
func ParseDfla(payload []byte) (streamInfo []byte, err error) {
	if len(payload) < 4+4 {
		return nil, fmt.Errorf("dfLa: %d bytes, need version/flags and a block header: %w", len(payload), errParse)
	}
	// Skip the FullBox version and 24-bit flags, then read the first metadata block
	// header: bit 7 is the last-block flag, bits 0..6 the block type.
	hdr := payload[4:]
	blockType := hdr[0] & 0x7f
	length := int(hdr[1])<<16 | int(hdr[2])<<8 | int(hdr[3])
	if blockType != 0 {
		return nil, fmt.Errorf("dfLa first metadata block type %d is not STREAMINFO: %w", blockType, errParse)
	}
	// The FLAC STREAMINFO metadata block is a fixed 34 bytes; a dfLa carrying any
	// other length is malformed, and a decoder built from it (via DecodeStreamInfo)
	// would reject it anyway.
	if length != streamInfoBodyLen {
		return nil, fmt.Errorf("dfLa STREAMINFO length %d, want %d: %w", length, streamInfoBodyLen, errParse)
	}
	body := hdr[4:]
	if len(body) < length {
		return nil, fmt.Errorf("dfLa STREAMINFO length %d overruns %d bytes: %w", length, len(body), errParse)
	}
	return body[:length], nil
}

// streamInfoBodyLen is the fixed byte length of a FLAC STREAMINFO metadata block
// body, the payload a dfLa box carries.
const streamInfoBodyLen = 34

// StscEntry is one run of the sample-to-chunk table: every chunk numbered at or
// after FirstChunk (until the next entry's FirstChunk) holds SamplesPerChunk
// samples described by SampleDescriptionIndex.
type StscEntry struct {
	FirstChunk             uint32
	SamplesPerChunk        uint32
	SampleDescriptionIndex uint32
}

// ParseStsc decodes the stsc (sample-to-chunk) FullBox body. The entry count is
// bounded by the body length before the entries slice is allocated.
func ParseStsc(payload []byte) ([]StscEntry, error) {
	if len(payload) < 8 {
		return nil, fmt.Errorf("stsc: %d bytes: %w", len(payload), errParse)
	}
	count := binary.BigEndian.Uint32(payload[4:])
	if 8+12*int64(count) > int64(len(payload)) {
		return nil, fmt.Errorf("stsc entry_count %d overruns %d bytes: %w", count, len(payload), errParse)
	}
	entries := make([]StscEntry, count)
	for i := range entries {
		o := 8 + 12*i
		entries[i] = StscEntry{
			FirstChunk:             binary.BigEndian.Uint32(payload[o:]),
			SamplesPerChunk:        binary.BigEndian.Uint32(payload[o+4:]),
			SampleDescriptionIndex: binary.BigEndian.Uint32(payload[o+8:]),
		}
	}
	return entries, nil
}

// ParseStsz decodes the stsz (sample size) FullBox body. When sample_size is
// nonzero it is the constant size shared by all count samples and sizes is nil.
// When sample_size is zero a per-sample size list of length count follows and is
// bounded by the body length before allocation.
func ParseStsz(payload []byte) (constantSize, count uint32, sizes []uint32, err error) {
	if len(payload) < 12 {
		return 0, 0, nil, fmt.Errorf("stsz: %d bytes: %w", len(payload), errParse)
	}
	sampleSize := binary.BigEndian.Uint32(payload[4:])
	count = binary.BigEndian.Uint32(payload[8:])
	if sampleSize != 0 {
		return sampleSize, count, nil, nil
	}
	if 12+4*int64(count) > int64(len(payload)) {
		return 0, 0, nil, fmt.Errorf("stsz sample_count %d overruns %d bytes: %w", count, len(payload), errParse)
	}
	sizes = make([]uint32, count)
	for i := range sizes {
		sizes[i] = binary.BigEndian.Uint32(payload[12+4*i:])
	}
	return 0, count, sizes, nil
}

// ParseStco decodes the stco (32-bit chunk offset) FullBox body into absolute
// file offsets. The entry count is bounded by the body length before allocation.
func ParseStco(payload []byte) ([]uint64, error) {
	if len(payload) < 8 {
		return nil, fmt.Errorf("stco: %d bytes: %w", len(payload), errParse)
	}
	count := binary.BigEndian.Uint32(payload[4:])
	if 8+4*int64(count) > int64(len(payload)) {
		return nil, fmt.Errorf("stco entry_count %d overruns %d bytes: %w", count, len(payload), errParse)
	}
	offsets := make([]uint64, count)
	for i := range offsets {
		offsets[i] = uint64(binary.BigEndian.Uint32(payload[8+4*i:]))
	}
	return offsets, nil
}

// ParseCo64 decodes the co64 (64-bit chunk offset) FullBox body into absolute
// file offsets. The entry count is bounded by the body length before allocation.
func ParseCo64(payload []byte) ([]uint64, error) {
	if len(payload) < 8 {
		return nil, fmt.Errorf("co64: %d bytes: %w", len(payload), errParse)
	}
	count := binary.BigEndian.Uint32(payload[4:])
	if 8+8*int64(count) > int64(len(payload)) {
		return nil, fmt.Errorf("co64 entry_count %d overruns %d bytes: %w", count, len(payload), errParse)
	}
	offsets := make([]uint64, count)
	for i := range offsets {
		offsets[i] = binary.BigEndian.Uint64(payload[8+8*i:])
	}
	return offsets, nil
}

// ParseStts decodes the stts (decoding time to sample) FullBox body and returns
// the total sample count sum(sample_count) and the total media duration
// sum(sample_count * sample_delta), both as uint64. The sample-count sum is the
// authoritative value (the reader cross-checks it against stsz); the duration
// sum is advisory, since a single sample_count * sample_delta term can already
// reach ~2^64 and wrap on a pathological table. The entry count is bounded by
// the body length before the loop, so the allocation stays input-proportional.
func ParseStts(payload []byte) (sampleCount, duration uint64, err error) {
	if len(payload) < 8 {
		return 0, 0, fmt.Errorf("stts: %d bytes: %w", len(payload), errParse)
	}
	entries := binary.BigEndian.Uint32(payload[4:])
	if 8+8*int64(entries) > int64(len(payload)) {
		return 0, 0, fmt.Errorf("stts entry_count %d overruns %d bytes: %w", entries, len(payload), errParse)
	}
	for i := 0; i < int(entries); i++ {
		o := 8 + 8*i
		cnt := uint64(binary.BigEndian.Uint32(payload[o:]))
		delta := uint64(binary.BigEndian.Uint32(payload[o+4:]))
		sampleCount += cnt
		duration += cnt * delta
	}
	return sampleCount, duration, nil
}

// ParseElst decodes the first entry of an elst (edit list) FullBox body,
// honoring the version-0 (32-bit) and version-1 (64-bit) layouts. hasEntry is
// false when entry_count is zero. media_time is returned as-is: a value of -1
// marks an empty edit, which the caller treats as zero encoder delay.
func ParseElst(payload []byte) (segmentDuration uint64, mediaTime int64, hasEntry bool, err error) {
	if len(payload) < 8 {
		return 0, 0, false, fmt.Errorf("elst: %d bytes: %w", len(payload), errParse)
	}
	version := payload[0]
	count := binary.BigEndian.Uint32(payload[4:])
	if count == 0 {
		return 0, 0, false, nil
	}
	p := payload[8:]
	if version == 1 {
		if len(p) < 20 { // segment_duration(8) + media_time(8) + media_rate(4)
			return 0, 0, false, fmt.Errorf("elst v1 entry: %d bytes: %w", len(p), errParse)
		}
		return binary.BigEndian.Uint64(p), int64(binary.BigEndian.Uint64(p[8:])), true, nil
	}
	if len(p) < 12 { // segment_duration(4) + media_time(4) + media_rate(4)
		return 0, 0, false, fmt.Errorf("elst v0 entry: %d bytes: %w", len(p), errParse)
	}
	return uint64(binary.BigEndian.Uint32(p)), int64(int32(binary.BigEndian.Uint32(p[4:]))), true, nil
}

// ParseHdlr decodes the handler_type from an hdlr (handler reference) FullBox
// body: version/flags(4) + pre_defined(4), then the four-byte handler type.
func ParseHdlr(payload []byte) (FourCC, error) {
	if len(payload) < 12 {
		return FourCC{}, fmt.Errorf("hdlr: %d bytes: %w", len(payload), errParse)
	}
	var f FourCC
	copy(f[:], payload[8:12])
	return f, nil
}

// ParseMvhd decodes the timescale and duration from an mvhd (movie header)
// FullBox body, honoring the version-0 and version-1 field widths.
func ParseMvhd(payload []byte) (timescale uint32, duration uint64, err error) {
	return parseMediaHeader(payload, "mvhd")
}

// ParseMdhd decodes the timescale and duration from an mdhd (media header)
// FullBox body, honoring the version-0 and version-1 field widths.
func ParseMdhd(payload []byte) (timescale uint32, duration uint64, err error) {
	return parseMediaHeader(payload, "mdhd")
}

// parseMediaHeader decodes the shared mvhd/mdhd prefix: version/flags,
// creation_time, modification_time, timescale, duration. The two boxes have an
// identical layout up to and including duration, which is all the reader needs.
func parseMediaHeader(payload []byte, name string) (timescale uint32, duration uint64, err error) {
	if len(payload) < 4 {
		return 0, 0, fmt.Errorf("%s: %d bytes: %w", name, len(payload), errParse)
	}
	if payload[0] == 1 {
		// version/flags(4) creation(8) modification(8) timescale(4) duration(8)
		if len(payload) < 32 {
			return 0, 0, fmt.Errorf("%s v1: %d bytes: %w", name, len(payload), errParse)
		}
		return binary.BigEndian.Uint32(payload[20:]), binary.BigEndian.Uint64(payload[24:]), nil
	}
	// version/flags(4) creation(4) modification(4) timescale(4) duration(4)
	if len(payload) < 20 {
		return 0, 0, fmt.Errorf("%s v0: %d bytes: %w", name, len(payload), errParse)
	}
	return binary.BigEndian.Uint32(payload[12:]), uint64(binary.BigEndian.Uint32(payload[16:])), nil
}
