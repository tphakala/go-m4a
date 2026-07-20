// SPDX-License-Identifier: MIT

package box

// Box type codes used only by the fragmented (CMAF) writer.
var (
	fourCCStyp = NewFourCC("styp")
	fourCCMvex = NewFourCC("mvex")
	fourCCTrex = NewFourCC("trex")
	fourCCMoof = NewFourCC("moof")
	fourCCMfhd = NewFourCC("mfhd")
	fourCCTraf = NewFourCC("traf")
	fourCCTfhd = NewFourCC("tfhd")
	fourCCTfdt = NewFourCC("tfdt")
	fourCCTrun = NewFourCC("trun")
)

// SyncSampleFlags is the sample_flags value for a sample every decoder can start
// from, which for audio is every sample. It packs sample_depends_on = 2 ("does
// not depend on others") into bits 24-25 and leaves sample_is_non_sync_sample
// clear, so no per-sample flags or sdtp box is needed for an audio track.
const SyncSampleFlags = 0x02000000

// tfhd flag bits (ISO/IEC 14496-12 8.8.7).
const (
	// tfhdDefaultBaseIsMoof makes every data offset in the fragment relative to
	// the start of its enclosing moof. CMAF requires it, and it is what makes a
	// segment relocatable; base_data_offset_present (0x000001) is correspondingly
	// forbidden, so this package never emits it.
	tfhdDefaultBaseIsMoof = 0x020000
	// tfhdDefaultSampleDurationPresent signals a fragment-wide sample duration,
	// letting trun omit a per-sample duration list.
	tfhdDefaultSampleDurationPresent = 0x000008
)

// trun flag bits (ISO/IEC 14496-12 8.8.8).
const (
	trunDataOffsetPresent     = 0x000001
	trunSampleDurationPresent = 0x000100
	trunSampleSizePresent     = 0x000200
)

// Fixed encoded sizes of the fragment boxes, so a writer can compute the total
// moof length before emitting any of it. trun's data_offset is measured from the
// first byte of the enclosing moof, which means it depends on moof's own length;
// predicting the size analytically keeps the byte stream purely sequential
// instead of requiring a patch-after-the-fact.
const (
	// MfhdSize is the encoded length of the mfhd box.
	MfhdSize = 16
	// TfdtSize is the encoded length of the 64-bit (version 1) tfdt box.
	TfdtSize = 20
	// MoofHeaderSize and TrafHeaderSize are the plain container headers.
	MoofHeaderSize = 8
	TrafHeaderSize = 8
	// MdatShortHeaderSize is the length of the plain 32-bit mdat header a media
	// segment uses, as opposed to the 64-bit MdatHeaderSize the non-fragmented
	// writer emits so it can patch the size in place at Close.
	MdatShortHeaderSize = 8
)

// AppendMdat appends a complete mdat box wrapping payload, using the plain 32-bit
// header. A media segment's payload is written all at once and is far below 4 GiB,
// so it needs neither the 64-bit form nor a size patched in afterwards.
func AppendMdat(dst, payload []byte) []byte {
	return AppendContainer(dst, fourCCMdat, payload)
}

// TfhdSize returns the encoded length of the tfhd box AppendTfhd would emit for
// defaultSampleDuration.
func TfhdSize(defaultSampleDuration uint32) int {
	size := 12 + 4 // FullBox header + track_ID
	if defaultSampleDuration != 0 {
		size += 4
	}
	return size
}

// TrunSize returns the encoded length of the trun box AppendTrun would emit for
// sampleCount samples, with per-sample durations included when withDurations is
// set.
func TrunSize(sampleCount int, withDurations bool) int {
	perSample := 4 // sample_size
	if withDurations {
		perSample += 4 // sample_duration
	}
	// FullBox header + sample_count + data_offset + the per-sample records.
	return 12 + 4 + 4 + perSample*sampleCount
}

// AppendStyp appends the styp (segment type) box, which has the same layout as
// ftyp and marks the start of a media segment.
func AppendStyp(dst []byte, majorBrand FourCC, minorVersion uint32, compatibleBrands ...FourCC) []byte {
	return appendBrandBox(dst, fourCCStyp, majorBrand, minorVersion, compatibleBrands...)
}

// AppendTrex appends the trex (track extends) FullBox, the per-track defaults a
// movie fragment inherits when its tfhd does not override them.
// defaultSampleDuration is zero for a codec whose frames vary in length, in which
// case each fragment supplies its own. default_sample_size stays 0 because
// compressed audio frames always vary in size, so trun carries them per sample.
func AppendTrex(dst []byte, trackID, defaultSampleDuration, defaultSampleFlags uint32) []byte {
	dst = AppendFullBoxHeader(dst, 32, fourCCTrex, 0, 0)
	dst = appendU32(dst, trackID)
	dst = appendU32(dst, 1) // default_sample_description_index (the single stsd entry)
	dst = appendU32(dst, defaultSampleDuration)
	dst = appendU32(dst, 0) // default_sample_size
	dst = appendU32(dst, defaultSampleFlags)
	return dst
}

