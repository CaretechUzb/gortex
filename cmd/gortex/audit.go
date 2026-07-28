// audit.go — `gortex audit` command. Produces a one-letter repo-level
// health grade (A-F) plus a shields.io-style SVG badge suitable for
// embedding in a README. The grade is computed by the daemon's
// audit_health tool (complexity-axis health score); this command renders
// the badge / JSON / text and writes the badge file locally.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"
	"github.com/spf13/cobra"

	gortexmcp "github.com/zzet/gortex/internal/mcp"
	"github.com/zzet/gortex/internal/progress"
	"github.com/zzet/gortex/internal/tui"
)

// auditReport / symbolScore alias the daemon's report types so the
// renderers below read unchanged.
type auditReport = gortexmcp.AuditReport
type symbolScore = gortexmcp.AuditSymbolScore

var (
	auditPath   string
	auditBadge  bool
	auditOut    string
	auditFormat string
)

var auditCmd = &cobra.Command{
	Use:   "audit",
	Short: "Compute a repo-level A-F health grade + emit a README-ready SVG badge",
	Long: `Reports a single A-F grade for the repo the daemon owns, based on
graph-topology complexity (fan-in / fan-out per callable symbol). Designed
for the README shield. Requires a running daemon that tracks the repo.

Output modes:

  --format svg   (default) shields.io-style SVG. Default path
                 .gortex/badge.svg. Embed in README via
                 ![gortex audit](.gortex/badge.svg).
  --format json  machine-readable score + per-axis breakdown.
  --format text  one-line grade + score for quick CLI use.

Examples:

  gortex audit                          # write .gortex/badge.svg
  gortex audit --format text            # "A · 87.4" on stdout
  gortex audit --format json --out -    # JSON on stdout
  gortex audit --path /tmp/myrepo       # audit a different tracked tree`,
	RunE: runAudit,
}

func init() {
	auditCmd.Flags().StringVar(&auditPath, "path", ".", "repository path to audit (the daemon must track it)")
	auditCmd.Flags().BoolVar(&auditBadge, "badge", true, "write an SVG shield (alias of --format svg)")
	auditCmd.Flags().StringVar(&auditOut, "out", "", "output path (default: .gortex/badge.svg for svg, stdout for json/text)")
	auditCmd.Flags().StringVar(&auditFormat, "format", "svg", "svg | json | text")
	rootCmd.AddCommand(auditCmd)
}

func runAudit(cmd *cobra.Command, _ []string) error {
	abs, err := filepath.Abs(auditPath)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	w := cmd.ErrOrStderr()
	emitAuditBanner(w, abs)

	// --path both selects the daemon connection and narrows the report.
	// Passing it as the `repo` selector is what makes `gortex audit
	// --path /tmp/myrepo` grade that tree rather than every repo the
	// daemon happens to track.
	out, err := requireDaemonTool(auditPath, "audit_health", map[string]any{"repo": abs})
	if err != nil {
		return err
	}
	var report auditReport
	if err := json.Unmarshal(out, &report); err != nil {
		return fmt.Errorf("decode audit report: %w", err)
	}

	// On non-TTY (or --no-progress / non-svg formats), preserve the legacy
	// summary line so script parsers keep working.
	if !progress.IsTTY(w) || noProgress || strings.ToLower(auditFormat) != "svg" {
		fmt.Fprintf(w,
			"[audit] %d callable symbols · mean complexity-health %.1f · grade %s\n",
			report.SymbolCount, report.MeanScore, report.Grade)
	} else {
		emitAuditGradeCard(w, report)
	}

	switch strings.ToLower(auditFormat) {
	case "svg":
		out := auditOut
		if out == "" {
			out = filepath.Join(abs, ".gortex", "badge.svg")
		}
		if out == "-" {
			_, err := cmd.OutOrStdout().Write([]byte(renderBadgeSVG(report)))
			return err
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(out, []byte(renderBadgeSVG(report)), 0o644); err != nil {
			return err
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "[audit] wrote %s\n", out)
		fmt.Fprintf(cmd.OutOrStdout(),
			"![gortex audit](%s) · grade %s · score %.1f\n",
			filepath.ToSlash(filepath.Clean(strings.TrimPrefix(out, abs+string(filepath.Separator)))),
			report.Grade, report.MeanScore)
	case "json":
		body := renderAuditJSON(report)
		if auditOut == "" || auditOut == "-" {
			_, _ = cmd.OutOrStdout().Write([]byte(body))
			_, _ = cmd.OutOrStdout().Write([]byte("\n"))
			return nil
		}
		return os.WriteFile(auditOut, []byte(body), 0o644)
	case "text":
		line := fmt.Sprintf("%s · %.1f", report.Grade, report.MeanScore)
		if auditOut == "" || auditOut == "-" {
			fmt.Fprintln(cmd.OutOrStdout(), line)
			return nil
		}
		return os.WriteFile(auditOut, []byte(line+"\n"), 0o644)
	default:
		return fmt.Errorf("unknown --format %q (want svg | json | text)", auditFormat)
	}
	return nil
}

// renderAuditJSON is the structured-output form of the report. Hand-built
// so the field order is stable regardless of map iteration.
func renderAuditJSON(r auditReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "{\n  \"symbol_count\": %d,\n  \"mean_score\": %.1f,\n  \"grade\": %q,\n",
		r.SymbolCount, r.MeanScore, r.Grade)
	b.WriteString("  \"grade_counts\": {")
	first := true
	for _, k := range []string{"A", "B", "C", "D", "F"} {
		if first {
			first = false
		} else {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n    %q: %d", k, r.GradeCounts[k])
	}
	b.WriteString("\n  },\n  \"worst_symbols\": [")
	for i, s := range r.WorstSymbols {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, "\n    {\"id\": %q, \"score\": %.1f, \"grade\": %q, \"file\": %q, \"line\": %d}",
			s.ID, s.Score, s.Grade, s.File, s.Line)
	}
	b.WriteString("\n  ]\n}")
	return b.String()
}

