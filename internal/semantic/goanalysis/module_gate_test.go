package goanalysis

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/tools/go/packages"
)

func writeProbeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("module probe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGoModulePresent(t *testing.T) {
	t.Run("root go.mod", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "go.mod"))
		if !goModulePresent(root) {
			t.Fatal("root go.mod not detected")
		}
	})
	t.Run("root go.work", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "go.work"))
		if !goModulePresent(root) {
			t.Fatal("root go.work not detected")
		}
	})
	t.Run("depth-two module", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "services", "api", "go.mod"))
		if !goModulePresent(root) {
			t.Fatal("depth-2 go.mod not detected")
		}
	})
	t.Run("depth-three module is out of reach", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "a", "b", "c", "go.mod"))
		if goModulePresent(root) {
			t.Fatal("depth-3 go.mod must not admit the pass")
		}
	})
	t.Run("non-Go repository", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "Cargo.toml"))
		writeProbeFile(t, filepath.Join(root, "src", "main.rs"))
		if goModulePresent(root) {
			t.Fatal("Cargo repo must not be admitted")
		}
	})
	t.Run("vendored and fixture manifests do not vouch", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "vendor", "go.mod"))
		writeProbeFile(t, filepath.Join(root, "node_modules", "pkg", "go.mod"))
		writeProbeFile(t, filepath.Join(root, "testdata", "go.mod"))
		if goModulePresent(root) {
			t.Fatal("vendored/fixture go.mod must not admit the pass")
		}
	})
}

func TestGoTypesProbeTimeoutEnv(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_PROBE_TIMEOUT", "")
	if got := goTypesProbeTimeout(); got != defaultGoTypesProbeTimeout {
		t.Fatalf("default = %v, want %v", got, defaultGoTypesProbeTimeout)
	}
	t.Setenv("GORTEX_GOTYPES_PROBE_TIMEOUT", "off")
	if got := goTypesProbeTimeout(); got != 0 {
		t.Fatalf("off = %v, want 0", got)
	}
	t.Setenv("GORTEX_GOTYPES_PROBE_TIMEOUT", "45s")
	if got := goTypesProbeTimeout(); got != 45*time.Second {
		t.Fatalf("override = %v, want 45s", got)
	}
	t.Setenv("GORTEX_GOTYPES_PROBE_TIMEOUT", "garbage")
	if got := goTypesProbeTimeout(); got != defaultGoTypesProbeTimeout {
		t.Fatalf("malformed = %v, want default", got)
	}
}

