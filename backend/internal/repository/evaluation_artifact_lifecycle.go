package repository

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/google/uuid"
)

func verifyArtifactObjectMetadata(expected, actual service.ArtifactObjectMetadata) error {
	if expected.ObjectKey != actual.ObjectKey || expected.Bytes != actual.Bytes ||
		!strings.EqualFold(strings.TrimSpace(expected.MIMEType), strings.TrimSpace(actual.MIMEType)) ||
		strings.TrimSpace(expected.SHA256) != strings.TrimSpace(actual.SHA256) {
		return service.ErrArtifactObjectMismatch
	}
	return nil
}

func artifactScanResultError(result service.ArtifactScanResult) error {
	if result.Status == service.ArtifactScanClean {
		return nil
	}
	reason := strings.TrimSpace(result.Reason)
	suffix := ""
	if reason != "" {
		suffix = ": " + reason
	}
	switch result.Status {
	case service.ArtifactScanRejected:
		return fmt.Errorf("%w%s", service.ErrArtifactScanRejected, suffix)
	case service.ArtifactScanFailed:
		return fmt.Errorf("%w%s", service.ErrArtifactScanFailed, suffix)
	default:
		return fmt.Errorf("%w: unknown scanner status %q%s", service.ErrArtifactScanFailed, result.Status, suffix)
	}
}

func bindEvidenceManifestArtifact(evidence json.RawMessage, artifacts []service.ArtifactReceipt) (string, uuid.UUID, error) {
	digest, err := service.DigestCanonicalJSON(evidence)
	if err != nil {
		return "", uuid.Nil, fmt.Errorf("%w: canonicalize evidence manifest: %v", service.ErrEvidenceMismatch, err)
	}
	boundID := uuid.Nil
	for _, artifact := range artifacts {
		mimeType := strings.ToLower(strings.TrimSpace(strings.SplitN(artifact.MIMEType, ";", 2)[0]))
		trusted := artifact.SHA256 == digest && artifact.Bytes > 0 && mimeType == "application/json" &&
			artifact.ScanStatus == string(service.ArtifactScanClean) && strings.TrimSpace(artifact.Scanner) != "" &&
			!artifact.ConfirmedAt.IsZero() && artifact.DeletedAt == nil
		if !trusted {
			continue
		}
		if boundID != uuid.Nil {
			return "", uuid.Nil, fmt.Errorf("%w: multiple evidence manifest artifacts match", service.ErrEvidenceMismatch)
		}
		boundID = artifact.ID
	}
	if boundID == uuid.Nil {
		return "", uuid.Nil, fmt.Errorf("%w: clean evidence manifest artifact is required", service.ErrEvidenceMismatch)
	}
	return digest, boundID, nil
}
