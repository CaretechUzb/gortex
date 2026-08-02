package goanalysis

// goUsePlan is a compiler-free in-memory projection grouped by package for
// bounded graph transactions. The measured dominant retention is the compiler
// AST/type graph; keeping this compact plan in memory avoids adding temporary-
// file I/O until profiling proves its residual heap is material.
type goUsePlan struct {
	packages [][]resolvedGoUse
	uses     int
}

func newGoUsePlan(packageCount int) *goUsePlan {
	return &goUsePlan{packages: make([][]resolvedGoUse, packageCount)}
}

func (p *goUsePlan) setPackage(index int, uses []resolvedGoUse) {
	p.packages[index] = uses
	p.uses += len(uses)
}

func (p *goUsePlan) len() int {
	if p == nil {
		return 0
	}
	return p.uses
}

func (p *goUsePlan) release() {
	if p == nil {
		return
	}
	for i := range p.packages {
		clear(p.packages[i])
		p.packages[i] = nil
	}
	p.packages = nil
	p.uses = 0
}
