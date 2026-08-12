package lsp

import (
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestRouter_For_NoSpec returns an error for unknown extensions.
func TestRouter_For_NoSpec(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if _, err := r.For("README.md"); err == nil {
		t.Fatal("expected error for unknown ext")
	}
}

// TestRouter_AvailableSpecs filters by exec.LookPath. We can't assume
// any LSP server is on PATH on CI, so just check the call doesn't
// panic and returns a sane shape.
func TestRouter_AvailableSpecs(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	specs := r.AvailableSpecs()
	for _, s := range specs {
		if s == nil {
			t.Fatal("nil spec returned")
		}
		if s.Name == "" {
			t.Fatal("empty name returned")
		}
	}
}

// TestRouter_Stats_EmptyOnConstruct confirms a fresh router exposes
// no live providers until For() succeeds.
func TestRouter_Stats_EmptyOnConstruct(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("expected empty stats, got %v", got)
	}
}

// TestRouter_SupportedLanguages doesn't depend on PATH binaries —
// AvailableSpecs may be empty on CI; the function should still return
// a sorted, deduplicated slice (possibly empty).
func TestRouter_SupportedLanguages(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	langs := r.SupportedLanguages()
	for i := 1; i < len(langs); i++ {
		if langs[i-1] >= langs[i] {
			t.Errorf("not sorted: %v", langs)
		}
	}
}

// TestRouter_NoOpReap returns nothing when the router is empty.
func TestRouter_NoOpReap(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop()).WithIdleTimeout(time.Millisecond)
	defer r.Close()
	if names := r.Reap(); len(names) != 0 {
		t.Fatalf("expected no names, got %v", names)
	}
}

// TestRouter_LanguageIDForPath delegates to the package helper.
func TestRouter_LanguageIDForPath(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if got := r.LanguageIDForPath("a.ts"); got != "typescript" {
		t.Fatalf("got %q, want typescript", got)
	}
}

// TestRouter_RegisterSpec_Idempotent confirms that registering the
// same spec multiple times leaves a single entry in EnabledSpecs and
// that a nil spec is silently skipped.
func TestRouter_RegisterSpec_Idempotent(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	spec := SpecByName("gopls")
	if spec == nil {
		t.Fatal("expected gopls spec in registry")
	}

	r.RegisterSpec(nil) // no-op
	r.RegisterSpec(spec)
	r.RegisterSpec(spec) // duplicate — should not double-count

	got := r.EnabledSpecs()
	if len(got) != 1 || got[0].Name != "gopls" {
		t.Fatalf("expected exactly [gopls], got %+v", got)
	}
}

// TestRouter_EnabledSpecNames returns the alphabetised names of every
// registered spec — independent of which ones resolve on PATH (so it's
// CI-safe).
func TestRouter_EnabledSpecNames(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	for _, name := range []string{"gopls", "rust-analyzer", "clangd"} {
		if spec := SpecByName(name); spec != nil {
			r.RegisterSpec(spec)
		}
	}

	names := r.EnabledSpecNames()
	want := []string{"clangd", "gopls", "rust-analyzer"}
	if len(names) != len(want) {
		t.Fatalf("got %v, want %v", names, want)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("got %v, want %v", names, want)
		}
	}
}

// TestRouter_SpecAvailable_NotRegistered returns false for any spec
// the router doesn't know about — guards against the
// HasProviders-loop-walks-and-spawns regression.
func TestRouter_SpecAvailable_NotRegistered(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if r.SpecAvailable("never-registered") {
		t.Fatal("expected SpecAvailable=false for unregistered spec")
	}
	// Stats should still show no live providers — SpecAvailable is
	// pure read.
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("expected empty stats after SpecAvailable, got %v", got)
	}
}

// TestRouter_ProviderForSpec_Unregistered returns an error without
// spawning anything.
func TestRouter_ProviderForSpec_Unregistered(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()
	if _, err := r.ProviderForSpec("never-registered"); err == nil {
		t.Fatal("expected error for unregistered spec")
	}
	if got := r.Stats(); len(got) != 0 {
		t.Fatalf("expected empty stats, got %v", got)
	}
}

