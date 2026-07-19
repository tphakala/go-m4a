// SPDX-License-Identifier: MIT

package box

import "math"

// Fixed field values shared by several boxes, from docs/box-layout.md.
const (
	rateUnity   = 0x00010000 // mvhd rate, 16.16 fixed point 1.0
	volumeFull  = 0x0100     // full audio volume, 8.8 fixed point 1.0
	nextTrackID = 2          // mvhd next_track_ID (track 1 is the audio track)
	languageUnd = 0x55C4     // "und", packed 5-bit ISO-639-2
	tkhdFlags   = 0x000007   // track enabled | in movie | in preview

	urlSelfContained = 0x000001 // dref "url " flag: media is in this file
)

// esds descriptor tags and fixed bytes (ISO/IEC 14496-1 and -3).
const (
	tagESDescriptor        = 0x03
	tagDecoderConfig       = 0x04
	tagDecoderSpecificInfo = 0x05
	tagSLConfig            = 0x06

	objectTypeAAC   = 0x40 // Audio ISO/IEC 14496-3 (AAC)
	streamTypeAudio = 0x15 // streamType 5 (audio) <<2 | upStream 0 <<1 | reserved 1
	slPredefinedMP4 = 0x02 // MP4 predefined SLConfigDescriptor
)

// unityMatrix is the 3x3 transform written into mvhd and tkhd as nine 16.16 /
// 2.30 fixed-point int32 values.
var unityMatrix = [9]int32{
	0x00010000, 0, 0,
	0, 0x00010000, 0,
	0, 0, 0x40000000,
}

func appendMatrix(dst []byte) []byte {
	for _, m := range unityMatrix {
		dst = appendI32(dst, m)
	}
	return dst
}

// Constants the writer needs when patching the streamed mdat box at Close.
const (
	// MdatHeaderSize is the length in bytes of the 64-bit largesize mdat header
	// emitted by AppendMdatHeader.
	MdatHeaderSize = 16
	// MdatLargeSizeOffset is the byte offset, within that header, of the 8-byte
	// big-endian largesize field the writer overwrites at Close.
	MdatLargeSizeOffset = 8
)

// AppendMdatHeader appends the 16-byte mdat box header in the 64-bit largesize
// form with a placeholder largesize of 0: size==1 (uint32), "mdat", then the
// largesize (uint64) at MdatLargeSizeOffset. The raw access-unit payload
// follows immediately. At Close the writer seeks to MdatLargeSizeOffset and
// overwrites the largesize with MdatHeaderSize + len(payload); AppendMdatLargeSize
// encodes that value.
func AppendMdatHeader(dst []byte) []byte {
	return AppendLargeBoxHeader(dst, 0, fourCCMdat)
}

// AppendMdatLargeSize appends the 8-byte big-endian largesize the writer writes
// at MdatLargeSizeOffset to finalize the streamed mdat box. largesize is the
// total box length: MdatHeaderSize + payload length.
func AppendMdatLargeSize(dst []byte, largesize uint64) []byte {
	return appendU64(dst, largesize)
}

// AppendFtyp appends the ftyp box: major_brand, minor_version, then the
// compatible brands. Per the contract the writer passes major "M4A ", minor 0,
// and compatible {"M4A ", "mp42", "isom"}.
func AppendFtyp(dst []byte, majorBrand FourCC, minorVersion uint32, compatibleBrands ...FourCC) []byte {
	size := uint32(8 + 8 + 4*len(compatibleBrands))
	dst = AppendBoxHeader(dst, size, fourCCFtyp)
	dst = append(dst, majorBrand[:]...)
	dst = appendU32(dst, minorVersion)
	for _, b := range compatibleBrands {
		dst = append(dst, b[:]...)
	}
	return dst
}

