package progress

import "github.com/charmbracelet/lipgloss"

// Cozy palette — gortex mark. One source of truth for brand color across the
// binary; the tracker binds these to its own writer-scoped renderer, while
// the static helpers below use the package-global styles.
var (
	colPerim     = lipgloss.Color("#F0F0F0")
	colInner     = lipgloss.Color("#46464E")
	colAccent    = lipgloss.Color("#5FD67A")
	colAccentDim = lipgloss.Color("#4FB268")
	colFg        = lipgloss.Color("#F4F3EF")
	colFgDim     = lipgloss.Color("#969699")
	colErr       = lipgloss.Color("#F06E64")

	stylePerim  = lipgloss.NewStyle().Foreground(colPerim)
	styleInner  = lipgloss.NewStyle().Foreground(colInner)
	styleAccent = lipgloss.NewStyle().Foreground(colAccent)
	styleLabel  = lipgloss.NewStyle().Bold(true).Foreground(colFg)
	styleSub    = lipgloss.NewStyle().Foreground(colFgDim)
	styleOK     = lipgloss.NewStyle().Foreground(colAccent)
	styleX      = lipgloss.NewStyle().Foreground(colErr)
)

// Exported palette — kept as a thin re-export layer over the package-private
// `col*` constants so the rest of the binary (cmd/gortex, internal/tui) can
// share one source of truth for brand colour without each callsite hard-coding
// hex literals.
//
// Anything inside internal/progress can keep using the short private names;
// callers outside the package should reach for these exported variants.
var (
	ColorAccent   = colAccent
	ColorPerim    = colPerim
	ColorInner    = colInner
	ColorFg       = colFg
	ColorFgDim    = colFgDim
	ColorErr      = colErr
	ColorWarn     = colWarn
	ColorMuted    = colMuted
	ColorBorder   = colBorder
	ColorInfoSoft = colInfoSoft
)

// Exported styles. Lipgloss styles are immutable on chaining (every setter
// returns a new copy) so exposing the package-level globals is safe — callers
// cannot mutate them in place.
var (
	StyleAccent  = styleAccent
	StyleLabel   = styleLabel
	StyleSub     = styleSub
	StyleOK      = styleOK
	StyleErr     = styleX
	StyleHeading = styleHeading
	StyleCount   = styleCount
	StyleKey     = styleKey
	StyleVal     = styleVal
	StyleHint    = styleHint
	StyleStep    = styleStep
	StyleStrong  = styleStrong
	StyleBox     = styleBox
)

// PaletteFg exposes the resolved lipgloss foreground color for callers that
// need to apply it to a freshly-built style (rather than re-using one of the
// pre-composed styles above). The returned value is a lipgloss.Color, ready to
// feed into any lipgloss.NewStyle().Foreground call.
func PaletteFg() lipgloss.Color { return colFg }