// TestRouter_SetDiagnosticsHook_PropagatesToExistingProvider — the
// hook installed on Router after a Provider is already alive must
// reach the existing Provider so old subscribers don't get stranded.
//
// We verify by attaching to a synthetically-constructed Provider via
// the package-internal seam (we don't spawn a real LSP — that needs
// a binary on PATH).
func TestRouter_SetDiagnosticsHook_PropagatesToExistingProvider(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	// Inject a placeholder provider directly into the router's map.
	// This bypasses the real spawn path (which needs an LSP binary).
	p := NewProvider("noop-cmd", nil, []string{"go"}, false, 1, zap.NewNop())
	r.mu.Lock()
	r.providers[providerKey{specName: "fake-spec", workspace: "/tmp/test"}] = &routedProvider{
		spec:      &ServerSpec{Name: "fake-spec", Languages: []string{"go"}},
		workspace: "/tmp/test",
		provider:  p,
	}
	r.mu.Unlock()

	var (
		gotSpec string
		gotPath string
		gotN    int
	)
	r.SetDiagnosticsHook(func(specName, absPath string, diags []Diagnostic) {
		gotSpec = specName
		gotPath = absPath
		gotN = len(diags)
	})

	// Trigger the existing provider's fanout — should reach our hook
	// because Router.SetDiagnosticsHook re-attached.
	p.fanoutDiagnostics("/abs/path/main.go", []Diagnostic{{Message: "x"}, {Message: "y"}})

	if gotSpec != "fake-spec" || gotPath != "/abs/path/main.go" || gotN != 2 {
		t.Fatalf("hook did not propagate — got specName=%q path=%q n=%d", gotSpec, gotPath, gotN)
	}
}

// TestRouter_SetDiagnosticsHook_Nil clears the hook on existing
// providers — no calls should land after detach.
func TestRouter_SetDiagnosticsHook_Nil(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	p := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	r.mu.Lock()
	r.providers[providerKey{specName: "fake-spec", workspace: "/tmp/test"}] = &routedProvider{
		spec:      &ServerSpec{Name: "fake-spec", Languages: []string{"go"}},
		workspace: "/tmp/test",
		provider:  p,
	}
	r.mu.Unlock()

	calls := 0
	r.SetDiagnosticsHook(func(_, _ string, _ []Diagnostic) { calls++ })
	p.fanoutDiagnostics("/abs/main.go", []Diagnostic{{}})
	if calls != 1 {
		t.Fatalf("expected 1 call after attach, got %d", calls)
	}
	r.SetDiagnosticsHook(nil)
	p.fanoutDiagnostics("/abs/main.go", []Diagnostic{{Message: "after-nil"}})
	if calls != 1 {
		t.Fatalf("expected hook to be cleared, got %d total calls", calls)
	}
}

// TestProvider_CloseDocument_Idempotent — closeDocument deletes the
// path from openDocs and is a no-op the second time. Sends nil
// payload to the LSP client when not initialised (test-only path).
func TestProvider_CloseDocument_Idempotent(t *testing.T) {
	p := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	// Mark the file as open via the internal state — bypasses the
	// LSP-spawn requirement of openDocument.
	p.docMu.Lock()
	p.openDocs["/abs/main.go"] = true
	p.docMu.Unlock()

	// First close attempts the LSP notification; client is nil so
	// it would panic — recover to confirm cleanup happened anyway.
	defer func() { _ = recover() }()
	_ = p.closeDocument("/abs/main.go")

	p.docMu.RLock()
	stillOpen := p.openDocs["/abs/main.go"]
	p.docMu.RUnlock()
	if stillOpen {
		t.Fatal("closeDocument should have removed the file from openDocs")
	}

	// Second close is a clean no-op (early-return before client touch).
	if err := p.closeDocument("/abs/main.go"); err != nil {
		t.Fatalf("expected nil on idempotent close, got %v", err)
	}
}

// TestProvider_SetDiagnosticsHook_DirectPath — Provider.fanoutDiagnostics
// invokes the per-Provider hook even without a Router.
func TestProvider_SetDiagnosticsHook_DirectPath(t *testing.T) {
	p := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	calls := 0
	p.SetDiagnosticsHook(func(_ string, _ []Diagnostic) { calls++ })
	p.fanoutDiagnostics("/abs/x.go", []Diagnostic{{}})
	p.SetDiagnosticsHook(nil)
	p.fanoutDiagnostics("/abs/y.go", []Diagnostic{{}})
	if calls != 1 {
		t.Fatalf("expected exactly one call, got %d", calls)
	}
}