// AppendMvhd appends the mvhd (movie header) FullBox. version 1 (64-bit
// creation/modification/duration) is selected when duration exceeds 32 bits,
// otherwise version 0. creation and modification times are 0.
func AppendMvhd(dst []byte, timescale uint32, duration uint64) []byte {
	v1 := duration > math.MaxUint32
	version := uint8(0)
	size := uint32(108)
	if v1 {
		version = 1
		size = 120
	}
	dst = AppendFullBoxHeader(dst, size, fourCCMvhd, version, 0)
	if v1 {
		dst = appendU64(dst, 0) // creation_time
		dst = appendU64(dst, 0) // modification_time
		dst = appendU32(dst, timescale)
		dst = appendU64(dst, duration)
	} else {
		dst = appendU32(dst, 0) // creation_time
		dst = appendU32(dst, 0) // modification_time
		dst = appendU32(dst, timescale)
		dst = appendU32(dst, uint32(duration))
	}
	dst = appendI32(dst, rateUnity)
	dst = appendI16(dst, volumeFull)
	dst = appendI16(dst, 0)  // reserved
	dst = appendU32(dst, 0)  // reserved[0]
	dst = appendU32(dst, 0)  // reserved[1]
	dst = appendMatrix(dst)  // unity matrix
	for i := 0; i < 6; i++ { // pre_defined[6]
		dst = appendU32(dst, 0)
	}
	dst = appendU32(dst, nextTrackID)
	return dst
}

// AppendTkhd appends the tkhd (track header) FullBox with flags 0x000007
// (enabled | in movie | in preview). version 1 is selected when duration
// exceeds 32 bits. duration is in the movie timescale.
func AppendTkhd(dst []byte, trackID uint32, duration uint64) []byte {
	v1 := duration > math.MaxUint32
	version := uint8(0)
	size := uint32(92)
	if v1 {
		version = 1
		size = 104
	}
	dst = AppendFullBoxHeader(dst, size, fourCCTkhd, version, tkhdFlags)
	if v1 {
		dst = appendU64(dst, 0) // creation_time
		dst = appendU64(dst, 0) // modification_time
		dst = appendU32(dst, trackID)
		dst = appendU32(dst, 0) // reserved
		dst = appendU64(dst, duration)
	} else {
		dst = appendU32(dst, 0) // creation_time
		dst = appendU32(dst, 0) // modification_time
		dst = appendU32(dst, trackID)
		dst = appendU32(dst, 0) // reserved
		dst = appendU32(dst, uint32(duration))
	}
	dst = appendU32(dst, 0)          // reserved[0]
	dst = appendU32(dst, 0)          // reserved[1]
	dst = appendI16(dst, 0)          // layer
	dst = appendI16(dst, 0)          // alternate_group
	dst = appendI16(dst, volumeFull) // volume (audio => 0x0100)
	dst = appendI16(dst, 0)          // reserved
	dst = appendMatrix(dst)          // unity matrix
	dst = appendU32(dst, 0)          // width (16.16)
	dst = appendU32(dst, 0)          // height (16.16)
	return dst
}

// AppendElst appends the edit-list elst FullBox with a single entry. version 1
// (64-bit segment_duration and media_time) is selected when segment_duration
// needs more than 31 bits (it is signed-effective at version 0) or media_time
// falls outside the int32 range; otherwise version 0. media_rate is fixed 1.0.
func AppendElst(dst []byte, segmentDuration uint64, mediaTime int64) []byte {
	v1 := segmentDuration > math.MaxInt32 || mediaTime > math.MaxInt32 || mediaTime < math.MinInt32
	version := uint8(0)
	size := uint32(28)
	if v1 {
		version = 1
		size = 36
	}
	dst = AppendFullBoxHeader(dst, size, fourCCElst, version, 0)
	dst = appendU32(dst, 1) // entry_count
	if v1 {
		dst = appendU64(dst, segmentDuration)
		dst = appendI64(dst, mediaTime)
	} else {
		dst = appendU32(dst, uint32(segmentDuration))
		dst = appendI32(dst, int32(mediaTime))
	}
	dst = appendI16(dst, 1) // media_rate_integer (1.0)
	dst = appendI16(dst, 0) // media_rate_fraction
	return dst
}

