package evaluation

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestEvaluationCaseSourceAndEligibilityPolicy(t *testing.T) {
	tests := []struct {
		name      string
		source    SourceKind
		caseType  CaseType
		outcome   ExpectedOutcome
		anchor    bool
		suppress  bool
		findingID bool
		consent   ConsentBasis
	}{
		{
			name: "human finding", source: SourceHumanConfirmedFinding,
			caseType: CasePositiveLocalized, outcome: OutcomeDefectPresent,
			anchor: true, findingID: true, consent: ConsentAuthorizedInternal,
		},
		{
			name: "false positive", source: SourceRejectedFalsePositive,
			caseType: CaseFalsePositiveRegression, outcome: OutcomeFalsePositive,
			suppress: true, findingID: true, consent: ConsentAuthorizedInternal,
		},
		{
			name: "missed defect", source: SourceIncidentMissedDefect,
			caseType: CaseMissedDefectRegression, outcome: OutcomeMissedDefect,
			anchor: true, consent: ConsentAuthorizedInternal,
		},
		{
			name: "bug fix", source: SourceReviewedBugFixPair,
			caseType: CaseFixValidation, outcome: OutcomeFixValid,
			consent: ConsentAuthorizedInternal,
		},
		{
			name: "mutation", source: SourceMutation,
			caseType: CaseMutationDiagnostic, outcome: OutcomeDefectPresent,
			consent: ConsentSynthetic,
		},
		{
			name: "synthetic workflow", source: SourceSynthetic,
			caseType: CaseWorkflowInvariant, outcome: OutcomeInvariantPass,
			consent: ConsentSynthetic,
		},
	}
	for index, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			evaluationCase := testActiveCase(
				"case-"+string(rune('a'+index)),
				"repo-"+string(rune('a'+index)),
				SplitTrain,
				testEpoch.AddDate(0, 0, index),
			)
			evaluationCase.Provenance.Kind = test.source
			evaluationCase.Type = test.caseType
			evaluationCase.Provenance.FindingID = ""
			if test.source == SourceIncidentMissedDefect {
				evaluationCase.Provenance.SourceRunID = "review-run"
			}
			if test.findingID {
				evaluationCase.Provenance.FindingID = "finding"
			}
			evaluationCase.LicenseConsent.Consent = test.consent
			evaluationCase.Label.ExpectedOutcome = test.outcome
			evaluationCase.Label.Anchors = []LabelAnchor{}
			evaluationCase.Label.AnchorRefs = []string{}
			evaluationCase.Label.SuppressionTargets = []SuppressionTarget{}
			if test.anchor {
				evaluationCase.Label.Anchors = []LabelAnchor{{
					Path: "internal/review.go", Side: "new", StartLine: 10, EndLine: 11,
					SourceDigest: evaluationDigest("anchor-" + evaluationCase.CaseID),
				}}
				evaluationCase.Label.AnchorRefs = []string{testArtifact("anchor-" + evaluationCase.CaseID)}
			}
			if test.suppress {
				evaluationCase.Label.SuppressionTargets = []SuppressionTarget{{
					ClusterFingerprint: evaluationDigest("suppression-" + evaluationCase.CaseID),
				}}
			}
			if test.outcome != OutcomeDefectPresent && test.outcome != OutcomeMissedDefect {
				evaluationCase.Label.Category = ""
				evaluationCase.Label.Severity = ""
			}
			if test.source == SourceMutation || test.source == SourceSynthetic {
				evaluationCase.Eligibility.ProductionDistribution = false
			}
			if err := evaluationCase.Validate(); err != nil {
				t.Fatalf("Validate() error = %v", err)
			}
		})
	}

	feedback := testFeedbackCase("feedback", testEpoch)
	if err := feedback.Validate(); err != nil {
		t.Fatalf("valid production feedback error = %v", err)
	}
	feedback.DatasetState = DatasetGold
	feedback.ReviewState = ReviewApproved
	if err := feedback.Validate(); err == nil ||
		!strings.Contains(err.Error(), "production feedback") {
		t.Fatalf("gold production feedback error = %v", err)
	}

	mutation := testActiveCase("mutation", "repo-mutation", SplitTrain, testEpoch)
	mutation.Provenance.Kind = SourceMutation
	mutation.Type = CaseMutationDiagnostic
	mutation.Eligibility.ProductionDistribution = true
	if err := mutation.Validate(); err == nil ||
		!strings.Contains(err.Error(), "production distribution") {
		t.Fatalf("mutation production distribution error = %v", err)
	}
}

