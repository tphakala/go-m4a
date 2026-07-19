// SPDX-License-Identifier: MIT

package m4a

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/tphakala/go-m4a/internal/box"
)

// Info summarizes an MP4/M4A file's single audio track (AAC-LC, Opus, or FLAC),
// populated by NewReader from the moov metadata. The fields come straight from the
// file's own sample tables, the codec-specific box (esds, dOps, or dfLa), and the
// edit list; Codec reports which codec, and CodecConfig carries its configuration.
type Info struct {
	// SampleRate is the audio sample rate in Hz, derived from the ASC and, when
	// the ASC does not carry an explicit rate, the mp4a AudioSampleEntry.
	SampleRate int

	// Channels is the channel count, normally 1 (mono) or 2 (stereo).
	Channels int

	// Codec identifies the audio codec of the selected track: CodecAACLC,
	// CodecOpus, or CodecFLAC.
	Codec Codec

	// ASC is the MPEG-4 AudioSpecificConfig from the esds DecoderSpecificInfo,
	// suitable for go-aac's pcm.WithRawStream. It is set only for AAC-LC; it is nil
	// for Opus and FLAC. Info returns a fresh copy.
	ASC []byte

	// CodecConfig is the codec-specific configuration recovered from the sample
	// entry: the AudioSpecificConfig for AAC-LC (same bytes as ASC), the
	// OpusSpecificBox (dOps) body for Opus, or the FLAC STREAMINFO metadata block
	// for FLAC. It is what a codec bridge passes to its decoder. Info returns a
	// fresh copy.
	CodecConfig []byte

	// FrameCount is the number of access units (MP4 "samples") in the track.
	FrameCount int

	// EncoderDelay is the leading priming sample count taken verbatim from the
	// edit list media_time. It is 0 when there is no edit list or the first edit
	// is an empty edit (media_time -1).
	EncoderDelay int64

	// Duration is the track presentation duration after the edit list.
	Duration time.Duration

	// Brand is the ftyp major brand.
	Brand string
}

// Reader demuxes a non-fragmented MP4/M4A file (AAC-LC, Opus, or FLAC) into its
// access units. NewReader parses the moov metadata up front; ReadFrame then returns each
// access unit in order by seeking to its computed (offset, size). The reader is
// not safe for concurrent use, and ReadFrame, RawStream, and their shared cursor
// advance together.
type Reader struct {
	r         io.ReadSeeker
	streamLen int64
	info      Info

	// Sample geometry. Exactly one of sizes (per-sample) or constSize (constant
	// size, sizes nil) describes the access-unit lengths. samplesPerChunk and
	// chunkOffsets are indexed by chunk; both are bounded by the chunk-offset
	// table length.
	sizes           []uint32
	constSize       uint32
	sampleCount     int
	samplesPerChunk []int64
	chunkOffsets    []int64

	// Running cursor shared by ReadFrame and RawStream, advanced one access unit
	// at a time so sequential reads are O(N) overall rather than O(N^2).
	nextSample    int
	curChunk      int
	sampleInChunk int64
	curOffset     int64
}

// Box and brand codes the reader matches while walking the tree.
var (
	fourccMoov = box.NewFourCC("moov")
	fourccTrak = box.NewFourCC("trak")
	fourccMvhd = box.NewFourCC("mvhd")
	fourccMvex = box.NewFourCC("mvex")
	fourccEdts = box.NewFourCC("edts")
	fourccElst = box.NewFourCC("elst")
	fourccMdia = box.NewFourCC("mdia")
	fourccMdhd = box.NewFourCC("mdhd")
	fourccHdlr = box.NewFourCC("hdlr")
	fourccMinf = box.NewFourCC("minf")
	fourccStbl = box.NewFourCC("stbl")
	fourccStsd = box.NewFourCC("stsd")
	fourccStts = box.NewFourCC("stts")
	fourccStsc = box.NewFourCC("stsc")
	fourccStsz = box.NewFourCC("stsz")
	fourccStz2 = box.NewFourCC("stz2")
	fourccStco = box.NewFourCC("stco")
	fourccCo64 = box.NewFourCC("co64")
	fourccEsds = box.NewFourCC("esds")

	fourccMp4a = box.NewFourCC("mp4a")
	fourccOpus = box.NewFourCC("Opus")
	fourccFlac = box.NewFourCC("fLaC")
	fourccDops = box.NewFourCC("dOps")
	fourccDfla = box.NewFourCC("dfLa")
	fourccSoun = box.NewFourCC("soun")

	fourccFtyp = box.NewFourCC("ftyp")
	fourccMoof = box.NewFourCC("moof")
	fourccStyp = box.NewFourCC("styp")
	fourccSidx = box.NewFourCC("sidx")
	fourccMfra = box.NewFourCC("mfra")
)

