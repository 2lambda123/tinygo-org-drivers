// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package jpeg

import (
	"image"
	"image/color"
)

// makeImg allocates and initializes the destination image.
func (d *decoder) makeImg(mxx, myy int) {
	if d.nComp == 1 {
		m := image.NewGray(image.Rect(0, 0, 8*mxx, 8*myy))
		d.img1 = m.SubImage(image.Rect(0, 0, d.width, d.height)).(*image.Gray)
		return
	}

	h0 := d.comp[0].h
	v0 := d.comp[0].v
	hRatio := h0 / d.comp[1].h
	vRatio := v0 / d.comp[1].v
	var subsampleRatio image.YCbCrSubsampleRatio
	switch hRatio<<4 | vRatio {
	case 0x11:
		subsampleRatio = image.YCbCrSubsampleRatio444
	case 0x12:
		subsampleRatio = image.YCbCrSubsampleRatio440
	case 0x21:
		subsampleRatio = image.YCbCrSubsampleRatio422
	case 0x22:
		subsampleRatio = image.YCbCrSubsampleRatio420
	case 0x41:
		subsampleRatio = image.YCbCrSubsampleRatio411
	case 0x42:
		subsampleRatio = image.YCbCrSubsampleRatio410
	default:
		panic("unreachable")
	}
	m := image.NewYCbCr(image.Rect(0, 0, 8*h0*mxx, 8*v0*myy), subsampleRatio)
	d.img3 = m.SubImage(image.Rect(0, 0, d.width, d.height)).(*image.YCbCr)

	if d.nComp == 4 {
		h3, v3 := d.comp[3].h, d.comp[3].v
		d.blackPix = make([]byte, 8*h3*mxx*8*v3*myy)
		d.blackStride = 8 * h3 * mxx
	}
}

// processSOSBuf is a Buffer for creating RGBBitmap in processSOS. It needs to
// hold four 8 x 8 pix 24bit color images.
var processSOSBuf [3 * 8 * 8 * 4]byte

