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

	"golang.org/x/image/font"
)

// Benchmark comparison charts: Vela researches real scores (web_search +
// fetch_url), then passes the numbers here to render a grouped-bar PNG that
// posts inline in Discord. Bars start at zero on a single axis; colors follow
// the model in listed order (stable across re-renders when the caller appends
// new models at the end); a score a lab doesn't report arrives as null and is
// drawn as an explicit gap, never invented.

type benchModel struct {
	Name   string     `json:"name"`
	Scores []*float64 `json:"scores"`
}

// benchPalette: categorical slots validated for CVD separation and ≥3:1
// contrast against colBG. Assigned by list position — never cycled, which is
// why maxBenchModels equals its length.
var benchPalette = []color.RGBA{
	{0x39, 0x87, 0xe5, 0xff}, // blue
	{0xd9, 0x59, 0x26, 0xff}, // orange
	{0x19, 0x9e, 0x70, 0xff}, // aqua
	{0xc9, 0x85, 0x00, 0xff}, // yellow
	{0xd5, 0x51, 0x81, 0xff}, // magenta
	{0x00, 0x83, 0x00, 0xff}, // green
	{0x90, 0x85, 0xe9, 0xff}, // violet
	{0xe6, 0x67, 0x67, 0xff}, // red
}

const (
	maxBenchModels = 8
	maxBenchmarks  = 10
)

// benchChart is the tool entry point: validate the shape, render, attach.
func (tc *ToolCtx) benchChart(a toolArgs) string {
	title := strings.TrimSpace(a.Title)
	if title == "" {
		title = "LLM benchmarks"
	}
	if len(a.Benchmarks) == 0 || len(a.Models) == 0 {
		return "bench error: need benchmarks (names) and models (each with scores aligned to benchmarks)."
	}
	if len(a.Benchmarks) > maxBenchmarks {
		return fmt.Sprintf("bench error: too many benchmarks (%d, max %d) — pick the most relevant ones.", len(a.Benchmarks), maxBenchmarks)
	}
	if len(a.Models) > maxBenchModels {
		return fmt.Sprintf("bench error: too many models (%d, max %d) — split into two charts.", len(a.Models), maxBenchModels)
	}
	for _, m := range a.Models {
		if strings.TrimSpace(m.Name) == "" {
			return "bench error: every model needs a name."
		}
		if len(m.Scores) != len(a.Benchmarks) {
			return fmt.Sprintf("bench error: %q has %d scores but there are %d benchmarks — align them 1:1 (use null for a benchmark that model doesn't report; never guess).",
				m.Name, len(m.Scores), len(a.Benchmarks))
		}
		for i, s := range m.Scores {
			if s != nil && (*s < 0 || math.IsNaN(*s) || math.IsInf(*s, 0)) {
				return fmt.Sprintf("bench error: bad score for %q on %q.", m.Name, a.Benchmarks[i])
			}
		}
	}
	src := strings.TrimSpace(a.Source)
	if src == "" {
		src = "mixed sources"
	}
	out := tc.saveArtifact(chartSlug(title)+".png",
		string(renderBenchPNG(title, src, a.Benchmarks, a.Models)))
	if strings.HasPrefix(out, "artifact error") {
		return out
	}
	return fmt.Sprintf("Charted %d model(s) across %d benchmark(s) — the image is attached to your reply. Give the key numbers and their source/date in your text too.",
		len(a.Models), len(a.Benchmarks))
}

func fillRect(img *image.RGBA, x, y, w, h int, c color.RGBA) {
	draw.Draw(img, image.Rect(x, y, x+w, y+h), image.NewUniform(c), image.Point{}, draw.Src)
}

// benchNum formats a score: integers with commas, else one decimal.
func benchNum(v float64) string {
	if v == math.Trunc(v) && math.Abs(v) < 1e6 {
		return withCommas(fmt.Sprintf("%.0f", v))
	}
	return fmt.Sprintf("%.1f", v)
}

// truncLabel shortens s (rune-safe, with an ellipsis) until it fits width px.
func truncLabel(face font.Face, s string, width int) string {
	if textW(face, s) <= width {
		return s
	}
	r := []rune(s)
	for len(r) > 1 {
		r = r[:len(r)-1]
		if t := string(r) + "…"; textW(face, t) <= width {
			return t
		}
	}
	return "…"
}

