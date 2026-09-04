// SPDX-License-Identifier: MIT

package m4a

// Codec identifies the audio codec of a track. It is the discriminator the reader
// reports in Info.Codec and the writer selects with WriterConfig.Codec. The zero
// value is CodecAACLC, so existing writers that set only ASC keep muxing AAC-LC.
type Codec uint8

const (
	// CodecAACLC is MPEG-4 AAC-LC, carried in an mp4a sample entry with an esds
	// box holding the AudioSpecificConfig. It is the zero value and the default.
	//
	// The reader identifies it from the container alone: an mp4a sample entry whose
	// esds objectTypeIndication is MPEG-4 Audio (0x40). That indication names the
	// whole MPEG-4 Audio family, not AAC-LC specifically, so the reader does not
	// verify the AudioSpecificConfig audio object type and reports CodecAACLC for an
	// HE-AAC or HE-AACv2 track too. Verifying the profile is the decoder's job: the
	// stdlib-only core cannot parse the codec bitstream, and aacm4a.NewDecoder
	// returns an error matching pcm.ErrUnsupportedSBR (HE-AAC) or pcm.ErrUnsupportedPS
	// (HE-AACv2) for a stream this container labels CodecAACLC but that is not
	// plain AAC-LC.
	CodecAACLC Codec = iota
	// CodecOpus is Opus, carried in an Opus sample entry with a dOps
	// OpusSpecificBox, per the Encapsulation of Opus in ISOBMFF.
	CodecOpus
	// CodecFLAC is FLAC, carried in a fLaC sample entry with a dfLa FLACSpecificBox
	// holding the STREAMINFO metadata block, per the Encapsulation of FLAC in
	// ISOBMFF.
	CodecFLAC
)

// String returns the codec's short name.
func (c Codec) String() string {
	switch c {
	case CodecAACLC:
		return "AAC-LC"
	case CodecOpus:
		return "Opus"
	case CodecFLAC:
		return "FLAC"
	default:
		return "unknown"
	}
}
