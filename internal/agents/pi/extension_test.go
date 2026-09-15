package pi

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The extension is TypeScript, so its behaviour is covered by a node:test
// suite in extension/. This drives that suite, so a failure there fails
// `go test ./...` too.
//
// Node 24 is the floor: the suite imports index.ts directly under type
// stripping, with no build step and no package.json in the tree.
const nodeMajorFloor = 24

// requireNodeEnv makes a missing or too-old toolchain a hard failure.
// CI sets it: a runner that quietly skipped would drop the only coverage
// the extension has, which is the outcome this suite exists to prevent.
const requireNodeEnv = "GORTEX_REQUIRE_NODE"

func TestPiExtensionHarness(t *testing.T) {
	node, reason := usableNode()
	if reason != "" {
		if required, _ := strconv.ParseBool(os.Getenv(requireNodeEnv)); required {
			t.Fatalf("%s is set and the Pi extension suite cannot run: %s", requireNodeEnv, reason)
		}
		t.Skipf("skipping the Pi extension suite: %s (set %s=1 to make this a failure)", reason, requireNodeEnv)
	}

	// The barrier parks a turn on a promise, so a regression can hang. Bound
	// it well above the suite's ~3s.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	// node --test resolves a positional argument as a file, so the directory
	// it discovers *_test.mjs in has to be the cwd.
	cmd := exec.CommandContext(ctx, node, "--test")
	cmd.Dir = "extension"
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("the Pi extension suite timed out after 2m:\n%s", out)
	}
	if err != nil {
		t.Fatalf("the Pi extension suite failed (%v):\n%s", err, out)
	}
}

// usableNode returns the node binary to run the suite with, or a reason it
// cannot be run.
func usableNode() (path, reason string) {
	node, err := exec.LookPath("node")
	if err != nil {
		return "", "no `node` on PATH"
	}

	out, err := exec.Command(node, "--version").Output()
	if err != nil {
		return "", "`node --version` failed: " + err.Error()
	}

	major, ok := nodeMajor(string(out))
	if !ok {
		return "", "could not parse a version out of `node --version` (" + strings.TrimSpace(string(out)) + ")"
	}
	if major < nodeMajorFloor {
		return "", "node " + strconv.Itoa(major) + " is older than the v" + strconv.Itoa(nodeMajorFloor) + " needed to run index.ts under type stripping"
	}
	return node, ""
}

// nodeMajor parses the major version out of `node --version` ("v24.16.0").
func nodeMajor(version string) (int, bool) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	major, _, _ := strings.Cut(v, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0, false
	}
	return n, true
}
