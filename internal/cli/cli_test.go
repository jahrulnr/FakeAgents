package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/jahrulnr/fakeagents/internal/cli"
)

func TestVersion(t *testing.T) {
	if code := cli.Run([]string{"version"}); code != 0 {
		t.Fatalf("version exit = %d", code)
	}
}

func TestHelp(t *testing.T) {
	if code := cli.Run([]string{"help"}); code != 0 {
		t.Fatalf("help exit = %d", code)
	}
}

func TestUnknownCommand(t *testing.T) {
	if code := cli.Run([]string{"nope"}); code != 2 {
		t.Fatalf("unknown command exit = %d", code)
	}
}

func TestInitValidateAndListScenarios(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeagents.json")
	if code := cli.Run([]string{"init", "-o", path, "--force"}); code != 0 {
		t.Fatalf("init exit = %d", code)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("generated config missing: %v", err)
	}
	if code := cli.Run([]string{"validate", "-c", path}); code != 0 {
		t.Fatalf("validate exit = %d", code)
	}
	if code := cli.Run([]string{"scenarios", "-c", path}); code != 0 {
		t.Fatalf("scenarios exit = %d", code)
	}
}

func TestInitRefusesOverwrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fakeagents.json")
	if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if code := cli.Run([]string{"init", "-o", path}); code != 2 {
		t.Fatalf("overwrite exit = %d", code)
	}
}

func TestValidateMissingFile(t *testing.T) {
	if code := cli.Run([]string{"validate", "-c", filepath.Join(t.TempDir(), "missing.json")}); code != 2 {
		t.Fatalf("missing file exit = %d", code)
	}
}
