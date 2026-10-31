// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jpegcheck

import "encoding/binary"

// Sequential (non-progressive) scans are validated by a dedicated bit reader
// over the whole in-memory input: a 64-bit accumulator refilled four bytes at
// a time, and a seqLutBits-bit look-up table that resolves a Huffman code and
// the length of its amplitude in one step. Coefficient values are never
// materialised; only the positions (zig) and the number of bits consumed
// matter, and those are exactly what image/jpeg computes.
//
// Whenever fewer than seqMinBits bits remain before a marker or EOF, the
// remainder of the block is handed to the reference reader (d.bits/d.bytes,
// slowSequentialBlock/slowSequentialAC), which is the unmodified image/jpeg
// logic. The two readers are kept in sync at block boundaries (seqSync /
// seqLoad), so every error and every "where does the scan end" decision is
// taken by the same rules as the standard library.

// seqLutBits is the log-2 size of the look-up table: a code of up to
// seqLutBits bits is resolved with a single lookup.
const seqLutBits = 10

// seqMinBits is the number of unread bits that must be present in the
// accumulator at the start of each symbol for the fast path to proceed without
// any further availability checks: a Huffman code (<= 16 bits) plus an
// amplitude (<= 16 bits). A four-byte refill leaves 32..63 bits.
const seqMinBits = 32

// seq table entry layout (uint16):
//
//	bits 0..4   bits consumed (code and amplitude for each ordinary
//	            coefficient, code alone for EOB/ZRL or an invalid DC size)
//	bits 5..12  the decoded symbol, or the zig-zag advance for a short AC code or pair
//	bit 15      entry encodes a zig-zag advance
//	0           the code is longer than seqLutBits bits (use the slow path)
const (
	seqEntryBitsMask = 0x1f
	seqEntrySymShift = 5
	seqAdvanceFlag   = 1 << 15
)

// seqReader is the fast bit reader. The top nb bits of acc are unread (MSB
// first) and the bits below them are zero. pos is the next raw input byte.
// base is the first byte after the scan start or restart marker.
type seqReader struct {
	data []byte
	pos  int
	base int
	acc  uint64
	nb   int
}

// buildSeqLut derives h.seq from the already-built 8-bit h.lut and the
// canonical (minCodes/maxCodes/valsIndices) tables, by simulating exactly what
// decodeHuffman does with at least 8 bits available: the 8-bit lut if it hits,
// the bit-by-bit canonical search otherwise. Malformed (over-subscribed) tables
// therefore resolve to the same symbols as in image/jpeg.
func (h *huffman) buildSeqLut(isAC, prefixFree bool) {
	for idx := range h.seq {
		var sym uint8
		var length int
		if v := h.lut[idx>>(seqLutBits-lutSize)]; v != 0 {
			sym = uint8(v >> 8)
			length = int(v&0xff) - 1
		} else {
			code := int32(0)
			found := false
			for i := 0; i < seqLutBits; i++ {
				code = code<<1 | int32(idx>>(seqLutBits-1-i))&1
				if code <= h.maxCodes[i] {
					sym = h.vals[h.valsIndices[i]+code-h.minCodes[i]]
					length = i + 1
					found = true
					break
				}
			}
			if !found {
				h.seq[idx] = 0
				continue
			}
		}
		total := length
		if isAC {
			total += int(sym & 15)
		} else if sym <= 16 {
			total += int(sym)
		}
		entry := uint16(total) | uint16(sym)<<seqEntrySymShift
		if isAC && prefixFree && sym&15 != 0 && length <= lutSize {
			advance := 1 + int(sym>>4)
			if total < seqLutBits {
				// Skip the first amplitude and align the remaining prefix
				// for the 8-bit LUT. Use only a fully determined second code.
				suffix := (idx << total) & (len(h.seq) - 1)
				next := h.lut[suffix>>(seqLutBits-lutSize)]
				length2 := int(next&0xff) - 1
				size2 := int(next>>8) & 15
				if size2 != 0 && total+length2 <= seqLutBits {
					advance += 1 + int(next>>12)
					total += length2 + size2
				}
			}
			entry = seqAdvanceFlag | uint16(advance)<<seqEntrySymShift | uint16(total)
		}
		h.seq[idx] = entry
	}
}

