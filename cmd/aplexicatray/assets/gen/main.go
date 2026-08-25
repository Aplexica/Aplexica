//go:build ignore

// Asset-icon regenerator for cmd/aplexicatray/assets/{idle,active,
// paused,conflict,error}.{png,ico}. Run with:
//
//	go run ./cmd/aplexicatray/assets/gen/main.go
//
// The base mark matches the Aplexica website favicon.
//
// Tray states use only that Aplexica mark:
//   - idle/active: logo + green status dot
//   - paused: logo + yellow status dot
//   - error/stopped: logo + red status dot
//   - conflict/attention: logo + yellow exclamation mark

package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"math"
	"os"
	"path/filepath"
)

const (
	iconSize    = 32
	renderScale = 4
)

type variant struct {
	name      string
	statusDot color.RGBA
	attention bool
}

var variants = []variant{
	{name: "idle", statusDot: green},
	{name: "active", statusDot: green},
	{name: "paused", statusDot: yellow},
	{name: "conflict", attention: true},
	{name: "error", statusDot: red},
}

var (
	transparent = color.RGBA{0x00, 0x00, 0x00, 0x00}
	black       = color.RGBA{0x00, 0x00, 0x00, 0xff}
	cream       = color.RGBA{0xef, 0xff, 0xeb, 0xff}
	coral       = color.RGBA{0xff, 0x60, 0x44, 0xff}
	green       = color.RGBA{0x30, 0xd1, 0x58, 0xff}
	yellow      = color.RGBA{0xff, 0xd6, 0x0a, 0xff}
	red         = color.RGBA{0xff, 0x45, 0x3a, 0xff}
)

func main() {
	outDir := "cmd/aplexicatray/assets"
	if len(os.Args) >= 2 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		log.Fatal(err)
	}
	for _, v := range variants {
		img := renderIcon(v)
		writePNG(filepath.Join(outDir, v.name+".png"), img)
		writeICO(filepath.Join(outDir, v.name+".ico"), img)
		log.Printf("wrote %s.png + %s.ico", v.name, v.name)
	}
}

func renderIcon(v variant) *image.RGBA {
	hi := image.NewRGBA(image.Rect(0, 0, iconSize*renderScale, iconSize*renderScale))
	drawRoundedTile(hi)
	if v.attention {
		drawLogo(hi, transform{sx: 0.82, sy: 1, tx: -1.2, ty: 0})
		drawExclamation(hi)
	} else {
		drawLogo(hi, transform{sx: 1, sy: 1, tx: 0, ty: 0})
		drawStatusDot(hi, v.statusDot)
	}
	return downsample(hi)
}

type transform struct {
	sx float64
	sy float64
	tx float64
	ty float64
}

func (t transform) p(x, y float64) (float64, float64) {
	return x*t.sx + t.tx, y*t.sy + t.ty
}

func drawRoundedTile(img *image.RGBA) {
	const radius = 6.0
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			fx, fy := samplePoint(x, y)
			if insideRoundedRect(fx, fy, iconSize, iconSize, radius) {
				img.SetRGBA(x, y, black)
			} else {
				img.SetRGBA(x, y, transparent)
			}
		}
	}
}

func insideRoundedRect(x, y, w, h, r float64) bool {
	cx := clamp(x, r, w-r)
	cy := clamp(y, r, h-r)
	dx := x - cx
	dy := y - cy
	return dx*dx+dy*dy <= r*r
}

func drawLogo(img *image.RGBA, tx transform) {
	x1, y1 := tx.p(6, 26)
	x2, y2 := tx.p(16, 5)
	x3, y3 := tx.p(26, 26)
	drawStrokeLine(img, x1, y1, x2, y2, 3, cream)
	drawStrokeLine(img, x2, y2, x3, y3, 3, cream)

	drawQuadraticStroke(img, tx, 11, 18, 13.5, 15, 16, 18, 2.6, coral)
	drawQuadraticStroke(img, tx, 16, 18, 18.5, 21, 21, 18, 2.6, coral)
}

func drawQuadraticStroke(img *image.RGBA, tx transform, x0, y0, cx, cy, x1, y1, width float64, c color.RGBA) {
	px, py := tx.p(x0, y0)
	const steps = 18
	for i := 1; i <= steps; i++ {
		t := float64(i) / steps
		x := (1-t)*(1-t)*x0 + 2*(1-t)*t*cx + t*t*x1
		y := (1-t)*(1-t)*y0 + 2*(1-t)*t*cy + t*t*y1
		nx, ny := tx.p(x, y)
		drawStrokeLine(img, px, py, nx, ny, width, c)
		px, py = nx, ny
	}
}

func drawStatusDot(img *image.RGBA, c color.RGBA) {
	drawCircle(img, 25, 25, 6.4, black)
	drawCircle(img, 25, 25, 4.9, c)
}

func drawExclamation(img *image.RGBA) {
	drawStrokeLine(img, 26.5, 7, 26.5, 19, 5, black)
	drawStrokeLine(img, 26.5, 7, 26.5, 19, 3, yellow)
	drawCircle(img, 26.5, 25, 3.3, black)
	drawCircle(img, 26.5, 25, 2.1, yellow)
}

