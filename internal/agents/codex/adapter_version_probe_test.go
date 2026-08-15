package codex

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// TestCodexVersionOutput_TimesOut pins the deadline on the `codex --version`
// probe. A wedged binary previously blocked the caller forever and left a
// child process behind that never exited; the probe must give up and report
// an error instead.
func TestCodexVersionOutput_TimesOut(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake binary is a POSIX shell script")
	}

	dir := t.TempDir()
	fake := filepath.Join(dir, "codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nsleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)

	original := codexVersionProbeTimeout
	codexVersionProbeTimeout = 100 * time.Millisecond
	t.Cleanup(func() { codexVersionProbeTimeout = original })

	done := make(chan error, 1)
	go func() {
		_, err := codexVersionOutput()
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("hung probe returned success; want a timeout error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("probe did not honour its deadline")
	}
}

// TestCodexSupportsDirectToolNamespaces_TreatsProbeFailureAsCurrent documents
// the posture the timeout relies on: a probe that fails for any reason —
// including the new deadline — leaves direct tool exposure enabled rather
// than silently downgrading the install.
func TestCodexSupportsDirectToolNamespaces_TreatsProbeFailureAsCurrent(t *testing.T) {
	original := codexVersionOutput
	codexVersionOutput = func() ([]byte, error) { return nil, os.ErrDeadlineExceeded }
	t.Cleanup(func() { codexVersionOutput = original })

	supported, detected := codexSupportsDirectToolNamespaces()
	if !supported {
		t.Error("probe failure disabled direct tool namespaces")
	}
	if detected != "" {
		t.Errorf("detected version = %q, want empty on probe failure", detected)
	}
}
