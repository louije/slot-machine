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

	// Build orchestrator if ORCHESTRATOR_BIN is not already set.
	if os.Getenv("ORCHESTRATOR_BIN") == "" {
		bin := filepath.Join(root, "slot-machine")
		if err := goBuild(root, bin, "./cmd/slot-machine/"); err != nil {
			fmt.Fprintf(os.Stderr, "building slot-machine: %v\n", err)
			os.Exit(1)
		}
		os.Setenv("ORCHESTRATOR_BIN", bin)
	}

	// Build testapp if not already present.
	testappPath := filepath.Join(root, "spec", "testapp", "testapp")
	if _, err := os.Stat(testappPath); err != nil {
		if err := goBuild(root, testappPath, "./spec/testapp/"); err != nil {
			fmt.Fprintf(os.Stderr, "building testapp: %v\n", err)
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
