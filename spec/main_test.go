package spec

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
)

// TestMain builds the orchestrator and testapp binaries before running tests.
// This lets `go test ./spec/` (or `go test ./...`) work without manual build steps.
func TestMain(m *testing.M) {
	// Find module root (directory containing go.mod).
	root, err := findModuleRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find module root: %v\n", err)
		os.Exit(1)
	}

	// Rebuild every binary the suite drives, unconditionally.
	//
	// These used to be built only when the file was absent, which meant the
	// suite silently tested whatever binary happened to be on disk. That is not
	// a theoretical hazard: when --verbose was added to the agent invocation and
	// mirrored into spec/testagent, a developer with a pre-existing
	// spec/testagent/testagent kept running the old one, so three agent tests
	// failed with no visible cause — and passed again after any unrelated manual
	// rebuild. It reads exactly like flakiness and is not.
	//
	// go build is incremental, so an up-to-date binary costs a few milliseconds.
	// That is a very cheap price for never debugging a stale artifact again.
	if os.Getenv("ORCHESTRATOR_BIN") == "" {
		bin := filepath.Join(root, "slot-machine")
		if err := goBuild(root, bin, "./cmd/slot-machine/"); err != nil {
			fmt.Fprintf(os.Stderr, "building slot-machine: %v\n", err)
			os.Exit(1)
		}
		os.Setenv("ORCHESTRATOR_BIN", bin)
	}

	for _, b := range []struct{ out, pkg string }{
		{filepath.Join(root, "spec", "testapp", "testapp"), "./spec/testapp/"},
		{filepath.Join(root, "spec", "testagent", "testagent"), "./spec/testagent/"},
	} {
		if err := goBuild(root, b.out, b.pkg); err != nil {
			fmt.Fprintf(os.Stderr, "building %s: %v\n", b.pkg, err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

func goBuild(dir, output, pkg string) error {
	cmd := exec.Command("go", "build", "-o", output, pkg)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func findModuleRoot() (string, error) {
	// Start from the directory containing this test file.
	_, filename, _, _ := runtime.Caller(0)
	dir := filepath.Dir(filename)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found")
		}
		dir = parent
	}
}
