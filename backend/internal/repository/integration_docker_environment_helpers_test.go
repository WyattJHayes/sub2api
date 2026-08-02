package repository

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type dockerContextHostDetector func(context.Context) (string, error)

type testcontainersPortDialer func(context.Context, int, time.Duration) error

type colimaSSHController func(context.Context, string, int) error

func configureTestcontainersDockerEnvironment(ctx context.Context, detect dockerContextHostDetector) error {
	host := strings.TrimSpace(os.Getenv("DOCKER_HOST"))
	if host == "" {
		if detect == nil {
			return fmt.Errorf("Docker context host detector is required")
		}
		var err error
		host, err = detect(ctx)
		if err != nil {
			return fmt.Errorf("detect Docker context host: %w", err)
		}
		host = strings.TrimSpace(host)
		if host == "" {
			return fmt.Errorf("Docker context host is empty")
		}
		if err := os.Setenv("DOCKER_HOST", host); err != nil {
			return fmt.Errorf("set DOCKER_HOST: %w", err)
		}
	}

	if strings.Contains(host, "/.colima/") {
		if strings.TrimSpace(os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE")) == "" {
			if err := os.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/var/run/docker.sock"); err != nil {
				return fmt.Errorf("set Testcontainers Docker socket override: %w", err)
			}
		}
		if strings.TrimSpace(os.Getenv("TESTCONTAINERS_RYUK_DISABLED")) == "" {
			if err := os.Setenv("TESTCONTAINERS_RYUK_DISABLED", "true"); err != nil {
				return fmt.Errorf("disable Testcontainers Ryuk for Colima: %w", err)
			}
		}
	}
	return nil
}

func detectCurrentDockerContextHost(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	cmd.Env = os.Environ()
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("inspect current Docker context: %w", err)
	}
	host := strings.TrimSpace(string(output))
	if host == "" {
		return "", fmt.Errorf("current Docker context has no host")
	}
	return host, nil
}

func ensureTestcontainersPortReachable(ctx context.Context, port int, dial testcontainersPortDialer, control colimaSSHController) (func(), error) {
	noop := func() {}
	if port < 1 || port > 65535 {
		return noop, fmt.Errorf("invalid Testcontainers mapped port %d", port)
	}
	if !strings.Contains(strings.TrimSpace(os.Getenv("DOCKER_HOST")), "/.colima/") {
		return noop, nil
	}
	if dial == nil {
		dial = dialLocalTestcontainersPort
	}
	if control == nil {
		control = runColimaSSHControl
	}
	if err := dial(ctx, port, 250*time.Millisecond); err == nil {
		return noop, nil
	}
	if err := control(ctx, "forward", port); err != nil {
		return noop, fmt.Errorf("forward Colima mapped port %d: %w", port, err)
	}

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = control(cleanupCtx, "cancel", port)
		})
	}
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		if err := dial(ctx, port, 250*time.Millisecond); err == nil {
			return cleanup, nil
		}
		select {
		case <-ctx.Done():
			cleanup()
			return noop, ctx.Err()
		case <-deadline.C:
			cleanup()
			return noop, fmt.Errorf("Colima mapped port %d did not become reachable", port)
		case <-ticker.C:
		}
	}
}

func dialLocalTestcontainersPort(ctx context.Context, port int, timeout time.Duration) error {
	dialer := net.Dialer{Timeout: timeout}
	connection, err := dialer.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	return connection.Close()
}

func runColimaSSHControl(ctx context.Context, operation string, port int) error {
	configPath, instance, err := colimaSSHConfig(strings.TrimSpace(os.Getenv("DOCKER_HOST")))
	if err != nil {
		return err
	}
	forward := fmt.Sprintf("127.0.0.1:%d:127.0.0.1:%d", port, port)
	cmd := exec.CommandContext(ctx, "ssh", "-F", configPath, "-O", operation, "-L", forward, instance)
	cmd.Env = os.Environ()
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("ssh control %s: %w: %s", operation, err, strings.TrimSpace(string(output)))
	}
	return nil
}

func colimaSSHConfig(dockerHost string) (string, string, error) {
	hostPath := strings.TrimPrefix(strings.TrimSpace(dockerHost), "unix://")
	marker := string(filepath.Separator) + ".colima" + string(filepath.Separator)
	markerIndex := strings.Index(hostPath, marker)
	if markerIndex <= 0 {
		return "", "", fmt.Errorf("Docker host is not a Colima socket: %s", dockerHost)
	}
	remainder := hostPath[markerIndex+len(marker):]
	profile := strings.SplitN(remainder, string(filepath.Separator), 2)[0]
	if profile == "" {
		return "", "", fmt.Errorf("Colima profile is missing from Docker host: %s", dockerHost)
	}
	instance := "colima"
	if profile != "default" {
		instance += "-" + profile
	}
	configPath := filepath.Join(hostPath[:markerIndex], ".colima", "_lima", instance, "ssh.config")
	if override := strings.TrimSpace(os.Getenv("COLIMA_SSH_CONFIG")); override != "" {
		configPath = override
	}
	return configPath, "lima-" + instance, nil
}