// NewReader reads and parses the moov metadata from r and returns a Reader
// positioned at the first access unit. It locates moov whether it precedes or
// follows mdat, selects the first "soun" track carrying a supported sample entry
// (mp4a AAC-LC, Opus, or fLaC), and builds the sample geometry from the
// stsc/stsz/stco tables. It returns a wrapped ErrCorrupt for malformed input and a
// wrapped ErrUnsupported for well-formed input outside scope (fragmented files, no
// supported audio track, an unsupported codec, or an AAC object type that is not
// MPEG-4 Audio).
func NewReader(r io.ReadSeeker) (*Reader, error) {
	if r == nil {
		return nil, fmt.Errorf("go-m4a: nil reader")
	}
	streamLen, err := r.Seek(0, io.SeekEnd)
	if err != nil {
		return nil, fmt.Errorf("go-m4a: seek end: %w", err)
	}
	if streamLen < 0 {
		return nil, fmt.Errorf("go-m4a: negative stream length %d: %w", streamLen, ErrCorrupt)
	}

	brand, moovPayload, err := scanTopLevel(r, streamLen)
	if err != nil {
		return nil, err
	}

	rd := &Reader{r: r, streamLen: streamLen}
	rd.info.Brand = brand
	if err := rd.parseMoov(moovPayload); err != nil {
		return nil, err
	}
	rd.resetCursor()
	return rd, nil
}

// scanTopLevel walks the top-level boxes of r, returning the ftyp major brand
// and the moov box body. It rejects fragmented input (moof/styp/sidx/mfra) as
// ErrUnsupported and a missing moov as ErrCorrupt.
func scanTopLevel(r io.ReadSeeker, streamLen int64) (brand string, moovPayload []byte, err error) {
	var moov []byte
	for off := int64(0); off < streamLen; {
		remaining := streamLen - off
		if remaining < 8 {
			break // trailing bytes too small to be a box header; tolerate as padding
		}
		hdrLen := int64(16)
		if remaining < 16 {
			hdrLen = remaining
		}
		head, rerr := readSection(r, off, hdrLen)
		if rerr != nil {
			return "", nil, fmt.Errorf("go-m4a: read box header at %d: %w", off, rerr)
		}
		h, perr := box.ParseHeader(head)
		if perr != nil {
			return "", nil, fmt.Errorf("go-m4a: %w: %w", perr, ErrCorrupt)
		}
		total := h.Total
		if h.ToEnd {
			total = remaining
		}
		if total < h.HeaderLen || total > remaining {
			return "", nil, fmt.Errorf("go-m4a: box %q at %d runs past stream: %w", h.Type, off, ErrCorrupt)
		}

		switch h.Type {
		case fourccMoof, fourccStyp, fourccSidx, fourccMfra:
			return "", nil, fmt.Errorf("go-m4a: fragmented input (%q): %w", h.Type, ErrUnsupported)
		case fourccFtyp:
			body, berr := readSection(r, off+h.HeaderLen, total-h.HeaderLen)
			if berr != nil {
				return "", nil, fmt.Errorf("go-m4a: read ftyp: %w", berr)
			}
			if len(body) >= 4 {
				brand = string(body[:4])
			}
		case fourccMoov:
			body, berr := readSection(r, off+h.HeaderLen, total-h.HeaderLen)
			if berr != nil {
				return "", nil, fmt.Errorf("go-m4a: read moov: %w", berr)
			}
			moov = body
		}
		off += total
	}
	if moov == nil {
		return "", nil, fmt.Errorf("go-m4a: no moov box: %w", ErrCorrupt)
	}
	return brand, moov, nil
}

// readSection seeks to off and reads exactly n bytes. n is always a header size
// or a box length already bounded against streamLen by the caller, so the
// allocation is bounded by the real input.
func readSection(r io.ReadSeeker, off, n int64) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("negative length %d: %w", n, ErrCorrupt)
	}
	if n > maxInt {
		// A >2 GiB box on a 32-bit build would panic make() or truncate; reject
		// it as corrupt rather than crash. Wrap ErrCorrupt so a caller
		// classifying with errors.Is catches it like any other malformation.
		return nil, fmt.Errorf("section length %d exceeds addressable memory: %w", n, ErrCorrupt)
	}
	if _, err := r.Seek(off, io.SeekStart); err != nil {
		return nil, err
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	return buf, nil
}

// track collects the fields the reader extracts from one trak while deciding
// whether it is the AAC-LC audio track to demux.
type track struct {
	handler box.FourCC
	codec   box.FourCC // first stsd sample-entry type
	haveSE  bool       // a first sample entry was parsed

	asc          []byte // AAC-LC AudioSpecificConfig (esds); nil for Opus/FLAC
	objectType   byte   // AAC-LC esds objectTypeIndication
	codecConfig  []byte // codec-specific config: ASC (AAC), dOps body (Opus), STREAMINFO (FLAC)
	opusPreSkip  uint16 // Opus dOps PreSkip, the pre-skip analog of the AAC encoder delay
	seChannels   uint16
	seSampleRate uint32

	stsc []byte
	stsz []byte
	stco []byte
	co64 []byte
	stts []byte
	seen struct{ stsc, stsz, stz2, stco, co64 bool }

	mdhdTS  uint32
	mdhdDur uint64

	hasElst   bool
	elstSeg   uint64
	elstMedia int64
}

