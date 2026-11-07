package sandbox

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

// udocker (https://github.com/indigo-dc/udocker) runs containers in user space
// without a daemon or root privileges. It has no detached mode: a container is
// a directory tree created once with "udocker create", and every "udocker run"
// starts a fresh process inside it. Volumes, env and user are not persisted, so
// they are re-applied on every exec.
//
// Limitations compared to Docker (udocker is not a security boundary):
// no network isolation, no memory/CPU/pids limits, no read-only root, no
// capability dropping, no tmpfs and no read-only bind mounts.

const (
	envUdockerBin      = "GOCLAW_UDOCKER_BIN"      // udocker executable (default "udocker")
	envUdockerExecMode = "GOCLAW_UDOCKER_EXECMODE" // optional execution mode (P1, P2, F1..F4, R1..R3, S1)
)

var udockerLimitationsOnce sync.Once

func udockerBinary() string {
	if v := strings.TrimSpace(os.Getenv(envUdockerBin)); v != "" {
		return v
	}
	return "udocker"
}

// udockerGlobalArgs returns options placed before the udocker subcommand.
// udocker refuses to run as root unless --allow-root is given.
func udockerGlobalArgs() []string {
	if os.Geteuid() == 0 {
		return []string{"--allow-root"}
	}
	return nil
}

// udockerCommand builds an exec.Cmd for a udocker subcommand. The whole process
// group is killed on context cancellation because udocker spawns proot/runc
// children that would otherwise outlive the timeout.
func udockerCommand(ctx context.Context, args ...string) *exec.Cmd {
	full := append(udockerGlobalArgs(), args...)
	cmd := exec.CommandContext(ctx, udockerBinary(), full...)
	configureProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// CheckUdockerAvailable verifies that the udocker CLI is installed and working.
func CheckUdockerAvailable(ctx context.Context) error {
	// First invocation may download udocker tools, allow extra time.
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	out, err := udockerCommand(ctx, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("udocker not available: %w (output: %s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// CheckRuntimeAvailable verifies that the given container runtime is usable.
func CheckRuntimeAvailable(ctx context.Context, rt Runtime) error {
	if rt == RuntimeDocker {
		return CheckDockerAvailable(ctx)
	}
	return CheckUdockerAvailable(ctx)
}

func warnUdockerLimitations(cfg Config) {
	udockerLimitationsOnce.Do(func() {
		var ignored []string
		if !cfg.NetworkEnabled {
			ignored = append(ignored, "network isolation")
		}
		if cfg.MemoryMB > 0 {
			ignored = append(ignored, "memory_mb")
		}
		if cfg.CPUs > 0 {
			ignored = append(ignored, "cpus")
		}
		if cfg.PidsLimit > 0 {
			ignored = append(ignored, "pids_limit")
		}
		if cfg.ReadOnlyRoot {
			ignored = append(ignored, "read_only_root")
		}
		if len(cfg.CapDrop) > 0 {
			ignored = append(ignored, "cap_drop")
		}
		if len(cfg.Tmpfs) > 0 {
			ignored = append(ignored, "tmpfs")
		}
		if cfg.WorkspaceAccess == AccessRO {
			ignored = append(ignored, "workspace_access=ro")
		}
		if len(ignored) > 0 {
			slog.Warn("sandbox: udocker runtime does not enforce these settings", "ignored", strings.Join(ignored, ", "))
		}
	})
}

// udockerImageAvailable reports whether the image exists in the local udocker repository.
func udockerImageAvailable(ctx context.Context, image string) bool {
	return udockerCommand(ctx, "inspect", image).Run() == nil
}

func ensureUdockerImage(ctx context.Context, image string) error {
	if udockerImageAvailable(ctx, image) {
		return nil
	}
	slog.Info("sandbox: pulling image with udocker", "image", image)
	out, err := udockerCommand(ctx, "pull", image).CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"udocker pull %s failed: %w\noutput: %s\nhint: run scripts/udocker-sandbox-image.sh to build the default sandbox image, or set GOCLAW_SANDBOX_IMAGE to a registry image (e.g. debian:bookworm-slim)",
			image, err, strings.TrimSpace(string(out)),
		)
	}
	return nil
}

// udockerRunArgs builds the per-run options shared by every exec in the container.
func udockerRunArgs(ctx context.Context, cfg Config, workspace string, mounts []ReadOnlyMount) ([]string, []string, error) {
	dockerArgs, sensitiveRoots, err := appendDockerMountArgs(ctx, nil, cfg, workspace, mounts, nil)
	if err != nil {
		return nil, nil, err
	}

	args := []string{"--nobanner"}
	if cfg.User != "" {
		args = append(args, "--user="+cfg.User)
	}
	for i := 0; i+1 < len(dockerArgs); i += 2 {
		if dockerArgs[i] != "-v" {
			continue
		}
		// udocker does not support mount options; strip ":ro"/":rw".
		spec := strings.TrimSuffix(strings.TrimSuffix(dockerArgs[i+1], ":rw"), ":ro")
		args = append(args, "--volume="+spec)
	}

	keys := make([]string, 0, len(cfg.Env))
	for k := range cfg.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env="+k+"="+cfg.Env[k])
	}
	return args, sensitiveRoots, nil
}

// udockerExecArgs builds "udocker run" arguments for a command.
func (s *DockerSandbox) udockerExecArgs(command []string, workDir string, env map[string]string) []string {
	args := append([]string{"run"}, s.runArgs...)
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "--env="+k+"="+env[k])
	}
	if workDir == "" {
		workDir = s.config.ContainerWorkdir()
	}
	args = append(args, "--workdir="+workDir, s.containerID)
	return append(args, command...)
}

