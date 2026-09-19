package service

import (
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func releaseSubjectFixture() ReleaseSubject {
	return ReleaseSubject{
		CandidateModelConfigSHA256: hashFixture("a"),
		BaselineID:                 uuid.MustParse("00000000-0000-4000-8000-000000000001"),
		DatasetManifestSHA256:      hashFixture("b"),
		RouteProfileVersion:        "route-v7",
		GatewayImageDigest:         "sha256:gateway",
		ControlPlaneImageDigest:    "sha256:control",
		RunnerImageDigests:         []string{"sha256:runner-b", "sha256:runner-a", "sha256:runner-a"},
		GraderImageDigests:         []string{"sha256:grader-b", "sha256:grader-a"},
		StatisticsImageDigests:     []string{"sha256:statistics"},
		AnalysisVersion:            "analysis-v3",
		RegionSet:                  []string{"us-west", "ap-east", "ap-east"},
		DeploymentEnvironment:      "staging",
		ScopeType:                  "global",
		ScopeID:                    "global",
	}
}

func hashFixture(char string) string {
	value := ""
	for len(value) < 64 {
		value += char
	}
	return value
}

func TestReleaseSubjectGoldenHashSortsDigestAndRegionSets(t *testing.T) {
	first, err := CanonicalizeReleaseSubject(releaseSubjectFixture())
	require.NoError(t, err)

	reordered := releaseSubjectFixture()
	reordered.RunnerImageDigests = []string{"sha256:runner-a", "sha256:runner-b"}
	reordered.GraderImageDigests = []string{"sha256:grader-a", "sha256:grader-b"}
	reordered.RegionSet = []string{"ap-east", "us-west"}
	second, err := CanonicalizeReleaseSubject(reordered)
	require.NoError(t, err)

	require.Equal(t, first.SHA256, second.SHA256)
	require.Equal(t, "2636384fbfd8fdc8e10fa02f5a2eadd6e280b6ed212edb1445f74110d787cdc8", first.SHA256)
}

func TestReleaseSubjectHashChangesForAnyDeployableIdentityChange(t *testing.T) {
	base := releaseSubjectFixture()
	canonical, err := CanonicalizeReleaseSubject(base)
	require.NoError(t, err)

	variants := map[string]func(*ReleaseSubject){
		"candidate config": func(s *ReleaseSubject) { s.CandidateModelConfigSHA256 = hashFixture("c") },
		"baseline":         func(s *ReleaseSubject) { s.BaselineID = uuid.New() },
		"dataset":          func(s *ReleaseSubject) { s.DatasetManifestSHA256 = hashFixture("d") },
		"route profile":    func(s *ReleaseSubject) { s.RouteProfileVersion = "route-v8" },
		"gateway image":    func(s *ReleaseSubject) { s.GatewayImageDigest = "sha256:gateway-new" },
		"control image":    func(s *ReleaseSubject) { s.ControlPlaneImageDigest = "sha256:control-new" },
		"runner image":     func(s *ReleaseSubject) { s.RunnerImageDigests = append(s.RunnerImageDigests, "sha256:runner-c") },
		"grader image":     func(s *ReleaseSubject) { s.GraderImageDigests = append(s.GraderImageDigests, "sha256:grader-c") },
		"statistics image": func(s *ReleaseSubject) { s.StatisticsImageDigests = []string{"sha256:statistics-new"} },
		"analysis":         func(s *ReleaseSubject) { s.AnalysisVersion = "analysis-v4" },
		"region":           func(s *ReleaseSubject) { s.RegionSet = append(s.RegionSet, "eu-central") },
		"environment":      func(s *ReleaseSubject) { s.DeploymentEnvironment = "production" },
		"scope":            func(s *ReleaseSubject) { s.ScopeType, s.ScopeID = "route", "route-a" },
	}
	for name, mutate := range variants {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.RunnerImageDigests = append([]string(nil), base.RunnerImageDigests...)
			candidate.GraderImageDigests = append([]string(nil), base.GraderImageDigests...)
			candidate.StatisticsImageDigests = append([]string(nil), base.StatisticsImageDigests...)
			candidate.RegionSet = append([]string(nil), base.RegionSet...)
			mutate(&candidate)
			changed, err := CanonicalizeReleaseSubject(candidate)
			require.NoError(t, err)
			require.NotEqual(t, canonical.SHA256, changed.SHA256)
		})
	}
}

func TestGlobalScopeUsesCanonicalGlobalID(t *testing.T) {
	subject := releaseSubjectFixture()
	subject.ScopeID = " GLOBAL "
	canonical, err := CanonicalizeReleaseSubject(subject)
	require.NoError(t, err)
	require.Equal(t, GlobalReleaseScopeID, canonical.Subject.ScopeID)
}
