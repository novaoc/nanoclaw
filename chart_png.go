package main

import (
	"bytes"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
	"strings"
	"time"

	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/gobold"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
)

// A static PNG line chart — renders inline in Discord (an HTML file only shows
// as a download). High resolution + a vector font so it stays crisp when Discord
// scales it (retina, mobile). Pure Go, no headless browser.

var (
	colBG   = color.RGBA{0x0d, 0x11, 0x17, 0xff}
	colGrid = color.RGBA{0x22, 0x28, 0x30, 0xff}
	colMut  = color.RGBA{0x8b, 0x94, 0x9e, 0xff}
	colFG   = color.RGBA{0xe6, 0xed, 0xf3, 0xff}
	colUp   = color.RGBA{0x3f, 0xb9, 0x50, 0xff}
	colDn   = color.RGBA{0xf8, 0x51, 0x49, 0xff}

	faceTitle, facePrice, faceSub, faceAxis font.Face
)

func mkFace(ttf []byte, pts float64) font.Face {
	f, err := opentype.Parse(ttf)
	if err != nil {
		return nil
	}
	fc, err := opentype.NewFace(f, &opentype.FaceOptions{Size: pts, DPI: 72, Hinting: font.HintingFull})
	if err != nil {
		return nil
	}
	return fc
}

func init() {
	faceTitle = mkFace(gobold.TTF, 30)
	facePrice = mkFace(gobold.TTF, 44)
	faceSub = mkFace(goregular.TTF, 20)
	faceAxis = mkFace(goregular.TTF, 20)
}

func withCommas(s string) string {
	n := len(s)
	if n <= 3 {
		return s
	}
	var b strings.Builder
	pre := n % 3
	if pre > 0 {
		b.WriteString(s[:pre])
	}
	for i := pre; i < n; i += 3 {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(s[i : i+3])
	}
	return b.String()
}

func chartMoney(v float64) string {
	if v < 10 {
		return fmt.Sprintf("$%.4g", v)
	}
	s := fmt.Sprintf("%.2f", v)
	if dot := strings.IndexByte(s, '.'); dot >= 0 {
		return "$" + withCommas(s[:dot]) + s[dot:]
	}
	return "$" + withCommas(s)
}

func drawText(img *image.RGBA, x, y int, s string, col color.Color, face font.Face) {
	(&font.Drawer{Dst: img, Src: image.NewUniform(col), Face: face, Dot: fixed.P(x, y)}).DrawString(s)
}

func textW(face font.Face, s string) int { return font.MeasureString(face, s).Round() }

func plot(img *image.RGBA, x, y int, c color.RGBA, r int) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			img.SetRGBA(x+dx, y+dy, c)
		}
	}
}

func drawSeg(img *image.RGBA, x0, y0, x1, y1 int, c color.RGBA, r int) {
	steps := int(math.Max(math.Abs(float64(x1-x0)), math.Abs(float64(y1-y0))))
	if steps == 0 {
		plot(img, x0, y0, c, r)
		return
	}
	for i := 0; i <= steps; i++ {
		t := float64(i) / float64(steps)
		plot(img, x0+int(float64(x1-x0)*t+0.5), y0+int(float64(y1-y0)*t+0.5), c, r)
	}
}

func hline(img *image.RGBA, x0, x1, y int, c color.RGBA) {
	for x := x0; x <= x1; x++ {
		img.SetRGBA(x, y, c)
	}
}

func renderChartPNG(title, sub string, pts []pricePoint) []byte {
	const W, H = 1600, 760
	const PL, PR, PT, PB = 128, 44, 168, 66
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), image.NewUniform(colBG), image.Point{}, draw.Src)

	x0ms, x1ms := pts[0].Ms, pts[len(pts)-1].Ms
	ymin, ymax := pts[0].Price, pts[0].Price
	for _, p := range pts {
		if p.Price < ymin {
			ymin = p.Price
		}
		if p.Price > ymax {
			ymax = p.Price
		}
	}
	pad := (ymax - ymin) * 0.08
	if pad == 0 {
		pad = math.Abs(ymax)*0.08 + 1
	}
	ymin, ymax = ymin-pad, ymax+pad
	X := func(ms int64) int {
		if x1ms == x0ms {
			return PL
		}
		return PL + int(float64(ms-x0ms)/float64(x1ms-x0ms)*float64(W-PL-PR))
	}
	Y := func(v float64) int {
		return PT + int((1-(v-ymin)/(ymax-ymin))*float64(H-PT-PB))
	}
	up := pts[len(pts)-1].Price >= pts[0].Price
	line := colUp
	if !up {
		line = colDn
	}

	for i := 0; i <= 4; i++ {
		v := ymin + (ymax-ymin)*float64(i)/4
		yy := Y(v)
		hline(img, PL, W-PR, yy, colGrid)
		lbl := chartMoney(v)
		drawText(img, PL-14-textW(faceAxis, lbl), yy+7, lbl, colMut, faceAxis)
	}
	for i := 0; i <= 3; i++ {
		ms := x0ms + (x1ms-x0ms)*int64(i)/3
		lbl := time.UnixMilli(ms).UTC().Format("Jan 2 '06")
		drawText(img, X(ms)-textW(faceAxis, lbl)/2, H-24, lbl, colMut, faceAxis)
	}
	for i := 1; i < len(pts); i++ {
		drawSeg(img, X(pts[i-1].Ms), Y(pts[i-1].Price), X(pts[i].Ms), Y(pts[i].Price), line, 2)
	}

	// header
	drawText(img, 40, 52, title, colFG, faceTitle)
	drawText(img, 40, 88, sub, colMut, faceSub)
	last, first := pts[len(pts)-1].Price, pts[0].Price
	chg := 0.0
	if first != 0 {
		chg = (last - first) / first * 100
	}
	lp := chartMoney(last)
	drawText(img, 40, 146, lp, colFG, facePrice)
	drawText(img, 40+textW(facePrice, lp)+22, 146, fmt.Sprintf("%+.1f%%", chg), line, faceSub)
	rng := fmt.Sprintf("%s – %s · %d pts",
		time.UnixMilli(x0ms).UTC().Format("Jan 2 '06"),
		time.UnixMilli(x1ms).UTC().Format("Jan 2 '06"), len(pts))
	drawText(img, W-PR-textW(faceAxis, rng), 52, rng, colMut, faceAxis)

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