// AppendMdhd appends the mdhd (media header) FullBox. version 1 is selected
// when duration exceeds 32 bits. duration counts all decoded media samples
// (FrameCount * 1024). language is packed "und".
func AppendMdhd(dst []byte, timescale uint32, duration uint64) []byte {
	v1 := duration > math.MaxUint32
	version := uint8(0)
	size := uint32(32)
	if v1 {
		version = 1
		size = 44
	}
	dst = AppendFullBoxHeader(dst, size, fourCCMdhd, version, 0)
	if v1 {
		dst = appendU64(dst, 0) // creation_time
		dst = appendU64(dst, 0) // modification_time
		dst = appendU32(dst, timescale)
		dst = appendU64(dst, duration)
	} else {
		dst = appendU32(dst, 0) // creation_time
		dst = appendU32(dst, 0) // modification_time
		dst = appendU32(dst, timescale)
		dst = appendU32(dst, uint32(duration))
	}
	dst = appendU16(dst, languageUnd)
	dst = appendU16(dst, 0) // pre_defined
	return dst
}

// AppendHdlr appends the hdlr (handler reference) FullBox. handlerType is
// "soun" for audio and name is a human-readable, NUL-terminated UTF-8 string
// ("SoundHandler").
func AppendHdlr(dst []byte, handlerType FourCC, name string) []byte {
	// body = pre_defined(4) + handler_type(4) + reserved[3](12) + name + NUL.
	size := uint32(12 + 4 + 4 + 12 + len(name) + 1)
	dst = AppendFullBoxHeader(dst, size, fourCCHdlr, 0, 0)
	dst = appendU32(dst, 0) // pre_defined
	dst = append(dst, handlerType[:]...)
	dst = appendU32(dst, 0) // reserved[0]
	dst = appendU32(dst, 0) // reserved[1]
	dst = appendU32(dst, 0) // reserved[2]
	dst = append(dst, name...)
	return append(dst, 0) // NUL terminator
}

// AppendSmhd appends the smhd (sound media header) FullBox: balance 0.
func AppendSmhd(dst []byte) []byte {
	dst = AppendFullBoxHeader(dst, 16, fourCCSmhd, 0, 0)
	dst = appendI16(dst, 0) // balance
	dst = appendI16(dst, 0) // reserved
	return dst
}

// AppendURLEntry appends the self-contained "url " data-entry FullBox: flags
// 0x000001 (media in this file) and no body.
func AppendURLEntry(dst []byte) []byte {
	return AppendFullBoxHeader(dst, 12, fourCCURL, 0, urlSelfContained)
}

// AppendDref appends the dref (data reference) FullBox carrying a single
// self-contained "url " entry.
func AppendDref(dst []byte) []byte {
	url := AppendURLEntry(nil)
	size := uint32(12 + 4 + len(url)) // header + entry_count + url entry
	dst = AppendFullBoxHeader(dst, size, fourCCDref, 0, 0)
	dst = appendU32(dst, 1) // entry_count
	return append(dst, url...)
}

// AppendDinf appends the dinf container box holding dref > "url ".
func AppendDinf(dst []byte) []byte {
	return AppendContainer(dst, fourCCDinf, AppendDref(nil))
}