func TestProbeGoPackagesLoadableRealModuleLoads(t *testing.T) {
	root := t.TempDir()
	writeProbeFile(t, filepath.Join(root, "go.mod"))
	if err := os.WriteFile(filepath.Join(root, "go.mod"),
		[]byte("module example.com/probe\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Provider{}
	loadable, real, errored := p.probeGoPackagesLoadable(context.Background(), root)
	if !loadable || real < 1 {
		t.Fatalf("real module not loadable: loadable=%v real=%d errored=%d", loadable, real, errored)
	}
}

func TestProbeGoPackagesLoadableSubdirModuleRootFailsClosed(t *testing.T) {
	// go.mod lives under backend/, so `go list ./...` from the ROOT is
	// out-of-module and must fail closed (the growth/vioportals pathology).
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	if err := os.MkdirAll(backend, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend, "go.mod"),
		[]byte("module example.com/backend\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(backend, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Provider{}
	if loadable, real, _ := p.probeGoPackagesLoadable(context.Background(), root); loadable || real != 0 {
		t.Fatalf("out-of-module root must fail closed: loadable=%v real=%d", loadable, real)
	}
}

func TestProbeGoPackagesLoadableDisabledFailsOpen(t *testing.T) {
	// A disabled probe (timeout off) never skips, even on a non-module dir.
	t.Setenv("GORTEX_GOTYPES_PROBE_TIMEOUT", "off")
	p := &Provider{}
	if loadable, _, _ := p.probeGoPackagesLoadable(context.Background(), t.TempDir()); !loadable {
		t.Fatal("disabled probe must fail open")
	}
}

func TestProbeGoPackagesLoadableHonorsInjectedLoader(t *testing.T) {
	t.Setenv("GORTEX_GOTYPES_PROBE_TIMEOUT", "30s")
	// Injected loader returning a real package -> loadable.
	ok := &Provider{packagesLoad: func(*packages.Config, ...string) ([]*packages.Package, error) {
		return []*packages.Package{{PkgPath: "example.com/x"}}, nil
	}}
	if loadable, real, _ := ok.probeGoPackagesLoadable(context.Background(), t.TempDir()); !loadable || real != 1 {
		t.Fatalf("valid injected package must be loadable: loadable=%v real=%d", loadable, real)
	}
	// Injected loader returning only errored packages -> not loadable (skip).
	bad := &Provider{packagesLoad: func(*packages.Config, ...string) ([]*packages.Package, error) {
		return []*packages.Package{{PkgPath: "", Errors: []packages.Error{{Msg: "boom"}}}}, nil
	}}
	if loadable, real, errored := bad.probeGoPackagesLoadable(context.Background(), t.TempDir()); loadable || real != 0 || errored != 1 {
		t.Fatalf("all-errored load must not be loadable: loadable=%v real=%d errored=%d", loadable, real, errored)
	}
}

func writeGoModule(t *testing.T, dir, path string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module "+path+"\n\ngo 1.21\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGoModuleRoots(t *testing.T) {
	t.Run("no manifest", func(t *testing.T) {
		if roots := goModuleRoots(t.TempDir()); len(roots) != 0 {
			t.Fatalf("want no roots, got %v", roots)
		}
	})
	t.Run("root manifest is first", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "go.mod"))
		writeProbeFile(t, filepath.Join(root, "services", "api", "go.mod"))
		roots := goModuleRoots(root)
		if len(roots) != 2 || roots[0] != root {
			t.Fatalf("root manifest must sort first, got %v", roots)
		}
	})
	t.Run("depth-two module is located, not just detected", func(t *testing.T) {
		root := t.TempDir()
		want := filepath.Join(root, "services", "domain-core")
		writeProbeFile(t, filepath.Join(want, "go.mod"))
		roots := goModuleRoots(root)
		if len(roots) != 1 || roots[0] != want {
			t.Fatalf("want [%s], got %v", want, roots)
		}
	})
}

func TestGoLoadDir(t *testing.T) {
	t.Run("root manifest keeps the root", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "go.mod"))
		writeProbeFile(t, filepath.Join(root, "tools", "go.mod"))
		dir, n := goLoadDir(root)
		if dir != root || n != 2 {
			t.Fatalf("want root dir with 2 roots, got dir=%s n=%d", dir, n)
		}
	})
	t.Run("single nested module becomes the load dir", func(t *testing.T) {
		root := t.TempDir()
		want := filepath.Join(root, "backend")
		writeProbeFile(t, filepath.Join(want, "go.mod"))
		dir, n := goLoadDir(root)
		if dir != want || n != 1 {
			t.Fatalf("want %s with 1 root, got dir=%s n=%d", want, dir, n)
		}
	})
	t.Run("several nested modules stay ambiguous", func(t *testing.T) {
		root := t.TempDir()
		writeProbeFile(t, filepath.Join(root, "a", "go.mod"))
		writeProbeFile(t, filepath.Join(root, "b", "go.mod"))
		dir, n := goLoadDir(root)
		if dir != root || n != 2 {
			t.Fatalf("ambiguous layout must not pick one: dir=%s n=%d", dir, n)
		}
	})
	t.Run("no module reports zero", func(t *testing.T) {
		root := t.TempDir()
		if dir, n := goLoadDir(root); dir != root || n != 0 {
			t.Fatalf("want root/0, got dir=%s n=%d", dir, n)
		}
	})
}

func TestProbeGoPackagesLoadableSubdirModuleLoadsAtItsOwnDir(t *testing.T) {
	// Same fixture as TestProbeGoPackagesLoadableSubdirModuleRootFailsClosed:
	// the module lives under backend/. Probed at the ROOT it fails closed,
	// which is correct — `./...` there is out-of-module. Probed at the
	// directory that owns the module it loads cleanly, which is the point:
	// the module is healthy, only the directory was wrong.
	root := t.TempDir()
	backend := filepath.Join(root, "backend")
	writeGoModule(t, backend, "example.com/backend")

	p := &Provider{}
	if loadable, real, _ := p.probeGoPackagesLoadable(context.Background(), root); loadable || real != 0 {
		t.Fatalf("root probe must still fail closed: loadable=%v real=%d", loadable, real)
	}

	dir, n := goLoadDir(root)
	if dir != backend || n != 1 {
		t.Fatalf("goLoadDir must select the module dir: dir=%s n=%d", dir, n)
	}
	loadable, real, errored := p.probeGoPackagesLoadable(context.Background(), dir)
	if !loadable || real < 1 {
		t.Fatalf("module dir must load cleanly: loadable=%v real=%d errored=%d", loadable, real, errored)
	}
}
