//go:build uifrontend

package main

import (
	"bytes"
	_ "embed"
	"image"
	"image/color"
	"image/png"
	"runtime"

	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

//go:embed gfs-neohellenic-bold-italic.ttf
var alphaFont []byte

// generateTrayIcon creates a 22x22 PNG of the Greek letter α (alpha) for the
// system tray. Uses the same GFS Neohellenic Bold Italic font as the app icon.
//
// macOS: black on transparent — the system tints it for dark/light mode
// automatically via SetTemplateIcon.
// Linux: white on transparent — libappindicator uses the icon as-is.
func generateTrayIcon() []byte {
	const size = 22

	fgColor := color.NRGBA{A: 255} // black template (macOS)
	if runtime.GOOS == "linux" {
		fgColor = color.NRGBA{R: 255, G: 255, B: 255, A: 255} // white
	}

	tt, err := opentype.Parse(alphaFont)
	if err != nil {
		return fallbackIcon(size, fgColor)
	}

	face, err := opentype.NewFace(tt, &opentype.FaceOptions{
		Size:    30,
		DPI:     72,
		Hinting: font.HintingFull,
	})
	if err != nil {
		return fallbackIcon(size, fgColor)
	}
	defer face.Close()

	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	// Center the glyph. bounds.Min.Y is negative (above baseline),
	// bounds.Max.Y is positive (below baseline).
	bounds, _, ok := face.GlyphBounds('α')
	if !ok {
		return fallbackIcon(size, fgColor)
	}
	glyphW := (bounds.Max.X - bounds.Min.X).Round()
	glyphH := (bounds.Max.Y - bounds.Min.Y).Round()
	dotX := (size-glyphW)/2 - bounds.Min.X.Round()
	dotY := (size-glyphH)/2 - bounds.Min.Y.Round()

	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(fgColor),
		Face: face,
		Dot:  fixed.P(dotX, dotY),
	}
	d.DrawString("α")

	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// fallbackIcon returns a simple filled circle as a last-resort tray icon.
func fallbackIcon(size int, c color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	cx, cy, r := float64(size)/2, float64(size)/2, float64(size)/2-2
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r*r {
				img.SetNRGBA(x, y, c)
			}
		}
	}
	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}