// parseMoov walks the moov box, reads the movie header, rejects fragmented
// files (an mvex box), then selects the first AAC-LC "soun" track and builds its
// sample geometry.
func (rd *Reader) parseMoov(moov []byte) error {
	var movieTS uint32
	var movieDur uint64
	var chosen *track
	var sawSoun bool

	err := box.WalkChildren(moov, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccMvhd:
			ts, dur, perr := box.ParseMvhd(body)
			if perr != nil {
				return perr
			}
			movieTS, movieDur = ts, dur
		case fourccMvex:
			return fmt.Errorf("go-m4a: fragmented input (mvex): %w", ErrUnsupported)
		case fourccTrak:
			if chosen != nil {
				return nil // already have the track we want
			}
			tr, perr := parseTrak(body)
			if perr != nil {
				return perr
			}
			if tr.handler == fourccSoun {
				sawSoun = true
				if isSupportedCodec(tr.codec) {
					chosen = tr
				}
			}
		}
		return nil
	})
	if err != nil {
		return wrapParse(err)
	}
	if chosen == nil {
		if sawSoun {
			return fmt.Errorf("go-m4a: audio track codec is not mp4a, Opus, or fLaC: %w", ErrUnsupported)
		}
		return fmt.Errorf("go-m4a: no supported audio track: %w", ErrUnsupported)
	}

	if err := rd.applyTrack(chosen, movieTS, movieDur); err != nil {
		return err
	}
	return nil
}

// parseTrak walks one trak box, collecting its handler type, edit list, media
// header, sample-entry codec, and sample tables into a track value.
func parseTrak(trak []byte) (*track, error) {
	tr := &track{}
	err := box.WalkChildren(trak, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccEdts:
			return parseEdts(body, tr)
		case fourccMdia:
			return parseMdia(body, tr)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return tr, nil
}

// parseEdts reads the first elst entry from an edts box.
func parseEdts(edts []byte, tr *track) error {
	return box.WalkChildren(edts, func(typ box.FourCC, body []byte) error {
		if typ != fourccElst {
			return nil
		}
		seg, media, has, perr := box.ParseElst(body)
		if perr != nil {
			return perr
		}
		tr.hasElst = has
		tr.elstSeg = seg
		tr.elstMedia = media
		return nil
	})
}

// parseMdia walks an mdia box, reading the media header, the handler type, and
// descending into minf > stbl.
func parseMdia(mdia []byte, tr *track) error {
	return box.WalkChildren(mdia, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccMdhd:
			ts, dur, perr := box.ParseMdhd(body)
			if perr != nil {
				return perr
			}
			tr.mdhdTS, tr.mdhdDur = ts, dur
		case fourccHdlr:
			ht, perr := box.ParseHdlr(body)
			if perr != nil {
				return perr
			}
			tr.handler = ht
		case fourccMinf:
			return box.WalkChildren(body, func(t box.FourCC, b []byte) error {
				if t == fourccStbl {
					return parseStbl(b, tr)
				}
				return nil
			})
		}
		return nil
	})
}

// parseStbl collects the sample-table boxes and the codec sample entry from an
// stbl box. It does not yet validate cross-table consistency; applyTrack does.
func parseStbl(stbl []byte, tr *track) error {
	return box.WalkChildren(stbl, func(typ box.FourCC, body []byte) error {
		switch typ {
		case fourccStsd:
			return parseStsd(body, tr)
		case fourccStts:
			tr.stts = body
		case fourccStsc:
			tr.stsc, tr.seen.stsc = body, true
		case fourccStsz:
			tr.stsz, tr.seen.stsz = body, true
		case fourccStz2:
			tr.seen.stz2 = true
		case fourccStco:
			tr.stco, tr.seen.stco = body, true
		case fourccCo64:
			tr.co64, tr.seen.co64 = body, true
		}
		return nil
	})
}

// isSupportedCodec reports whether the sample-entry fourCC is one this reader can
// demux: mp4a (AAC-LC), Opus, or fLaC.
func isSupportedCodec(codec box.FourCC) bool {
	return codec == fourccMp4a || codec == fourccOpus || codec == fourccFlac
}

