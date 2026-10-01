// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

package main

import (
	"image/color"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/image/bmp"
)

// masterSVG is a stand-in for assets/logo.svg: a badge with the mark
// knocked out of it, in a colour no constant in this package holds.
func masterSVG(t *testing.T, badge string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "logo.svg")
	svg := `<?xml version="1.0" encoding="UTF-8"?>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">
  <path d="M0 0H64V64H0Z" fill="` + badge + `"/>
  <path fill="#F8FAFC" d="M20 20H44V44H20Z"/>
</svg>
`
	require.NoError(t, os.WriteFile(path, []byte(svg), 0o600))
	return path
}

// bmpPixel reads one pixel back out of a written bitmap, so a test reads
// the file the installer will use rather than the image it was drawn from.
func bmpPixel(t *testing.T, path string, x, y int) color.RGBA {
	t.Helper()

	file, err := os.Open(path)
	require.NoError(t, err)
	defer file.Close()

	img, err := bmp.Decode(file)
	require.NoError(t, err)
	pixel, ok := color.RGBAModel.Convert(img.At(x, y)).(color.RGBA)
	require.True(t, ok, "%s: pixel at (%d,%d) is not an RGBA colour", path, x, y)
	return pixel
}

// The wizard's panel is painted in the master's own badge colour. Holding
// the generated bitmap to the file it came from is what keeps a change to
// the logo from leaving the installer painted in the colour before it,
// with the new badge drawn on top of the old panel.
func TestRun_PaintsTheWizardInTheMastersColour(t *testing.T) {
	badge := color.RGBA{R: 0x12, G: 0x34, B: 0x56, A: 0xFF}
	outDir := t.TempDir()

	require.NoError(t, run(masterSVG(t, "#123456"), outDir, "", "amd64", "0.0.0"))

	dialog := filepath.Join(outDir, "dialog.bmp")
	// The left panel is the badge colour; the text area beside it is that
	// same colour lightened, and neither is written down twice.
	require.Equal(t, badge, bmpPixel(t, dialog, 8, 300), "the wizard's panel is not the master's colour")
	require.Equal(t, tint(badge, dialogTint), bmpPixel(t, dialog, 480, 300),
		"the wizard's text area is not the master's colour lightened")
}

func TestBadgeFill_ReadsTheFirstPath(t *testing.T) {
	got, err := badgeFill(masterSVG(t, "#4F46E5"))
	require.NoError(t, err)
	require.Equal(t, color.RGBA{R: 0x4F, G: 0x46, B: 0xE5, A: 0xFF}, got)
}

// A master this cannot read fails the build rather than falling back to a
// colour of its own: a wrong colour ships, a failure does not.
func TestBadgeFill_RejectsAMasterItCannotRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		svg  string
	}{
		{name: "no path at all", svg: `<svg xmlns="http://www.w3.org/2000/svg"><rect fill="#123456"/></svg>`},
		{name: "a badge with no fill", svg: `<svg xmlns="http://www.w3.org/2000/svg"><path d="M0 0H1V1H0Z"/></svg>`},
		{name: "a fill that is not a colour", svg: `<svg xmlns="http://www.w3.org/2000/svg"><path fill="indigo" d="M0 0H1V1H0Z"/></svg>`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "logo.svg")
			require.NoError(t, os.WriteFile(path, []byte(tc.svg), 0o600))
			_, err := badgeFill(path)
			require.Error(t, err, "accepted a master whose badge colour cannot be read")
		})
	}
}

func TestParseHexColor(t *testing.T) {
	for _, tc := range []struct {
		name    string
		value   string
		want    color.RGBA
		wantErr bool
	}{
		{name: "six digits", value: "#1b1b1b", want: color.RGBA{R: 0x1b, G: 0x1b, B: 0x1b, A: 0xFF}},
		{name: "three digits", value: "#4ae", want: color.RGBA{R: 0x44, G: 0xAA, B: 0xEE, A: 0xFF}},
		{name: "no hash", value: "1b1b1b", want: color.RGBA{R: 0x1b, G: 0x1b, B: 0x1b, A: 0xFF}},
		{name: "a name", value: "rebeccapurple", wantErr: true},
		{name: "not hex", value: "#zzzzzz", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseHexColor(tc.value)
			if tc.wantErr {
				require.Error(t, err, "%q was accepted as a colour", tc.value)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestTint(t *testing.T) {
	ink := color.RGBA{R: 0x1b, G: 0x1b, B: 0x1b, A: 0xFF}
	require.Equal(t, ink, tint(ink, 0), "tinting by 0 must leave the colour unchanged")
	require.Equal(t, color.RGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}, tint(ink, 1), "tinting by 1 must give white")
	require.Equal(t, color.RGBA{R: 0xED, G: 0xED, B: 0xED, A: 0xFF}, tint(ink, dialogTint))
}
