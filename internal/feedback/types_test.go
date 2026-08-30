package feedback

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestFeedbackValidateAcceptsEveryAction(t *testing.T) {
	t.Parallel()
	actions := []FeedbackAction{
		FeedbackAccept,
		FeedbackDismiss,
		FeedbackWontFix,
		FeedbackOutdated,
		FeedbackNeedsDiscussion,
	}
	for _, action := range actions {
		action := action
		t.Run(string(action), func(t *testing.T) {
			t.Parallel()
			fact := validFeedback(time.Date(2026, 7, 27, 1, 0, 0, 0, time.UTC))
			fact.Action = action
			if err := fact.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
			eligibility := fact.EvaluationCandidateEligibility()
			if eligibility.Use != EvaluationUseCandidateOnly {
				t.Fatalf("evaluation use = %q, want candidate_only", eligibility.Use)
			}
			if !eligibility.GovernanceReviewRequired {
				t.Fatal("production feedback must require governance review")
			}
			if len(eligibility.ReasonCodes) != 2 ||
				eligibility.ReasonCodes[1] != "production_feedback_cannot_be_direct_gold" {
				t.Fatalf("eligibility reason codes = %#v", eligibility.ReasonCodes)
			}
		})
	}
}

func TestOutcomeValidateAcceptsEveryState(t *testing.T) {
	t.Parallel()
	states := []OutcomeState{
		OutcomeFixed,
		OutcomeRecurred,
		OutcomeEscaped,
		OutcomeUnknown,
	}
	for _, state := range states {
		state := state
		t.Run(string(state), func(t *testing.T) {
			t.Parallel()
			fact := validOutcome(time.Date(2026, 7, 27, 2, 0, 0, 0, time.UTC))
			fact.State = state
			if err := fact.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}
}

func TestFeedbackAndOutcomeValidationRejectsUnsafeOrAmbiguousFacts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 7, 27, 3, 0, 0, 0, time.UTC)
	tests := []struct {
		name string
		run  func() error
	}{
		{
			name: "unsupported feedback action",
			run: func() error {
				fact := validFeedback(now)
				fact.Action = "approve"
				return fact.Validate()
			},
		},
		{
			name: "unsafe stable id",
			run: func() error {
				fact := validFeedback(now)
				fact.FeedbackID = "../feedback"
				return fact.Validate()
			},
		},
		{
			name: "feedback source refs required",
			run: func() error {
				fact := validFeedback(now)
				fact.SourceRefs = nil
				return fact.Validate()
			},
		},
		{
			name: "source refs canonical order",
			run: func() error {
				fact := validFeedback(now)
				fact.SourceRefs = []SourceRef{
					{Kind: SourceRefIssue, Authority: "tracker", ID: "issue-1"},
					{Kind: SourceRefComment, Authority: "code-host", ID: "comment-1"},
				}
				return fact.Validate()
			},
		},
		{
			name: "artifact digest required",
			run: func() error {
				fact := validFeedback(now)
				fact.SourceRefs = []SourceRef{
					{Kind: SourceRefArtifact, Authority: "argus", ID: "artifact-1"},
				}
				return fact.Validate()
			},
		},
		{
			name: "recorded before occurred",
			run: func() error {
				fact := validFeedback(now)
				fact.RecordedAt = fact.OccurredAt.Add(-time.Second)
				return fact.Validate()
			},
		},
		{
			name: "unsupported outcome",
			run: func() error {
				fact := validOutcome(now)
				fact.State = "passed"
				return fact.Validate()
			},
		},
		{
			name: "outcome outside attribution window",
			run: func() error {
				fact := validOutcome(now)
				fact.Window.End = fact.OccurredAt.Add(-time.Second)
				return fact.Validate()
			},
		},
		{
			name: "self feedback correction",
			run: func() error {
				fact := validFeedback(now)
				fact.PriorFeedbackID = fact.FeedbackID
				return fact.Validate()
			},
		},
		{
			name: "self outcome correction",
			run: func() error {
				fact := validOutcome(now)
				fact.PriorOutcomeID = fact.OutcomeID
				return fact.Validate()
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			if err := test.run(); err == nil {
				t.Fatal("Validate() unexpectedly succeeded")
			}
		})
	}
}

