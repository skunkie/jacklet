// SPDX-FileCopyrightText: 2026 TorrPlay
//
// SPDX-License-Identifier: MIT

// Command mkico derives every raster asset from the master logo: the
// Windows icon, the resource object that carries it into jacklet.exe, the
// installer's two bitmaps, and the PNGs the documentation uses.
//
// It is a module of its own, which keeps a rasterizer out of Jacklet's
// dependencies, the same way packaging/mkman keeps a markdown renderer
// out of them. Nothing it writes is committed: the SVG is the source, and
// a release derives the rest, so an icon can never disagree with the logo
// it came from.
package main

import (
	"encoding/hex"
	"encoding/xml"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/josephspurrier/goversioninfo"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
	"golang.org/x/image/bmp"
	xdraw "golang.org/x/image/draw"
)

// masterSize is the resolution the logo is rasterized at once, before
// every asset is scaled down from it.
//
// Rendering small and rendering large then reducing are not equivalent:
// the reduction resolves the hook's opening and the badge's corners with
// the whole pixel grid to work with, which is what keeps the 16-pixel
// frame legible.
const masterSize = 1024

// iconSizes are the frames a Windows icon carries, smallest first. 16 is
// the list view and the window caption, 256 is the extra-large view.
var iconSizes = []int{16, 20, 24, 32, 48, 64, 128, 256}

// docSizes are the PNGs the documentation and the repository listing use.
var docSizes = []int{128, 256, 512}

// Installer bitmap dimensions, which Windows Installer does not scale.
const (
	bannerWidth, bannerHeight = 493, 58
	dialogWidth, dialogHeight = 493, 312
)

// dialogTint is how far towards white the wizard's tall bitmap lightens
// the badge colour for its text area: far enough for the installer's own
// black body text to sit on it, near enough that the panel beside it
// still reads as the same colour.
const dialogTint = 0.92

func main() {
	var (
		svgPath = flag.String("svg", filepath.Join("..", "..", "assets", "logo.svg"), "master logo to derive from")
		outDir  = flag.String("out", "dist", "directory to write the assets to")
		sysoOut = flag.String("syso", "", "path to write the Windows resource object to")
		arch    = flag.String("arch", "amd64", "architecture for the resource object: amd64, arm64 or 386")
		version = flag.String("version", "0.0.0", "version recorded in the resource object")
	)
	flag.Parse()

	if err := run(*svgPath, *outDir, *sysoOut, *arch, *version); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(svgPath, outDir, sysoOut, arch, version string) error {
	master, err := rasterize(svgPath, masterSize)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", outDir, err)
	}

	iconPath := filepath.Join(outDir, "jacklet.ico")
	if err := writeIcon(iconPath, master); err != nil {
		return err
	}
	badge, err := badgeFill(svgPath)
	if err != nil {
		return err
	}
	if err := writeBitmaps(outDir, master, badge); err != nil {
		return err
	}
	if err := writeDocPNGs(outDir, master); err != nil {
		return err
	}
	if sysoOut != "" {
		if err := writeSyso(sysoOut, iconPath, arch, version); err != nil {
			return err
		}
	}
	return nil
}

// badgeFill reads the badge's own colour out of the master, which is what
// keeps the installer artwork from disagreeing with the logo: a second
// copy of the value here would go on painting the wizard's panel in last
// season's colour, with the badge drawn over it in this season's, and
// nothing in the build saying so.
//
// The badge is the first path the master draws, since the mark is knocked
// out of it.
func badgeFill(svgPath string) (color.RGBA, error) {
	file, err := os.Open(svgPath)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("opening the logo: %w", err)
	}
	defer file.Close()

	decoder := xml.NewDecoder(file)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return color.RGBA{}, fmt.Errorf("reading %s: %w", svgPath, err)
		}
		start, ok := token.(xml.StartElement)
		if !ok || start.Name.Local != "path" {
			continue
		}
		for _, attr := range start.Attr {
			if attr.Name.Local == "fill" {
				return parseHexColor(attr.Value)
			}
		}
		return color.RGBA{}, fmt.Errorf("%s: the badge path carries no fill", svgPath)
	}
	return color.RGBA{}, fmt.Errorf("%s: draws no path", svgPath)
}

