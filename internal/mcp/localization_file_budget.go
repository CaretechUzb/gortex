package mcp

import (
	"context"
	"sync"
)

type localizationFileRequestBudgetContextKey struct{}

// localizationFileRequestBudget is shared by every bounded file-summary
// lookup descended from one MCP request context. Reservations make parallel
// builders conservative: their combined in-flight and consumed allowance can
// never exceed the request cap.
type localizationFileRequestBudget struct {
	mu        sync.Mutex
	remaining int
}

func withLocalizationFileRequestBudget(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if budget, _ := ctx.Value(localizationFileRequestBudgetContextKey{}).(*localizationFileRequestBudget); budget != nil {
		return ctx
	}
	return context.WithValue(ctx, localizationFileRequestBudgetContextKey{}, &localizationFileRequestBudget{
		remaining: localizationFileRequestLimit,
	})
}

// localizationFileBudgetFor returns the request-owned counter when middleware
// installed one. Direct compatibility callers receive a private counter, so
// they remain bounded without sharing state across unrelated calls.
func localizationFileBudgetFor(ctx context.Context) *localizationFileRequestBudget {
	if ctx != nil {
		if budget, _ := ctx.Value(localizationFileRequestBudgetContextKey{}).(*localizationFileRequestBudget); budget != nil {
			return budget
		}
	}
	return &localizationFileRequestBudget{remaining: localizationFileRequestLimit}
}

func (budget *localizationFileRequestBudget) reserve(limit int) int {
	if budget == nil || limit <= 0 {
		return 0
	}
	budget.mu.Lock()
	defer budget.mu.Unlock()
	if budget.remaining <= 0 {
		return 0
	}
	if limit > budget.remaining {
		limit = budget.remaining
	}
	budget.remaining -= limit
	return limit
}

// finish refunds the portion of a successful reservation that storage proved
// it did not consume. Callers deliberately do not invoke this after errors or
// cancellation: the amount inspected is then uncertain, so the full
// reservation stays charged and subsequent attribution fails closed.
func (budget *localizationFileRequestBudget) finish(reserved, consumed int) {
	if budget == nil || reserved <= 0 {
		return
	}
	if consumed < 0 {
		consumed = 0
	}
	if consumed > reserved {
		consumed = reserved
	}
	refund := reserved - consumed
	if refund == 0 {
		return
	}
	budget.mu.Lock()
	budget.remaining += refund
	budget.mu.Unlock()
}