// Specified in section B.2.3.
func (d *decoder) processSOS(n int) error {
	if d.nComp == 0 {
		return FormatError("missing SOF marker")
	}
	if n < 6 || 4+2*d.nComp < n || n%2 != 0 {
		return FormatError("SOS has wrong length")
	}
	if err := d.readFull(d.tmp[:n]); err != nil {
		return err
	}
	nComp := int(d.tmp[0])
	if n != 4+2*nComp {
		return FormatError("SOS length inconsistent with number of components")
	}
	var scan [maxComponents]struct {
		compIndex uint8
		td        uint8 // DC table selector.
		ta        uint8 // AC table selector.
	}
	totalHV := 0
	for i := 0; i < nComp; i++ {
		cs := d.tmp[1+2*i] // Component selector.
		compIndex := -1
		for j, comp := range d.comp[:d.nComp] {
			if cs == comp.c {
				compIndex = j
			}
		}
		if compIndex < 0 {
			return FormatError("unknown component selector")
		}
		scan[i].compIndex = uint8(compIndex)
		// Section B.2.3 states that "the value of Cs_j shall be different from
		// the values of Cs_1 through Cs_(j-1)". Since we have previously
		// verified that a frame's component identifiers (C_i values in section
		// B.2.2) are unique, it suffices to check that the implicit indexes
		// into d.comp are unique.
		for j := 0; j < i; j++ {
			if scan[i].compIndex == scan[j].compIndex {
				return FormatError("repeated component selector")
			}
		}
		totalHV += d.comp[compIndex].h * d.comp[compIndex].v

		// The baseline t <= 1 restriction is specified in table B.3.
		scan[i].td = d.tmp[2+2*i] >> 4
		if t := scan[i].td; t > maxTh || (d.baseline && t > 1) {
			return FormatError("bad Td value")
		}
		scan[i].ta = d.tmp[2+2*i] & 0x0f
		if t := scan[i].ta; t > maxTh || (d.baseline && t > 1) {
			return FormatError("bad Ta value")
		}
	}
	// Section B.2.3 states that if there is more than one component then the
	// total H*V values in a scan must be <= 10.
	if d.nComp > 1 && totalHV > 10 {
		return FormatError("total sampling factors too large")
	}

	// zigStart and zigEnd are the spectral selection bounds.
	// ah and al are the successive approximation high and low values.
	// The spec calls these values Ss, Se, Ah and Al.
	//
	// For progressive JPEGs, these are the two more-or-less independent
	// aspects of progression. Spectral selection progression is when not
	// all of a block's 64 DCT coefficients are transmitted in one pass.
	// For example, three passes could transmit coefficient 0 (the DC
	// component), coefficients 1-5, and coefficients 6-63, in zig-zag
	// order. Successive approximation is when not all of the bits of a
	// band of coefficients are transmitted in one pass. For example,
	// three passes could transmit the 6 most significant bits, followed
	// by the second-least significant bit, followed by the least
	// significant bit.
	//
	// For sequential JPEGs, these parameters are hard-coded to 0/63/0/0, as
	// per table B.3.
	zigStart, zigEnd, ah, al := int32(0), int32(blockSize-1), uint32(0), uint32(0)
	if d.progressive {
		zigStart = int32(d.tmp[1+2*nComp])
		zigEnd = int32(d.tmp[2+2*nComp])
		ah = uint32(d.tmp[3+2*nComp] >> 4)
		al = uint32(d.tmp[3+2*nComp] & 0x0f)
		if (zigStart == 0 && zigEnd != 0) || zigStart > zigEnd || blockSize <= zigEnd {
			return FormatError("bad spectral selection bounds")
		}
		if zigStart != 0 && nComp != 1 {
			return FormatError("progressive AC coefficients for more than one component")
		}
		if ah != 0 && ah != al+1 {
			return FormatError("bad successive approximation values")
		}
	}

	// mxx and myy are the number of MCUs (Minimum Coded Units) in the image.
	h0, v0 := d.comp[0].h, d.comp[0].v // The h and v values from the Y components.
	mxx := (d.width + 8*h0 - 1) / (8 * h0)
	myy := (d.height + 8*v0 - 1) / (8 * v0)
	if d.img1 == nil && d.img3 == nil {
		// Minimizes memory usage in order to run on TinyGo. In order to keep
		// the amount of code changes down, the image is created as a 1 x 1
		// image at this point.
		d.makeImg(1, 1)
	}
	if d.progressive {
		for i := 0; i < nComp; i++ {
			compIndex := scan[i].compIndex
			if d.progCoeffs[compIndex] == nil {
				d.progCoeffs[compIndex] = make([]block, mxx*myy*d.comp[compIndex].h*d.comp[compIndex].v)
			}
		}
	}

	d.bits = bits{}
	mcu, expectedRST := 0, uint8(rst0Marker)
	var (
		// b is the decoded coefficients, in natural (not zig-zag) order.
		b  block
		dc [maxComponents]int32
		// bx and by are the location of the current block, in units of 8x8
		// blocks: the third block in the first row has (bx, by) = (2, 0).
		bx, by     int
		blockCount int
	)
	for my := 0; my < myy; my++ {
		for mx := 0; mx < mxx; mx++ {
			for i := 0; i < nComp; i++ {
				compIndex := scan[i].compIndex
				hi := d.comp[compIndex].h
				vi := d.comp[compIndex].v
				for j := 0; j < hi*vi; j++ {
					// The blocks are traversed one MCU at a time. For 4:2:0 chroma
					// subsampling, there are four Y 8x8 blocks in every 16x16 MCU.
					//
					// For a sequential 32x16 pixel image, the Y blocks visiting order is:
					//	0 1 4 5
					//	2 3 6 7
					//
					// For progressive images, the interleaved scans (those with nComp > 1)
					// are traversed as above, but non-interleaved scans are traversed left
					// to right, top to bottom:
					//	0 1 2 3
					//	4 5 6 7
					// Only DC scans (zigStart == 0) can be interleaved. AC scans must have
					// only one component.
					//
					// To further complicate matters, for non-interleaved scans, there is no
					// data for any blocks that are inside the image at the MCU level but
					// outside the image at the pixel level. For example, a 24x16 pixel 4:2:0
					// progressive image consists of two 16x16 MCUs. The interleaved scans
					// will process 8 Y blocks:
					//	0 1 4 5
					//	2 3 6 7
					// The non-interleaved scans will process only 6 Y blocks:
					//	0 1 2
					//	3 4 5
					if nComp != 1 {
						bx = hi*mx + j%hi
						by = vi*my + j/hi
					} else {
						q := mxx * hi
						bx = blockCount % q
						by = blockCount / q
						blockCount++
						if bx*8 >= d.width || by*8 >= d.height {
							continue
						}
					}

					// Load the previous partially decoded coefficients, if applicable.
					if d.progressive {
						b = d.progCoeffs[compIndex][by*mxx*hi+bx]
					} else {
						b = block{}
					}

					if ah != 0 {
						if err := d.refine(&b, &d.huff[acTable][scan[i].ta], zigStart, zigEnd, 1<<al); err != nil {
							return err
						}
					} else {
						zig := zigStart
						if zig == 0 {
							zig++
							// Decode the DC coefficient, as specified in section F.2.2.1.
							value, err := d.decodeHuffman(&d.huff[dcTable][scan[i].td])
							if err != nil {
								return err
							}
							if value > 16 {
								return UnsupportedError("excessive DC component")
							}
							dcDelta, err := d.receiveExtend(value)
							if err != nil {
								return err
							}
							dc[compIndex] += dcDelta
							b[0] = dc[compIndex] << al
						}

						if zig <= zigEnd && d.eobRun > 0 {
							d.eobRun--
						} else {
							// Decode the AC coefficients, as specified in section F.2.2.2.
							huff := &d.huff[acTable][scan[i].ta]
							for ; zig <= zigEnd; zig++ {
								value, err := d.decodeHuffman(huff)
								if err != nil {
									return err
								}
								val0 := value >> 4
								val1 := value & 0x0f
								if val1 != 0 {
									zig += int32(val0)
									if zig > zigEnd {
										break
									}
									ac, err := d.receiveExtend(val1)
									if err != nil {
										return err
									}
									b[unzig[zig]] = ac << al
								} else {
									if val0 != 0x0f {
										d.eobRun = uint16(1 << val0)
										if val0 != 0 {
											bits, err := d.decodeBits(int32(val0))
											if err != nil {
												return err
											}
											d.eobRun |= uint16(bits)
										}
										d.eobRun--
										break
									}
									zig += 0x0f
								}
							}
						}
					}

					if d.progressive {
						// Save the coefficients.
						d.progCoeffs[compIndex][by*mxx*hi+bx] = b
						// At this point, we could call reconstructBlock to dequantize and perform the
						// inverse DCT, to save early stages of a progressive image to the *image.YCbCr
						// buffers (the whole point of progressive encoding), but in Go, the jpeg.Decode
						// function does not return until the entire image is decoded, so we "continue"
						// here to avoid wasted computation. Instead, reconstructBlock is called on each
						// accumulated block by the reconstructProgressiveImage method after all of the
						// SOS markers are processed.
						continue
					}
					if dst, err := d.reconstructBlock(&b, bx, by, int(compIndex)); err != nil {
						return err
					} else {
						// Currently, only the YCbCr422 format is supported.
						switch compIndex {
						case 0: // Y
							bx8 := bx * 8
							by8 := by * 8
							bx16 := bx8 % 16
							by16 := by8 % 16
							for cy := 0; cy < 8; cy++ {
								for cx := 0; cx < 8; cx++ {
									processSOSBuf[((cy+by16)*16+(cx+bx16))*3+0] = dst[cy*8+cx]
								}
							}
						case 1: // Cb
							bx8 := bx * 8 * 2
							by8 := by * 8 * 2
							bx16 := bx8 % 16
							by16 := by8 % 16

							for cy := 0; cy < 8; cy++ {
								for cx := 0; cx < 8; cx++ {
									processSOSBuf[((cy*2+0+by16)*16+(cx*2+0+bx16))*3+1] = dst[cy*8+cx]
									processSOSBuf[((cy*2+0+by16)*16+(cx*2+1+bx16))*3+1] = dst[cy*8+cx]
									processSOSBuf[((cy*2+1+by16)*16+(cx*2+0+bx16))*3+1] = dst[cy*8+cx]
									processSOSBuf[((cy*2+1+by16)*16+(cx*2+1+bx16))*3+1] = dst[cy*8+cx]
								}
							}
						case 2: // Cr
							bx8 := bx * 8 * 2
							by8 := by * 8 * 2
							bx16 := bx8 % 16
							by16 := by8 % 16

							for cy := 0; cy < 8; cy++ {
								for cx := 0; cx < 8; cx++ {
									processSOSBuf[((cy*2+0+by16)*16+(cx*2+0+bx16))*3+2] = dst[cy*8+cx]
									processSOSBuf[((cy*2+0+by16)*16+(cx*2+1+bx16))*3+2] = dst[cy*8+cx]
									processSOSBuf[((cy*2+1+by16)*16+(cx*2+0+bx16))*3+2] = dst[cy*8+cx]
									processSOSBuf[((cy*2+1+by16)*16+(cx*2+1+bx16))*3+2] = dst[cy*8+cx]
								}
							}

							for cy := 0; cy < 16; cy++ {
								for cx := 0; cx < 16; cx++ {
									yy := processSOSBuf[(cy*16+cx)*3+0]
									cb := processSOSBuf[(cy*16+cx)*3+1]
									cr := processSOSBuf[(cy*16+cx)*3+2]
									r, g, b := color.YCbCrToRGB(yy, cb, cr)
									callbackBuf[cy*16+cx] = uint16(((uint16(r) << 8) & 0xF800) +
										(((uint16(g) << 8) & 0xFC00) >> 5) +
										(((uint16(b) << 8) & 0xF800) >> 11))
								}
							}
							callback(callbackBuf[:8*8*4], int16(bx8-bx16), int16(by8-by16), 16, 16, int16(d.width), int16(d.height))
						}
					}
				} // for j
			} // for i
			mcu++
			if d.ri > 0 && mcu%d.ri == 0 && mcu < mxx*myy {
				// A more sophisticated decoder could use RST[0-7] markers to resynchronize from corrupt input,
				// but this one assumes well-formed input, and hence the restart marker follows immediately.
				if err := d.readFull(d.tmp[:2]); err != nil {
					return err
				}

				// Section F.1.2.3 says that "Byte alignment of markers is
				// achieved by padding incomplete bytes with 1-bits. If padding
				// with 1-bits creates a X’FF’ value, a zero byte is stuffed
				// before adding the marker."
				//
				// Seeing "\xff\x00" here is not spec compliant, as we are not
				// expecting an *incomplete* byte (that needed padding). Still,
				// some real world encoders (see golang.org/issue/28717) insert
				// it, so we accept it and re-try the 2 byte read.
				//
				// libjpeg issues a warning (but not an error) for this:
				// https://github.com/LuaDist/libjpeg/blob/6c0fcb8ddee365e7abc4d332662b06900612e923/jdmarker.c#L1041-L1046
				if d.tmp[0] == 0xff && d.tmp[1] == 0x00 {
					if err := d.readFull(d.tmp[:2]); err != nil {
						return err
					}
				}

				if d.tmp[0] != 0xff || d.tmp[1] != expectedRST {
					return FormatError("bad RST marker")
				}
				expectedRST++
				if expectedRST == rst7Marker+1 {
					expectedRST = rst0Marker
				}
				// Reset the Huffman decoder.
				d.bits = bits{}
				// Reset the DC components, as per section F.2.1.3.1.
				dc = [maxComponents]int32{}
				// Reset the progressive decoder state, as per section G.1.2.2.
				d.eobRun = 0
			}
		} // for mx
	} // for my

	return nil
}

