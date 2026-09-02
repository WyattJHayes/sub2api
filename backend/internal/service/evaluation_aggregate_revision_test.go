package service

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/require"
)

func TestCellInputSetHashIgnoresInputOrder(t *testing.T) {
	first := aggregateCellPairFixture(1, "1")
	second := aggregateCellPairFixture(2, "2")

	forward, err := CanonicalCellInputSetHash([]CellPairInput{first, second})
	require.NoError(t, err)
	reversed, err := CanonicalCellInputSetHash([]CellPairInput{second, first})
	require.NoError(t, err)

	require.Regexp(t, "^[0-9a-f]{64}$", forward)
	require.Equal(t, forward, reversed)
}

func TestCellInputSetHashBindsCompositeScoreRef(t *testing.T) {
	input := aggregateCellPairFixture(1, "1")
	initial, err := CanonicalCellInputSetHash([]CellPairInput{input})
	require.NoError(t, err)

	input.CandidateScore.CreatedAt = input.CandidateScore.CreatedAt.Add(time.Microsecond)
	changed, err := CanonicalCellInputSetHash([]CellPairInput{input})
	require.NoError(t, err)

	require.NotEqual(t, initial, changed)
}

func TestGlobalInputHashChangesWhenAnyCellHeadChanges(t *testing.T) {
	window := time.Date(2026, 7, 28, 0, 0, 0, 0, time.UTC)
	first := GlobalCellInput{
		CapabilityDomain: "coding", CanonicalModelRoute: "route-a",
		Snapshot:          SnapshotRef{ID: uuid.MustParse("10000000-0000-0000-0000-000000000001"), WindowStart: window},
		AggregateRevision: 1, InputSetHash: aggregateHashFixture("a"), AggregateHash: aggregateHashFixture("b"),
	}
	second := GlobalCellInput{
		CapabilityDomain: "reasoning", CanonicalModelRoute: "route-b",
		Snapshot:          SnapshotRef{ID: uuid.MustParse("20000000-0000-0000-0000-000000000002"), WindowStart: window},
		AggregateRevision: 3, InputSetHash: aggregateHashFixture("c"), AggregateHash: aggregateHashFixture("d"),
	}

	before, err := CanonicalGlobalInputSetHash([]GlobalCellInput{second, first})
	require.NoError(t, err)
	second.AggregateRevision = 4
	after, err := CanonicalGlobalInputSetHash([]GlobalCellInput{first, second})
	require.NoError(t, err)

	require.Regexp(t, "^[0-9a-f]{64}$", before)
	require.NotEqual(t, before, after)
}

func TestCanonicalModelRouteRemovesOnlyOneSidePrefix(t *testing.T) {
	require.Equal(t, "route-a", CanonicalModelRoute("baseline:route-a"))
	require.Equal(t, "candidate:route-a", CanonicalModelRoute("baseline:candidate:route-a"))
	require.Equal(t, "route-a", CanonicalModelRoute("route-a"))
	require.Equal(t, " baseline:route-a ", CanonicalModelRoute(" baseline:route-a "))
}

func aggregateCellPairFixture(index int, suffix string) CellPairInput {
	createdAt := time.Date(2026, 7, 28, 1, index, 0, 0, time.UTC)
	return CellPairInput{
		CaseID:                   uuid.MustParse("00000000-0000-0000-0000-00000000000" + suffix),
		SampleIndex:              index,
		PairSpecHash:             aggregateHashFixture("1"),
		BaselineSideSpecHash:     aggregateHashFixture("2"),
		CandidateSideSpecHash:    aggregateHashFixture("3"),
		PairBindingHash:          aggregateHashFixture("4"),
		GraderID:                 "grader",
		GraderVersion:            "v1",
		BaselineHeadVersion:      1,
		BaselineScore:            ScoreRef{ID: uuid.MustParse("10000000-0000-0000-0000-00000000000" + suffix), CreatedAt: createdAt},
		BaselineSourceAssignment: uuid.MustParse("20000000-0000-0000-0000-00000000000" + suffix),
		BaselineEvidenceSetHash:  aggregateHashFixture("5"),
		CandidateHeadVersion:     2,
		CandidateScore:           ScoreRef{ID: uuid.MustParse("30000000-0000-0000-0000-00000000000" + suffix), CreatedAt: createdAt},
		CandidateSourceAssignment: uuid.MustParse(
			"40000000-0000-0000-0000-00000000000" + suffix,
		),
		CandidateEvidenceSetHash: aggregateHashFixture("6"),
		CaseWeight:               decimal.RequireFromString("1.5"),
	}
}

func aggregateHashFixture(value string) string {
	result := ""
	for len(result) < 64 {
		result += value
	}
	return result[:64]
}