// parseStsd reads the first sample entry from an stsd box, recording its codec,
// the shared AudioSampleEntry fields, and the codec-specific configuration box:
// esds (AAC-LC), dOps (Opus), or dfLa (FLAC).
func parseStsd(stsd []byte, tr *track) error {
	// stsd body: version/flags(4) + entry_count(4), then the sample entries.
	if len(stsd) < 8 {
		return fmt.Errorf("go-m4a: stsd too short: %d bytes: %w", len(stsd), ErrCorrupt)
	}
	entries := stsd[8:]
	h, err := box.ParseHeader(entries)
	if err != nil {
		return err
	}
	if h.ToEnd || h.Total < h.HeaderLen || h.Total > int64(len(entries)) {
		return fmt.Errorf("go-m4a: stsd sample entry runs past box: %w", ErrCorrupt)
	}
	tr.codec = h.Type
	tr.haveSE = true
	if !isSupportedCodec(h.Type) {
		return nil // an unsupported codec; track selection rejects it later
	}

	// Every supported codec uses the same AudioSampleEntry fixed fields; only the
	// codec-specific child box differs.
	se := entries[h.HeaderLen:h.Total]
	ch, rate, childOff, err := box.ParseAudioSampleEntry(se)
	if err != nil {
		return err
	}
	tr.seChannels = ch
	tr.seSampleRate = rate
	children := se[childOff:]

	switch h.Type {
	case fourccMp4a:
		// Find the esds child (afconvert follows esds with a sibling box, so scan
		// rather than assume position).
		return box.WalkChildren(children, func(t box.FourCC, b []byte) error {
			if t != fourccEsds {
				return nil
			}
			asc, objType, perr := box.ParseEsds(b)
			if perr != nil {
				return perr
			}
			tr.asc = asc
			tr.codecConfig = asc
			tr.objectType = objType
			return nil
		})
	case fourccOpus:
		return box.WalkChildren(children, func(t box.FourCC, b []byte) error {
			if t != fourccDops {
				return nil
			}
			chs, preSkip, _, perr := box.ParseDops(b)
			if perr != nil {
				return perr
			}
			tr.opusPreSkip = preSkip
			tr.codecConfig = b // the dOps body
			if chs != 0 {
				tr.seChannels = uint16(chs) // dOps OutputChannelCount is authoritative
			}
			return nil
		})
	case fourccFlac:
		return box.WalkChildren(children, func(t box.FourCC, b []byte) error {
			if t != fourccDfla {
				return nil
			}
			si, perr := box.ParseDfla(b)
			if perr != nil {
				return perr
			}
			tr.codecConfig = si // the STREAMINFO metadata block
			return nil
		})
	}
	return nil
}

// applyTrack validates the selected track's tables, fills Info, and builds the
// sample geometry (per-sample sizes plus per-chunk sample counts and offsets).
func (rd *Reader) applyTrack(tr *track, movieTS uint32, movieDur uint64) error {
	// Codec-specific configuration must be present and, for AAC, the object type
	// must be MPEG-4 Audio. The sample geometry below is codec-agnostic.
	switch tr.codec {
	case fourccMp4a:
		if tr.asc == nil {
			return fmt.Errorf("go-m4a: mp4a track has no esds AudioSpecificConfig: %w", ErrCorrupt)
		}
		if tr.objectType != objectTypeMP4Audio {
			return fmt.Errorf("go-m4a: object type %#x is not AAC: %w", tr.objectType, ErrUnsupported)
		}
		rd.info.Codec = CodecAACLC
	case fourccOpus:
		if tr.codecConfig == nil {
			return fmt.Errorf("go-m4a: Opus track has no dOps OpusSpecificBox: %w", ErrCorrupt)
		}
		rd.info.Codec = CodecOpus
	case fourccFlac:
		if tr.codecConfig == nil {
			return fmt.Errorf("go-m4a: fLaC track has no dfLa STREAMINFO: %w", ErrCorrupt)
		}
		rd.info.Codec = CodecFLAC
	default:
		return fmt.Errorf("go-m4a: unsupported codec %q: %w", tr.codec, ErrUnsupported)
	}
	if tr.seen.stz2 && !tr.seen.stsz {
		return fmt.Errorf("go-m4a: stz2 sample sizes are unsupported: %w", ErrUnsupported)
	}
	if !tr.seen.stsz {
		return fmt.Errorf("go-m4a: missing stsz: %w", ErrCorrupt)
	}
	if !tr.seen.stsc {
		return fmt.Errorf("go-m4a: missing stsc: %w", ErrCorrupt)
	}
	if !tr.seen.stco && !tr.seen.co64 {
		return fmt.Errorf("go-m4a: missing chunk offsets (stco/co64): %w", ErrCorrupt)
	}

	if err := rd.buildGeometry(tr); err != nil {
		return err
	}

	// Cross-check the stts total against the stsz sample count. A spec-compliant
	// file agrees; a mismatch signals corruption. stts may be absent in an
	// otherwise-readable file, so this only checks when the box was present.
	if tr.stts != nil {
		sttsSamples, _, serr := box.ParseStts(tr.stts)
		if serr != nil {
			return wrapParse(serr)
		}
		if sttsSamples != uint64(rd.sampleCount) {
			return fmt.Errorf("go-m4a: stts sample count %d disagrees with stsz %d: %w",
				sttsSamples, rd.sampleCount, ErrCorrupt)
		}
	}

	// Copy the codec config (and the ASC for AAC) so the Reader is independent of
	// the moov buffer. tr.asc is nil for Opus and FLAC, so ASC stays nil there.
	if tr.asc != nil {
		rd.info.ASC = append([]byte(nil), tr.asc...)
	}
	rd.info.CodecConfig = append([]byte(nil), tr.codecConfig...)
	rd.info.FrameCount = rd.sampleCount
	rd.info.SampleRate, rd.info.Channels = resolveFormat(tr)

	if tr.hasElst && tr.elstMedia >= 0 {
		rd.info.EncoderDelay = tr.elstMedia
	} else if tr.codec == fourccOpus && tr.opusPreSkip > 0 {
		// No edit list: fall back to the dOps PreSkip, the canonical Opus priming
		// signal per the Encapsulation of Opus in ISOBMFF (RFC 7845). A file this
		// package writes always carries the elst, so this covers foreign Opus files.
		rd.info.EncoderDelay = int64(tr.opusPreSkip)
	}
	rd.info.Duration = presentationDuration(tr, movieTS, movieDur)
	return nil
}