// seqInit points the fast reader at the reference reader's current position
// with an empty bit buffer (scan start, or just after a restart marker).
func (d *decoder) seqInit() {
	d.seq = seqReader{data: d.bytes.buf, pos: d.bytes.i, base: d.bytes.i}
}

// seqLoad copies the reference reader's state into the fast reader.
func (d *decoder) seqLoad() {
	r := &d.seq
	r.pos = d.bytes.i
	r.nb = int(d.bits.n)
	r.acc = (uint64(d.bits.a) & (uint64(1)<<uint(r.nb) - 1)) << uint(64-r.nb)
}

// seqSync copies the fast reader's state back into the reference reader. Whole
// unread bytes are given back (walking over 0xff 0x00 stuffing), leaving fewer
// than 8 bits in d.bits: this is precisely the state image/jpeg is in after
// readFull's unreadByteStuffedByte at the end of a scan or before a RST marker.
func (d *decoder) seqSync() {
	r := &d.seq
	k := r.nb >> 3
	rem := r.nb & 7
	p := r.pos
	for ; k > 0; k-- {
		p--
		if r.data[p] == 0x00 && p > r.base && r.data[p-1] == 0xff {
			p--
		}
	}
	d.bytes.i = p
	d.bytes.nUnreadable = 0
	d.bits.n = int32(rem)
	d.bits.a = uint32(r.acc >> uint(64-rem))
	d.bits.m = (uint32(1) << uint(rem)) >> 1
}

// seqAppendWord appends four bytes when they are available without byte
// stuffing. The caller has fewer than 32 unread bits, so the word always fits.
func seqAppendWord(data []byte, acc uint64, nb int) (uint64, bool) {
	if len(data) < 4 {
		return acc, false
	}
	word := binary.BigEndian.Uint32(data)
	// A zero byte in the complement is a 0xff in the input.
	x := ^word
	if (x-0x01010101)&^x&0x80808080 != 0 {
		return acc, false
	}
	return acc | uint64(word)<<(uint(32-nb)&63), true
}

// seqRefillSlow tops the accumulator up byte by byte, undoing 0xff 0x00 byte
// stuffing and stopping at a marker (0xff followed by anything else) or EOF.
// It is kept out of line to keep the scan loop small.
//
//go:noinline
func seqRefillSlow(data []byte, pos int, acc uint64, nb int) (int, uint64, int) {
	for nb <= 56 && pos < len(data) {
		b := data[pos]
		if b == 0xff {
			if pos+1 < len(data) && data[pos+1] == 0x00 {
				pos += 2
			} else {
				break
			}
		} else {
			pos++
		}
		acc |= uint64(b) << uint(56-nb)
		nb += 8
	}
	return pos, acc, nb
}

// seqSlowHuffman resolves a code longer than seqLutBits bits: the canonical
// search of decodeHuffman's slow path, continued from length seqLutBits+1
// (buildSeqLut has already established that no shorter code matches). The
// caller guarantees nb >= 16.
func seqSlowHuffman(h *huffman, acc uint64) (sym uint8, length int, err error) {
	for l := seqLutBits + 1; l <= maxCodeLength; l++ {
		code := int32(acc >> uint(64-l))
		if code <= h.maxCodes[l-1] {
			return h.vals[h.valsIndices[l-1]+code-h.minCodes[l-1]], l, nil
		}
	}
	return 0, 0, FormatError("bad Huffman code")
}