// renderBadgeSVG produces a shields.io-style two-cell badge.
func renderBadgeSVG(r auditReport) string {
	label := "gortex audit"
	grade := r.Grade
	colour := gradeColour(grade)

	labelW := 78
	gradeW := 26
	totalW := labelW + gradeW

	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="gortex audit: %s">
  <title>gortex audit: %s · %.1f</title>
  <linearGradient id="s" x2="0" y2="100%%">
    <stop offset="0" stop-color="#bbb" stop-opacity=".1"/>
    <stop offset="1" stop-opacity=".1"/>
  </linearGradient>
  <clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>
  <g clip-path="url(#r)">
    <rect width="%d" height="20" fill="#555"/>
    <rect x="%d" width="%d" height="20" fill="%s"/>
    <rect width="%d" height="20" fill="url(#s)"/>
  </g>
  <g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" font-size="11">
    <text x="%d" y="14">%s</text>
    <text x="%d" y="14">%s</text>
  </g>
</svg>
`,
		totalW, grade, grade, r.MeanScore,
		totalW,
		labelW,
		labelW, gradeW, colour,
		totalW,
		labelW/2, label,
		labelW+gradeW/2, grade,
	)
}

// emitAuditBanner prints the gortex banner naming the repo under audit.
func emitAuditBanner(w io.Writer, repoPath string) {
	if !progress.IsTTY(w) || noProgress {
		return
	}
	short := repoPath
	if home, err := os.UserHomeDir(); err == nil && strings.HasPrefix(repoPath, home) {
		short = "~" + strings.TrimPrefix(repoPath, home)
	}
	banner := tui.Banner{
		Title:    "gortex audit",
		Subtitle: "Complexity-axis health grade for " + filepath.Base(repoPath) + ".",
	}.Render()
	fmt.Fprintln(w)
	fmt.Fprintln(w, banner)
	fmt.Fprintln(w, "  "+progress.Row("repo", short, 8))
	fmt.Fprintln(w)
}

// emitAuditGradeCard renders the result panel: a colour-tiered grade chip,
// stat strip, and per-grade distribution.
func emitAuditGradeCard(w io.Writer, r auditReport) {
	gradeStyle := lipgloss.NewStyle().
		Bold(true).
		Foreground(progress.PaletteFg()).
		Background(auditGradeBG(r.Grade)).
		Padding(0, 2)
	gradeChip := gradeStyle.Render(" " + r.Grade + " ")

	fmt.Fprintln(w, "  "+gradeChip+"   "+
		progress.StyleStrong.Render(fmt.Sprintf("%.1f", r.MeanScore))+
		"  "+progress.StyleHint.Render("/ 100  ·  mean complexity-health"))

	stats := []string{
		progress.Stat(strconv.Itoa(r.SymbolCount), "callable symbols", progress.StatNeutral),
	}
	stats = append(stats, progress.Stat(gradeBlurb(r.Grade), "", auditStatSeverity(r.Grade)))
	fmt.Fprintln(w, "     "+progress.StatStrip(stats...))

	if len(r.GradeCounts) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "     "+progress.Heading("grade distribution"))
		parts := make([]string, 0, 5)
		for _, k := range []string{"A", "B", "C", "D", "F"} {
			parts = append(parts,
				lipgloss.NewStyle().Bold(true).Foreground(auditGradeBG(k)).Render(k)+
					"  "+progress.StyleVal.Render(strconv.Itoa(r.GradeCounts[k])),
			)
		}
		fmt.Fprintln(w, "     "+strings.Join(parts, progress.StyleHint.Render("   ·   ")))
	}
	fmt.Fprintln(w)
}

// Coverage-grade badge colours — the shields.io standard palette, shared
// by the terminal badge background (auditGradeBG) and the SVG/markdown
// badge colour (gradeColour).
const (
	badgeBrightGreen = "#4c1"
	badgeGreen       = "#97ca00"
	badgeYellow      = "#dfb317"
	badgeOrange      = "#fe7d37"
	badgeRed         = "#e05d44"
)

func auditGradeBG(grade string) lipgloss.Color {
	return lipgloss.Color(gradeColour(grade))
}

func gradeBlurb(grade string) string {
	switch grade {
	case "A":
		return "excellent topology"
	case "B":
		return "healthy"
	case "C":
		return "watch fan-out hotspots"
	case "D":
		return "consider refactoring"
	default:
		return "high coupling risk"
	}
}

func auditStatSeverity(grade string) progress.StatSeverity {
	switch grade {
	case "A", "B":
		return progress.StatGood
	case "C":
		return progress.StatNeutral
	case "D":
		return progress.StatWarn
	default:
		return progress.StatBad
	}
}

func gradeColour(grade string) string {
	switch grade {
	case "A":
		return badgeBrightGreen
	case "B":
		return badgeGreen
	case "C":
		return badgeYellow
	case "D":
		return badgeOrange
	default:
		return badgeRed
	}
}
