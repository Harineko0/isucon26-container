// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jpegcheck

import "encoding/binary"

// In a sequential scan no later scan refines these coefficients. Consume every
// Huffman code and its amplitude, but do not materialize the coefficient values.
func (d *decoder) validateSequentialBlock(dc, ac *huffman) error {
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
	for zig := 1; zig < blockSize; {
		if next := d.skipShortAC(ac, zig); next != zig {
			zig = next
			continue
		}
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

// Peek at four ordinary entropy bytes. Each table entry gives both the number
// of zeros plus one coefficient, and the combined Huffman + amplitude length.
// Commit only bytes used so the normal reader still owns stuffing and markers.
func (d *decoder) skipShortAC(h *huffman, zig int) int {
	if d.bytes.j-d.bytes.i < 4 {
		return zig
	}
	p := d.bytes.buf[d.bytes.i : d.bytes.i+4]
	if p[0] == 0xff || p[1] == 0xff || p[2] == 0xff || p[3] == 0xff {
		return zig
	}
	word := uint64(d.bits.a)<<32 | uint64(binary.BigEndian.Uint32(p))
	available := d.bits.n + 32
	start := zig
	for available >= lutSize {
		step := h.acStep[uint8(word>>uint(available-lutSize))]
		if step.bits == 0 || int32(step.bits) > available || zig+int(step.coefficients) > blockSize {
			break
		}
		available -= int32(step.bits)
		zig += int(step.coefficients)
	}
	if zig != start {
		// Bits below the cursor belong to peeked bytes. Round their count down to
		// whole bytes: these stay unread in d.bytes. No marker is consumed here.
		unread := min(available/8, 4)
		d.bytes.i += 4 - int(unread)
		if unread != 4 {
			d.bytes.nUnreadable = 1
		}
		d.bits.n = available - 8*unread
		d.bits.a = uint32(word >> uint(8*unread))
		d.bits.m = (uint32(1) << uint(d.bits.n)) >> 1
	}
	return zig
}
