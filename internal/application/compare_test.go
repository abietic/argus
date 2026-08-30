package application

import "testing"

func TestCompareCandidatesUsesStableFormalFingerprintAcrossRunLocalIDs(t *testing.T) {
	const fingerprint = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	baseline := []comparableCandidate{{
		MatchKey: fingerprint, CandidateID: "candidate-baseline",
		Fingerprint: fingerprint, RuleID: "correctness", Path: "review.go", Line: 12,
	}}
	variant := []comparableCandidate{{
		MatchKey: fingerprint, CandidateID: "candidate-variant",
		Fingerprint: fingerprint, RuleID: "correctness", Path: "review.go", Line: 12,
	}}

	added, removed, unchanged := compareCandidates(baseline, variant)
	if len(added) != 0 || len(removed) != 0 || len(unchanged) != 1 ||
		unchanged[0].CandidateID != "candidate-baseline" ||
		unchanged[0].VariantCandidateID != "candidate-variant" ||
		unchanged[0].Fingerprint != fingerprint {
		t.Fatalf("candidate comparison added=%+v removed=%+v unchanged=%+v", added, removed, unchanged)
	}
}
