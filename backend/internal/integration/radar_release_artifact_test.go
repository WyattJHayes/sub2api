package integration

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRadarStagingDockerfileBuildsReleaseArtifactFromSource(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, "deploy", "Dockerfile.radar-control-staging"))
	if err != nil {
		t.Fatalf("read Radar staging Dockerfile: %v", err)
	}
	dockerfile := string(contents)

	required := []string{
		"# syntax=docker/dockerfile:1.7",
		"ARG ALPINE_IMAGE=alpine:3.20",
		"AS frontend-builder",
		"corepack prepare pnpm@11.5.2 --activate",
		"COPY frontend/package.json frontend/pnpm-lock.yaml frontend/pnpm-workspace.yaml ./",
		"--mount=type=cache,id=sub2api-radar-pnpm-v11,target=/root/.local/share/pnpm/store",
		"pnpm install --frozen-lockfile --prefer-offline",
		"--fetch-retries=4",
		"--fetch-timeout=120000",
		"AS backend-builder",
		"COPY --from=frontend-builder /app/backend/internal/web/dist ./internal/web/dist",
		"go build",
		"-tags embed",
		"FROM ${ALPINE_IMAGE}",
		"COPY --from=backend-builder /app/sub2api /app/sub2api",
		"COPY deploy/docker-entrypoint.sh /app/docker-entrypoint.sh",
		"ENTRYPOINT [\"/app/docker-entrypoint.sh\"]",
		"CMD [\"/app/sub2api\"]",
	}
	for _, fragment := range required {
		if !strings.Contains(dockerfile, fragment) {
			t.Errorf("Radar staging Dockerfile missing %q", fragment)
		}
	}
	if strings.Contains(dockerfile, "COPY radar-control-plane /app/sub2api") {
		t.Error("Radar staging Dockerfile must not copy a manually built control-plane binary")
	}
	if strings.Contains(dockerfile, "sub2api-custom") {
		t.Error("Radar staging Dockerfile must not depend on an unpublished local runtime image")
	}
}

func TestRadarStagingComposeRequiresImmutableBuildIdentity(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, "deploy", "docker-compose.radar-staging.yml"))
	if err != nil {
		t.Fatalf("read Radar staging Compose file: %v", err)
	}
	compose := string(contents)

	for _, fragment := range []string{
		"VERSION: ${RADAR_RELEASE_VERSION:?RADAR_RELEASE_VERSION is required}",
		"COMMIT: ${RADAR_RELEASE_COMMIT:?RADAR_RELEASE_COMMIT is required}",
		"DATE: ${RADAR_RELEASE_DATE:?RADAR_RELEASE_DATE is required}",
	} {
		if !strings.Contains(compose, fragment) {
			t.Errorf("Radar staging Compose file missing %q", fragment)
		}
	}
}