// AppendMvex appends the mvex container wrapping children (trex). Its presence in
// moov is what declares the file fragmented.
func AppendMvex(dst, children []byte) []byte { return AppendContainer(dst, fourCCMvex, children) }

// AppendMoofHeader appends only the moof container header, with a size the caller
// has computed in advance, so the children can be appended straight after it into
// the same buffer. The usual assemble-children-then-wrap form would need an
// intermediate allocation per box, and a live stream emits a segment every couple
// of seconds. Predicting the sizes is required anyway, because trun's data_offset
// counts from the start of moof.
func AppendMoofHeader(dst []byte, size uint32) []byte {
	return AppendBoxHeader(dst, size, fourCCMoof)
}

// AppendTrafHeader appends the traf container header for a box of the given total
// size, so its children can follow directly. See AppendMoofHeader.
func AppendTrafHeader(dst []byte, size uint32) []byte {
	return AppendBoxHeader(dst, size, fourCCTraf)
}

// AppendMfhd appends the mfhd (movie fragment header) FullBox. sequenceNumber
// counts fragments from 1 and must increase by one per fragment.
func AppendMfhd(dst []byte, sequenceNumber uint32) []byte {
	dst = AppendFullBoxHeader(dst, MfhdSize, fourCCMfhd, 0, 0)
	return appendU32(dst, sequenceNumber)
}

// AppendTfhd appends the tfhd (track fragment header) FullBox. It always sets
// default-base-is-moof, so sample data is addressed relative to the enclosing
// moof and the segment stays relocatable, as CMAF requires. A non-zero
// defaultSampleDuration is emitted as the fragment-wide sample duration, which
// lets AppendTrun omit the per-sample duration list; pass 0 when the fragment's
// samples do not share one duration.
func AppendTfhd(dst []byte, trackID, defaultSampleDuration uint32) []byte {
	flags := uint32(tfhdDefaultBaseIsMoof)
	if defaultSampleDuration != 0 {
		flags |= tfhdDefaultSampleDurationPresent
	}
	dst = AppendFullBoxHeader(dst, uint32(TfhdSize(defaultSampleDuration)), fourCCTfhd, 0, flags)
	dst = appendU32(dst, trackID)
	if defaultSampleDuration != 0 {
		dst = appendU32(dst, defaultSampleDuration)
	}
	return dst
}

// AppendTfdt appends the tfdt (track fragment decode time) FullBox in its
// version 1, 64-bit form. Version 1 is unconditional: a 32-bit baseMediaDecodeTime
// at a 48 kHz timescale wraps after about 24.8 hours, which a live stream reaches.
func AppendTfdt(dst []byte, baseMediaDecodeTime uint64) []byte {
	dst = AppendFullBoxHeader(dst, TfdtSize, fourCCTfdt, 1, 0)
	return appendU64(dst, baseMediaDecodeTime)
}

// AppendTrun appends the trun (track fragment run) FullBox describing one run of
// samples. dataOffset is the distance in bytes from the start of the enclosing
// moof to the first sample byte (that is, the moof length plus the mdat header),
// which the caller computes from TrunSize and the other fixed sizes. sizes gives
// every sample's byte length. durations may be nil when the fragment's tfhd
// carries a default sample duration; otherwise it must be the same length as
// sizes. Sample flags are never written per sample: audio samples are all sync
// samples and inherit trex's default_sample_flags.
func AppendTrun(dst []byte, dataOffset int32, sizes, durations []uint32) []byte {
	// Make the documented precondition self-enforcing. A short durations slice
	// would otherwise panic partway through the loop, after dst had already been
	// extended with a half-written box.
	if durations != nil && len(durations) != len(sizes) {
		panic("go-m4a/box: AppendTrun: durations and sizes differ in length")
	}
	flags := uint32(trunDataOffsetPresent | trunSampleSizePresent)
	if durations != nil {
		flags |= trunSampleDurationPresent
	}
	size := uint32(TrunSize(len(sizes), durations != nil))
	dst = AppendFullBoxHeader(dst, size, fourCCTrun, 0, flags)
	dst = appendU32(dst, uint32(len(sizes))) // sample_count
	dst = appendI32(dst, dataOffset)
	for i, s := range sizes {
		if durations != nil {
			dst = appendU32(dst, durations[i])
		}
		dst = appendU32(dst, s)
	}
	return dst
}

// AppendStscEntries appends the stsc FullBox from an arbitrary entry list, the
// marshalling counterpart of ParseStsc and its StscEntry. A nil
// or empty list yields the entry_count 0 form a fragmented init segment needs,
// where the sample tables are structurally present but carry no samples.
func AppendStscEntries(dst []byte, entries []StscEntry) []byte {
	size := uint32(12 + 4 + 12*len(entries))
	dst = AppendFullBoxHeader(dst, size, fourCCStsc, 0, 0)
	dst = appendU32(dst, uint32(len(entries)))
	for _, e := range entries {
		dst = appendU32(dst, e.FirstChunk)
		dst = appendU32(dst, e.SamplesPerChunk)
		dst = appendU32(dst, e.SampleDescriptionIndex)
	}
	return dst
}