// refine decodes a successive approximation refinement block, as specified in
// section G.1.2.
func (d *decoder) refine(b *block, h *huffman, zigStart, zigEnd, delta int32) error {
	// Refining a DC component is trivial.
	if zigStart == 0 {
		if zigEnd != 0 {
			panic("unreachable")
		}
		bit, err := d.decodeBit()
		if err != nil {
			return err
		}
		if bit {
			b[0] |= delta
		}
		return nil
	}

	// Refining AC components is more complicated; see sections G.1.2.2 and G.1.2.3.
	zig := zigStart
	if d.eobRun == 0 {
	loop:
		for ; zig <= zigEnd; zig++ {
			z := int32(0)
			value, err := d.decodeHuffman(h)
			if err != nil {
				return err
			}
			val0 := value >> 4
			val1 := value & 0x0f

			switch val1 {
			case 0:
				if val0 != 0x0f {
					d.eobRun = uint16(1 << val0)
					if val0 != 0 {
						bits, err := d.decodeBits(int32(val0))
						if err != nil {
							return err
						}
						d.eobRun |= uint16(bits)
					}
					break loop
				}
			case 1:
				z = delta
				bit, err := d.decodeBit()
				if err != nil {
					return err
				}
				if !bit {
					z = -z
				}
			default:
				return FormatError("unexpected Huffman code")
			}

			zig, err = d.refineNonZeroes(b, zig, zigEnd, int32(val0), delta)
			if err != nil {
				return err
			}
			if zig > zigEnd {
				return FormatError("too many coefficients")
			}
			if z != 0 {
				b[unzig[zig]] = z
			}
		}
	}
	if d.eobRun > 0 {
		d.eobRun--
		if _, err := d.refineNonZeroes(b, zig, zigEnd, -1, delta); err != nil {
			return err
		}
	}
	return nil
}

