// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"testing"
)

// solid returns a frame filled with one colour, for checking the bytes an
// encoder lays down rather than how a drawing looks.
func solid(size int, c color.RGBA) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	for y := range size {
		for x := range size {
			img.SetRGBA(x, y, c)
		}
	}
	return img
}

// icoFrame is one frame read back out of an encoded icon.
type icoFrame struct {
	bits          uint16
	body          []byte
	width, height int
}

// readICO parses an encoded icon, so a test reads the format back rather
// than asserting against the same constants the writer used.
func readICO(t *testing.T, data []byte) []icoFrame {
	t.Helper()

	if len(data) < 6 {
		t.Fatalf("an icon needs at least a directory header, got %d bytes", len(data))
	}
	reserved := binary.LittleEndian.Uint16(data[0:2])
	kind := binary.LittleEndian.Uint16(data[2:4])
	count := binary.LittleEndian.Uint16(data[4:6])
	if reserved != 0 || kind != 1 {
		t.Fatalf("want a reserved word of 0 and type 1, got %d and %d", reserved, kind)
	}

	frames := make([]icoFrame, 0, count)
	for i := range int(count) {
		entry := data[6+16*i : 6+16*(i+1)]
		size := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		if offset+size > len(data) {
			t.Fatalf("frame %d claims bytes %d..%d of a %d byte file", i, offset, offset+size, len(data))
		}
		// Zero stands for 256, which is how a byte-wide field describes
		// the largest frame.
		width, height := int(entry[0]), int(entry[1])
		if width == 0 {
			width = 256
		}
		if height == 0 {
			height = 256
		}
		frames = append(frames, icoFrame{
			bits:   binary.LittleEndian.Uint16(entry[6:8]),
			body:   data[offset : offset+size],
			width:  width,
			height: height,
		})
	}
	return frames
}

func TestWriteICO_DescribesEveryFrame(t *testing.T) {
	var buf bytes.Buffer
	sizes := []int{16, 32, 256}
	frames := make([]*image.RGBA, 0, len(sizes))
	for _, size := range sizes {
		frames = append(frames, solid(size, color.RGBA{R: 0x4F, G: 0x46, B: 0xE5, A: 0xFF}))
	}
	if err := writeICO(&buf, frames); err != nil {
		t.Fatalf("writeICO: %v", err)
	}

	read := readICO(t, buf.Bytes())
	if len(read) != len(sizes) {
		t.Fatalf("want %d frames, got %d", len(sizes), len(read))
	}
	for i, frame := range read {
		if frame.width != sizes[i] || frame.height != sizes[i] {
			t.Errorf("frame %d: want %dx%d, got %dx%d", i, sizes[i], sizes[i], frame.width, frame.height)
		}
		if frame.bits != 32 {
			t.Errorf("frame %d: want 32 bits per pixel, got %d", i, frame.bits)
		}
	}
}

// TestWriteICO_StoresTheLargestFrameAsPNG pins the two encodings an icon
// mixes. A 256x256 frame stored uncompressed costs a quarter of a
// megabyte, and the byte-wide dimension fields cannot spell 256 at all,
// so that frame is the one that differs.
func TestWriteICO_StoresTheLargestFrameAsPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := writeICO(&buf, []*image.RGBA{
		solid(32, color.RGBA{A: 0xFF}),
		solid(256, color.RGBA{A: 0xFF}),
	}); err != nil {
		t.Fatalf("writeICO: %v", err)
	}

	frames := readICO(t, buf.Bytes())
	pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

	if bytes.HasPrefix(frames[0].body, pngMagic) {
		t.Error("the 32px frame is a PNG; frames below the threshold are stored uncompressed")
	}
	if !bytes.HasPrefix(frames[1].body, pngMagic) {
		t.Error("the 256px frame is not a PNG; it is too large to store uncompressed")
	}
	// A zero in the directory is what the reader turns back into 256.
	if buf.Bytes()[6+16] != 0 {
		t.Errorf("want the 256px frame's width byte to be 0, got %d", buf.Bytes()[6+16])
	}
}