// buildGeometry decodes the size and chunk tables, cross-checks their counts,
// and lays out the per-chunk sample counts and offsets. Every count is bounded
// against the stream length before allocation and every chunk offset is bounded
// against the stream length so no ReadFrame can seek out of bounds.
func (rd *Reader) buildGeometry(tr *track) error {
	constSize, count, sizes, err := box.ParseStsz(tr.stsz)
	if err != nil {
		return wrapParse(err)
	}
	// A constant-size table carries no per-sample entries, so bound its declared
	// count against the stream: every access unit occupies constSize file bytes.
	if constSize != 0 && int64(count)*int64(constSize) > rd.streamLen {
		return fmt.Errorf("go-m4a: stsz constant %d x count %d exceeds stream %d: %w",
			constSize, count, rd.streamLen, ErrCorrupt)
	}
	if int64(count) > maxInt {
		// On a 32-bit build int(count) for count >= 2^31 would go negative and
		// silently truncate the sample count; reject it as corrupt.
		return fmt.Errorf("go-m4a: stsz sample count %d exceeds addressable memory: %w", count, ErrCorrupt)
	}
	rd.constSize = constSize
	rd.sizes = sizes
	rd.sampleCount = int(count)

	stsc, err := box.ParseStsc(tr.stsc)
	if err != nil {
		return wrapParse(err)
	}
	chunkOffsets, err := rd.chunkOffsetTable(tr)
	if err != nil {
		return err
	}
	rd.chunkOffsets = chunkOffsets

	perChunk, err := expandStsc(stsc, len(chunkOffsets), rd.sampleCount)
	if err != nil {
		return err
	}
	rd.samplesPerChunk = perChunk

	return rd.validateOffsets()
}

// chunkOffsetTable returns the chunk offsets from stco or co64 as int64 file
// offsets, rejecting an offset that does not fit a signed 64-bit file position.
func (rd *Reader) chunkOffsetTable(tr *track) ([]int64, error) {
	var raw []uint64
	var err error
	if tr.seen.co64 {
		raw, err = box.ParseCo64(tr.co64)
	} else {
		raw, err = box.ParseStco(tr.stco)
	}
	if err != nil {
		return nil, wrapParse(err)
	}
	offsets := make([]int64, len(raw))
	for i, o := range raw {
		if o > 1<<62 {
			return nil, fmt.Errorf("go-m4a: chunk offset %d too large: %w", o, ErrCorrupt)
		}
		offsets[i] = int64(o)
	}
	return offsets, nil
}