func renderBenchPNG(title, source string, benchmarks []string, models []benchModel) []byte {
	const W, H = 1600, 900
	const PL, PR, PB = 100, 44, 74
	img := image.NewRGBA(image.Rect(0, 0, W, H))
	draw.Draw(img, img.Bounds(), image.NewUniform(colBG), image.Point{}, draw.Src)

	drawText(img, 40, 56, truncLabel(faceTitle, title, W-80), colFG, faceTitle)
	drawText(img, 40, 92, truncLabel(faceSub, source, W-80), colMut, faceSub)

	// Legend (models are the series): swatch + name, wrapping rows. A single
	// model needs no legend — the title names it.
	legendY := 130
	if len(models) > 1 {
		lx := 40
		for i, m := range models {
			w := 28 + textW(faceSub, m.Name) + 36
			if lx+w > W-40 && lx > 40 {
				lx, legendY = 40, legendY+36
			}
			fillRect(img, lx, legendY-16, 20, 20, benchPalette[i])
			drawText(img, lx+28, legendY, m.Name, colFG, faceSub)
			lx += w
		}
		legendY += 24
	} else {
		legendY = 110
	}
	PT := legendY + 30

	// Y scale: bars always start at zero; 0–100 for percentage-style scores,
	// else a padded ceiling for bigger scales (Elo, token counts).
	ymax := 0.0
	for _, m := range models {
		for _, s := range m.Scores {
			if s != nil && *s > ymax {
				ymax = *s
			}
		}
	}
	if ymax <= 100 {
		ymax = 100
	} else {
		step := math.Pow(10, math.Floor(math.Log10(ymax*1.05)))
		ymax = math.Ceil(ymax * 1.05 / step) * step
	}
	plotW, plotH := W-PL-PR, H-PT-PB
	baseY := PT + plotH
	Y := func(v float64) int { return baseY - int(v/ymax*float64(plotH)) }

	for i := 0; i <= 4; i++ {
		v := ymax * float64(i) / 4
		yy := Y(v)
		hline(img, PL, W-PR, yy, colGrid)
		lbl := benchNum(v)
		drawText(img, PL-14-textW(faceAxis, lbl), yy+7, lbl, colMut, faceAxis)
	}

	nb, nm := len(benchmarks), len(models)
	groupW := plotW / nb
	const gap = 3 // spacer between adjacent bars
	pad := groupW / 8
	if pad < 8 {
		pad = 8
	}
	barW := (groupW - 2*pad - (nm-1)*gap) / nm
	if barW < 4 {
		barW = 4
	}
	if barW > 130 {
		barW = 130
	}
	innerW := nm*barW + (nm-1)*gap

	for bi, bname := range benchmarks {
		gx := PL + bi*groupW
		x0 := gx + (groupW-innerW)/2
		for mi, m := range models {
			bx := x0 + mi*(barW+gap)
			s := m.Scores[bi]
			if s == nil {
				// unreported: an explicit muted baseline tick, not a guessed bar
				fillRect(img, bx, baseY-4, barW, 4, colMut)
				lbl := "n/a"
				if lw := textW(faceAxis, lbl); lw <= barW+gap {
					drawText(img, bx+(barW-lw)/2, baseY-14, lbl, colMut, faceAxis)
				}
				continue
			}
			top := Y(*s)
			if *s > 0 && top > baseY-2 {
				top = baseY - 2 // keep a sliver visible for tiny nonzero scores
			}
			fillRect(img, bx, top, barW, baseY-top, benchPalette[mi])
			// value label in ink (not series color), only where it fits
			lbl := benchNum(*s)
			if lw := textW(faceAxis, lbl); lw <= barW+gap {
				drawText(img, bx+(barW-lw)/2, top-8, lbl, colFG, faceAxis)
			}
		}
		lbl := truncLabel(faceAxis, bname, groupW-8)
		drawText(img, gx+(groupW-textW(faceAxis, lbl))/2, H-28, lbl, colMut, faceAxis)
	}
	hline(img, PL, W-PR, baseY, colMut)

	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}
