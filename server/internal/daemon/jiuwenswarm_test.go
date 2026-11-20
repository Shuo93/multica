package daemon

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestJiuwenSwarmDetectedWithExecutableOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("requires POSIX executable")
	}
	bin := filepath.Join(t.TempDir(), "swarm")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '0.2.9b5\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	redirectTestHome(t, t.TempDir())
	t.Setenv("MULTICA_DAEMON_ID", "11111111-1111-1111-1111-111111111111")
	t.Setenv("MULTICA_JIUWENSWARM_PATH", bin)
	cfg, err := LoadConfig(Overrides{ServerURL: "http://localhost:0", WorkspacesRoot: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Agents["jiuwenswarm"].Path; canonicalExecutablePath(got) != canonicalExecutablePath(bin) {
		t.Fatalf("detected executable = %q, want %q", got, bin)
	}
}