// expandStsc turns the run-length sample-to-chunk table into a per-chunk sample
// count of length numChunks, validating that first_chunk values are strictly
// increasing and in range and that the totals match the sample count.
func expandStsc(entries []box.StscEntry, numChunks, sampleCount int) ([]int64, error) {
	if numChunks == 0 {
		if sampleCount == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("go-m4a: %d samples but no chunks: %w", sampleCount, ErrCorrupt)
	}
	if len(entries) == 0 {
		return nil, fmt.Errorf("go-m4a: empty stsc: %w", ErrCorrupt)
	}
	if entries[0].FirstChunk != 1 {
		return nil, fmt.Errorf("go-m4a: stsc first_chunk %d, want 1: %w", entries[0].FirstChunk, ErrCorrupt)
	}

	perChunk := make([]int64, numChunks)
	var total int64
	prevFirst := uint32(0)
	for i, e := range entries {
		if e.FirstChunk <= prevFirst || e.FirstChunk > uint32(numChunks) {
			return nil, fmt.Errorf("go-m4a: stsc first_chunk %d out of order or range: %w", e.FirstChunk, ErrCorrupt)
		}
		prevFirst = e.FirstChunk
		last := uint32(numChunks) // this run extends to the final chunk...
		if i+1 < len(entries) {
			// ...or up to the next run's first chunk. The next entry's
			// FirstChunk is used here, one iteration before its own turn at the
			// top-of-loop validation, so validate it now: an unchecked value of
			// 0 underflows to 0xFFFFFFFF, and any value past numChunks makes the
			// fill loop below write out of bounds.
			next := entries[i+1].FirstChunk
			if next <= e.FirstChunk || next > uint32(numChunks) {
				return nil, fmt.Errorf("go-m4a: stsc first_chunk %d out of order or range: %w", next, ErrCorrupt)
			}
			last = next - 1
		}
		runChunks := int64(last) - int64(e.FirstChunk) + 1
		// runChunks is now bounded by numChunks, but SamplesPerChunk is an
		// unbounded uint32, so compute the product overflow-safely: compare
		// runChunks against remaining/spc rather than forming runChunks*spc,
		// which could overflow int64 on a hostile table.
		spc := int64(e.SamplesPerChunk)
		remaining := int64(sampleCount) - total
		if spc > 0 && runChunks > remaining/spc {
			return nil, fmt.Errorf("go-m4a: stsc samples exceed sample_count %d: %w", sampleCount, ErrCorrupt)
		}
		samples := runChunks * spc
		for c := e.FirstChunk; c <= last; c++ {
			perChunk[c-1] = spc
		}
		total += samples
	}
	if total != int64(sampleCount) {
		return nil, fmt.Errorf("go-m4a: stsc maps %d samples, stsz has %d: %w", total, sampleCount, ErrCorrupt)
	}
	return perChunk, nil
}

// validateOffsets walks every sample once, confirming each access unit lies
// wholly within the stream, so ReadFrame can seek without a further bounds
// surprise. It also confirms the chunk table and sample count agree.
func (rd *Reader) validateOffsets() error {
	sample := 0
	for c, base := range rd.chunkOffsets {
		if c >= len(rd.samplesPerChunk) {
			break
		}
		cursor := base
		for k := int64(0); k < rd.samplesPerChunk[c]; k++ {
			if sample >= rd.sampleCount {
				return fmt.Errorf("go-m4a: chunk table overshoots sample count: %w", ErrCorrupt)
			}
			size := int64(rd.sizeOf(sample))
			if cursor < 0 || size < 0 || cursor+size > rd.streamLen {
				return fmt.Errorf("go-m4a: sample %d at [%d,%d) outside stream %d: %w",
					sample, cursor, cursor+size, rd.streamLen, ErrCorrupt)
			}
			cursor += size
			sample++
		}
	}
	if sample != rd.sampleCount {
		return fmt.Errorf("go-m4a: chunk table covers %d of %d samples: %w", sample, rd.sampleCount, ErrCorrupt)
	}
	return nil
}

// resolveFormat prefers the ASC-derived sample rate and channel count, falling
// back to the mp4a AudioSampleEntry fields when the ASC does not carry them (an
// explicit-rate index or a zero channel configuration).
func resolveFormat(tr *track) (sampleRate, channels int) {
	sampleRate = int(tr.seSampleRate)
	channels = int(tr.seChannels)
	switch {
	case tr.codec == fourccMp4a && len(tr.asc) >= 2:
		// For AAC the ASC is more authoritative than the AudioSampleEntry.
		_, sfi, chanCfg := parseASC(tr.asc)
		if int(sfi) < len(samplingFrequencyTable) {
			sampleRate = samplingFrequencyTable[sfi]
		}
		if chanCfg >= 1 && chanCfg <= 2 {
			channels = int(chanCfg)
		}
	case tr.codec == fourccOpus:
		// Opus always decodes at 48 kHz, and the encapsulation fixes the sample
		// entry samplerate at 48000, so report that regardless of a foreign or
		// malformed file's samplerate field. The dOps OutputChannelCount already
		// set tr.seChannels.
		sampleRate = opusTimescale
	}
	// FLAC has no ASC: the sample entry samplerate and channel count are authoritative.
	return sampleRate, channels
}

// presentationDuration converts the track's presentation length to a
// time.Duration. With an edit list the segment_duration is in the movie
// timescale; without one the movie header duration applies, and the media
// header is the last resort.
func presentationDuration(tr *track, movieTS uint32, movieDur uint64) time.Duration {
	switch {
	case tr.hasElst && movieTS > 0:
		return ticksToDuration(tr.elstSeg, movieTS)
	case movieTS > 0 && movieDur > 0:
		return ticksToDuration(movieDur, movieTS)
	case tr.mdhdTS > 0:
		return ticksToDuration(tr.mdhdDur, tr.mdhdTS)
	default:
		return 0
	}
}

