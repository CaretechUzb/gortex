package progress

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The gortex mark is a 5×5 dot-matrix "G": an open ring with a tongue in the
// middle row whose tip — the accent dot — always renders in brand green. The
// live renderer lights the remaining dots progressively as work completes, so
// the logo itself is the coarse progress gauge; banners render it fully lit.
type gDot struct {
	r, c   int
	accent bool
}

// gDots lists the mark's dots in row-major order — the order they light up.
var gDots = buildGDots()

func buildGDots() []gDot {
	const cells = 5
	mid := cells / 2
	var dots []gDot
	for r := 0; r < cells; r++ {
		for c := 0; c < cells; c++ {
			onTop := r == 0 && c >= 1 && c <= cells-2
			onBot := r == cells-1 && c >= 1 && c <= cells-2
			onLeft := c == 0 && r >= 1 && r <= cells-2
			onTongue := r == mid && c >= mid && c <= cells-1
			onTopRight := c == cells-1 && r == 1
			onBotRight := c == cells-1 && r == cells-2
			if onTop || onBot || onLeft || onTongue || onTopRight || onBotRight {
				dots = append(dots, gDot{r: r, c: c, accent: r == mid && c == cells-1})
			}
		}
	}
	return dots
}

// litDotCount is the number of dots that participate in progressive lighting
// (every dot except the always-lit accent tip).
func litDotCount() int { return len(gDots) - 1 }

// logoLines renders the mark as 5 rows. frac in [0,1] lights that fraction of
// the dots bottom-up in row-major order; frac < 0 selects the indeterminate
// marquee — a short lit window orbiting the mark at the given tick. glint, when
// >= 0, additionally paints dot (glint % len) in accent green: the traveling
// highlight used by animated banners over a fully lit mark.
func logoLines(lit, dim, accent lipgloss.Style, g glyphSet, frac float64, tick, glint int) [5]string {
	const cells = 5
	byPos := make(map[[2]int]int, len(gDots))
	for i, d := range gDots {
		byPos[[2]int{d.r, d.c}] = i
	}

	litN := -1
	if frac >= 0 {
		if frac > 1 {
			frac = 1
		}
		litN = int(frac*float64(litDotCount()) + 0.5)
	}

	// Non-accent ordinal per dot index, for both fill and marquee windows.
	ordinal := make([]int, len(gDots))
	ord := 0
	for i, d := range gDots {
		if d.accent {
			ordinal[i] = -1
			continue
		}
		ordinal[i] = ord
		ord++
	}

	const marqueeWindow = 4
	var lines [5]string
	for r := 0; r < cells; r++ {
		var row strings.Builder
		for c := 0; c < cells; c++ {
			if c > 0 {
				row.WriteString(" ")
			}
			idx, isDot := byPos[[2]int{r, c}]
			if !isDot {
				row.WriteString(" ")
				continue
			}
			d := gDots[idx]
			switch {
			case d.accent:
				row.WriteString(accent.Render(g.Dot))
			case glint >= 0 && idx == glint%len(gDots):
				row.WriteString(accent.Render(g.Dot))
			case litN >= 0 && ordinal[idx] < litN:
				row.WriteString(lit.Render(g.Dot))
			case litN < 0 && marqueeHit(ordinal[idx], tick, marqueeWindow):
				row.WriteString(lit.Render(g.Dot))
			default:
				row.WriteString(dim.Render(g.DotDim))
			}
		}
		lines[r] = row.String()
	}
	return lines
}

// marqueeHit reports whether the dot with the given non-accent ordinal falls
// inside the lit window at this tick.
func marqueeHit(ord, tick, window int) bool {
	n := litDotCount()
	if ord < 0 || n == 0 {
		return false
	}
	start := tick % n
	for i := 0; i < window; i++ {
		if (start+i)%n == ord {
			return true
		}
	}
	return false
}

// MeshLogo renders one static frame of the gortex mark, fully lit, styled
// with the package palette. A positive tick adds a traveling green glint —
// animated banners (the wizard dashboard) pass a rising tick; static banners
// pass 0 for the pure mark.
func MeshLogo(tick int) string {
	glint := -1
	if tick > 0 {
		glint = tick
	}
	lines := logoLines(stylePerim, styleInner, styleAccent, activeGlyphs(), 1, tick, glint)
	return strings.Join(lines[:], "\n")
}

// MeshLogoLines returns the number of vertical rows the mark occupies.
// Exported so wizard / dashboard layouts can reserve space without re-counting
// the constant.
func MeshLogoLines() int { return 5 }

// MeshLogoWidth returns the rendered visual width of the mark in cells.
// 5 cells × "● " spacing = 9 chars.
func MeshLogoWidth() int { return 9 }