// validateSequentialScan is processSOS's MCU loop for a sequential scan: the
// same traversal (including the non-interleaved block skipping and the
// restart handling), with the per-block coefficient decoding replaced by
// bit-exact consumption on the fast reader.
func (d *decoder) validateSequentialScan(scan *[maxComponents]scanComponent, nComp, mxx, myy int) error {
	d.seqInit()
	r := &d.seq
	data, pos, acc, nb := r.data, r.pos, r.acc, r.nb
	mcu, expectedRST := 0, uint8(rst0Marker)
	blockCount := 0
	for my := 0; my < myy; my++ {
		for mx := 0; mx < mxx; mx++ {
			for i := 0; i < nComp; i++ {
				compIndex := scan[i].compIndex
				hi := d.comp[compIndex].h
				vi := d.comp[compIndex].v
				dc := &d.huff[dcTable][scan[i].td]
				ac := &d.huff[acTable][scan[i].ta]
				for j := 0; j < hi*vi; j++ {
					if nComp == 1 {
						// Non-interleaved: blocks are traversed left to right, top
						// to bottom, and blocks outside the image carry no data.
						q := mxx * hi
						bx := blockCount % q
						by := blockCount / q
						blockCount++
						if bx*8 >= d.width || by*8 >= d.height {
							continue
						}
					}

					if nb < seqMinBits {
						if next, ok := seqAppendWord(data[pos:], acc, nb); ok {
							pos, acc, nb = pos+4, next, nb+32
						} else {
							pos, acc, nb = seqRefillSlow(data, pos, acc, nb)
						}
					}
					if nb < seqMinBits {
						// Near a marker or EOF: the reference reader takes this block.
						r.pos, r.acc, r.nb = pos, acc, nb
						d.seqSync()
						if err := d.slowSequentialBlock(dc, ac); err != nil {
							return err
						}
						d.seqLoad()
						pos, acc, nb = r.pos, r.acc, r.nb
						continue
					}

					// DC coefficient (section F.2.2.1).
					if dc.nCodes == 0 {
						return FormatError("uninitialized Huffman table")
					}
					if e := dc.seq[acc>>(64-seqLutBits)]; e != 0 {
						if sym := e >> seqEntrySymShift & 0xff; sym > 16 {
							return UnsupportedError("excessive DC component")
						}
						k := uint(e & seqEntryBitsMask)
						acc <<= k
						nb -= int(k)
					} else {
						sym, l, err := seqSlowHuffman(dc, acc)
						if err != nil {
							return err
						}
						if sym > 16 {
							return UnsupportedError("excessive DC component")
						}
						k := l + int(sym)
						acc <<= uint(k)
						nb -= k
					}

					if d.eobRun > 0 {
						d.eobRun--
						continue
					}

					// AC coefficients (section F.2.2.2).
					if ac.nCodes == 0 {
						return FormatError("uninitialized Huffman table")
					}
					for zig := 1; zig <= blockSize-1; {
						if nb < seqMinBits {
							if next, ok := seqAppendWord(data[pos:], acc, nb); ok {
								pos, acc, nb = pos+4, next, nb+32
							} else {
								pos, acc, nb = seqRefillSlow(data, pos, acc, nb)
							}
						}
						if nb < seqMinBits {
							r.pos, r.acc, r.nb = pos, acc, nb
							d.seqSync()
							if err := d.slowSequentialAC(ac, zig); err != nil {
								return err
							}
							d.seqLoad()
							pos, acc, nb = r.pos, r.acc, r.nb
							break
						}

						var sym, k int
						e := ac.seq[acc>>(64-seqLutBits)]
						if e&seqAdvanceFlag != 0 {
							advance := int((e &^ seqAdvanceFlag) >> seqEntrySymShift)
							if zig+advance <= blockSize {
								bits := uint(e & seqEntryBitsMask)
								acc <<= bits
								nb -= int(bits)
								zig += advance
								continue
							}
							// The run may overshoot, or the first coefficient may
							// end the block. Decode the original short code alone.
							first := ac.lut[acc>>(64-lutSize)]
							sym = int(first >> 8)
							k = int(first&0xff) - 1 + (sym & 15)
						} else if e != 0 {
							sym = int(e>>seqEntrySymShift) & 0xff
							k = int(e & seqEntryBitsMask)
						} else {
							s, l, err := seqSlowHuffman(ac, acc)
							if err != nil {
								return err
							}
							sym, k = int(s), l+int(s&15)
						}
						run, size := sym>>4, sym&15
						if size != 0 {
							zig += run
							if zig > blockSize-1 {
								// image/jpeg stops the block before reading the amplitude.
								k -= size
								acc <<= uint(k)
								nb -= k
								break
							}
							acc <<= uint(k)
							nb -= k
							zig++
							continue
						}
						acc <<= uint(k)
						nb -= k
						if run == 15 {
							zig += 16
							continue
						}
						// image/jpeg also accepts progressive-style end-of-band runs here.
						d.eobRun = 1 << uint(run)
						if run != 0 {
							d.eobRun |= uint16(acc >> uint(64-run))
							acc <<= uint(run)
							nb -= run
						}
						d.eobRun--
						break
					}
				} // for j
			} // for i
			mcu++
			if d.ri > 0 && mcu%d.ri == 0 && mcu < mxx*myy {
				// For well-formed input, the RST[0-7] restart marker follows
				// immediately. For corrupt input, call findRST to try to
				// resynchronize.
				r.pos, r.acc, r.nb = pos, acc, nb
				d.seqSync()
				if err := d.readFull(d.tmp[:2]); err != nil {
					return err
				} else if d.tmp[0] != 0xff || d.tmp[1] != expectedRST {
					if err := d.findRST(expectedRST); err != nil {
						return err
					}
				}
				expectedRST++
				if expectedRST == rst7Marker+1 {
					expectedRST = rst0Marker
				}
				// Reset the Huffman decoder (the DC predictors are not tracked).
				d.bits = bits{}
				d.seqInit()
				pos, acc, nb = r.pos, r.acc, r.nb
				// Reset the progressive decoder state, as per section G.1.2.2.
				d.eobRun = 0
			}
		} // for mx
	} // for my
	r.pos, r.acc, r.nb = pos, acc, nb
	d.seqSync()
	return nil
}

