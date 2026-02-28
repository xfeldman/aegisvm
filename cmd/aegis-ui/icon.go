//go:build uifrontend

package main

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// generateTrayIcon creates a 22x22 shield-shaped PNG for the system tray.
// On macOS this is used as a template icon — the system tints it for
// dark/light mode automatically (black shape on transparent background).
//
// The shield shape: dome top (semicircle) + tapered body → pointed bottom.
// This is a placeholder; replace with a designed icon for production.
func generateTrayIcon() []byte {
	const size = 22
	img := image.NewNRGBA(image.Rect(0, 0, size, size))

	cx := float64(size) / 2 // center x = 11

	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5

			if isInShield(px, py, cx) {
				img.SetNRGBA(x, y, color.NRGBA{A: 255}) // black, full opacity
			}
		}
	}

	var buf bytes.Buffer
	png.Encode(&buf, img)
	return buf.Bytes()
}

// isInShield checks if a point (px, py) falls inside the shield shape.
//
// Shield geometry (22x22 canvas):
//
//	Dome:  semicircle centered at (11, 7) with radius 7
//	Body:  from y=7 to y=20, width tapers quadratically to a point
func isInShield(px, py, cx float64) bool {
	const (
		domeCenter = 7.0  // y-center of dome arc
		domeRadius = 6.5  // horizontal and vertical radius
		bodyBottom = 19.5 // y-coordinate of the shield point
	)

	// Above the dome
	if py < domeCenter-domeRadius {
		return false
	}
	// Below the point
	if py > bodyBottom {
		return false
	}

	dx := math.Abs(px - cx)

	if py <= domeCenter {
		// Dome region: semicircular arc
		dy := py - domeCenter
		sq := domeRadius*domeRadius - dy*dy
		if sq <= 0 {
			return false
		}
		return dx <= math.Sqrt(sq)
	}

	// Body region: quadratic taper from full width to point
	t := (py - domeCenter) / (bodyBottom - domeCenter) // 0 at dome, 1 at point
	halfW := domeRadius * (1 - t*t)                    // quadratic for smoother tip
	return dx <= halfW
}