func drawCircle(img *image.RGBA, cx, cy, r float64, c color.RGBA) {
	rr := r * r
	for y := 0; y < img.Bounds().Dy(); y++ {
		for x := 0; x < img.Bounds().Dx(); x++ {
			fx, fy := samplePoint(x, y)
			dx := fx - cx
			dy := fy - cy
			if dx*dx+dy*dy <= rr {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func drawStrokeLine(img *image.RGBA, x1, y1, x2, y2, width float64, c color.RGBA) {
	r := width / 2
	minX := int(math.Floor((math.Min(x1, x2) - r - 1) * renderScale))
	maxX := int(math.Ceil((math.Max(x1, x2) + r + 1) * renderScale))
	minY := int(math.Floor((math.Min(y1, y2) - r - 1) * renderScale))
	maxY := int(math.Ceil((math.Max(y1, y2) + r + 1) * renderScale))
	minX = maxInt(minX, 0)
	minY = maxInt(minY, 0)
	maxX = minInt(maxX, img.Bounds().Dx()-1)
	maxY = minInt(maxY, img.Bounds().Dy()-1)

	rr := r * r
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			fx, fy := samplePoint(x, y)
			if distToSegmentSquared(fx, fy, x1, y1, x2, y2) <= rr {
				img.SetRGBA(x, y, c)
			}
		}
	}
}

func distToSegmentSquared(px, py, x1, y1, x2, y2 float64) float64 {
	dx := x2 - x1
	dy := y2 - y1
	if dx == 0 && dy == 0 {
		ox := px - x1
		oy := py - y1
		return ox*ox + oy*oy
	}
	t := ((px-x1)*dx + (py-y1)*dy) / (dx*dx + dy*dy)
	t = clamp(t, 0, 1)
	nx := x1 + t*dx
	ny := y1 + t*dy
	ox := px - nx
	oy := py - ny
	return ox*ox + oy*oy
}

func samplePoint(x, y int) (float64, float64) {
	return (float64(x) + 0.5) / renderScale, (float64(y) + 0.5) / renderScale
}

func downsample(src *image.RGBA) *image.RGBA {
	dst := image.NewRGBA(image.Rect(0, 0, iconSize, iconSize))
	samples := renderScale * renderScale
	for y := 0; y < iconSize; y++ {
		for x := 0; x < iconSize; x++ {
			var sumR, sumG, sumB, sumA int
			for yy := 0; yy < renderScale; yy++ {
				for xx := 0; xx < renderScale; xx++ {
					p := src.RGBAAt(x*renderScale+xx, y*renderScale+yy)
					sumR += int(p.R) * int(p.A)
					sumG += int(p.G) * int(p.A)
					sumB += int(p.B) * int(p.A)
					sumA += int(p.A)
				}
			}
			if sumA == 0 {
				dst.SetRGBA(x, y, transparent)
				continue
			}
			dst.SetRGBA(x, y, color.RGBA{
				R: uint8(sumR / sumA),
				G: uint8(sumG / sumA),
				B: uint8(sumB / sumA),
				A: uint8(sumA / samples),
			})
		}
	}
	return dst
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func writePNG(path string, img *image.RGBA) {
	f, err := os.Create(path)
	if err != nil {
		log.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		log.Fatal(err)
	}
}

func writeICO(path string, img *image.RGBA) {
	const w = iconSize
	const pixelBytes = w * w * 4
	const maskBytes = w * w / 8
	const bmpHeaderSize = 40
	const dirEntrySize = 16
	const dirSize = 6

	bmpDataSize := pixelBytes + maskBytes
	dirEntryOffset := dirSize + dirEntrySize

	var buf bytes.Buffer

	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: 1 = icon
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // count

	buf.WriteByte(byte(w))
	buf.WriteByte(byte(w))
	buf.WriteByte(0)                                                           // colorCount
	buf.WriteByte(0)                                                           // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))                         // planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))                        // bitcount
	binary.Write(&buf, binary.LittleEndian, uint32(bmpHeaderSize+bmpDataSize)) // bytesInRes
	binary.Write(&buf, binary.LittleEndian, uint32(dirEntryOffset))            // imageOffset

	binary.Write(&buf, binary.LittleEndian, uint32(bmpHeaderSize))
	binary.Write(&buf, binary.LittleEndian, int32(w))
	binary.Write(&buf, binary.LittleEndian, int32(w*2)) // biHeight = XOR+AND stacked
	binary.Write(&buf, binary.LittleEndian, uint16(1))
	binary.Write(&buf, binary.LittleEndian, uint16(32))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(bmpDataSize))
	binary.Write(&buf, binary.LittleEndian, int32(0))
	binary.Write(&buf, binary.LittleEndian, int32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))
	binary.Write(&buf, binary.LittleEndian, uint32(0))

	// Pixel data — BGRA, bottom-up rows.
	for y := w - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			p := img.RGBAAt(x, y)
			buf.WriteByte(p.B)
			buf.WriteByte(p.G)
			buf.WriteByte(p.R)
			buf.WriteByte(p.A)
		}
	}
	// AND-mask — 1 means transparent. Keep it aligned with alpha so
	// Windows preserves the rounded tile corners.
	for y := w - 1; y >= 0; y-- {
		for bx := 0; bx < w/8; bx++ {
			var mask byte
			for bit := 0; bit < 8; bit++ {
				x := bx*8 + bit
				if img.RGBAAt(x, y).A < 128 {
					mask |= 1 << (7 - bit)
				}
			}
			buf.WriteByte(mask)
		}
	}

	if err := os.WriteFile(path, buf.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}
