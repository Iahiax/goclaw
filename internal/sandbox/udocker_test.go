package sandbox

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseRuntime(t *testing.T) {
	tests := map[string]Runtime{
		"":         RuntimeUdocker,
		"udocker":  RuntimeUdocker,
		"docker":   RuntimeDocker,
		" Docker ": RuntimeDocker,
		"unknown":  RuntimeUdocker,
	}
	for in, want := range tests {
		if got := ParseRuntime(in); got != want {
			t.Errorf("ParseRuntime(%q) = %q, want %q", in, got, want)
		}
	}
	if DefaultConfig().Runtime != RuntimeUdocker {
		t.Errorf("default runtime = %q, want udocker", DefaultConfig().Runtime)
	}
}

func TestUdockerRunArgs(t *testing.T) {
	ws := canonicalTempDir(t)
	cfg := DefaultConfig()
	cfg.User = "1000:1000"
	cfg.WorkspaceAccess = AccessRO
	cfg.Env = map[string]string{"B": "2", "A": "1"}

	args, roots, err := udockerRunArgs(context.Background(), cfg, ws, nil)
	if err != nil {
		t.Fatalf("udockerRunArgs: %v", err)
	}
	want := []string{
		"--nobanner",
		"--user=1000:1000",
		"--volume=" + ws + ":/workspace",
		"--env=A=1",
		"--env=B=2",
	}
	if !slices.Equal(args, want) {
		t.Fatalf("args = %q, want %q", args, want)
	}
	if !slices.Contains(roots, ws) {
		t.Fatalf("sensitive roots %q missing workspace", roots)
	}
}

func TestUdockerRunArgsNoWorkspaceAccess(t *testing.T) {
	cfg := DefaultConfig()
	cfg.WorkspaceAccess = AccessNone
	args, _, err := udockerRunArgs(context.Background(), cfg, canonicalTempDir(t), nil)
	if err != nil {
		t.Fatalf("udockerRunArgs: %v", err)
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--volume=") {
			t.Fatalf("unexpected volume with access=none: %q", args)
		}
	}
}

func TestUdockerExecArgs(t *testing.T) {
	sb := &DockerSandbox{
		containerID: "cid",
		runtime:     RuntimeUdocker,
		runArgs:     []string{"--nobanner", "--volume=/h:/workspace"},
		config:      DefaultConfig(),
	}
	got := sb.udockerExecArgs([]string{"sh", "-c", "echo hi"}, "", map[string]string{"TOKEN": "x"})
	want := []string{"run", "--nobanner", "--volume=/h:/workspace", "--env=TOKEN=x", "--workdir=/workspace", "cid", "sh", "-c", "echo hi"}
	if !slices.Equal(got, want) {
		t.Fatalf("args = %q, want %q", got, want)
	}

	got = sb.udockerExecArgs([]string{"ls"}, "/workspace/sub", nil)
	if !slices.Contains(got, "--workdir=/workspace/sub") {
		t.Fatalf("workdir not applied: %q", got)
	}
	// Persistent run args must not be mutated by per-call args.
	if len(sb.runArgs) != 2 {
		t.Fatalf("runArgs mutated: %q", sb.runArgs)
	}
}

func TestLastNonEmptyLine(t *testing.T) {
	if got := lastNonEmptyLine("Info: x\n8d99-uuid\n\n"); got != "8d99-uuid" {
		t.Fatalf("got %q", got)
	}
	if got := lastNonEmptyLine("  \n"); got != "" {
		t.Fatalf("got %q", got)
	}
}

// TestUdockerSandboxIntegration runs a real udocker container.
// Enable with GOCLAW_TEST_UDOCKER=1 (optionally GOCLAW_TEST_UDOCKER_IMAGE).
func TestUdockerSandboxIntegration(t *testing.T) {
	if os.Getenv("GOCLAW_TEST_UDOCKER") == "" {
		t.Skip("set GOCLAW_TEST_UDOCKER=1 to run udocker integration test")
	}
	if _, err := exec.LookPath(udockerBinary()); err != nil {
		t.Skipf("udocker not found: %v", err)
	}
	ctx := context.Background()
	if err := CheckRuntimeAvailable(ctx, RuntimeUdocker); err != nil {
		t.Fatalf("CheckRuntimeAvailable: %v", err)
	}

	image := os.Getenv("GOCLAW_TEST_UDOCKER_IMAGE")
	if image == "" {
		image = "debian:bookworm-slim"
	}
	cfg := DefaultConfig()
	cfg.Mode = ModeAll
	cfg.Image = image
	cfg.TimeoutSec = 5
	cfg.ContainerPrefix = "goclaw-sbx-test-"
	cfg.Env = map[string]string{"SANDBOX_VAR": "from-config"}

	mgr := NewDockerManager(cfg)
	defer mgr.Stop()
	defer mgr.ReleaseAll(ctx)

	ws := canonicalTempDir(t)
	if err := os.WriteFile(filepath.Join(ws, "host.txt"), []byte("from-host"), 0o644); err != nil {
		t.Fatal(err)
	}

	sb, err := mgr.Get(ctx, "session-1", ws, nil)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	res, err := sb.Exec(ctx, []string{"sh", "-c", "cat host.txt; echo; pwd; echo $SANDBOX_VAR $CALL_VAR; exit 3"}, "", WithEnv(map[string]string{"CALL_VAR": "per-call"}))
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3 (stderr: %s)", res.ExitCode, res.Stderr)
	}
	for _, want := range []string{"from-host", "/workspace", "from-config per-call"} {
		if !strings.Contains(res.Stdout, want) {
			t.Errorf("stdout %q missing %q", res.Stdout, want)
		}
	}

	bridge := NewFsBridgeForSandbox(sb, "/workspace")
	if err := bridge.WriteFile(ctx, "sub/out.txt", "written-in-sandbox", false); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(ws, "sub", "out.txt"))
	if err != nil || string(data) != "written-in-sandbox" {
		t.Fatalf("host file = %q, %v", data, err)
	}
	got, err := bridge.ReadFile(ctx, "sub/out.txt")
	if err != nil || got != "written-in-sandbox" {
		t.Fatalf("ReadFile = %q, %v", got, err)
	}
	if _, err := bridge.ReadFile(ctx, "/etc/passwd"); err == nil {
		t.Fatal("ReadFile outside workdir should fail")
	}

	// Container filesystem persists between execs.
	if _, err := sb.Exec(ctx, []string{"sh", "-c", "echo persisted > /tmp/state"}, ""); err != nil {
		t.Fatal(err)
	}
	res, err = sb.Exec(ctx, []string{"cat", "/tmp/state"}, "")
	if err != nil || strings.TrimSpace(res.Stdout) != "persisted" {
		t.Fatalf("persisted state = %+v, %v", res, err)
	}

	start := time.Now()
	res, err = sb.Exec(ctx, []string{"sleep", "60"}, "")
	if time.Since(start) > 20*time.Second {
		t.Fatalf("timeout not enforced, took %s", time.Since(start))
	}
	if err == nil && res.ExitCode == 0 {
		t.Fatalf("sleep beyond timeout should fail, got %+v", res)
	}

	id := sb.ID()
	if err := mgr.Release(ctx, "session-1"); err != nil {
		t.Fatalf("Release: %v", err)
	}
	if udockerCommand(ctx, "inspect", id).Run() == nil {
		t.Fatalf("container %s still exists after Release", id)
	}
}