// ticksToDuration converts ticks at the given timescale (ticks per second) to a
// time.Duration.
func ticksToDuration(ticks uint64, timescale uint32) time.Duration {
	if timescale == 0 {
		return 0
	}
	return time.Duration(float64(ticks) / float64(timescale) * float64(time.Second))
}

// sizeOf returns the byte length of sample i, from the per-sample table or the
// constant size.
func (rd *Reader) sizeOf(i int) uint32 {
	if rd.sizes != nil {
		return rd.sizes[i]
	}
	return rd.constSize
}

// resetCursor positions the running cursor at the first access unit, skipping
// any leading chunks that hold zero samples.
func (rd *Reader) resetCursor() {
	rd.nextSample = 0
	rd.curChunk = 0
	rd.sampleInChunk = 0
	if rd.sampleCount == 0 {
		return
	}
	for rd.curChunk < len(rd.samplesPerChunk) && rd.samplesPerChunk[rd.curChunk] == 0 {
		rd.curChunk++
	}
	if rd.curChunk < len(rd.chunkOffsets) {
		rd.curOffset = rd.chunkOffsets[rd.curChunk]
	}
}

// Info returns a copy of the parsed track summary, including a fresh copy of the
// ASC so the caller cannot mutate the Reader's state.
func (rd *Reader) Info() Info {
	out := rd.info
	out.ASC = append([]byte(nil), rd.info.ASC...)
	out.CodecConfig = append([]byte(nil), rd.info.CodecConfig...)
	return out
}

// ASC returns a fresh copy of the AudioSpecificConfig, suitable for passing to
// go-aac's pcm.WithRawStream.
func (rd *Reader) ASC() []byte {
	return append([]byte(nil), rd.info.ASC...)
}

// ReadFrame returns the next access unit in decode order and advances the
// cursor. It returns io.EOF after the last access unit. A frame whose computed
// extent falls outside the stream, or a short read, is a wrapped ErrCorrupt.
func (rd *Reader) ReadFrame() ([]byte, error) {
	size, err := rd.nextFrameSize()
	if err != nil {
		return nil, err
	}
	buf := make([]byte, size)
	if err := rd.readFrameInto(buf, size); err != nil {
		return nil, err
	}
	return buf, nil
}

// ReadFrameInto reads the next access unit into dst and returns its length,
// advancing the cursor. It is the zero-allocation form of ReadFrame for callers
// that reuse a buffer across frames. If dst is too small to hold the frame,
// ReadFrameInto reads nothing, does not advance, and returns the required length
// with io.ErrShortBuffer, so the caller can grow dst and call again. It returns
// io.EOF after the last access unit, and a wrapped ErrCorrupt for a frame whose
// extent falls outside the stream or a short read.
func (rd *Reader) ReadFrameInto(dst []byte) (int, error) {
	size, err := rd.nextFrameSize()
	if err != nil {
		return 0, err
	}
	if len(dst) < int(size) {
		return int(size), io.ErrShortBuffer
	}
	if err := rd.readFrameInto(dst, size); err != nil {
		return 0, err
	}
	return int(size), nil
}

// nextFrameSize validates and returns the size of the next access unit without
// advancing the cursor. It returns io.EOF at the end of the track, and a
// wrapped ErrCorrupt for a zero-length frame or one whose extent falls outside
// the stream. The size is bounded against streamLen, so a caller may allocate a
// buffer of that size safely (on a 32-bit build a >2 GiB frame is rejected by
// the maxInt guard before any allocation).
func (rd *Reader) nextFrameSize() (uint32, error) {
	if rd.nextSample >= rd.sampleCount {
		return 0, io.EOF
	}
	size := rd.sizeOf(rd.nextSample)
	if size == 0 {
		return 0, fmt.Errorf("go-m4a: frame %d has zero length: %w", rd.nextSample, ErrCorrupt)
	}
	off := rd.curOffset
	if off < 0 || off+int64(size) > rd.streamLen {
		return 0, fmt.Errorf("go-m4a: frame %d at [%d,%d) outside stream %d: %w",
			rd.nextSample, off, off+int64(size), rd.streamLen, ErrCorrupt)
	}
	if int64(size) > maxInt {
		return 0, fmt.Errorf("go-m4a: frame %d of %d bytes exceeds addressable memory: %w",
			rd.nextSample, size, ErrCorrupt)
	}
	return size, nil
}

