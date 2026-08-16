package resolver

import "github.com/zzet/gortex/internal/graph"

// churnShapeClass splits the non-conversion ("churn") share of a pass's
// reindex volume by what actually changed on the row, so the log can say
// which store write path the volume needs: a moved (still-unresolved)
// target rewrites the row identity, a kind promotion rewrites the key's
// kind column, and a payload-only rewrite (confidence / origin / meta /
// terminality) is servable by the attribute-only UPDATE arm of
// ReindexEdges without touching the row identity at all.
type churnShapeClass int

const (
	churnShapeRetarget churnShapeClass = iota
	churnShapeKind
	churnShapeAttrs
)

// churnShape classifies one non-conversion reindex. A target move dominates
// a simultaneous kind change: identity rewrite cost is what the class
// tracks, and a moved target forces it regardless of the kind.
func churnShape(oldTo, newTo string, oldKind, newKind graph.EdgeKind) churnShapeClass {
	switch {
	case newTo != oldTo:
		return churnShapeRetarget
	case newKind != oldKind:
		return churnShapeKind
	default:
		return churnShapeAttrs
	}
}