// refineNonZeroes refines non-zero entries of b in zig-zag order. If nz >= 0,
// the first nz zero entries are skipped over.
func (d *decoder) refineNonZeroes(b *block, zig, zigEnd, nz, delta int32) (int32, error) {
	for ; zig <= zigEnd; zig++ {
		u := unzig[zig]
		if b[u] == 0 {
			if nz == 0 {
				break
			}
			nz--
			continue
		}
		bit, err := d.decodeBit()
		if err != nil {
			return 0, err
		}
		if !bit {
			continue
		}
		if b[u] >= 0 {
			b[u] += delta
		} else {
			b[u] -= delta
		}
	}
	return zig, nil
}

func (d *decoder) reconstructProgressiveImage() error {
	// The h0, mxx, by and bx variables have the same meaning as in the
	// processSOS method.
	h0 := d.comp[0].h
	mxx := (d.width + 8*h0 - 1) / (8 * h0)
	for i := 0; i < d.nComp; i++ {
		if d.progCoeffs[i] == nil {
			continue
		}
		v := 8 * d.comp[0].v / d.comp[i].v
		h := 8 * d.comp[0].h / d.comp[i].h
		stride := mxx * d.comp[i].h
		for by := 0; by*v < d.height; by++ {
			for bx := 0; bx*h < d.width; bx++ {
				if _, err := d.reconstructBlock(&d.progCoeffs[i][by*stride+bx], bx, by, i); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// reconstructBlockBuf is a Buffer for the 8x8 pix data to be processed by
// reconstructBlock. It is defined and used as a package variable to reduce
// memory usage.
var reconstructBlockBuf [64]byte

// reconstructBlock dequantizes, performs the inverse DCT and stores the block
// to the image.
// In the original Go source, it was expanded to a position that matched the
// coordinates of the original image. Note that TinyGo does not transform the
// coordinate system, so the movement is different.
func (d *decoder) reconstructBlock(b *block, bx, by, compIndex int) ([]byte, error) {
	qt := &d.quant[d.comp[compIndex].tq]
	for zig := 0; zig < blockSize; zig++ {
		b[unzig[zig]] *= qt[zig]
	}
	idct(b)
	// Level shift by +128, clip to [0, 255], and write to dst.
	var buf = reconstructBlockBuf[:]
	for y := 0; y < 8; y++ {
		y8 := y * 8
		for x := 0; x < 8; x++ {
			c := b[y8+x]
			if c < -128 {
				c = 0
			} else if c > 127 {
				c = 255
			} else {
				c += 128
			}
			buf[y*8+x] = uint8(c)
		}
	}
	return buf, nil
}
