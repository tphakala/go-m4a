// SPDX-License-Identifier: MIT

// Package box implements the low-level ISO Base Media File Format (ISO/IEC
// 14496-12) byte primitives and typed marshalers that the go-m4a writer emits.
// Everything is big-endian and append-style: each writer takes a destination
// slice and returns it extended, allocation-free when dst has spare capacity,
// mirroring strconv.AppendInt and go-aac's AppendADTSHeader. The byte layout
// follows docs/box-layout.md exactly; that document is the contract.
package box

import "encoding/binary"

// FourCC is a four-character box or brand code, stored as raw bytes.
type FourCC [4]byte

// NewFourCC builds a FourCC from s. s should be exactly four bytes; a shorter
// string is right-padded with NUL and a longer one is truncated to four bytes.
func NewFourCC(s string) FourCC {
	var f FourCC
	copy(f[:], s)
	return f
}

// String returns the four bytes of the code as a string.
func (f FourCC) String() string {
	return string(f[:])
}

// Box type and brand codes used by the writer.
var (
	fourCCFtyp = NewFourCC("ftyp")
	fourCCMdat = NewFourCC("mdat")
	fourCCMoov = NewFourCC("moov")
	fourCCMvhd = NewFourCC("mvhd")
	fourCCTrak = NewFourCC("trak")
	fourCCTkhd = NewFourCC("tkhd")
	fourCCEdts = NewFourCC("edts")
	fourCCElst = NewFourCC("elst")
	fourCCMdia = NewFourCC("mdia")
	fourCCMdhd = NewFourCC("mdhd")
	fourCCHdlr = NewFourCC("hdlr")
	fourCCMinf = NewFourCC("minf")
	fourCCSmhd = NewFourCC("smhd")
	fourCCDinf = NewFourCC("dinf")
	fourCCDref = NewFourCC("dref")
	fourCCURL  = NewFourCC("url ")
	fourCCStbl = NewFourCC("stbl")
	fourCCStsd = NewFourCC("stsd")
	fourCCMp4a = NewFourCC("mp4a")
	fourCCEsds = NewFourCC("esds")
	fourCCOpus = NewFourCC("Opus")
	fourCCDops = NewFourCC("dOps")
	fourCCFlac = NewFourCC("fLaC")
	fourCCDfla = NewFourCC("dfLa")
	fourCCStts = NewFourCC("stts")
	fourCCStsc = NewFourCC("stsc")
	fourCCStsz = NewFourCC("stsz")
	fourCCStco = NewFourCC("stco")
	fourCCCo64 = NewFourCC("co64")
)

// AppendBoxHeader appends the 8-byte 32-bit box header: size (uint32, total box
// length including this header) then the four type bytes.
func AppendBoxHeader(dst []byte, size uint32, typ FourCC) []byte {
	dst = appendU32(dst, size)
	return append(dst, typ[:]...)
}

// AppendLargeBoxHeader appends the 16-byte 64-bit box header: size field == 1
// (the sentinel meaning "largesize follows"), the four type bytes, then the
// 64-bit largesize. mdat always uses this form so its size can grow past 4 GiB
// and be patched in place at Close without shifting the payload.
func AppendLargeBoxHeader(dst []byte, largesize uint64, typ FourCC) []byte {
	dst = appendU32(dst, 1)
	dst = append(dst, typ[:]...)
	return appendU64(dst, largesize)
}

// AppendFullBoxHeader appends a FullBox header: the 8-byte box header, then a
// 1-byte version and a 24-bit flags field. size is the total box length.
func AppendFullBoxHeader(dst []byte, size uint32, typ FourCC, version uint8, flags uint32) []byte {
	dst = AppendBoxHeader(dst, size, typ)
	dst = append(dst, version)
	return appendU24(dst, flags)
}

// AppendContainer appends a container box wrapping children: an 8-byte box
// header whose size is 8+len(children) followed by the children bytes. It is
// the recursive helper the parent boxes (moov, trak, mdia, minf, stbl, edts)
// use to compute their own size. children is assumed to be smaller than 4 GiB,
// which always holds for go-m4a's metadata boxes (only mdat can be large, and
// it uses the 64-bit form).
func AppendContainer(dst []byte, typ FourCC, children []byte) []byte {
	dst = AppendBoxHeader(dst, uint32(8+len(children)), typ)
	return append(dst, children...)
}

// AppendDescriptorSize appends the ISO/IEC 14496-1 expandable size encoding of
// size used inside esds descriptors: 7 bits per byte, most significant group
// first, with the high bit set on every byte except the last. The encoding is
// minimal, one byte for size < 128.
func AppendDescriptorSize(dst []byte, size uint32) []byte {
	// Split into 7-bit groups, least significant first.
	var groups [5]byte
	n := 0
	for {
		groups[n] = byte(size & 0x7f)
		size >>= 7
		n++
		if size == 0 {
			break
		}
	}
	// Emit most significant group first; continuation bit on all but the last.
	for i := n - 1; i > 0; i-- {
		dst = append(dst, groups[i]|0x80)
	}
	return append(dst, groups[0])
}

// Big-endian field append helpers. Kept unexported: callers use the typed box
// marshalers, not raw fields.

func appendU16(dst []byte, v uint16) []byte {
	return binary.BigEndian.AppendUint16(dst, v)
}

func appendU24(dst []byte, v uint32) []byte {
	return append(dst, byte(v>>16), byte(v>>8), byte(v))
}

func appendU32(dst []byte, v uint32) []byte {
	return binary.BigEndian.AppendUint32(dst, v)
}

func appendU64(dst []byte, v uint64) []byte {
	return binary.BigEndian.AppendUint64(dst, v)
}

func appendI16(dst []byte, v int16) []byte {
	return binary.BigEndian.AppendUint16(dst, uint16(v))
}

func appendI32(dst []byte, v int32) []byte {
	return binary.BigEndian.AppendUint32(dst, uint32(v))
}

func appendI64(dst []byte, v int64) []byte {
	return binary.BigEndian.AppendUint64(dst, uint64(v))
}
