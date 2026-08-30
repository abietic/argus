package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestGovernedCandidateSetSealDecodeAndStrictRejections(t *testing.T) {
	set, err := SealGovernedCandidateSet(GovernedCandidateSet{
		ReviewRunID: "formal-run-test", ExecutionID: "execution-test",
		HypothesisSetID: "hypothesis-set-test",
		TargetDigest:    strings.Repeat("a", 64),
		Completeness:    AgentReviewComplete,
		Candidates:      []GovernedReviewCandidate{},
		GeneratedAt:     time.Date(2026, 8, 25, 1, 2, 3, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(set)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeGovernedCandidateSet(data)
	if err != nil || decoded.CandidateSetID != set.CandidateSetID {
		t.Fatalf("DecodeGovernedCandidateSet() = %+v, %v", decoded, err)
	}
	mutations := map[string][]byte{
		"unknown":  []byte(strings.Replace(string(data), "{", `{"unknown":true,`, 1)),
		"null":     []byte(strings.Replace(string(data), `"candidates":[]`, `"candidates":null`, 1)),
		"trailing": append(append([]byte{}, data...), []byte(" true")...),
	}
	for name, candidate := range mutations {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeGovernedCandidateSet(candidate); err == nil {
				t.Fatalf("DecodeGovernedCandidateSet accepted %s input", name)
			}
		})
	}
}