// AppendEsds appends the esds (elementary stream descriptor) FullBox for an
// AAC-LC track: ES_Descriptor > DecoderConfigDescriptor (objectType 0x40,
// streamType audio) > DecoderSpecificInfo (asc), plus the mandatory
// SLConfigDescriptor (predefined 0x02). asc is the AudioSpecificConfig.
func AppendEsds(dst, asc []byte) []byte {
	// DecoderSpecificInfo (0x05): payload is the ASC bytes.
	dsi := appendDescriptor(nil, tagDecoderSpecificInfo, asc)

	// DecoderConfigDescriptor (0x04): fixed fields then the DecoderSpecificInfo.
	var dcd []byte
	dcd = append(dcd, objectTypeAAC, streamTypeAudio)
	dcd = appendU24(dcd, 0) // bufferSizeDB
	dcd = appendU32(dcd, 0) // maxBitrate
	dcd = appendU32(dcd, 0) // avgBitrate
	dcd = append(dcd, dsi...)
	dcdDesc := appendDescriptor(nil, tagDecoderConfig, dcd)

	// SLConfigDescriptor (0x06): MP4 predefined, mandatory in MP4.
	sl := appendDescriptor(nil, tagSLConfig, []byte{slPredefinedMP4})

	// ES_Descriptor (0x03): ES_ID, flags, then the two child descriptors.
	var es []byte
	es = appendU16(es, 0) // ES_ID
	es = append(es, 0)    // flags (no dependence, URL, or OCR stream)
	es = append(es, dcdDesc...)
	es = append(es, sl...)
	esDesc := appendDescriptor(nil, tagESDescriptor, es)

	size := uint32(12 + len(esDesc)) // FullBox header + ES_Descriptor
	dst = AppendFullBoxHeader(dst, size, fourCCEsds, 0, 0)
	return append(dst, esDesc...)
}

// appendDescriptor appends one ISO/IEC 14496-1 descriptor: tag, expandable
// size, then payload.
func appendDescriptor(dst []byte, tag byte, payload []byte) []byte {
	dst = append(dst, tag)
	dst = AppendDescriptorSize(dst, uint32(len(payload)))
	return append(dst, payload...)
}

// appendAudioSampleEntry appends the AudioSampleEntry shared by the mp4a, Opus,
// and fLaC sample entries: the box header for typ, then the 28 bytes of fixed
// fields (channelcount, samplesize 16, samplerate as a 16.16 value of SampleRate),
// then the codec-specific child box (esds, dOps, or dfLa). The samplerate 16.16
// field is valid for rates <= 65535 Hz, which covers 44100/48000 and the 48000 Hz
// Opus timescale.
func appendAudioSampleEntry(dst []byte, typ FourCC, channels uint16, sampleRate uint32, child []byte) []byte {
	size := uint32(8 + 28 + len(child))
	dst = AppendBoxHeader(dst, size, typ)
	dst = append(dst, 0, 0, 0, 0, 0, 0)  // reserved[6]
	dst = appendU16(dst, 1)              // data_reference_index
	dst = appendU32(dst, 0)              // reserved[0]
	dst = appendU32(dst, 0)              // reserved[1]
	dst = appendU16(dst, channels)       // channelcount
	dst = appendU16(dst, 16)             // samplesize
	dst = appendI16(dst, 0)              // pre_defined
	dst = appendI16(dst, 0)              // reserved
	dst = appendU32(dst, sampleRate<<16) // samplerate (16.16)
	return append(dst, child...)
}

// AppendMp4a appends the mp4a AudioSampleEntry followed by the esds child carrying
// asc.
func AppendMp4a(dst []byte, channels uint16, sampleRate uint32, asc []byte) []byte {
	return appendAudioSampleEntry(dst, fourCCMp4a, channels, sampleRate, AppendEsds(nil, asc))
}

// AppendDops appends the OpusSpecificBox (dOps), per the Encapsulation of Opus in
// ISOBMFF: Version(1)=0, OutputChannelCount(1), PreSkip(2), InputSampleRate(4),
// OutputGain(2)=0, ChannelMappingFamily(1)=0. Mono and stereo use mapping family
// 0, so no channel mapping table follows.
func AppendDops(dst []byte, channels uint8, preSkip uint16, inputSampleRate uint32) []byte {
	dst = AppendBoxHeader(dst, 8+11, fourCCDops)
	dst = append(dst, 0)          // Version
	dst = append(dst, channels)   // OutputChannelCount
	dst = appendU16(dst, preSkip) // PreSkip (48 kHz samples)
	dst = appendU32(dst, inputSampleRate)
	dst = appendU16(dst, 0) // OutputGain (Q7.8) = 0
	dst = append(dst, 0)    // ChannelMappingFamily = 0
	return dst
}