// parseHexColor reads a #rgb or #rrggbb fill. The master is written by
// hand, so the short form is worth accepting rather than failing a release
// over a form an editor may well have left behind.
func parseHexColor(value string) (color.RGBA, error) {
	digits := strings.TrimPrefix(value, "#")
	if len(digits) == 3 {
		digits = string([]byte{digits[0], digits[0], digits[1], digits[1], digits[2], digits[2]})
	}
	if len(digits) != 6 {
		return color.RGBA{}, fmt.Errorf("%q is not a hex colour", value)
	}
	rgb, err := hex.DecodeString(digits)
	if err != nil {
		return color.RGBA{}, fmt.Errorf("%q is not a hex colour: %w", value, err)
	}
	return color.RGBA{R: rgb[0], G: rgb[1], B: rgb[2], A: 0xFF}, nil
}

// tint lightens a colour towards white by the given fraction, 0 leaving
// it alone and 1 returning white.
func tint(c color.RGBA, towardsWhite float64) color.RGBA {
	lighten := func(v uint8) uint8 {
		return uint8(float64(v) + (0xFF-float64(v))*towardsWhite + 0.5)
	}
	return color.RGBA{R: lighten(c.R), G: lighten(c.G), B: lighten(c.B), A: 0xFF}
}

// rasterize renders the SVG at size by size.
func rasterize(path string, size int) (*image.RGBA, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening the logo: %w", err)
	}
	defer file.Close()

	icon, err := oksvg.ReadIconStream(file)
	if err != nil {
		return nil, fmt.Errorf("reading the logo: %w", err)
	}
	icon.SetTarget(0, 0, float64(size), float64(size))

	raster := image.NewRGBA(image.Rect(0, 0, size, size))
	icon.Draw(rasterx.NewDasher(size, size, rasterx.NewScannerGV(size, size, raster, raster.Bounds())), 1.0)
	return raster, nil
}

// resize reduces the master to size by size.
func resize(master *image.RGBA, size int) *image.RGBA {
	out := image.NewRGBA(image.Rect(0, 0, size, size))
	xdraw.CatmullRom.Scale(out, out.Bounds(), master, master.Bounds(), xdraw.Src, nil)
	return out
}

func writeIcon(path string, master *image.RGBA) error {
	frames := make([]*image.RGBA, 0, len(iconSizes))
	for _, size := range iconSizes {
		frames = append(frames, resize(master, size))
	}

	file, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("creating the icon: %w", err)
	}
	defer file.Close()

	if err := writeICO(file, frames); err != nil {
		return fmt.Errorf("writing the icon: %w", err)
	}
	fmt.Printf("wrote %s (%s)\n", path, joinSizes(iconSizes))
	return nil
}