// TestEncodeDIB_HasADoubledHeightAndAMask covers the two parts of the
// format that are easy to leave out: the header describes the colour
// bitmap and the mask together, and the mask must be present even though
// a 32-bit frame carries its own alpha.
func TestEncodeDIB_HasADoubledHeightAndAMask(t *testing.T) {
	const size = 16
	body := encodeDIB(solid(size, color.RGBA{R: 1, G: 2, B: 3, A: 0xFF}))

	height := binary.LittleEndian.Uint32(body[8:12])
	if height != size*2 {
		t.Errorf("want a stored height of %d for a %dpx frame, got %d", size*2, size, height)
	}

	maskStride := ((size + 31) / 32) * 4
	want := 40 + size*size*4 + maskStride*size
	if len(body) != want {
		t.Errorf("want %d bytes (header, pixels and mask), got %d", want, len(body))
	}

	mask := body[40+size*size*4:]
	for i, b := range mask {
		if b != 0 {
			t.Fatalf("mask byte %d is %#x; the mask is clear because the alpha channel carries the shape", i, b)
		}
	}
}

// TestEncodeDIB_WritesBGRABottomUp pins the two conventions a bitmap
// inverts relative to Go's image: the channel order and the row order.
func TestEncodeDIB_WritesBGRABottomUp(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.SetRGBA(0, 0, color.RGBA{R: 0xFF, A: 0xFF}) // top-left is red
	img.SetRGBA(0, 1, color.RGBA{B: 0xFF, A: 0xFF}) // bottom-left is blue

	pixels := encodeDIB(img)[40:]
	// The first row written is the image's last, so it is the blue one,
	// stored blue-green-red-alpha.
	if got := pixels[0:4]; !bytes.Equal(got, []byte{0xFF, 0x00, 0x00, 0xFF}) {
		t.Errorf("want the bottom row first, in BGRA, got % x", got)
	}
	// The image's top row comes last.
	if got := pixels[8:12]; !bytes.Equal(got, []byte{0x00, 0x00, 0xFF, 0xFF}) {
		t.Errorf("want the top row last, in BGRA, got % x", got)
	}
}

func TestWriteICO_RejectsAnEmptyIcon(t *testing.T) {
	if err := writeICO(&bytes.Buffer{}, nil); err == nil {
		t.Error("want an error for an icon with no frames")
	}
}

func TestSplitVersion(t *testing.T) {
	for _, tc := range []struct {
		version             string
		major, minor, patch int
	}{
		{version: "1.2.3", major: 1, minor: 2, patch: 3},
		{version: "v1.2.3", major: 1, minor: 2, patch: 3},
		{version: "v1.2.3-rc.1", major: 1, minor: 2, patch: 3},
		{version: "v1.2.3+meta", major: 1, minor: 2, patch: 3},
		{version: "v2", major: 2},
		{version: "dev"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			major, minor, patch := splitVersion(tc.version)
			if major != tc.major || minor != tc.minor || patch != tc.patch {
				t.Errorf("want %d.%d.%d, got %d.%d.%d", tc.major, tc.minor, tc.patch, major, minor, patch)
			}
		})
	}
}

// TestEncodeDIB_WritesStraightAlpha covers the convention an icon frame
// uses, which is the opposite of Go's. Go stores colour premultiplied by
// alpha; an icon stores it straight. Only a partly transparent pixel can
// show the difference, which is why the frames built from opaque colours
// above cannot.
func TestEncodeDIB_WritesStraightAlpha(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 1, 1))
	// Half-transparent white: premultiplied, every colour channel is the
	// alpha; straight, every one is full.
	img.SetRGBA(0, 0, color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0x80})

	pixels := encodeDIB(img)[dibHeaderSize:]
	want := []byte{0xFF, 0xFF, 0xFF, 0x80}
	if !bytes.Equal(pixels[:4], want) {
		t.Errorf("want % x (straight alpha), got % x", want, pixels[:4])
	}
}

func TestEncodeDIB_LeavesOpaqueAndClearPixelsAlone(t *testing.T) {
	img := image.NewRGBA(image.Rect(0, 0, 2, 1))
	img.SetRGBA(0, 0, color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xFF})
	img.SetRGBA(1, 0, color.RGBA{})

	pixels := encodeDIB(img)[dibHeaderSize:]
	if want := []byte{0x30, 0x20, 0x10, 0xFF}; !bytes.Equal(pixels[0:4], want) {
		t.Errorf("opaque pixel: want % x, got % x", want, pixels[0:4])
	}
	if want := []byte{0, 0, 0, 0}; !bytes.Equal(pixels[4:8], want) {
		t.Errorf("clear pixel: want % x, got % x", want, pixels[4:8])
	}
}
