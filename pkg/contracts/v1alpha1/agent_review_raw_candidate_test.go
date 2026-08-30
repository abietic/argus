package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestAgentReviewRawCandidateCollectionStrictRoundTripAndTamper(t *testing.T) {
	collection := validAgentReviewRawCandidateCollection(t)
	data, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeAgentReviewRawCandidateCollection(data)
	if err != nil {
		t.Fatalf("strict round trip: %v", err)
	}
	if decoded.RawCandidates[0].Action != HypothesisNormalizationRejectedInvalid ||
		decoded.RawCandidates[0].Claim.Title != "candidate with an invalid anchor" {
		t.Fatalf("decoded raw candidate = %+v", decoded.RawCandidates[0])
	}

	for _, test := range []struct {
		name   string
		mutate func(*AgentReviewRawCandidateCollection)
		want   string
	}{
		{name: "claim tamper", mutate: func(value *AgentReviewRawCandidateCollection) {
			value.RawCandidates[0].Claim.Title = "changed"
		}, want: "claim_digest"},
		{name: "decision tamper", mutate: func(value *AgentReviewRawCandidateCollection) {
			value.RawCandidates[0].Action = HypothesisNormalizationRetained
		}, want: "reason_code"},
		{name: "authority tamper", mutate: func(value *AgentReviewRawCandidateCollection) {
			value.Authority = "platform_attested"
		}, want: "worker_self_report"},
	} {
		t.Run(test.name, func(t *testing.T) {
			value := collection
			value.RawCandidates = append([]AgentReviewRawCandidatePayload{}, collection.RawCandidates...)
			test.mutate(&value)
			if err := value.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want containing %q", err, test.want)
			}
		})
	}

	unknown := append(data[:len(data)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeAgentReviewRawCandidateCollection(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown field error = %v", err)
	}
	trailing := append(append([]byte{}, data...), []byte(` {}`)...)
	if _, err := DecodeAgentReviewRawCandidateCollection(trailing); err == nil {
		t.Fatal("trailing JSON was accepted")
	}
}

func validAgentReviewRawCandidateCollection(t *testing.T) AgentReviewRawCandidateCollection {
	t.Helper()
	claim := AgentReviewRawCandidateClaim{
		Category: "correctness", Severity: HypothesisSeverityHigh,
		Title:       "candidate with an invalid anchor",
		Description: "the worker proposed a claim whose anchor cannot be admitted",
		Impact:      "the claim could become a noisy review comment",
		Anchor: AgentReviewRawCandidateSourceAnchor{
			Path: "outside/change.go", Side: HypothesisAnchorNew,
			StartLine: 7, EndLine: 7,
		},
		Evidence: []AgentReviewRawCandidateEvidence{{
			Statement: "worker supplied this source excerpt",
			Anchor: AgentReviewRawCandidateSourceAnchor{
				Path: "outside/change.go", Side: HypothesisAnchorNew,
				StartLine: 7, EndLine: 7,
			},
			Excerpt: "value.Field()",
		}},
	}
	digest, err := DigestAgentReviewRawCandidateClaim(claim)
	if err != nil {
		t.Fatal(err)
	}
	return AgentReviewRawCandidateCollection{
		SchemaVersion: AgentReviewRawCandidateCollectionSchemaVersion,
		PlanID:        "plan-1", SourceRunID: "source-run-1",
		ExecutionID: "execution-1", ReviewRunID: "review-run-1",
		TargetDigest: testDigest,
		Authority:    AgentReviewRawCandidateAuthorityWorkerSelfReport,
		Disposition:  AgentReviewRawCandidateDispositionShadowOnly,
		RawCandidates: []AgentReviewRawCandidatePayload{{
			RawCandidateID: "raw-candidate-1", GroupID: "group-1",
			Dimension: testVersionedRef("correctness"), Ordinal: 0,
			ClaimDigest: digest, Action: HypothesisNormalizationRejectedInvalid,
			ReasonCode: "invalid_candidate", Claim: claim,
		}},
	}
}
