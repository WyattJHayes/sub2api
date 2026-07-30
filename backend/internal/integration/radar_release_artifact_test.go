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

func TestRadarStagingDockerfileRejectsDetachedModuleDownloadProtections(t *testing.T) {
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
	mutations := map[string]string{
		"cache mount": `RUN set -eu; \
    attempt=1; \
    until go mod download; do \
      if [ "$attempt" -ge 4 ]; then \
        exit 1; \
      fi; \
      sleep $((attempt * 2)); \
      attempt=$((attempt + 1)); \
    done
RUN --mount=type=cache,id=sub2api-radar-gomod,target=/go/pkg/mod true`,
		"retry loop": `RUN --mount=type=cache,id=sub2api-radar-gomod,target=/go/pkg/mod go mod download
RUN printf '%s\n' 'until go mod download; do' 'if [ "$attempt" -ge 4 ]'`,
	}

	for protection, replacement := range mutations {
		t.Run(protection, func(t *testing.T) {
			mutated := strings.Replace(dockerfile, moduleDownloadRun, replacement, 1)
			if mutated == dockerfile {
				t.Fatal("test mutation must replace the module download instruction")
			}

			if len(validateRadarStagingDockerfile(mutated)) == 0 {
				t.Fatalf("detaching the module download %s must be rejected", protection)
			}
		})
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

func TestRadarWorkerDockerfileReplacesInheritedSourceTree(t *testing.T) {
	for _, validationError := range validateRadarWorkerDockerfile(readRadarWorkerDockerfile(t)) {
		t.Error(validationError)
	}
}

func TestRadarWorkerDockerfileRejectsDetachedInheritedSourceCleanup(t *testing.T) {
	dockerfile := readRadarWorkerDockerfile(t)
	cleanup := "USER root\nRUN rm -rf /opt/radar-worker/src\n"
	mutated := strings.Replace(dockerfile, cleanup, "", 1)
	mutated = strings.Replace(
		mutated,
		"FROM sub2api/radar-worker:staging AS runtime",
		"FROM alpine:3.20 AS detached-cleanup\n"+cleanup+"\nFROM sub2api/radar-worker:staging AS runtime",
		1,
	)
	if mutated == dockerfile {
		t.Fatal("test mutation must move the inherited source cleanup to another build stage")
	}

	if len(validateRadarWorkerDockerfile(mutated)) == 0 {
		t.Fatal("cleaning an unrelated build stage must not satisfy the Worker runtime source replacement contract")
	}
}

func TestRadarWorkerDockerfileRejectsBypassedSourceReplacementContract(t *testing.T) {
	dockerfile := readRadarWorkerDockerfile(t)
	mutations := map[string]string{
		"final root user": dockerfile + "\nUSER root\n",
		"no-op cleanup": strings.Replace(
			dockerfile,
			"RUN rm -rf /opt/radar-worker/src",
			"RUN printf '%s\\n' 'rm -rf /opt/radar-worker/src'",
			1,
		),
		"source copied from old stage": strings.Replace(
			dockerfile,
			"COPY src ./src",
			"COPY --from=old /opt/radar-worker/src ./src",
			1,
		),
	}

	for name, mutated := range mutations {
		t.Run(name, func(t *testing.T) {
			if mutated == dockerfile {
				t.Fatal("test mutation must change the Worker Dockerfile")
			}
			if len(validateRadarWorkerDockerfile(mutated)) == 0 {
				t.Fatal("bypassing the Worker source replacement contract must be rejected")
			}
		})
	}
}

func TestRadarWorkerDockerignoreExcludesNestedPythonBuildArtifacts(t *testing.T) {
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, "radar-worker", ".dockerignore"))
	if err != nil {
		t.Fatalf("read Radar Worker .dockerignore: %v", err)
	}
	dockerignore := string(contents)

	for _, pattern := range []string{"**/__pycache__/", "**/*.py[cod]"} {
		if !strings.Contains(dockerignore, pattern) {
			t.Errorf("Radar Worker .dockerignore must exclude nested Python build artifacts with %q", pattern)
		}
	}
}

func readRadarWorkerDockerfile(t *testing.T) string {
	t.Helper()
	root := filepath.Clean(filepath.Join("..", "..", ".."))
	contents, err := os.ReadFile(filepath.Join(root, "radar-worker", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Radar Worker Dockerfile: %v", err)
	}
	return string(contents)
}

func validateRadarWorkerDockerfile(dockerfile string) []string {
	var validationErrors []string
	instructions := dockerInstructions(dockerfile)
	runtimeStageStart := -1
	for index, instruction := range instructions {
		if strings.HasPrefix(instruction, "FROM ") {
			runtimeStageStart = index
		}
	}
	if runtimeStageStart < 0 {
		return []string{"Radar Worker Dockerfile must define a runtime build stage"}
	}

	activeUser := ""
	cleanedInheritedSource := false
	copiedCurrentSource := false
	restoredRuntimeUser := false

	for _, instruction := range instructions[runtimeStageStart+1:] {
		switch {
		case strings.HasPrefix(instruction, "USER "):
			activeUser = strings.TrimSpace(strings.TrimPrefix(instruction, "USER "))
			if copiedCurrentSource && activeUser == "radar-worker" {
				restoredRuntimeUser = true
			}
		case instruction == "RUN rm -rf /opt/radar-worker/src":
			if activeUser != "root" {
				validationErrors = append(validationErrors, "Radar Worker Dockerfile must clean the inherited source tree as root")
			}
			cleanedInheritedSource = true
		case copiesCurrentWorkerSource(instruction):
			if !cleanedInheritedSource {
				validationErrors = append(validationErrors, "Radar Worker Dockerfile must clean the inherited source tree before copying current source")
			}
			copiedCurrentSource = true
		}
	}

	if !cleanedInheritedSource {
		validationErrors = append(validationErrors, "Radar Worker Dockerfile must remove the inherited source tree")
	}
	if !copiedCurrentSource {
		validationErrors = append(validationErrors, "Radar Worker Dockerfile must copy the current source tree")
	}
	if !restoredRuntimeUser || activeUser != "radar-worker" {
		validationErrors = append(validationErrors, "Radar Worker Dockerfile must restore the radar-worker runtime user after copying source")
	}

	return validationErrors
}

func copiesCurrentWorkerSource(instruction string) bool {
	fields := strings.Fields(instruction)
	if len(fields) < 3 || fields[0] != "COPY" {
		return false
	}

	var operands []string
	for _, field := range fields[1:] {
		if strings.HasPrefix(field, "--from=") {
			return false
		}
		if strings.HasPrefix(field, "--") {
			continue
		}
		operands = append(operands, field)
	}
	return len(operands) == 2 && operands[0] == "src" && operands[1] == "./src"
}