func TestStructuredLabelAnchorsFailClosed(t *testing.T) {
	valid := testActiveCase("anchor-case", "repo-anchor", SplitTest, testEpoch)
	tests := []struct {
		name   string
		mutate func(*EvaluationCase)
	}{
		{"missing structured anchors", func(value *EvaluationCase) { value.Label.Anchors = []LabelAnchor{} }},
		{"unsafe path", func(value *EvaluationCase) { value.Label.Anchors[0].Path = "../secret.go" }},
		{"unknown side", func(value *EvaluationCase) { value.Label.Anchors[0].Side = "working" }},
		{"invalid digest", func(value *EvaluationCase) { value.Label.Anchors[0].SourceDigest = "not-a-digest" }},
		{"duplicate anchor", func(value *EvaluationCase) {
			value.Label.Anchors = append(value.Label.Anchors, value.Label.Anchors[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Label.Anchors = append([]LabelAnchor(nil), valid.Label.Anchors...)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("EvaluationCase.Validate() accepted invalid structured anchor")
			}
		})
	}
}

func TestStructuredSuppressionTargetsFailClosed(t *testing.T) {
	valid := testActiveCase("suppression-case", "repo-suppression", SplitTest, testEpoch)
	valid.Provenance.Kind = SourceRejectedFalsePositive
	valid.Provenance.FindingID = "finding-rejected-1"
	valid.Type = CaseFalsePositiveRegression
	valid.Label = Label{
		ExpectedOutcome: OutcomeFalsePositive,
		Anchors:         []LabelAnchor{}, AnchorRefs: []string{},
		SuppressionTargets: []SuppressionTarget{{
			ClusterFingerprint: evaluationDigest("suppression-target"),
		}},
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid false-positive regression error = %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*EvaluationCase)
	}{
		{"missing target", func(value *EvaluationCase) {
			value.Label.SuppressionTargets = []SuppressionTarget{}
		}},
		{"invalid fingerprint", func(value *EvaluationCase) {
			value.Label.SuppressionTargets[0].ClusterFingerprint = "not-a-digest"
		}},
		{"duplicate target", func(value *EvaluationCase) {
			value.Label.SuppressionTargets = append(
				value.Label.SuppressionTargets, value.Label.SuppressionTargets[0],
			)
		}},
		{"target on positive case", func(value *EvaluationCase) {
			value.Provenance.Kind = SourceHumanConfirmedFinding
			value.Type = CasePositiveLocalized
			value.Label = testDefectLabel("medium")
			value.Label.SuppressionTargets = []SuppressionTarget{{
				ClusterFingerprint: evaluationDigest("suppression-target"),
			}}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := valid
			candidate.Label.SuppressionTargets = append(
				[]SuppressionTarget(nil), valid.Label.SuppressionTargets...,
			)
			test.mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("EvaluationCase.Validate() accepted invalid suppression truth")
			}
		})
	}
}

func TestValidateDatasetContaminationUsesRepositoryCloneAndTimeBoundaries(t *testing.T) {
	train := testActiveCase("train", "repo-train", SplitTrain, testEpoch)
	dev := testActiveCase("dev", "repo-dev", SplitDev, testEpoch.AddDate(0, 0, 1))
	testCase := testActiveCase("test", "repo-test", SplitTest, testEpoch.AddDate(0, 0, 2))
	holdout := testActiveCase("holdout", "repo-holdout", SplitHoldout, testEpoch.AddDate(0, 0, 3))
	if err := ValidateDatasetContamination(
		[]EvaluationCase{holdout, train, testCase, dev},
	); err != nil {
		t.Fatalf("valid shuffled dataset error = %v", err)
	}

	t.Run("repository", func(t *testing.T) {
		contaminated := dev
		contaminated.Provenance.RepositoryID = train.Provenance.RepositoryID
		if err := ValidateDatasetContamination(
			[]EvaluationCase{train, contaminated},
		); !errors.Is(err, ErrContaminated) {
			t.Fatalf("repository contamination error = %v", err)
		}
	})
	t.Run("semantic clone", func(t *testing.T) {
		contaminated := dev
		contaminated.CloneGroupID = train.CloneGroupID
		if err := ValidateDatasetContamination(
			[]EvaluationCase{train, contaminated},
		); !errors.Is(err, ErrContaminated) {
			t.Fatalf("clone contamination error = %v", err)
		}
	})
	t.Run("time", func(t *testing.T) {
		laterTrain := train
		laterTrain.Provenance.ObservedAt = dev.Provenance.ObservedAt.AddDate(0, 0, 1)
		laterTrain.Provenance.CollectedAt = laterTrain.Provenance.ObservedAt.Add(1)
		laterTrain.CreatedAt = laterTrain.Provenance.ObservedAt.Add(2)
		if err := ValidateDatasetContamination(
			[]EvaluationCase{laterTrain, dev},
		); !errors.Is(err, ErrContaminated) {
			t.Fatalf("time contamination error = %v", err)
		}
	})
}