// slowSequentialBlock consumes one block on the reference reader (d.bits /
// d.bytes): the image/jpeg logic with the coefficient stores removed.
func (d *decoder) slowSequentialBlock(dc, ac *huffman) error {
	size, err := d.decodeHuffman(dc)
	if err != nil {
		return err
	}
	if size > 16 {
		return UnsupportedError("excessive DC component")
	}
	if err := d.skipAmplitude(size); err != nil {
		return err
	}
	if d.eobRun > 0 {
		d.eobRun--
		return nil
	}
	return d.slowSequentialAC(ac, 1)
}

// slowSequentialAC consumes the AC codes of a block from zig onwards on the
// reference reader.
func (d *decoder) slowSequentialAC(ac *huffman, zig int) error {
	for zig < blockSize {
		symbol, err := d.decodeHuffman(ac)
		if err != nil {
			return err
		}
		run, size := int(symbol>>4), symbol&15
		if size != 0 {
			zig += run
			// image/jpeg stops this block before the amplitude if the run overshoots.
			if zig >= blockSize {
				return nil
			}
			if err := d.skipAmplitude(size); err != nil {
				return err
			}
			zig++
		} else if run == 15 {
			zig += 16
		} else {
			// image/jpeg also accepts progressive-style end-of-band runs here.
			extra, err := d.decodeBits(int32(run))
			if err != nil {
				return err
			}
			d.eobRun = (1 << run) + uint16(extra) - 1
			return nil
		}
	}
	return nil
}

func (d *decoder) skipAmplitude(size uint8) error {
	if d.bits.n < int32(size) {
		if err := d.ensureNBits(int32(size)); err != nil {
			return err
		}
	}
	d.bits.n -= int32(size)
	d.bits.m >>= size
	return nil
}