// TestRouter_PerWorkspaceCacheIsolation — two requests for the same
// spec from different workspaces produce two distinct cache entries.
// Reusing the same workspace returns the same provider. Stats and
// Names reflect both (spec, workspace) pairs.
func TestRouter_PerWorkspaceCacheIsolation(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	specA := &ServerSpec{Name: "fake-spec", Languages: []string{"go"}}

	pA := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())
	pB := NewProvider("noop", nil, []string{"go"}, false, 1, zap.NewNop())

	// Inject two distinct providers under the SAME spec but different
	// workspaces — bypasses the real spawn (no LSP binary in tests).
	r.mu.Lock()
	r.providers[providerKey{specName: specA.Name, workspace: "/repo/a"}] = &routedProvider{
		spec:      specA,
		workspace: "/repo/a",
		provider:  pA,
		lastUsed:  timeNow(),
	}
	r.providers[providerKey{specName: specA.Name, workspace: "/repo/b"}] = &routedProvider{
		spec:      specA,
		workspace: "/repo/b",
		provider:  pB,
		lastUsed:  timeNow(),
	}
	r.mu.Unlock()

	if got := r.Names(); len(got) != 2 || got[0] != "fake-spec@/repo/a" || got[1] != "fake-spec@/repo/b" {
		t.Fatalf("expected two workspace-keyed names, got %v", got)
	}
	stats := r.Stats()
	if len(stats) != 2 {
		t.Fatalf("expected two stats rows, got %d", len(stats))
	}
	if stats[0].Workspace != "/repo/a" || stats[1].Workspace != "/repo/b" {
		t.Fatalf("expected stats sorted by spec then workspace, got %+v", stats)
	}
}

// TestRouter_DefaultWorkspace — ForSpec without explicit workspace
// uses the router's default. Used by Manager batch enrichment.
func TestRouter_DefaultWorkspace(t *testing.T) {
	tmp := t.TempDir()
	r := NewRouter(tmp, zap.NewNop())
	defer r.Close()
	if got := r.DefaultWorkspace(); got == "" || got[len(got)-len(tmp):] != tmp {
		t.Fatalf("DefaultWorkspace not resolved: got %q want suffix %q", got, tmp)
	}
}

// timeNow is a tiny shim so the test file doesn't need the time
// import for one call.
func timeNow() (t time.Time) { return time.Now() }

// TestRouter_RegisterAvailable_RespectsDisabled — every spec marked
// disabled in the input map must be skipped, even if its binary
// resolves on PATH. We pre-poison the avail cache so this test runs
// the same way on a CI box without any LSP binaries installed.
func TestRouter_RegisterAvailable_RespectsDisabled(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	// Force-mark every known spec as available so the test isn't
	// sensitive to the host's LSP install. Then exercise the
	// disabled-set logic in isolation.
	r.availMu.Lock()
	for _, s := range AllSpecs() {
		r.avail[s.Name] = true
	}
	r.availMu.Unlock()

	registered := r.RegisterAvailable(map[string]bool{
		"gopls":         true,
		"rust-analyzer": true,
	})

	for _, name := range registered {
		if name == "gopls" || name == "rust-analyzer" {
			t.Fatalf("disabled spec %q must not appear in registered list: %v", name, registered)
		}
	}
	enabled := r.EnabledSpecNames()
	for _, name := range enabled {
		if name == "gopls" || name == "rust-analyzer" {
			t.Fatalf("disabled spec %q must not appear in EnabledSpecNames: %v", name, enabled)
		}
	}
	// And at least one other spec should have been registered (the
	// registry contains 18+ entries; with PATH availability forced,
	// every non-disabled one lands).
	if len(registered) == 0 {
		t.Fatal("expected RegisterAvailable to register at least one non-disabled spec")
	}
}

// TestRouter_RegisterAvailable_SkipsBinariesNotOnPath — a spec whose
// command isn't on PATH must NOT be registered. Asserts the
// PATH-probe gate kicks in even when the disabled set is empty.
func TestRouter_RegisterAvailable_SkipsBinariesNotOnPath(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	// Pre-poison avail to false for every spec so RegisterAvailable
	// has to skip them all.
	r.availMu.Lock()
	for _, s := range AllSpecs() {
		r.avail[s.Name] = false
	}
	r.availMu.Unlock()

	registered := r.RegisterAvailable(nil)
	if len(registered) != 0 {
		t.Fatalf("expected zero registrations when no binaries are on PATH, got %v", registered)
	}
	if names := r.EnabledSpecNames(); len(names) != 0 {
		t.Fatalf("expected EnabledSpecNames to stay empty, got %v", names)
	}
}

