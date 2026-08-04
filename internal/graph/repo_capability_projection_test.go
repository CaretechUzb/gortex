package graph

import "testing"

type capabilityScannerCapture struct {
	Store
	calls      int
	prefixes   []string
	pageSize   int
	yieldCalls int
}

func (c *capabilityScannerCapture) ScanRepoCapabilityEdges(
	repoPrefixes []string,
	pageSize int,
	yield func([]RepoCapabilityEdge) bool,
) {
	c.calls++
	c.prefixes = repoPrefixes
	c.pageSize = pageSize
	if yield != nil {
		c.yieldCalls++
		yield(nil)
	}
}

func TestScanRepoCapabilityEdgesNilScopeMeansWholeGraph(t *testing.T) {
	capture := &capabilityScannerCapture{}
	ScanRepoCapabilityEdges(capture, nil, 0, func([]RepoCapabilityEdge) bool { return true })

	if capture.calls != 1 {
		t.Fatalf("scanner calls = %d, want 1", capture.calls)
	}
	if capture.prefixes != nil {
		t.Fatalf("prefixes = %#v, want nil whole-graph scope", capture.prefixes)
	}
	if capture.pageSize != defaultRepoCapabilityEdgePageSize {
		t.Fatalf("page size = %d, want %d", capture.pageSize, defaultRepoCapabilityEdgePageSize)
	}
	if capture.pageSize != 4096 {
		t.Fatalf("default page size = %d, want bounded 4096", capture.pageSize)
	}
}

func TestScanRepoCapabilityEdgesExactEmptyScopeDoesNotScan(t *testing.T) {
	capture := &capabilityScannerCapture{}
	ScanRepoCapabilityEdges(capture, []string{}, 32, func([]RepoCapabilityEdge) bool { return true })
	if capture.calls != 0 {
		t.Fatalf("scanner calls = %d, want 0", capture.calls)
	}
}

func TestScanRepoCapabilityEdgesForwardsExplicitScopeAndPage(t *testing.T) {
	capture := &capabilityScannerCapture{}
	prefixes := []string{"a", "b"}
	ScanRepoCapabilityEdges(capture, prefixes, 73, func([]RepoCapabilityEdge) bool { return true })
	if capture.calls != 1 || capture.pageSize != 73 {
		t.Fatalf("calls/page = %d/%d, want 1/73", capture.calls, capture.pageSize)
	}
	if len(capture.prefixes) != 2 || capture.prefixes[0] != "a" || capture.prefixes[1] != "b" {
		t.Fatalf("prefixes = %#v, want %#v", capture.prefixes, prefixes)
	}
}
