package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/formalreview"
	"argus.local/argus/internal/normalizationpromotion"
	"argus.local/argus/internal/pireviewmap"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestEvaluationNormalizationCLISealsOracleAndRecomputesQualityFromExactClosure(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node is unavailable")
	}
	node, err = filepath.Abs(node)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := filepath.Abs(filepath.Join("..", "..", "runtime", "pi-review", "dist", "worker.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(worker); err != nil {
		t.Skipf("built Pi worker is unavailable: %v", err)
	}

	repositoryPath := newCLITargetRepository(t)
	storePath, configState := t.TempDir(), t.TempDir()
	sourceRunID := createFormalCorpusSourceRun(t, repositoryPath, storePath, "normalization")
	state, err := local.Open(storePath)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(state)
	if err != nil {
		t.Fatal(err)
	}
	sourceRun, err := runs.LoadRun(sourceRunID)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := evaluation.New(state)
	if err != nil {
		t.Fatal(err)
	}
	caseAt := time.Now().UTC().Add(-10 * time.Minute)
	roles := []evaluation.Role{evaluation.RoleDatasetCurator}
	evaluationCase := testCLIEvaluationCase("normalization-case-test", "normalization-source", evaluation.SplitTest, caseAt)
	evaluationCase.InputSnapshotRef = sourceRun.TargetSnapshotRef.URI
	evaluationCase.LicenseConsent.AllowedUses = []evaluation.UseScope{evaluation.UseEvaluation}
	evaluationCase.ReviewState = evaluation.ReviewApproved
	evaluationCase.DatasetState = evaluation.DatasetActive
	evaluationCase.Eligibility = evaluation.Eligibility{Evaluation: true}
	governedImport := testCLIExternalGovernedImport(t, evaluationCase)
	registeredAt := caseAt.Add(-time.Minute)
	if _, err := repository.RegisterGovernanceTrustKey(context.Background(), evaluation.GovernanceTrustKeyRegistration{
		SchemaVersion: evaluation.GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           governedImport.TrustedKey, RegisteredAt: registeredAt,
	}, evaluation.Mutation{
		IdempotencyKey: "normalization-register-key", Actor: "normalization-trust-admin",
		Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin}, Audit: "register normalization test key", At: registeredAt,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ImportGovernedCase(context.Background(), governedImport, evaluation.Mutation{
		IdempotencyKey: "normalization-import-case", Actor: "normalization-curator",
		Roles: roles, Audit: "import normalization test case", At: caseAt,
	}); err != nil {
		t.Fatal(err)
	}
	access := evaluation.Access{Actor: "normalization-curator", Roles: roles}
	accessPath := writeBatchJSON(t, "normalization-access.json", access)
	snapshotAt := caseAt.Add(time.Minute)
	snapshotRequest := evaluation.CorpusSnapshotRequest{
		SchemaVersion: evaluation.CorpusSnapshotRequestSchemaVersion,
		CorpusID:      "normalization-test-corpus", Revision: "revision-1",
		Purpose: evaluation.CorpusPurposeQualityGate, Split: evaluation.SplitTest,
		Cases: []evaluation.CorpusSnapshotCaseRequest{{
			CaseID: evaluationCase.CaseID, ExpectedGovernanceRevision: 1,
			ExpectedLabelRevision: 1, SourceReviewRunID: sourceRunID,
		}},
		CreatedAt: snapshotAt,
	}
	var snapshotBytes bytes.Buffer
	if err := runEvaluationCorpus(t.Context(), []string{
		"snapshot", "build", "--store", storePath,
		"--input", writeBatchJSON(t, "normalization-snapshot-request.json", snapshotRequest),
		"--access", accessPath, "--json",
	}, &snapshotBytes); err != nil {
		t.Fatal(err)
	}
	var snapshotOutput corpusSnapshotOutput
	decodeCLIOutput(t, snapshotBytes.Bytes(), &snapshotOutput)

	exposure := []evaluation.ExposureObservation{
		{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-1"},
		{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-1"},
		{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-1"},
		{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-1"},
	}
	batchAt := snapshotAt.Add(time.Minute)
	batchRequest := evaluation.FormalCorpusBatchRequest{
		SchemaVersion: evaluation.FormalCorpusBatchRequestSchemaVersion,
		BatchID:       "normalization-formal-batch", EvaluationRunID: "normalization-evaluation-run",
		EvaluatorRevision: "defect-presence-1", ExecutorRevision: localFormalCorpusExecutorRevision,
		CorpusSnapshotRef: snapshotOutput.SnapshotRef, MaxConcurrency: 1,
		Cases: []evaluation.FormalCorpusBatchCase{{
			CaseID: evaluationCase.CaseID, ExpectedLabelRevision: 1,
			SourceReviewRunID: sourceRunID, ExposureObservations: exposure,
		}},
		CreatedAt: batchAt,
	}
	mutation := evaluation.Mutation{
		IdempotencyKey: "normalization-formal-intent", Actor: access.Actor,
		Roles: roles, Audit: "run normalization formal case", At: batchAt,
	}
	arguments := []string{
		"--store", storePath,
		"--input", writeBatchJSON(t, "normalization-formal-request.json", batchRequest),
		"--mutation", writeBatchJSON(t, "normalization-formal-mutation.json", mutation),
		"--json", "--", "--config-state-dir", configState,
		"--node", node, "--worker-script", worker,
		"--provider-profile", "deepseek-anthropic-env", "--model", "deepseek-chat",
		"--normalization-revision", contractsv1alpha1.CandidateNormalizationRevisionV1,
		"--input-micros-per-million", "1", "--output-micros-per-million", "1", "--max-bytes-per-input-token", "4",
	}
	formalFlags, err := parseFormalAgentRunFlags(append([]string{
		"--store", storePath, "--source-run", "placeholder-source", "--idempotency-key", "placeholder-key",
	}, arguments[argumentsIndexAfterSeparator(arguments):]...))
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := formalreview.BuildLocalPiBootstrap(formalFlags.options)
	if err != nil {
		t.Fatal(err)
	}
	runner := &formalSucceededRunner{promptDigest: bootstrap.Manifest.Prompt.SHA256, confirmedFinding: true}
	var formalBytes bytes.Buffer
	if err := runEvaluationCorpusExecutionWithRunner(t.Context(), arguments, &formalBytes, runner); err != nil {
		t.Fatalf("run formal normalization case: %v", err)
	}
	var formalOutput formalCorpusOutput
	decodeCLIOutput(t, formalBytes.Bytes(), &formalOutput)
	formalRunID := formalOutput.Result.Cases[0].FormalReviewRunID
	formalRun, err := runs.LoadRun(formalRunID)
	if err != nil {
		t.Fatal(err)
	}
	formalSnapshot, err := runs.LoadExecutionSnapshot(formalRun.ExecutionSnapshotID)
	if err != nil {
		t.Fatal(err)
	}
	formalBundleData, err := runs.ReadArtifact(formalSnapshot.ConfigBundleRef)
	if err != nil {
		t.Fatal(err)
	}
	formalBundle, err := reviewconfig.DecodeBundle(formalBundleData)
	if err != nil {
		t.Fatal(err)
	}
	if formalBundle.AgentReview == nil {
		t.Fatal("formal baseline has no agent_review policy")
	}
	var normalizationOwner reviewconfig.SourceRef
	for _, field := range formalBundle.FieldSources {
		if field.Field == "agent_review.normalization" && len(field.Sources) > 0 {
			normalizationOwner = field.Sources[len(field.Sources)-1]
			break
		}
	}
	if normalizationOwner.ID == "" {
		t.Fatal("formal baseline has no normalization field owner")
	}
	rawData, err := runs.ReadArtifact(*formalRun.RawCandidateCollectionRef)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := contractsv1alpha1.DecodeAgentReviewRawCandidateCollection(rawData)
	if err != nil {
		t.Fatal(err)
	}
	eligible := make([]string, 0, len(raw.RawCandidates))
	for _, candidate := range raw.RawCandidates {
		if candidate.Action == contractsv1alpha1.HypothesisNormalizationRetained || candidate.Action == contractsv1alpha1.HypothesisNormalizationMergedDuplicate {
			eligible = append(eligible, candidate.RawCandidateID)
		}
	}
	slices.Sort(eligible)
	if len(eligible) == 0 {
		t.Fatal("formal fixture produced no eligible raw candidates")
	}
	evidenceRef, err := runs.PutArtifact("argus.external_normalization_oracle_evidence.v1alpha1", []byte("independently adjudicated equivalence evidence"))
	if err != nil {
		t.Fatal(err)
	}
	formalRunRef, err := runs.CommittedRunRef(formalRunID)
	if err != nil {
		t.Fatal(err)
	}
	if formalRun.CompletedAt == nil {
		t.Fatal("formal run has no completion time")
	}
	adjudicatedAt := formalRun.CompletedAt.Add(time.Minute)
	oracle := evaluation.NormalizationOracle{
		SchemaVersion: evaluation.NormalizationOracleSchemaVersion,
		OracleID:      "normalization-oracle-test", CorpusSnapshotRef: snapshotOutput.SnapshotRef,
		CaseID: evaluationCase.CaseID, Split: evaluation.SplitTest, LabelRevision: 1,
		SourceReviewRunID: formalRunID, SourceReviewRunRef: formalRunRef,
		RawCandidateCollectionRef: *formalRun.RawCandidateCollectionRef, TargetDigest: raw.TargetDigest,
		EligibleRawCandidateIDs: eligible,
		EquivalenceClasses: []evaluation.NormalizationEquivalenceClass{{
			ClassID: "independent-defect-1", RawCandidateIDs: slices.Clone(eligible),
		}},
		Adjudication: evaluation.NormalizationOracleAdjudication{
			Authority: "external-review-board", Revision: "board-v1",
			ReviewerIDs: []string{"reviewer-a", "reviewer-b"}, AdjudicatorID: "adjudicator-c",
			EvidenceRefs: []runmodel.ArtifactRef{evidenceRef}, ObservedPolicyRevisions: []string{},
			AdjudicatedAt: adjudicatedAt,
		},
		CreatedAt: adjudicatedAt.Add(time.Minute),
	}
	var oracleBytes bytes.Buffer
	if err := runEvaluationNormalization(t.Context(), []string{
		"oracle", "seal", "--store", storePath,
		"--input", writeBatchJSON(t, "normalization-oracle.json", oracle),
		"--access", accessPath, "--json",
	}, &oracleBytes); err != nil {
		t.Fatalf("seal normalization oracle: %v", err)
	}
	var oracleOutput normalizationOracleOutput
	decodeCLIOutput(t, oracleBytes.Bytes(), &oracleOutput)
	oracleKeySeed := sha256.Sum256([]byte("argus-cli-normalization-oracle-key"))
	oraclePrivateKey := ed25519.NewKeyFromSeed(oracleKeySeed[:])
	oracleKey := evaluation.TrustedGovernanceKey{
		SchemaVersion: evaluation.TrustedGovernanceKeySchemaVersion,
		Authority:     oracle.Adjudication.Authority, KeyID: "normalization-oracle-key", Revision: "key-v1",
		PublicKeyBase64:        base64.StdEncoding.EncodeToString(oraclePrivateKey.Public().(ed25519.PublicKey)),
		RepositoryIDs:          []string{evaluationCase.Provenance.RepositoryID},
		AllowedClassifications: []evaluation.Classification{evaluationCase.Classification},
		ValidFrom:              oracle.CreatedAt.Add(-time.Minute), ValidUntil: oracle.CreatedAt.Add(time.Hour),
	}
	oracleKeyRegisteredAt := oracle.CreatedAt
	if _, err := repository.RegisterGovernanceTrustKey(context.Background(), evaluation.GovernanceTrustKeyRegistration{
		SchemaVersion: evaluation.GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           oracleKey, RegisteredAt: oracleKeyRegisteredAt,
	}, evaluation.Mutation{
		IdempotencyKey: "normalization-register-oracle-key", Actor: "normalization-oracle-trust-admin",
		Roles: []evaluation.Role{evaluation.RoleGovernanceTrustAdmin}, Audit: "register external oracle signing key", At: oracleKeyRegisteredAt,
	}); err != nil {
		t.Fatal(err)
	}
	attestation := evaluation.NormalizationOracleAttestation{
		SchemaVersion: evaluation.NormalizationOracleAttestationSchemaVersion,
		AttestationID: "normalization-oracle-attestation-test", OracleID: oracle.OracleID,
		OracleSHA256: oracleOutput.OracleRef.SHA256, Authority: oracleKey.Authority,
		KeyID: oracleKey.KeyID, KeyRevision: oracleKey.Revision,
		PolicyRevision: oracle.Adjudication.Revision, Decision: "approved",
		IssuedAt: oracle.CreatedAt.Add(time.Minute),
	}
	signingBytes, err := attestation.SigningBytes()
	if err != nil {
		t.Fatal(err)
	}
	attestation.SignatureBase64 = base64.StdEncoding.EncodeToString(ed25519.Sign(oraclePrivateKey, signingBytes))
	oracleRegistration := evaluation.NormalizationOracleRegistration{
		SchemaVersion: evaluation.NormalizationOracleRegistrationSchemaVersion,
		Oracle:        oracle, OracleRef: oracleOutput.OracleRef, Attestation: attestation, TrustedKey: oracleKey,
		ExpectedCurrentRevision: 0, ExpectedCurrentRegisteredEventID: "",
		RegisteredAt: attestation.IssuedAt.Add(time.Minute),
	}
	oracleMutation := evaluation.Mutation{
		IdempotencyKey: "normalization-register-oracle", Actor: access.Actor, Roles: roles,
		Audit: "register signed normalization oracle", At: oracleRegistration.RegisteredAt,
	}
	var registrationBytes bytes.Buffer
	if err := runEvaluationNormalization(t.Context(), []string{
		"oracle", "register", "--store", storePath,
		"--input", writeBatchJSON(t, "normalization-oracle-registration.json", oracleRegistration),
		"--mutation", writeBatchJSON(t, "normalization-oracle-registration-mutation.json", oracleMutation), "--json",
	}, &registrationBytes); err != nil {
		t.Fatalf("register normalization oracle: %v", err)
	}
	var registrationOutput normalizationOracleRecordOutput
	decodeCLIOutput(t, registrationBytes.Bytes(), &registrationOutput)
	qualityRequest := evaluation.NormalizationQualityRunRequest{
		SchemaVersion: evaluation.NormalizationQualityRunRequestSchemaVersion,
		QualityRunID:  "normalization-quality-test", PolicyRevision: pireviewmap.NormalizationPreviewPolicyRevision,
		OracleBindings: []evaluation.NormalizationOracleBinding{registrationOutput.Record.Binding},
		CreatedAt:      oracleRegistration.RegisteredAt.Add(time.Minute),
	}
	var qualityBytes bytes.Buffer
	if err := runEvaluationNormalization(t.Context(), []string{
		"run", "--store", storePath,
		"--input", writeBatchJSON(t, "normalization-quality-request.json", qualityRequest),
		"--access", accessPath, "--json",
	}, &qualityBytes); err != nil {
		t.Fatalf("run normalization quality: %v", err)
	}
	var qualityOutput normalizationQualityOutput
	decodeCLIOutput(t, qualityBytes.Bytes(), &qualityOutput)
	if qualityOutput.Run.Summary.Cases != 1 || qualityOutput.Run.Summary.ExactPartitionMatches != 1 ||
		qualityOutput.Run.Summary.PolicyExposedCases != 0 || qualityOutput.Run.Summary.IndependentEvidenceCases != 1 {
		t.Fatalf("normalization quality output = %+v", qualityOutput.Run)
	}
	refPath := writeBatchJSON(t, "normalization-quality-ref.json", qualityOutput.RunRef)
	var shown bytes.Buffer
	if err := runEvaluationNormalization(t.Context(), []string{
		"show", "--store", storePath, "--ref", refPath, "--access", accessPath, "--json",
	}, &shown); err != nil {
		t.Fatalf("show normalization quality: %v", err)
	}
	var shownOutput normalizationQualityOutput
	decodeCLIOutput(t, shown.Bytes(), &shownOutput)
	if shownOutput.RunRef != qualityOutput.RunRef || shownOutput.Run.QualityRunID != qualityOutput.Run.QualityRunID {
		t.Fatalf("shown normalization quality differs: %+v", shownOutput)
	}
	promotionAt := qualityRequest.CreatedAt.Add(time.Minute)
	thresholds := normalizationpromotion.Thresholds{
		MinimumCases: 1, MinimumEligibleCandidates: 1,
		MinimumOracleDuplicatePairs: 1, MinimumOracleDistinctPairs: 1,
		MinimumPairwisePrecisionPPM: 900_000, MinimumPairwiseRecallPPM: 900_000,
		MaximumFalseMergeRatePPM: 100_000, MinimumExactPartitionPPM: 900_000,
	}
	promotionRequest := normalizationpromotion.PrepareRequest{
		SchemaVersion:            normalizationpromotion.PrepareRequestSchemaVersion,
		PromotionVariantID:       "normalization-promotion-test",
		BaselineReviewRunID:      formalRunID,
		BaselineConfigRevisionID: normalizationOwner.ID,
		BaselineConfigRevision:   normalizationOwner.Revision,
		VariantConfigRevisionID:  normalizationOwner.ID,
		VariantConfigRevision:    normalizationOwner.Revision + "-normalization-v2",
		VariantPolicyRevision:    pireviewmap.NormalizationPreviewPolicyRevision,
		RollbackPolicyRevision:   "argus-pi-review-workflow-v1",
		VariantImplementation: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationRevisionV2,
			SHA256:   formalBundle.AgentReview.Agent.SHA256,
		},
		RollbackImplementation: contractsv1alpha1.VersionedRef{
			ID:       contractsv1alpha1.CandidateNormalizationImplementationID,
			Revision: contractsv1alpha1.CandidateNormalizationRevisionV1,
			SHA256:   formalBundle.AgentReview.Agent.SHA256,
		},
		PromotionPolicyRevision: "normalization-promotion-policy-v1",
		Owner:                   "normalization-owner",
		Policy: normalizationpromotion.Policy{
			SchemaVersion: normalizationpromotion.PolicySchemaVersion,
			PolicyID:      "normalization-quality-gate", Revision: "1",
			Test: thresholds, Holdout: thresholds, CreatedAt: promotionAt,
			MinimumShadowRuns: 1, MinimumCanaryRuns: 1, CanaryPercentage: 10,
		},
		CreatedAt: promotionAt,
	}
	promotionRoles := []evaluation.Role{evaluation.RoleDatasetCurator, evaluation.RolePromotionOperator}
	var preparationBytes bytes.Buffer
	if err := runEvaluationNormalization(t.Context(), []string{
		"promotion", "prepare", "--store", storePath, "--config-state-dir", configState,
		"--input", writeBatchJSON(t, "normalization-promotion-prepare.json", promotionRequest),
		"--mutation", writeBatchJSON(t, "normalization-promotion-prepare-mutation.json", evaluation.Mutation{
			IdempotencyKey: "normalization-promotion-prepare", Actor: "normalization-operator",
			Roles: promotionRoles, Audit: "prepare normalization promotion test gate", At: promotionAt,
		}), "--json",
	}, &preparationBytes); err != nil {
		t.Fatalf("prepare normalization promotion: %v", err)
	}
	var preparationOutput normalizationPromotionPreparationOutput
	decodeCLIOutput(t, preparationBytes.Bytes(), &preparationOutput)
	if preparationOutput.Preparation.Promotion.NextGate == nil ||
		*preparationOutput.Preparation.Promotion.NextGate != evaluation.GateTargetedRegression {
		t.Fatalf("prepared promotion = %+v", preparationOutput.Preparation.Promotion)
	}
	gateAt := promotionAt.Add(time.Minute)
	gateRequest := normalizationpromotion.GateRequest{
		SchemaVersion: normalizationpromotion.GateRequestSchemaVersion,
		VariantID:     promotionRequest.PromotionVariantID, Gate: evaluation.GateTargetedRegression,
		PolicyRef: preparationOutput.Preparation.PolicyRef, QualityRunRef: qualityOutput.RunRef,
		EvaluatedAt: gateAt,
	}
	var gateBytes bytes.Buffer
	if err := runEvaluationNormalization(t.Context(), []string{
		"promotion", "gate", "--store", storePath,
		"--input", writeBatchJSON(t, "normalization-promotion-gate.json", gateRequest),
		"--mutation", writeBatchJSON(t, "normalization-promotion-gate-mutation.json", evaluation.Mutation{
			IdempotencyKey: "normalization-promotion-test-gate", Actor: "normalization-operator",
			Roles: promotionRoles, Audit: "recompute exact normalization test quality", At: gateAt,
		}), "--json",
	}, &gateBytes); err != nil {
		t.Fatalf("record normalization promotion gate: %v", err)
	}
	var gateOutput normalizationPromotionGateOutput
	decodeCLIOutput(t, gateBytes.Bytes(), &gateOutput)
	if gateOutput.Gate.Decision.Outcome != evaluation.GateInconclusive ||
		!slices.Contains(gateOutput.Gate.Decision.ReasonCodes, "insufficient_sample") {
		t.Fatalf("gate decision = %+v, want fail-closed insufficient sample", gateOutput.Gate.Decision)
	}
	revokedAt := qualityRequest.CreatedAt.Add(time.Minute)
	revocation := evaluation.NormalizationOracleRevocation{
		SchemaVersion: evaluation.NormalizationOracleRevocationSchemaVersion,
		OracleID:      oracle.OracleID, ExpectedRevision: registrationOutput.Record.Binding.Revision,
		ExpectedRegisteredEventID: registrationOutput.Record.Binding.RegisteredEventID,
		Reason:                    "replace independently adjudicated truth", RevokedAt: revokedAt,
	}
	if err := runEvaluationNormalization(t.Context(), []string{
		"oracle", "revoke", "--store", storePath,
		"--input", writeBatchJSON(t, "normalization-oracle-revocation.json", revocation),
		"--mutation", writeBatchJSON(t, "normalization-oracle-revocation-mutation.json", evaluation.Mutation{
			IdempotencyKey: "normalization-revoke-oracle", Actor: "normalization-revocation-curator",
			Roles: roles, Audit: "revoke stale normalization oracle", At: revokedAt,
		}), "--json",
	}, &bytes.Buffer{}); err != nil {
		t.Fatalf("revoke normalization oracle: %v", err)
	}
	if err := runEvaluationNormalization(t.Context(), []string{
		"show", "--store", storePath, "--ref", refPath, "--access", accessPath, "--json",
	}, &bytes.Buffer{}); !errors.Is(err, evaluation.ErrUnauthorized) {
		t.Fatalf("revoked oracle quality show error = %v, want ErrUnauthorized", err)
	}
}
