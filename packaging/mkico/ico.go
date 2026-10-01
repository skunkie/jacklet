// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
)

// Sizes of the icon's fixed structures, in bytes.
const (
	iconDirSize      = 6
	iconDirEntrySize = 16
	dibHeaderSize    = 40
)

// pngFrameThreshold is the size from which a frame is stored as PNG
// rather than as an uncompressed bitmap.
//
// Windows has read PNG-compressed frames since Vista, and at 256x256 an
// uncompressed frame costs a quarter of a megabyte. Below that the
// uncompressed form is small anyway and is what every consumer of an icon
// handles without question, so the large frame is the only one worth
// compressing. It is also the only one the directory cannot describe:
// its dimension fields are a byte wide, and 256 is stored as zero.
const pngFrameThreshold = 256

// maxFrameSize is the largest frame an icon can describe.
const maxFrameSize = 256

// The icon format stores every length, offset and dimension in a fixed
// width field, so each conversion is guarded rather than assumed. A frame
// is validated as square and at most maxFrameSize before any of this runs,
// which is what makes the guards unreachable in practice.
func u32(n int) uint32 {
	if n < 0 || int64(n) > math.MaxUint32 {
		panic(fmt.Sprintf("value %d does not fit a 32-bit field", n))
	}
	return uint32(n)
}

func u16(n int) uint16 {
	if n < 0 || n > math.MaxUint16 {
		panic(fmt.Sprintf("value %d does not fit a 16-bit field", n))
	}
	return uint16(n)
}

func u8(n int) byte {
	if n < 0 || n > math.MaxUint8 {
		panic(fmt.Sprintf("value %d does not fit an 8-bit field", n))
	}
	return byte(n)
}

// writeICO writes frames as a Windows icon.
func writeICO(w io.Writer, frames []*image.RGBA) error {
	if len(frames) == 0 {
		return errors.New("an icon needs at least one frame")
	}

	bodies := make([][]byte, len(frames))
	for i, frame := range frames {
		size := frame.Bounds().Dx()
		if size <= 0 || size > maxFrameSize || frame.Bounds().Dy() != size {
			return fmt.Errorf("a frame must be square and at most %d pixels, got %dx%d",
				maxFrameSize, size, frame.Bounds().Dy())
		}
		body, err := encodeFrame(frame)
		if err != nil {
			return err
		}
		bodies[i] = body
	}

	directory := make([]byte, iconDirSize+iconDirEntrySize*len(frames))
	binary.LittleEndian.PutUint16(directory[0:], 0)                // reserved
	binary.LittleEndian.PutUint16(directory[2:], 1)                // type: icon
	binary.LittleEndian.PutUint16(directory[4:], u16(len(frames))) // frame count

	offset := u32(len(directory))
	for i, frame := range frames {
		entry := directory[iconDirSize+iconDirEntrySize*i:]
		// A 256-pixel frame is stored as zero, which is what lets a
		// byte-wide field describe the largest one.
		size := u8(frame.Bounds().Dx() % maxFrameSize)
		entry[0] = size                              // width
		entry[1] = size                              // height
		entry[2] = 0                                 // palette entries: none, the frame is true colour
		entry[3] = 0                                 // reserved
		binary.LittleEndian.PutUint16(entry[4:], 1)  // colour planes
		binary.LittleEndian.PutUint16(entry[6:], 32) // bits per pixel
		binary.LittleEndian.PutUint32(entry[8:], u32(len(bodies[i])))
		binary.LittleEndian.PutUint32(entry[12:], offset)
		offset += u32(len(bodies[i]))
	}

	var out bytes.Buffer
	out.Write(directory)
	for _, body := range bodies {
		out.Write(body)
	}
	_, err := w.Write(out.Bytes())
	return err
}

// encodeFrame renders one frame's bitmap.
func encodeFrame(frame *image.RGBA) ([]byte, error) {
	if frame.Bounds().Dx() >= pngFrameThreshold {
		var buf bytes.Buffer
		if err := png.Encode(&buf, frame); err != nil {
			return nil, fmt.Errorf("encoding the %dpx frame: %w", frame.Bounds().Dx(), err)
		}
		return buf.Bytes(), nil
	}
	return encodeDIB(frame), nil
}

// encodeDIB writes a frame as the uncompressed bitmap an icon expects: a
// BITMAPINFOHEADER, the pixels bottom-up in BGRA, then an AND mask.
//
// The header's height is doubled because it describes the colour bitmap
// and the mask together. The mask is left clear: a 32-bit frame carries
// its own alpha, and every Windows version that reads one uses it, but
// the mask is still part of the format and the frame is rejected without
// it.
func encodeDIB(frame *image.RGBA) []byte {
	bounds := frame.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	// Each mask row is padded to a four-byte boundary.
	maskStride := ((width + 31) / 32) * 4

	out := make([]byte, dibHeaderSize, dibHeaderSize+width*height*4+maskStride*height)
	binary.LittleEndian.PutUint32(out[0:], dibHeaderSize)
	binary.LittleEndian.PutUint32(out[4:], u32(width))
	binary.LittleEndian.PutUint32(out[8:], u32(height*2)) // colour plus mask
	binary.LittleEndian.PutUint16(out[12:], 1)            // colour planes
	binary.LittleEndian.PutUint16(out[14:], 32)           // bits per pixel
	binary.LittleEndian.PutUint32(out[16:], 0)            // compression: none
	binary.LittleEndian.PutUint32(out[20:], u32(width*height*4+maskStride*height))
	// The remaining header fields -- the two resolutions and the two
	// palette counts -- stay zero.

	for y := height - 1; y >= 0; y-- {
		for x := range width {
			b, g, r, a := straightBGRA(frame.RGBAAt(bounds.Min.X+x, bounds.Min.Y+y))
			out = append(out, b, g, r, a)
		}
	}
	return append(out, make([]byte, maskStride*height)...)
}

// straightBGRA converts a pixel to the channel order and alpha convention
// an icon frame uses.
//
// Go stores colour premultiplied by alpha; an icon stores it straight. On
// a fully opaque pixel the two are identical, which is why this shows up
// only along an antialiased edge, as a dark fringe, and why the PNG frame
// disagrees with the rest: encoding a PNG converts back on its own.
func straightBGRA(c color.RGBA) (b, g, r, a byte) {
	if c.A == 0 || c.A == 0xFF {
		return c.B, c.G, c.R, c.A
	}
	unpremultiply := func(v byte) byte {
		return byte(min(0xFF, (uint32(v)*0xFF+uint32(c.A)/2)/uint32(c.A)))
	}
	return unpremultiply(c.B), unpremultiply(c.G), unpremultiply(c.R), c.A
}
