package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	jsoncanonicalizer "github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/google/uuid"
)

const GlobalReleaseScopeID = "global"

type ReleaseSubject struct {
	CandidateModelConfigSHA256 string    `json:"candidate_model_config_sha256"`
	BaselineID                 uuid.UUID `json:"baseline_id"`
	DatasetManifestSHA256      string    `json:"dataset_manifest_sha256"`
	RouteProfileVersion        string    `json:"route_profile_version"`
	GatewayImageDigest         string    `json:"gateway_image_digest"`
	ControlPlaneImageDigest    string    `json:"control_plane_image_digest"`
	RunnerImageDigests         []string  `json:"runner_image_digests"`
	GraderImageDigests         []string  `json:"grader_image_digests"`
	StatisticsImageDigests     []string  `json:"statistics_image_digests"`
	AnalysisVersion            string    `json:"analysis_version"`
	RegionSet                  []string  `json:"region_set"`
	DeploymentEnvironment      string    `json:"deployment_environment"`
	ScopeType                  string    `json:"scope_type"`
	ScopeID                    string    `json:"scope_id"`
}

type CanonicalReleaseSubject struct {
	Subject ReleaseSubject
	Bytes   json.RawMessage
	SHA256  string
}

func CanonicalizeReleaseSubject(subject ReleaseSubject) (CanonicalReleaseSubject, error) {
	subject.CandidateModelConfigSHA256 = strings.TrimSpace(subject.CandidateModelConfigSHA256)
	subject.DatasetManifestSHA256 = strings.TrimSpace(subject.DatasetManifestSHA256)
	subject.RouteProfileVersion = strings.TrimSpace(subject.RouteProfileVersion)
	subject.GatewayImageDigest = strings.TrimSpace(subject.GatewayImageDigest)
	subject.ControlPlaneImageDigest = strings.TrimSpace(subject.ControlPlaneImageDigest)
	subject.AnalysisVersion = strings.TrimSpace(subject.AnalysisVersion)
	subject.DeploymentEnvironment = strings.ToLower(strings.TrimSpace(subject.DeploymentEnvironment))
	subject.ScopeType = strings.ToLower(strings.TrimSpace(subject.ScopeType))
	subject.ScopeID = strings.TrimSpace(subject.ScopeID)
	if subject.ScopeType == "global" {
		subject.ScopeID = GlobalReleaseScopeID
	}
	subject.RunnerImageDigests = canonicalReleaseSet(subject.RunnerImageDigests)
	subject.GraderImageDigests = canonicalReleaseSet(subject.GraderImageDigests)
	subject.StatisticsImageDigests = canonicalReleaseSet(subject.StatisticsImageDigests)
	subject.RegionSet = canonicalReleaseSet(subject.RegionSet)

	if !isLowerHexSHA256(subject.CandidateModelConfigSHA256) || !isLowerHexSHA256(subject.DatasetManifestSHA256) {
		return CanonicalReleaseSubject{}, errors.New("release subject model and dataset hashes must be lowercase SHA256")
	}
	if subject.BaselineID == uuid.Nil {
		return CanonicalReleaseSubject{}, errors.New("release subject baseline is required")
	}
	if subject.RouteProfileVersion == "" || subject.GatewayImageDigest == "" || subject.ControlPlaneImageDigest == "" || subject.AnalysisVersion == "" {
		return CanonicalReleaseSubject{}, errors.New("release subject deployable identity is incomplete")
	}
	if len(subject.RunnerImageDigests) == 0 || len(subject.GraderImageDigests) == 0 || len(subject.StatisticsImageDigests) == 0 || len(subject.RegionSet) == 0 {
		return CanonicalReleaseSubject{}, errors.New("release subject worker images and regions are required")
	}
	if subject.DeploymentEnvironment == "" || subject.ScopeType == "" || subject.ScopeID == "" {
		return CanonicalReleaseSubject{}, errors.New("release subject deployment scope is required")
	}

	encoded, err := json.Marshal(subject)
	if err != nil {
		return CanonicalReleaseSubject{}, err
	}
	canonical, err := jsoncanonicalizer.Transform(encoded)
	if err != nil {
		return CanonicalReleaseSubject{}, err
	}
	digest := sha256.Sum256(canonical)
	return CanonicalReleaseSubject{
		Subject: subject,
		Bytes:   append(json.RawMessage(nil), canonical...),
		SHA256:  hex.EncodeToString(digest[:]),
	}, nil
}

func CanonicalizeGovernanceScope(scope RadarGovernanceScope) (RadarGovernanceScope, error) {
	scope.Environment = strings.ToLower(strings.TrimSpace(scope.Environment))
	scope.ScopeType = strings.ToLower(strings.TrimSpace(scope.ScopeType))
	scope.ScopeID = strings.TrimSpace(scope.ScopeID)
	if scope.ScopeType == "global" {
		scope.ScopeID = GlobalReleaseScopeID
	}
	if scope.Environment == "" || scope.ScopeType == "" || scope.ScopeID == "" {
		return RadarGovernanceScope{}, errors.New("governance scope is incomplete")
	}
	if len(scope.Environment) > 64 || len(scope.ScopeType) > 32 || len(scope.ScopeID) > 200 {
		return RadarGovernanceScope{}, errors.New("governance scope exceeds storage limits")
	}
	return scope, nil
}

func canonicalReleaseSet(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func isLowerHexSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