func TestStrictEvaluationJSONRejectsUnknownDuplicateAndTrailingData(t *testing.T) {
	evaluationCase := testActiveCase("strict", "repo-strict", SplitTrain, testEpoch)
	data, err := json.Marshal(evaluationCase)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeEvaluationCase(data); err != nil {
		t.Fatalf("DecodeEvaluationCase(valid) error = %v", err)
	}
	unknown := append(append([]byte(nil), data[:len(data)-1]...), []byte(`,"unknown":true}`)...)
	if _, err := DecodeEvaluationCase(unknown); err == nil ||
		!strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field error = %v", err)
	}
	duplicate := append(
		append([]byte(nil), data[:len(data)-1]...),
		[]byte(`,"case_id":"strict"}`)...,
	)
	if _, err := DecodeEvaluationCase(duplicate); err == nil ||
		!strings.Contains(err.Error(), "duplicate JSON field") {
		t.Fatalf("duplicate field error = %v", err)
	}
	if _, err := DecodeEvaluationCase(append(data, []byte(` {}`)...)); err == nil ||
		!strings.Contains(err.Error(), "trailing") {
		t.Fatalf("trailing JSON error = %v", err)
	}
}

func TestAllPublicEvaluationDecodersRoundTripAndRejectUnknownFields(t *testing.T) {
	evaluationCase := testActiveCase("decoder", "repo-decoder", SplitTrain, testEpoch)
	values := []struct {
		name   string
		value  any
		decode func([]byte) error
	}{
		{
			name:  "case",
			value: evaluationCase,
			decode: func(data []byte) error {
				_, err := DecodeEvaluationCase(data)
				return err
			},
		},
		{
			name: "correction",
			value: LabelCorrection{
				SchemaVersion:         LabelCorrectionSchemaVersion,
				CaseID:                evaluationCase.CaseID,
				ExpectedLabelRevision: 1,
				Label:                 testDefectLabel("high"),
				LabelPolicyRevision:   "label-policy-2",
				Reason:                "correction",
				AffectedExperimentIDs: []string{},
			},
			decode: func(data []byte) error {
				_, err := DecodeLabelCorrection(data)
				return err
			},
		},
		{
			name: "exposure",
			value: testExposure(
				"decode-run", evaluationCase.CaseID,
				evaluationCase.CreatedAt.Add(1), ExposureNotSeen,
			),
			decode: func(data []byte) error {
				_, err := DecodeExposure(data)
				return err
			},
		},
		{
			name:  "promotion variant",
			value: testPromotionVariant("decoder-variant", evaluationCase.CreatedAt.Add(1)),
			decode: func(data []byte) error {
				_, err := DecodePromotionVariant(data)
				return err
			},
		},
		{
			name:  "gate result",
			value: testGateResult("decoder-variant", GateSchemaContract),
			decode: func(data []byte) error {
				_, err := DecodeGateResult(data)
				return err
			},
		},
	}
	for _, test := range values {
		t.Run(test.name, func(t *testing.T) {
			data, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			if err := test.decode(data); err != nil {
				t.Fatalf("decode(valid) error = %v", err)
			}
			unknown := append(
				append([]byte(nil), data[:len(data)-1]...),
				[]byte(`,"unknown":true}`)...,
			)
			if err := test.decode(unknown); err == nil ||
				!strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("decode(unknown) error = %v", err)
			}
		})
	}
}
