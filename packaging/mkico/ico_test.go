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

	"github.com/stretchr/testify/require"
)

// solid returns a frame filled with one color, for checking the bytes an
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
	height, width int
}

// readICO parses an encoded icon, so a test reads the format back rather
// than asserting against the same constants the writer used.
func readICO(t *testing.T, data []byte) []icoFrame {
	t.Helper()

	require.GreaterOrEqual(t, len(data), 6, "an icon needs at least a directory header")
	require.Zero(t, binary.LittleEndian.Uint16(data[0:2]), "the reserved word")
	require.Equal(t, uint16(1), binary.LittleEndian.Uint16(data[2:4]), "the type of an icon")
	count := binary.LittleEndian.Uint16(data[4:6])

	frames := make([]icoFrame, 0, count)
	for i := range int(count) {
		entry := data[6+16*i : 6+16*(i+1)]
		size := int(binary.LittleEndian.Uint32(entry[8:12]))
		offset := int(binary.LittleEndian.Uint32(entry[12:16]))
		require.LessOrEqual(t, offset+size, len(data), "frame %d runs past the end of the file", i)
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
			height: height,
			width:  width,
		})
	}
	return frames
}

func TestWriteICO(t *testing.T) {
	t.Run("describes every frame", func(t *testing.T) {
		var buf bytes.Buffer
		sizes := []int{16, 32, 256}
		frames := make([]*image.RGBA, 0, len(sizes))
		for _, size := range sizes {
			frames = append(frames, solid(size, color.RGBA{R: 0x4F, G: 0x46, B: 0xE5, A: 0xFF}))
		}
		require.NoError(t, writeICO(&buf, frames))

		read := readICO(t, buf.Bytes())
		require.Len(t, read, len(sizes))
		for i, frame := range read {
			require.Equal(t, sizes[i], frame.width, "frame %d", i)
			require.Equal(t, sizes[i], frame.height, "frame %d", i)
			require.Equal(t, uint16(32), frame.bits, "frame %d", i)
		}
	})

	// Pins the two encodings an icon mixes. A 256x256 frame stored
	// uncompressed costs a quarter of a megabyte, and the byte-wide dimension
	// fields cannot spell 256 at all, so that frame is the one that differs.
	t.Run("stores the largest frame as PNG", func(t *testing.T) {
		var buf bytes.Buffer
		require.NoError(t, writeICO(&buf, []*image.RGBA{
			solid(32, color.RGBA{A: 0xFF}),
			solid(256, color.RGBA{A: 0xFF}),
		}))

		frames := readICO(t, buf.Bytes())
		pngMagic := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}

		require.False(t, bytes.HasPrefix(frames[0].body, pngMagic),
			"the 32px frame is a PNG; frames below the threshold are stored uncompressed")
		require.True(t, bytes.HasPrefix(frames[1].body, pngMagic),
			"the 256px frame is not a PNG; it is too large to store uncompressed")
		// A zero in the directory is what the reader turns back into 256.
		require.Zero(t, buf.Bytes()[6+16], "the 256px frame's width byte")
	})

	t.Run("rejects an empty icon", func(t *testing.T) {
		require.Error(t, writeICO(&bytes.Buffer{}, nil), "an icon with no frames")
	})
}

func TestEncodeDIB(t *testing.T) {
	// Covers the two parts of the format that are easy to leave out: the
	// header describes the color bitmap and the mask together, and the mask
	// must be present even though a 32-bit frame carries its own alpha.
	t.Run("has a doubled height and a mask", func(t *testing.T) {
		const size = 16
		body := encodeDIB(solid(size, color.RGBA{R: 1, G: 2, B: 3, A: 0xFF}))

		require.Equal(t, uint32(size*2), binary.LittleEndian.Uint32(body[8:12]),
			"the stored height covers the color bitmap and the mask together")

		maskStride := ((size + 31) / 32) * 4
		require.Len(t, body, 40+size*size*4+maskStride*size, "header, pixels and mask")

		mask := body[40+size*size*4:]
		require.Equal(t, make([]byte, len(mask)), mask, "the mask is clear because the alpha channel carries the shape")
	})

	// Pins the two conventions a bitmap inverts relative to Go's image: the
	// channel order and the row order.
	t.Run("writes BGRA bottom up", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 2, 2))
		img.SetRGBA(0, 0, color.RGBA{R: 0xFF, A: 0xFF}) // top-left is red
		img.SetRGBA(0, 1, color.RGBA{B: 0xFF, A: 0xFF}) // bottom-left is blue

		pixels := encodeDIB(img)[40:]
		// The first row written is the image's last, so it is the blue one,
		// stored blue-green-red-alpha.
		require.Equal(t, []byte{0xFF, 0x00, 0x00, 0xFF}, pixels[0:4], "the bottom row comes first, in BGRA")
		// The image's top row comes last.
		require.Equal(t, []byte{0x00, 0x00, 0xFF, 0xFF}, pixels[8:12], "the top row comes last, in BGRA")
	})

	// Covers the convention an icon frame uses, which is the opposite of
	// Go's. Go stores color premultiplied by alpha; an icon stores it
	// straight. Only a partly transparent pixel can show the difference,
	// which is why the frames built from opaque colors above cannot.
	t.Run("writes straight alpha", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 1, 1))
		// Half-transparent white: premultiplied, every color channel is the
		// alpha; straight, every one is full.
		img.SetRGBA(0, 0, color.RGBA{R: 0x80, G: 0x80, B: 0x80, A: 0x80})

		pixels := encodeDIB(img)[dibHeaderSize:]
		require.Equal(t, []byte{0xFF, 0xFF, 0xFF, 0x80}, pixels[:4], "straight alpha")
	})

	t.Run("leaves opaque and clear pixels alone", func(t *testing.T) {
		img := image.NewRGBA(image.Rect(0, 0, 2, 1))
		img.SetRGBA(0, 0, color.RGBA{R: 0x10, G: 0x20, B: 0x30, A: 0xFF})
		img.SetRGBA(1, 0, color.RGBA{})

		pixels := encodeDIB(img)[dibHeaderSize:]
		require.Equal(t, []byte{0x30, 0x20, 0x10, 0xFF}, pixels[0:4], "opaque pixel")
		require.Equal(t, []byte{0, 0, 0, 0}, pixels[4:8], "clear pixel")
	})
}

func TestSplitVersion(t *testing.T) {
	for _, tc := range []struct {
		version   string
		wantMajor int
		wantMinor int
		wantPatch int
	}{
		{version: "1.2.3", wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{version: "v1.2.3", wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{version: "v1.2.3-rc.1", wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{version: "v1.2.3+meta", wantMajor: 1, wantMinor: 2, wantPatch: 3},
		{version: "v2", wantMajor: 2},
		{version: "dev"},
	} {
		t.Run(tc.version, func(t *testing.T) {
			major, minor, patch := splitVersion(tc.version)
			require.Equal(t, []int{tc.wantMajor, tc.wantMinor, tc.wantPatch}, []int{major, minor, patch})
		})
	}
}