// TestRouter_RegisterAvailable_Idempotent — re-running the auto-
// register pass over an already-populated router doesn't duplicate
// entries. Same shape as TestRouter_RegisterSpec_Idempotent but for
// the bulk path.
func TestRouter_RegisterAvailable_Idempotent(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	r.availMu.Lock()
	for _, s := range AllSpecs() {
		r.avail[s.Name] = true
	}
	r.availMu.Unlock()

	first := r.RegisterAvailable(nil)
	second := r.RegisterAvailable(nil)
	if len(first) != len(second) {
		t.Fatalf("re-running RegisterAvailable changed the registered set: first=%v second=%v", first, second)
	}
	enabled := r.EnabledSpecNames()
	if len(enabled) != len(first) {
		t.Fatalf("EnabledSpecNames length %d should equal RegisterAvailable return %d", len(enabled), len(first))
	}
}

// TestRouter_RegisterAvailable_KeepsConfigOverride — the auto-register
// pass must not clobber a spec already registered from .gortex.yaml
// with command / args / env overrides (the jdtls JRE-pin path).
func TestRouter_RegisterAvailable_KeepsConfigOverride(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop())
	defer r.Close()

	r.availMu.Lock()
	for _, s := range AllSpecs() {
		r.avail[s.Name] = true
	}
	r.availMu.Unlock()

	// The config loop registers an overridden jdtls spec first.
	override := SpecWithOverrides(SpecByName("jdtls"), "",
		[]string{"--jvm-arg=-Xmx4G"}, []string{"JAVA_HOME=/opt/jdk21"})
	r.RegisterSpec(override)

	// The auto-register pass runs and must leave the override intact.
	r.RegisterAvailable(nil)

	var got *ServerSpec
	for _, s := range r.EnabledSpecs() {
		if s.Name == "jdtls" {
			got = s
			break
		}
	}
	if got == nil {
		t.Fatal("jdtls not registered")
	}
	if len(got.Env) != 1 || got.Env[0] != "JAVA_HOME=/opt/jdk21" {
		t.Errorf("RegisterAvailable clobbered the config env override: %v", got.Env)
	}
	if len(got.Args) != 1 || got.Args[0] != "--jvm-arg=-Xmx4G" {
		t.Errorf("RegisterAvailable clobbered the config args override: %v", got.Args)
	}
}

// injectProvider adds a placeholder routedProvider directly into the
// router's cache, bypassing the spawn path (which needs a real LSP
// binary). Used by the pool-cap / eviction tests.
func injectProvider(r *Router, name, workspace string, lastUsed time.Time, inUse int) {
	p := NewProvider("noop-cmd", nil, []string{"go"}, false, 1, zap.NewNop())
	r.mu.Lock()
	r.providers[providerKey{specName: name, workspace: workspace}] = &routedProvider{
		spec:      &ServerSpec{Name: name, Languages: []string{"go"}},
		workspace: workspace,
		provider:  p,
		lastUsed:  lastUsed,
		inUse:     inUse,
	}
	r.mu.Unlock()
}

// TestRouter_SetMaxAlive_RaceSafe exercises SetMaxAlive / MaxAlive /
// EvictionCount concurrently — the -race detector must stay quiet.
func TestRouter_SetMaxAlive_RaceSafe(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop()).WithMaxAlive(6)
	defer r.Close()

	const goroutines = 16
	done := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func(n int) {
			for j := 0; j < 200; j++ {
				r.SetMaxAlive(4 + (n+j)%8)
				_ = r.MaxAlive()
				_ = r.EvictionCount()
			}
			done <- struct{}{}
		}(i)
	}
	for i := 0; i < goroutines; i++ {
		<-done
	}
}

// TestRouter_SetMaxAlive_RaisingPreventsEviction — with more live
// providers than the base cap, raising the cap above the live count
// leaves every provider intact and evicts nothing.
func TestRouter_SetMaxAlive_RaisingPreventsEviction(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop()).WithMaxAlive(2)
	defer r.Close()

	base := time.Now()
	injectProvider(r, "spec-a", "/repo/a", base, 0)
	injectProvider(r, "spec-b", "/repo/b", base.Add(time.Second), 0)
	injectProvider(r, "spec-c", "/repo/c", base.Add(2*time.Second), 0)

	// Raising the cap to accommodate every live provider evicts none.
	r.SetMaxAlive(6)
	if got := len(r.Names()); got != 3 {
		t.Fatalf("raising the cap evicted providers: want 3 alive, got %d", got)
	}
	if got := r.EvictionCount(); got != 0 {
		t.Fatalf("raising the cap must not evict: EvictionCount=%d", got)
	}
}