// AppendOpusEntry appends the Opus AudioSampleEntry with its dOps child. sampleRate
// is the container rate, which the Opus encapsulation fixes at 48000.
func AppendOpusEntry(dst []byte, channels uint16, sampleRate uint32, dops []byte) []byte {
	return appendAudioSampleEntry(dst, fourCCOpus, channels, sampleRate, dops)
}

// AppendDfla appends the FLACSpecificBox (dfLa), per the Encapsulation of FLAC in
// ISOBMFF: a FullBox (version 0, flags 0) then the STREAMINFO metadata block, a
// 1-byte last-flag|type header (0x80 | 0), a 24-bit length, and the streamInfo
// body (34 bytes).
func AppendDfla(dst []byte, streamInfo []byte) []byte {
	size := uint32(12 + 4 + len(streamInfo)) // FullBox header + block header + body
	dst = AppendFullBoxHeader(dst, size, fourCCDfla, 0, 0)
	dst = append(dst, 0x80) // last-metadata-block flag | block type 0 (STREAMINFO)
	dst = appendU24(dst, uint32(len(streamInfo)))
	return append(dst, streamInfo...)
}

// AppendFlacEntry appends the fLaC AudioSampleEntry with its dfLa child.
func AppendFlacEntry(dst []byte, channels uint16, sampleRate uint32, dfla []byte) []byte {
	return appendAudioSampleEntry(dst, fourCCFlac, channels, sampleRate, dfla)
}

// AppendStsdEntry appends the stsd (sample description) FullBox holding one
// pre-built sample entry (mp4a, Opus, or fLaC), so the writer chooses the codec
// entry and this box stays codec-agnostic.
func AppendStsdEntry(dst []byte, entry []byte) []byte {
	size := uint32(12 + 4 + len(entry)) // header + entry_count + entry
	dst = AppendFullBoxHeader(dst, size, fourCCStsd, 0, 0)
	dst = appendU32(dst, 1) // entry_count
	return append(dst, entry...)
}

// AppendStsd appends the stsd FullBox holding one mp4a entry for an AAC-LC track.
func AppendStsd(dst []byte, channels uint16, sampleRate uint32, asc []byte) []byte {
	return AppendStsdEntry(dst, AppendMp4a(nil, channels, sampleRate, asc))
}

// SttsRun is one run of the stts (decoding time to sample) table: Count
// consecutive samples each of duration Delta in the media timescale.
type SttsRun struct {
	Count uint32
	Delta uint32
}

// AppendStts appends the stts FullBox with a single run: sampleCount samples each
// of duration sampleDelta. It is the fixed-duration case (AAC-LC, every AU 1024
// samples) of AppendSttsRuns.
func AppendStts(dst []byte, sampleCount, sampleDelta uint32) []byte {
	return AppendSttsRuns(dst, []SttsRun{{Count: sampleCount, Delta: sampleDelta}})
}

// AppendSttsRuns appends the stts FullBox from run-length-encoded per-sample
// durations, for codecs (Opus, FLAC) whose frames do not all decode to the same
// number of samples, so the final frame's shorter duration needs its own run.
func AppendSttsRuns(dst []byte, runs []SttsRun) []byte {
	size := uint32(12 + 4 + 8*len(runs)) // FullBox header + entry_count + entries
	dst = AppendFullBoxHeader(dst, size, fourCCStts, 0, 0)
	dst = appendU32(dst, uint32(len(runs))) // entry_count
	for _, r := range runs {
		dst = appendU32(dst, r.Count)
		dst = appendU32(dst, r.Delta)
	}
	return dst
}

