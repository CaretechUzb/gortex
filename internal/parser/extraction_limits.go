package parser

import (
	"context"
	"errors"
	"fmt"
)

const (
	defaultExtractionMaxNodes       = 4096
	defaultExtractionMaxEdges       = 16384
	defaultExtractionMaxStdoutBytes = 16 << 20
	defaultExtractionMaxResultBytes = 16 << 20
	defaultExtractionMaxStderrBytes = 64 << 10
)

// ExtractionLimits bounds one extractor invocation. MaxNodes includes the
// always-emitted file node. A zero field is a strict zero budget; callers that
// want the finite defaults start from DefaultExtractionLimits.
type ExtractionLimits struct {
	MaxNodes int
	MaxEdges int
	// MaxStdoutBytes bounds plugin wire output.
	MaxStdoutBytes int
	// MaxResultBytes bounds the combined retained wire output and cumulative
	// synthesized node-ID bytes.
	MaxResultBytes int
	// MaxStderrBytes bounds retained diagnostics only. Zero retains none;
	// excess is discarded and does not fail an otherwise successful extraction.
	MaxStderrBytes int
}

// DefaultExtractionLimits returns the finite hard envelope used by legacy
// extraction. Bounded callers may narrow any field, including to zero.
func DefaultExtractionLimits() ExtractionLimits {
	return ExtractionLimits{
		MaxNodes:       defaultExtractionMaxNodes,
		MaxEdges:       defaultExtractionMaxEdges,
		MaxStdoutBytes: defaultExtractionMaxStdoutBytes,
		MaxResultBytes: defaultExtractionMaxResultBytes,
		MaxStderrBytes: defaultExtractionMaxStderrBytes,
	}
}

// Clamped restricts every caller-provided field to its finite hard maximum.
// Negative values fail closed to zero; positive values can never widen the
// process envelope beyond DefaultExtractionLimits.
func (limits ExtractionLimits) Clamped() ExtractionLimits {
	defaults := DefaultExtractionLimits()
	limits.MaxNodes = clampExtractionLimit(limits.MaxNodes, defaults.MaxNodes)
	limits.MaxEdges = clampExtractionLimit(limits.MaxEdges, defaults.MaxEdges)
	limits.MaxStdoutBytes = clampExtractionLimit(limits.MaxStdoutBytes, defaults.MaxStdoutBytes)
	limits.MaxResultBytes = clampExtractionLimit(limits.MaxResultBytes, defaults.MaxResultBytes)
	limits.MaxStderrBytes = clampExtractionLimit(limits.MaxStderrBytes, defaults.MaxStderrBytes)
	return limits
}

func clampExtractionLimit(value, hardMax int) int {
	if value < 0 {
		return 0
	}
	if value > hardMax {
		return hardMax
	}
	return value
}

// ErrExtractionLimit classifies every hard extraction-envelope failure.
var ErrExtractionLimit = errors.New("extraction limit exceeded")

// ExtractionLimitError reports that an extractor crossed a caller-visible
// hard envelope. Callers must discard any partial result on this error.
type ExtractionLimitError struct {
	Resource string
	Limit    int64
	Observed int64
}

func (e *ExtractionLimitError) Error() string {
	if e == nil {
		return ErrExtractionLimit.Error()
	}
	return fmt.Sprintf("%s: %s observed %d exceeds limit %d", ErrExtractionLimit, e.Resource, e.Observed, e.Limit)
}

func (e *ExtractionLimitError) Unwrap() error { return ErrExtractionLimit }

// ExtractionUsage reports conservative charges for one successful bounded
// extraction. RawNodes includes the mandatory file node and every inspected
// plugin node row; RawEdges includes every inspected edge row. Counts and
// ResultBytes are cumulative across duplicate fields (no refund). ResultBytes
// includes exact StdoutBytes plus synthesized node IDs. Failures report zero.
type ExtractionUsage struct {
	RawNodes    int
	RawEdges    int
	StdoutBytes int64
	ResultBytes int64
}

// BoundedExtractor is an optional Extractor capability for request-scoped
// cancellation and hard output/result envelopes. Legacy callers continue to
// use Extract unchanged.
type BoundedExtractor interface {
	Extractor
	ExtractBounded(context.Context, string, []byte, ExtractionLimits) (*ExtractionResult, ExtractionUsage, error)
}
