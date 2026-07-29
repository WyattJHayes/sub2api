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

	for _, validationError := range validateRadarStagingDockerfile(dockerfile) {
		t.Error(validationError)
	}
}

func TestRadarStagingDockerfileRejectsIdentityArgumentsBeforeModuleDownload(t *testing.T) {
	for _, argument := range []string{"ARG VERSION", "ARG COMMIT", "ARG DATE"} {
		t.Run(argument, func(t *testing.T) {
			dockerfile := readRadarStagingDockerfile(t)
			mutated := strings.Replace(dockerfile, argument, "", 1)
			mutated = strings.Replace(mutated, "ARG GOPROXY", argument+"\nARG GOPROXY", 1)

			if len(validateRadarStagingDockerfile(mutated)) == 0 {
				t.Fatalf("moving %s before the module download layer must be rejected", argument)
			}
		})
	}
}

func TestRadarStagingDockerfileRejectsDetachedModuleDownloadCacheAndRetry(t *testing.T) {
	dockerfile := readRadarStagingDockerfile(t)
	moduleDownloadRun := `RUN --mount=type=cache,id=sub2api-radar-gomod,target=/go/pkg/mod \
    set -eu; \
    attempt=1; \
    until go mod download; do \
      if [ "$attempt" -ge 4 ]; then \
        exit 1; \
      fi; \
      sleep $((attempt * 2)); \
      attempt=$((attempt + 1)); \
    done`
	mutated := strings.Replace(dockerfile, moduleDownloadRun, `RUN --mount=type=cache,id=sub2api-radar-gomod,target=/go/pkg/mod true
RUN printf '%s\n' 'until go mod download; do' 'if [ "$attempt" -ge 4 ]'
RUN go mod download`, 1)
	if mutated == dockerfile {
		t.Fatal("test mutation must replace the module download instruction")
	}

	if len(validateRadarStagingDockerfile(mutated)) == 0 {
		t.Fatal("detaching the module cache and retry loop from go mod download must be rejected")
	}
}

func readRadarStagingDockerfile(t *testing.T) string {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, "deploy", "Dockerfile.radar-control-staging"))
	if err != nil {
		t.Fatalf("read Radar staging Dockerfile: %v", err)
	}
	return string(contents)
}

func validateRadarStagingDockerfile(dockerfile string) []string {
	var validationErrors []string
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
		"--mount=type=cache,id=sub2api-radar-gomod,target=/go/pkg/mod",
		"until go mod download; do",
		"if [ \"$attempt\" -ge 4 ]",
		"--mount=type=cache,id=sub2api-radar-gobuild,target=/root/.cache/go-build",
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
			validationErrors = append(validationErrors, "Radar staging Dockerfile missing "+fragment)
		}
	}
	if strings.Contains(dockerfile, "COPY radar-control-plane /app/sub2api") {
		validationErrors = append(validationErrors, "Radar staging Dockerfile must not copy a manually built control-plane binary")
	}
	if strings.Contains(dockerfile, "sub2api-custom") {
		validationErrors = append(validationErrors, "Radar staging Dockerfile must not depend on an unpublished local runtime image")
	}
	moduleDownloadInstruction := -1
	for index, instruction := range dockerInstructions(dockerfile) {
		if strings.HasPrefix(instruction, "RUN ") && strings.Contains(instruction, "go mod download") {
			moduleDownloadInstruction = index
			if !strings.Contains(instruction, "--mount=type=cache,id=sub2api-radar-gomod,target=/go/pkg/mod") ||
				!strings.Contains(instruction, "until go mod download; do") ||
				!strings.Contains(instruction, `if [ "$attempt" -ge 4 ]`) {
				validationErrors = append(validationErrors, "Radar staging Dockerfile must keep the Go module cache mount and retry loop in the go mod download RUN instruction")
			}
			break
		}
	}
	if moduleDownloadInstruction < 0 {
		validationErrors = append(validationErrors, "Radar staging Dockerfile must download Go modules")
	} else {
		for index, instruction := range dockerInstructions(dockerfile) {
			if index < moduleDownloadInstruction && (instruction == "ARG VERSION" || instruction == "ARG COMMIT" || instruction == "ARG DATE") {
				validationErrors = append(validationErrors, "Radar staging Dockerfile must declare revision build arguments after the cached module download layer")
				break
			}
		}
	}
	return validationErrors
}

func dockerInstructions(dockerfile string) []string {
	var instructions []string
	var instruction strings.Builder

	for _, line := range strings.Split(dockerfile, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		if instruction.Len() > 0 {
			instruction.WriteByte(' ')
		}
		instruction.WriteString(strings.TrimSuffix(trimmed, "\\"))
		if strings.HasSuffix(trimmed, "\\") {
			continue
		}
		instructions = append(instructions, instruction.String())
		instruction.Reset()
	}
	if instruction.Len() > 0 {
		instructions = append(instructions, instruction.String())
	}
	return instructions
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