// writeBitmaps writes the installer's banner and dialog artwork.
//
// Both are opaque, which is what makes them 24-bit: a 32-bit bitmap draws
// a black box where the alpha should be on some Windows versions, and the
// installer does not scale either one, so the dimensions are exact.
func writeBitmaps(outDir string, master *image.RGBA, badge color.RGBA) error {
	banner := image.NewRGBA(image.Rect(0, 0, bannerWidth, bannerHeight))
	draw.Draw(banner, banner.Bounds(), &image.Uniform{color.White}, image.Point{}, draw.Src)
	// Right-aligned, which is where the stock wizard leaves room for it.
	placeMark(banner, master, bannerHeight-16, bannerWidth-bannerHeight+8, 8)

	dialog := image.NewRGBA(image.Rect(0, 0, dialogWidth, dialogHeight))
	draw.Draw(dialog, dialog.Bounds(), &image.Uniform{tint(badge, dialogTint)}, image.Point{}, draw.Src)
	// The stock wizard writes its welcome text over the right two thirds,
	// so the mark sits in the left panel.
	draw.Draw(dialog, image.Rect(0, 0, 164, dialogHeight), &image.Uniform{badge}, image.Point{}, draw.Src)
	placeMark(dialog, master, 96, 34, 60)

	for path, img := range map[string]image.Image{
		filepath.Join(outDir, "banner.bmp"): banner,
		filepath.Join(outDir, "dialog.bmp"): dialog,
	} {
		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
		if err := bmp.Encode(file, img); err != nil {
			file.Close()
			return fmt.Errorf("writing %s: %w", path, err)
		}
		file.Close()
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}

// placeMark draws the logo at size, with its top-left corner at x, y.
func placeMark(dst *image.RGBA, master *image.RGBA, size, x, y int) {
	mark := resize(master, size)
	draw.Draw(dst, mark.Bounds().Add(image.Pt(x, y)), mark, image.Point{}, draw.Over)
}

func writeDocPNGs(outDir string, master *image.RGBA) error {
	for _, size := range docSizes {
		path := filepath.Join(outDir, fmt.Sprintf("logo-%d.png", size))
		file, err := os.Create(path)
		if err != nil {
			return fmt.Errorf("creating %s: %w", path, err)
		}
		if err := png.Encode(file, resize(master, size)); err != nil {
			file.Close()
			return fmt.Errorf("writing %s: %w", path, err)
		}
		file.Close()
		fmt.Printf("wrote %s\n", path)
	}
	return nil
}

// writeSyso writes the Windows resource object that carries the icon and
// the version details into the executable.
//
// The Go linker picks a .syso up by its GOOS and GOARCH suffix, so the
// caller names it; an unsuffixed one would be linked into every build,
// including the ones that are not Windows at all.
func writeSyso(path, iconPath, arch, version string) error {
	major, minor, patch := splitVersion(version)
	// The resource's own version strings are parsed as x.y.z, so they
	// carry the numeric version rather than the tag it came from.
	numeric := fmt.Sprintf("%d.%d.%d", major, minor, patch)

	info := &goversioninfo.VersionInfo{
		FixedFileInfo: goversioninfo.FixedFileInfo{
			FileVersion:    goversioninfo.FileVersion{Major: major, Minor: minor, Patch: patch},
			ProductVersion: goversioninfo.FileVersion{Major: major, Minor: minor, Patch: patch},
			FileFlagsMask:  "3f",
			FileOS:         "040004",
			FileType:       "01",
		},
		StringFileInfo: goversioninfo.StringFileInfo{
			CompanyName:      "TorrPlay",
			FileDescription:  "Torznab/Newznab indexer proxy",
			FileVersion:      numeric,
			InternalName:     "jacklet",
			LegalCopyright:   "Copyright (c) TorrPlay. MIT licensed.",
			OriginalFilename: "jacklet.exe",
			ProductName:      "Jacklet",
			ProductVersion:   numeric,
		},
		VarFileInfo: goversioninfo.VarFileInfo{
			Translation: goversioninfo.Translation{LangID: 0x0409, CharsetID: 0x04B0},
		},
		IconPath: iconPath,
	}

	info.Build()
	info.Walk()
	if err := info.WriteSyso(path, arch); err != nil {
		return fmt.Errorf("writing the resource object: %w", err)
	}
	fmt.Printf("wrote %s (%s, version %s)\n", path, arch, numeric)
	return nil
}

// splitVersion reads the numeric parts of a version, ignoring a leading
// "v" and any pre-release suffix, which the resource object cannot carry.
func splitVersion(version string) (major, minor, patch int) {
	trimmed := strings.TrimPrefix(version, "v")
	if cut := strings.IndexAny(trimmed, "-+"); cut >= 0 {
		trimmed = trimmed[:cut]
	}
	parts := strings.Split(trimmed, ".")
	numbers := make([]int, 3)
	for i := range numbers {
		if i < len(parts) {
			numbers[i], _ = strconv.Atoi(parts[i])
		}
	}
	return numbers[0], numbers[1], numbers[2]
}

func joinSizes(sizes []int) string {
	parts := make([]string, len(sizes))
	for i, size := range sizes {
		parts[i] = strconv.Itoa(size)
	}
	return strings.Join(parts, ", ")
}