// newUdockerSandbox creates a udocker container for sandboxed execution.
func newUdockerSandbox(
	ctx context.Context,
	name string,
	cfg Config,
	workspace string,
	readOnlyMounts ...ReadOnlyMount,
) (*DockerSandbox, error) {
	warnUdockerLimitations(cfg)
	if len(readOnlyMounts) > 0 {
		slog.Warn("sandbox: udocker cannot enforce read-only mounts; they are mounted read-write", "count", len(readOnlyMounts))
	}

	runArgs, sensitiveMountRoots, err := udockerRunArgs(ctx, cfg, workspace, readOnlyMounts)
	if err != nil {
		return nil, err
	}

	if err := ensureUdockerImage(ctx, cfg.Image); err != nil {
		return nil, err
	}

	// A container with the same name may be left over from a previous run.
	_ = udockerCommand(ctx, "rm", "-f", name).Run()

	var stdout, stderr bytes.Buffer
	createCmd := udockerCommand(ctx, "create", "--name="+name, cfg.Image)
	createCmd.Stdout = &stdout
	createCmd.Stderr = &stderr
	if err := createCmd.Run(); err != nil {
		return nil, fmt.Errorf("udocker create failed: %w\nstderr: %s", err, redactMountRoots(stderr.String(), sensitiveMountRoots))
	}
	containerID := lastNonEmptyLine(stdout.String())
	if containerID == "" {
		return nil, fmt.Errorf("udocker create returned no container id")
	}

	sb := &DockerSandbox{
		containerID: containerID,
		runtime:     RuntimeUdocker,
		runArgs:     runArgs,
		config:      cfg,
		workspace:   workspace,
	}

	if mode := strings.TrimSpace(os.Getenv(envUdockerExecMode)); mode != "" {
		if out, err := udockerCommand(ctx, "setup", "--execmode="+mode, containerID).CombinedOutput(); err != nil {
			_ = sb.Destroy(ctx)
			return nil, fmt.Errorf("udocker setup --execmode=%s failed: %w\noutput: %s", mode, err, strings.TrimSpace(string(out)))
		}
	}

	// udocker requires the working directory to exist; Docker creates it implicitly.
	if workspace == "" || cfg.WorkspaceAccess == AccessNone {
		mkdirArgs := append([]string{"run"}, runArgs...)
		mkdirArgs = append(mkdirArgs, "--workdir=/", containerID, "mkdir", "-p", cfg.ContainerWorkdir())
		if out, err := udockerCommand(ctx, mkdirArgs...).CombinedOutput(); err != nil {
			_ = sb.Destroy(ctx)
			return nil, fmt.Errorf("udocker: create workdir failed: %w\noutput: %s", err, strings.TrimSpace(string(out)))
		}
	}

	slog.Info("sandbox container created", "runtime", "udocker", "id", containerID, "name", name, "image", cfg.Image)

	if cfg.SetupCommand != "" {
		setupCmd := udockerCommand(ctx, sb.udockerExecArgs([]string{"sh", "-lc", cfg.SetupCommand}, "", nil)...)
		if out, err := setupCmd.CombinedOutput(); err != nil {
			slog.Warn("sandbox setup command failed", "id", containerID, "error", err, "output", string(out))
		} else {
			slog.Info("sandbox setup command completed", "id", containerID)
		}
	}

	now := time.Now()
	sb.createdAt = now
	sb.lastUsed = now
	return sb, nil
}

// fsExec runs a command for FsBridge using the sandbox's runtime.
func (s *DockerSandbox) fsExec(ctx context.Context, stdin []byte, args ...string) (string, string, int, error) {
	if s.runtime == RuntimeUdocker {
		return runCollect(udockerCommand(ctx, s.udockerExecArgs(args, "", nil)...), stdin)
	}
	return dockerFsExec(ctx, s.containerID, stdin, args...)
}

func lastNonEmptyLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" {
			return l
		}
	}
	return ""
}