func TestDecodeFeedbackJSONIsStrict(t *testing.T) {
	t.Parallel()
	fact := validFeedback(time.Date(2026, 7, 27, 4, 0, 0, 0, time.UTC))
	data, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeFeedbackJSON(data)
	if err != nil {
		t.Fatalf("DecodeFeedbackJSON(valid) error = %v", err)
	}
	if decoded.FeedbackID != fact.FeedbackID {
		t.Fatalf("decoded id = %q", decoded.FeedbackID)
	}

	unknown := []byte(strings.Replace(
		string(data),
		`"schema_version":`,
		`"unknown":true,"schema_version":`,
		1,
	))
	duplicate := []byte(strings.Replace(
		string(data),
		`"feedback_id":`,
		`"feedback_id":"duplicate","feedback_id":`,
		1,
	))
	for name, malformed := range map[string][]byte{
		"unknown":   unknown,
		"duplicate": duplicate,
		"trailing":  append(append([]byte(nil), data...), []byte(` {}`)...),
	} {
		if _, err := DecodeFeedbackJSON(malformed); err == nil {
			t.Fatalf("DecodeFeedbackJSON(%s) unexpectedly succeeded", name)
		}
	}
}

func TestDecodeOutcomeJSONIsStrict(t *testing.T) {
	t.Parallel()
	fact := validOutcome(time.Date(2026, 7, 27, 5, 0, 0, 0, time.UTC))
	data, err := json.Marshal(fact)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeOutcomeJSON(data); err != nil {
		t.Fatalf("DecodeOutcomeJSON(valid) error = %v", err)
	}
	unknown := []byte(strings.Replace(
		string(data),
		`"schema_version":`,
		`"unknown":true,"schema_version":`,
		1,
	))
	if _, err := DecodeOutcomeJSON(unknown); err == nil {
		t.Fatal("DecodeOutcomeJSON(unknown field) unexpectedly succeeded")
	}
}

func validFeedback(at time.Time) Feedback {
	return Feedback{
		SchemaVersion: FeedbackSchemaVersion,
		FeedbackID:    "feedback-1",
		FindingID:     "finding-1",
		RunID:         "run-1",
		Action:        FeedbackAccept,
		Actor:         ActorRef{Kind: ActorHuman, ID: "user-1"},
		Source:        Source{Kind: SourceCodeHost, ID: "github-installation-1"},
		OccurredAt:    at,
		RecordedAt:    at.Add(time.Second),
		SourceRefs: []SourceRef{
			{Kind: SourceRefComment, Authority: "github", ID: "comment-1"},
		},
		IdempotencyKey: "record-feedback-1",
	}
}

func validOutcome(at time.Time) Outcome {
	return Outcome{
		SchemaVersion: OutcomeSchemaVersion,
		OutcomeID:     "outcome-1",
		FindingID:     "finding-1",
		RunID:         "run-1",
		State:         OutcomeFixed,
		Actor:         ActorRef{Kind: ActorService, ID: "github-adapter"},
		Source:        Source{Kind: SourceCodeHost, ID: "github-installation-1"},
		OccurredAt:    at,
		RecordedAt:    at.Add(time.Second),
		Window: AttributionWindow{
			Start: at.Add(-24 * time.Hour),
			End:   at.Add(24 * time.Hour),
		},
		SourceRefs: []SourceRef{
			{Kind: SourceRefChange, Authority: "github", ID: "commit-1", Revision: "abc123"},
		},
		IdempotencyKey: "record-outcome-1",
	}
}