// TestRouter_SetMaxAlive_LoweringEvictsAndCounts — lowering the cap
// below the live count evicts the least-recently-used providers and the
// eviction counter increments once per genuine eviction.
func TestRouter_SetMaxAlive_LoweringEvictsAndCounts(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop()).WithMaxAlive(6)
	defer r.Close()

	base := time.Now()
	injectProvider(r, "spec-a", "/repo/a", base, 0) // oldest → evicted first
	injectProvider(r, "spec-b", "/repo/b", base.Add(time.Second), 0)
	injectProvider(r, "spec-c", "/repo/c", base.Add(2*time.Second), 0) // newest → survives

	r.SetMaxAlive(1)

	if got := len(r.Names()); got != 1 {
		t.Fatalf("want 1 provider alive after lowering cap to 1, got %d", got)
	}
	if got := r.EvictionCount(); got != 2 {
		t.Fatalf("want EvictionCount=2 after evicting 2 providers, got %d", got)
	}
	// The most-recently-used provider is the survivor.
	names := r.Names()
	if len(names) != 1 || names[0] != "spec-c@/repo/c" {
		t.Fatalf("LRU order wrong: survivor names=%v, want [spec-c@/repo/c]", names)
	}
}

// TestRouter_SetMaxAlive_NeverEvictsPinned — a provider pinned in-use is
// never evicted even when it is over the cap; only unpinned providers go.
func TestRouter_SetMaxAlive_NeverEvictsPinned(t *testing.T) {
	r := NewRouter(t.TempDir(), zap.NewNop()).WithMaxAlive(6)
	defer r.Close()

	base := time.Now()
	injectProvider(r, "pinned", "/repo/p", base, 1) // oldest but pinned
	injectProvider(r, "spec-b", "/repo/b", base.Add(time.Second), 0)
	injectProvider(r, "spec-c", "/repo/c", base.Add(2*time.Second), 0)

	r.SetMaxAlive(1)

	names := r.Names()
	found := false
	for _, n := range names {
		if n == "pinned@/repo/p" {
			found = true
		}
	}
	if !found {
		t.Fatalf("pinned provider was evicted: alive=%v", names)
	}
	// Only the two unpinned providers may be evicted.
	if got := r.EvictionCount(); got != 2 {
		t.Fatalf("want 2 unpinned evictions, got %d", got)
	}
}

// TestRouter_Stats_ReportsInUsePin — Stats must carry the pin count,
// not just the last-used time. A pinned provider is skipped by both
// the reaper and the LRU evictor, so a count that never returns to
// zero holds a language-server subprocess alive for the life of the
// daemon; without InUse in Stats that leak is invisible to
// `gortex daemon status`.
func TestRouter_Stats_ReportsInUsePin(t *testing.T) {
	workspace := t.TempDir()
	r := NewRouter(workspace, zap.NewNop())
	defer r.Close()

	const specName = "fake-spec"
	injectProvider(r, specName, workspace, time.Now(), 0)
	// Register the spec and force-mark it available so the pinning
	// fetch takes the cache-hit path without a real binary on PATH.
	r.RegisterSpec(&ServerSpec{Name: specName, Languages: []string{"go"}})
	r.availMu.Lock()
	r.avail[specName] = true
	r.availMu.Unlock()

	stats := r.Stats()
	if len(stats) != 1 {
		t.Fatalf("want 1 stats row, got %d", len(stats))
	}
	if stats[0].InUse != 0 {
		t.Fatalf("want InUse=0 before pinning, got %d", stats[0].InUse)
	}

	if _, err := r.ProviderForSpecWorkspace(specName, workspace); err != nil {
		t.Fatalf("ProviderForSpecWorkspace: %v", err)
	}
	stats = r.Stats()
	if len(stats) != 1 || stats[0].InUse != 1 {
		t.Fatalf("want InUse=1 while pinned, got %+v", stats)
	}

	r.ReleaseSpecWorkspace(specName, workspace)
	stats = r.Stats()
	if len(stats) != 1 || stats[0].InUse != 0 {
		t.Fatalf("want InUse=0 after release, got %+v", stats)
	}
}
