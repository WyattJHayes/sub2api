package repository

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestConfigureTestcontainersDockerEnvironmentInfersColimaContext(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	t.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "")
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "")
	host := "unix:///Users/test/.colima/default/docker.sock"

	err := configureTestcontainersDockerEnvironment(context.Background(), func(context.Context) (string, error) {
		return host, nil
	})

	require.NoError(t, err)
	require.Equal(t, host, os.Getenv("DOCKER_HOST"))
	require.Equal(t, "/var/run/docker.sock", os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"))
	require.Equal(t, "true", os.Getenv("TESTCONTAINERS_RYUK_DISABLED"))
}

func TestConfigureTestcontainersDockerEnvironmentPreservesExplicitSettings(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///custom/docker.sock")
	t.Setenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE", "/custom/container.sock")
	t.Setenv("TESTCONTAINERS_RYUK_DISABLED", "false")
	called := false

	err := configureTestcontainersDockerEnvironment(context.Background(), func(context.Context) (string, error) {
		called = true
		return "", errors.New("must not be called")
	})

	require.NoError(t, err)
	require.False(t, called)
	require.Equal(t, "unix:///custom/docker.sock", os.Getenv("DOCKER_HOST"))
	require.Equal(t, "/custom/container.sock", os.Getenv("TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE"))
	require.Equal(t, "false", os.Getenv("TESTCONTAINERS_RYUK_DISABLED"))
}

func TestConfigureTestcontainersDockerEnvironmentReturnsContextDetectionError(t *testing.T) {
	t.Setenv("DOCKER_HOST", "")
	wantErr := errors.New("context unavailable")

	err := configureTestcontainersDockerEnvironment(context.Background(), func(context.Context) (string, error) {
		return "", wantErr
	})

	require.ErrorIs(t, err, wantErr)
}

func TestEnsureTestcontainersPortReachableCreatesAndCancelsColimaTunnel(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///Users/test/.colima/default/docker.sock")
	dialCalls := 0
	operations := make([]string, 0, 2)

	cleanup, err := ensureTestcontainersPortReachable(
		context.Background(),
		5432,
		func(context.Context, int, time.Duration) error {
			dialCalls++
			if dialCalls == 1 {
				return errors.New("connection refused")
			}
			return nil
		},
		func(_ context.Context, operation string, port int) error {
			operations = append(operations, operation+":"+strconv.Itoa(port))
			return nil
		},
	)

	require.NoError(t, err)
	require.Equal(t, []string{"forward:5432"}, operations)
	cleanup()
	require.Equal(t, []string{"forward:5432", "cancel:5432"}, operations)
}

func TestEnsureTestcontainersPortReachableLeavesNonColimaHostUntouched(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///var/run/docker.sock")
	controlCalled := false

	cleanup, err := ensureTestcontainersPortReachable(
		context.Background(),
		5432,
		func(context.Context, int, time.Duration) error { return errors.New("connection refused") },
		func(context.Context, string, int) error {
			controlCalled = true
			return nil
		},
	)

	require.NoError(t, err)
	require.False(t, controlCalled)
	cleanup()
}