// readFrameInto seeks to the current frame, reads exactly size bytes into
// dst[:size], and advances the cursor. dst must have length at least size (the
// caller sizes it from nextFrameSize). Splitting the read from allocation lets
// RawStream reuse one buffer across frames instead of allocating per frame.
func (rd *Reader) readFrameInto(dst []byte, size uint32) error {
	off := rd.curOffset
	if _, err := rd.r.Seek(off, io.SeekStart); err != nil {
		return fmt.Errorf("go-m4a: seek frame %d: %w", rd.nextSample, err)
	}
	if _, err := io.ReadFull(rd.r, dst[:size]); err != nil {
		// Bounds were checked in nextFrameSize, so a short read here is a
		// truncated or misbehaving stream. Fold in the message as text rather
		// than wrapping the error, so a stray io.EOF cannot masquerade as a
		// clean end of frames.
		return fmt.Errorf("go-m4a: read frame %d: %s: %w", rd.nextSample, err.Error(), ErrCorrupt)
	}
	rd.advance(size)
	return nil
}

// advance moves the running cursor past the just-read sample, stepping to the
// next non-empty chunk when the current one is exhausted.
func (rd *Reader) advance(size uint32) {
	rd.nextSample++
	rd.sampleInChunk++
	rd.curOffset += int64(size)
	if rd.nextSample >= rd.sampleCount {
		return
	}
	for rd.curChunk < len(rd.samplesPerChunk) && rd.sampleInChunk >= rd.samplesPerChunk[rd.curChunk] {
		rd.curChunk++
		rd.sampleInChunk = 0
		if rd.curChunk < len(rd.chunkOffsets) {
			rd.curOffset = rd.chunkOffsets[rd.curChunk]
		}
	}
}

// RawStream returns an io.Reader that emits each access unit framed as a 2-byte
// big-endian length prefix followed by the access-unit bytes, exactly the
// framing go-aac's pcm.WithRawStream consumes. It shares the Reader's cursor
// with ReadFrame and reports ErrUnsupported if any access unit exceeds 65535
// bytes (impossible for AAC-LC, guarded regardless).
func (rd *Reader) RawStream() io.Reader {
	return &rawReader{rd: rd}
}

// rawReader adapts sequential frames into a length-prefixed byte stream,
// buffering one framed access unit at a time. It reuses a single scratch buffer
// across frames (grown to the largest frame seen), so the streaming decode path
// allocates nothing per frame after warm-up rather than the two heap buffers a
// ReadFrame-then-copy adapter would.
type rawReader struct {
	rd      *Reader
	scratch []byte // reused: 2-byte big-endian length prefix followed by the access unit
	buf     []byte // unread window into scratch
	err     error
}

// Read fills p from the buffered frame, fetching and framing the next access
// unit directly into the reused scratch buffer when the window drains. It
// surfaces io.EOF at the end of the stream and caches a terminal error so later
// calls stay consistent.
func (rr *rawReader) Read(p []byte) (int, error) {
	for len(rr.buf) == 0 {
		if rr.err != nil {
			return 0, rr.err
		}
		size, err := rr.rd.nextFrameSize()
		if err != nil {
			rr.err = err
			return 0, err
		}
		if size > 0xffff {
			rr.err = fmt.Errorf("go-m4a: access unit %d bytes exceeds 65535: %w", size, ErrUnsupported)
			return 0, rr.err
		}
		need := int(size) + 2
		if cap(rr.scratch) < need {
			rr.scratch = make([]byte, need)
		} else {
			rr.scratch = rr.scratch[:need]
		}
		binary.BigEndian.PutUint16(rr.scratch, uint16(size))
		if err := rr.rd.readFrameInto(rr.scratch[2:need], size); err != nil {
			rr.err = err
			return 0, err
		}
		rr.buf = rr.scratch[:need]
	}
	n := copy(p, rr.buf)
	rr.buf = rr.buf[n:]
	return n, nil
}

// wrapParse turns a box-package parse error into a wrapped ErrCorrupt, unless it
// already carries a public sentinel (ErrCorrupt or ErrUnsupported), in which
// case it passes through unchanged. This keeps the descriptive box message while
// making errors.Is(err, ErrCorrupt) hold for low-level malformations.
func wrapParse(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, ErrCorrupt) || errors.Is(err, ErrUnsupported) {
		return err
	}
	return fmt.Errorf("go-m4a: %w: %w", err, ErrCorrupt)
}

// objectTypeMP4Audio is the esds DecoderConfigDescriptor objectTypeIndication
// for MPEG-4 Audio (ISO/IEC 14496-3), which covers AAC. The container derives
// AAC-LC specifics from the ASC rather than this registration byte.
const objectTypeMP4Audio = 0x40

// maxInt is the largest value the platform int can hold: math.MaxInt64 on a
// 64-bit build, math.MaxInt32 on a 32-bit one. A count or length parsed from a
// file as a 32-bit field is bounded against it before any make() or int
// conversion, so a hostile value cannot truncate to a negative int or panic
// make() on a 32-bit target (BirdNET-Go ships on 32-bit ARM).
const maxInt = int64(^uint(0) >> 1)