// AppendStsc appends the stsc (sample to chunk) FullBox with a single entry
// mapping every sample into one chunk.
func AppendStsc(dst []byte, firstChunk, samplesPerChunk, sampleDescriptionIndex uint32) []byte {
	dst = AppendFullBoxHeader(dst, 28, fourCCStsc, 0, 0)
	dst = appendU32(dst, 1) // entry_count
	dst = appendU32(dst, firstChunk)
	dst = appendU32(dst, samplesPerChunk)
	dst = appendU32(dst, sampleDescriptionIndex)
	return dst
}

// AppendStsz appends the stsz (sample size) FullBox with sample_size 0 and one
// entry per access unit giving its byte length.
func AppendStsz(dst []byte, sizes []uint32) []byte {
	size := uint32(12 + 4 + 4 + 4*len(sizes)) // header + sample_size + sample_count + entries
	dst = AppendFullBoxHeader(dst, size, fourCCStsz, 0, 0)
	dst = appendU32(dst, 0)                  // sample_size 0 => per-sample entries follow
	dst = appendU32(dst, uint32(len(sizes))) // sample_count
	for _, s := range sizes {
		dst = appendU32(dst, s)
	}
	return dst
}

// AppendStco appends the stco (32-bit chunk offset) FullBox. The writer uses a
// single offset; the general slice form keeps it reusable.
func AppendStco(dst []byte, offsets []uint32) []byte {
	size := uint32(12 + 4 + 4*len(offsets)) // header + entry_count + entries
	dst = AppendFullBoxHeader(dst, size, fourCCStco, 0, 0)
	dst = appendU32(dst, uint32(len(offsets))) // entry_count
	for _, o := range offsets {
		dst = appendU32(dst, o)
	}
	return dst
}

// AppendCo64 appends the co64 (64-bit chunk offset) FullBox, the 64-bit
// counterpart to AppendStco. The writer never emits it, because its single
// chunk offset always fits uint32; it is kept for symmetry with ParseCo64 so
// the marshaler and parser cover the same box set, and it backs the reader's
// co64 round-trip test.
func AppendCo64(dst []byte, offsets []uint64) []byte {
	size := uint32(12 + 4 + 8*len(offsets)) // header + entry_count + entries
	dst = AppendFullBoxHeader(dst, size, fourCCCo64, 0, 0)
	dst = appendU32(dst, uint32(len(offsets))) // entry_count
	for _, o := range offsets {
		dst = appendU64(dst, o)
	}
	return dst
}

// Container assemblers. Each wraps pre-built child bytes and computes its own
// size, so the writer controls the exact set and order of children (for
// example stco versus co64, or omitting edts when there is no edit list).

// AppendMoov appends the moov container wrapping children (mvhd, trak).
func AppendMoov(dst, children []byte) []byte { return AppendContainer(dst, fourCCMoov, children) }

// AppendTrak appends the trak container wrapping children (tkhd, edts, mdia).
func AppendTrak(dst, children []byte) []byte { return AppendContainer(dst, fourCCTrak, children) }

// AppendEdts appends the edts container wrapping children (elst).
func AppendEdts(dst, children []byte) []byte { return AppendContainer(dst, fourCCEdts, children) }

// AppendMdia appends the mdia container wrapping children (mdhd, hdlr, minf).
func AppendMdia(dst, children []byte) []byte { return AppendContainer(dst, fourCCMdia, children) }

// AppendMinf appends the minf container wrapping children (smhd, dinf, stbl).
func AppendMinf(dst, children []byte) []byte { return AppendContainer(dst, fourCCMinf, children) }

// AppendStbl appends the stbl container wrapping children (stsd, stts, stsc,
// stsz, stco or co64).
func AppendStbl(dst, children []byte) []byte { return AppendContainer(dst, fourCCStbl, children) }
