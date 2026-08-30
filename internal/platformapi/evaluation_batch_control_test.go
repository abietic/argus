package platformapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/workflow"
)

type evaluationBatchControllerStub struct {
	experimentID           string
	repeatabilityID        string
	experimentMutation     evaluation.Mutation
	repeatMutation         evaluation.Mutation
	submittedExperiment    evaluation.ExperimentBatchRequest
	submittedRepeatability evaluation.RepeatabilityBatchRequest
	budgetTimeoutMS        int64
	submittedVariant       ExperimentBatchExecutionVariant
}

func (stub *evaluationBatchControllerStub) SubmitExperiment(
	_ context.Context,
	request evaluation.ExperimentBatchRequest,
	variant ExperimentBatchExecutionVariant,
	mutation evaluation.Mutation,
) (evaluation.ExperimentBatchRecord, error) {
	stub.submittedExperiment = request
	stub.submittedVariant = variant
	stub.budgetTimeoutMS = variant.BudgetTimeoutMS
	stub.experimentMutation = mutation
	return evaluation.ExperimentBatchRecord{Request: request, Intent: mutation, Status: evaluation.ExperimentBatchRunning}, nil
}

func (stub *evaluationBatchControllerStub) SubmitRepeatability(
	_ context.Context,
	request evaluation.RepeatabilityBatchRequest,
	mutation evaluation.Mutation,
) (evaluation.RepeatabilityBatchRecord, error) {
	stub.submittedRepeatability = request
	stub.repeatMutation = mutation
	return evaluation.RepeatabilityBatchRecord{Request: request, Intent: mutation, Status: evaluation.RepeatabilityBatchRunning}, nil
}

func (stub *evaluationBatchControllerStub) ResumeExperiment(
	_ context.Context,
	id string,
	mutation evaluation.Mutation,
) (evaluation.ExperimentBatchRecord, error) {
	stub.experimentID = id
	stub.experimentMutation = mutation
	return evaluation.ExperimentBatchRecord{
		Request: evaluation.ExperimentBatchRequest{BatchID: id},
		Status:  evaluation.ExperimentBatchRunning,
	}, nil
}

func (stub *evaluationBatchControllerStub) ResumeRepeatability(
	_ context.Context,
	id string,
	mutation evaluation.Mutation,
) (evaluation.RepeatabilityBatchRecord, error) {
	stub.repeatabilityID = id
	stub.repeatMutation = mutation
	return evaluation.RepeatabilityBatchRecord{
		Request: evaluation.RepeatabilityBatchRequest{BatchID: id},
		Status:  evaluation.RepeatabilityBatchRunning,
	}, nil
}

func platformBatchObservations() []evaluation.ExposureObservation {
	return []evaluation.ExposureObservation{
		{Component: evaluation.ExposurePrompt, Status: evaluation.ExposureNotSeen, Revision: "prompt-v1"},
		{Component: evaluation.ExposureRule, Status: evaluation.ExposureNotSeen, Revision: "rule-v1"},
		{Component: evaluation.ExposureModel, Status: evaluation.ExposureNotSeen, Revision: "model-v1"},
		{Component: evaluation.ExposureIndex, Status: evaluation.ExposureNotSeen, Revision: "index-v1"},
	}
}

func platformExperimentSubmitCommand(at time.Time) ExperimentBatchSubmitCommand {
	timeoutMS := int64(45000)
	return ExperimentBatchSubmitCommand{
		SchemaVersion: ExperimentBatchSubmitCommandSchemaVersion,
		Request: evaluation.ExperimentBatchRequest{
			SchemaVersion: evaluation.ExperimentBatchRequestSchemaVersion,
			BatchID:       "experiment-submit-1", BaselineEvaluationRunID: "evaluation-baseline-1",
			VariantEvaluationRunID: "evaluation-budget-1", ExperimentRunID: "experiment-budget-1",
			ExperimentRevision: "budget-v1", Variable: runmodel.ReplayVariableBudget,
			ExecutorRevision: "argus-local-formal-pi-replay-1", MaxConcurrency: 2,
			Cases: []evaluation.ExperimentBatchCase{{
				CaseID: "case-1", ExpectedLabelRevision: 1, BaselineReviewRunID: "review-baseline-1",
				ExpectedBaselineConfigSHA256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				ExpectedVariantConfigSHA256:  "ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
				ExposureObservations:         platformBatchObservations(),
			}},
			CreatedAt: at,
		},
		BudgetTimeoutMS: &timeoutMS,
		Mutation: MutationInput{
			SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "submit-budget-1",
			Audit: "submit budget experiment through local platform", At: at,
		},
	}
}

func platformRepeatabilitySubmitCommand(at time.Time) RepeatabilityBatchSubmitCommand {
	return RepeatabilityBatchSubmitCommand{
		SchemaVersion: RepeatabilityBatchSubmitCommandSchemaVersion,
		Request: evaluation.RepeatabilityBatchRequest{
			SchemaVersion: evaluation.RepeatabilityBatchRequestSchemaVersion,
			BatchID:       "repeatability-submit-1", BaselineEvaluationRunID: "evaluation-baseline-1",
			ReplayEvaluationRunIDs: []string{"evaluation-replay-1", "evaluation-replay-2"},
			RepeatabilityRunID:     "repeatability-run-1", RepeatabilityRevision: "exact-v1",
			ExecutorRevision: "argus-local-formal-pi-replay-1", MaxConcurrency: 2,
			Cases: []evaluation.RepeatabilityBatchCase{{
				CaseID: "case-1", ExpectedLabelRevision: 1, BaselineReviewRunID: "review-baseline-1",
				ExpectedBaselineConfigSHA256: "eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
				ExposureObservations:         platformBatchObservations(),
			}},
			CreatedAt: at,
		},
		Mutation: MutationInput{
			SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "submit-repeatability-1",
			Audit: "submit exact repeatability batch through local platform", At: at,
		},
	}
}

func TestExperimentBatchSubmitCommandAdmitsOnlySealedFindingGovernance(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	profile, err := reviewconfig.SealCalibrationProfile(
		"platform-confidence", "2", []reviewconfig.CalibrationPoint{
			{RawPPM: 0, ConfidencePPM: 0},
			{RawPPM: 1_000_000, ConfidencePPM: 900_000},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	policy, err := reviewconfig.SealFindingGovernancePolicy(profile, 700_000, 3)
	if err != nil {
		t.Fatal(err)
	}
	command := platformExperimentSubmitCommand(at)
	command.Request.Variable = runmodel.ReplayVariableFilterPolicy
	command.BudgetTimeoutMS = nil
	command.FindingGovernance = &policy
	variant, err := command.executionVariant()
	if err != nil || variant.FindingGovernance == nil ||
		variant.FindingGovernance.SHA256 != policy.SHA256 {
		t.Fatalf("finding_governance variant=%+v error=%v", variant, err)
	}
	variant.FindingGovernance.CalibrationProfile.Points[0].ConfidencePPM = 1
	if command.FindingGovernance.CalibrationProfile.Points[0].ConfidencePPM != 0 {
		t.Fatal("execution variant aliased caller policy points")
	}
	tampered := command
	tampered.FindingGovernance = cloneFindingGovernanceForPlatformTest(policy)
	tampered.FindingGovernance.SHA256 = strings.Repeat("0", 64)
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered finding governance policy was admitted")
	}
	withModel := command
	withModel.Model = "deepseek-chat"
	if err := withModel.Validate(); err == nil {
		t.Fatal("finding governance experiment admitted a model override")
	}
}

func TestExperimentBatchSubmitCommandAdmitsOnlySealedRulePack(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 15, 0, 0, time.UTC)
	pack, err := reviewconfig.SealRulePack("agent-review-rules", "2", []reviewconfig.RuleDefinition{{
		ID: "correctness", Revision: "2", Kind: "agent",
		Detector: reviewconfig.VersionedRef{
			ID: "pi-review", Revision: "1", SHA256: strings.Repeat("a", 64),
		},
		Languages: []string{"go"}, PathPrefixes: []string{},
		EvidenceKinds: []string{"file_content", "target_line"},
		Severity:      "high", Enabled: true,
	}})
	if err != nil {
		t.Fatal(err)
	}
	command := platformExperimentSubmitCommand(at)
	command.Request.Variable = runmodel.ReplayVariableRulePack
	command.BudgetTimeoutMS = nil
	command.RulePack = &pack
	variant, err := command.executionVariant()
	if err != nil || variant.RulePack == nil || variant.RulePack.SHA256 != pack.SHA256 {
		t.Fatalf("rule_pack variant=%+v error=%v", variant, err)
	}
	variant.RulePack.Rules[0].Languages[0] = "typescript"
	if command.RulePack.Rules[0].Languages[0] != "go" {
		t.Fatal("execution variant aliased caller rule slices")
	}
	tampered := command
	tamperedPack := clonePlatformRulePack(pack)
	tamperedPack.SHA256 = strings.Repeat("0", 64)
	tampered.RulePack = &tamperedPack
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered rule pack was admitted")
	}
	withModel := command
	withModel.Model = "deepseek-chat"
	if err := withModel.Validate(); err == nil {
		t.Fatal("rule_pack experiment admitted a model override")
	}
}

func TestExperimentBatchSubmitCommandAdmitsExactWorkflowOnly(t *testing.T) {
	at := time.Date(2026, 8, 27, 12, 15, 0, 0, time.UTC)
	definition := workflow.FormalAgentReviewDefinition()
	definition.Revision = "stage-budget-v2"
	definition.Stages = slices.Clone(definition.Stages)
	definition.Stages[0].Budget.TimeoutMS = 60_000
	command := platformExperimentSubmitCommand(at)
	command.Request.Variable = runmodel.ReplayVariableWorkflow
	command.BudgetTimeoutMS = nil
	command.WorkflowDefinition = &definition
	variant, err := command.executionVariant()
	if err != nil || variant.WorkflowDefinition == nil ||
		variant.WorkflowDefinition.Revision != definition.Revision {
		t.Fatalf("workflow variant=%+v error=%v", variant, err)
	}
	variant.WorkflowDefinition.Stages[0].Budget.TimeoutMS = 1
	if command.WorkflowDefinition.Stages[0].Budget.TimeoutMS != 60_000 {
		t.Fatal("execution variant aliased caller workflow stages")
	}
	withModel := command
	withModel.Model = "deepseek-chat"
	if err := withModel.Validate(); err == nil {
		t.Fatal("workflow experiment admitted a second atomic variable")
	}
}

func TestExperimentBatchSubmitCommandAdmitsOrderedIndexProvidersOnly(t *testing.T) {
	at := time.Date(2026, 8, 26, 12, 30, 0, 0, time.UTC)
	providers := []reviewconfig.ContextProviderDefinition{{
		ID: "go-ast", Revision: "1", Kind: "go_ast",
		Adapter: reviewconfig.VersionedRef{
			ID: "argus-go-ast", Revision: "2",
			SHA256: "f343930b884436b3cef6e35fda496e4780fa2edfc94285299f23db552821e961",
		},
	}}
	command := platformExperimentSubmitCommand(at)
	command.Request.Variable = runmodel.ReplayVariableIndex
	command.BudgetTimeoutMS = nil
	command.ContextProviders = &providers
	variant, err := command.executionVariant()
	if err != nil || !reflect.DeepEqual(variant.ContextProviders, providers) {
		t.Fatalf("index variant=%+v error=%v", variant, err)
	}
	variant.ContextProviders[0].ID = "mutated"
	if providers[0].ID != "go-ast" {
		t.Fatal("execution variant aliased caller context providers")
	}

	empty := command
	emptyProviders := []reviewconfig.ContextProviderDefinition{}
	empty.ContextProviders = &emptyProviders
	if err := empty.Validate(); err == nil {
		t.Fatal("index experiment admitted an empty context provider set")
	}
	duplicate := command
	duplicateProviders := append(slices.Clone(providers), providers[0])
	duplicate.ContextProviders = &duplicateProviders
	if err := duplicate.Validate(); err == nil {
		t.Fatal("index experiment admitted duplicate context provider IDs")
	}
	withModel := command
	withModel.Model = "deepseek-chat"
	if err := withModel.Validate(); err == nil {
		t.Fatal("index experiment admitted a model override")
	}
}

func cloneFindingGovernanceForPlatformTest(
	policy reviewconfig.FindingGovernancePolicy,
) *reviewconfig.FindingGovernancePolicy {
	copy := policy
	copy.CalibrationProfile.Points = append([]reviewconfig.CalibrationPoint{}, policy.CalibrationProfile.Points...)
	return &copy
}

func TestEvaluationBatchSubmitAPIInjectsPrincipalAndReturnsAccepted(t *testing.T) {
	controller := &evaluationBatchControllerStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "evaluation-operator",
		Roles:       []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []Permission{PermissionEvaluationWrite}, ProfileRevision: "operator-v1",
	}
	handler, err := NewHandler(Services{EvaluationBatches: controller}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	experiment := platformExperimentSubmitCommand(at)
	body, _ := json.Marshal(experiment)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted ||
		controller.submittedExperiment.BatchID != experiment.Request.BatchID ||
		experiment.BudgetTimeoutMS == nil || controller.budgetTimeoutMS != *experiment.BudgetTimeoutMS ||
		controller.experimentMutation.Actor != principal.Actor ||
		controller.submittedExperiment.ExecutorTemplateRef != nil {
		t.Fatalf("experiment submit status=%d body=%s controller=%+v",
			response.Code, response.Body.String(), controller)
	}

	repeatability := platformRepeatabilitySubmitCommand(at.Add(time.Minute))
	body, _ = json.Marshal(repeatability)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/repeatability-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted ||
		controller.submittedRepeatability.BatchID != repeatability.Request.BatchID ||
		controller.repeatMutation.Actor != principal.Actor ||
		controller.submittedRepeatability.ExecutorTemplateRef != nil {
		t.Fatalf("repeatability submit status=%d body=%s controller=%+v",
			response.Code, response.Body.String(), controller)
	}

	prompt := platformExperimentSubmitCommand(at.Add(2 * time.Minute))
	prompt.Request.BatchID = "experiment-submit-prompt-1"
	prompt.Request.VariantEvaluationRunID = "evaluation-prompt-1"
	prompt.Request.ExperimentRunID = "experiment-prompt-1"
	prompt.Request.Variable = runmodel.ReplayVariablePrompt
	prompt.BudgetTimeoutMS = nil
	prompt.ComponentRefs = []reviewconfig.VersionedRef{{
		ID: "pi-review-prompts", Revision: "prompt-v2",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	prompt.Mutation.IdempotencyKey = "submit-prompt-1"
	body, _ = json.Marshal(prompt)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted ||
		controller.submittedVariant.Variable != runmodel.ReplayVariablePrompt ||
		len(controller.submittedVariant.ComponentRefs) != 1 ||
		controller.submittedVariant.ComponentRefs[0] != prompt.ComponentRefs[0] ||
		controller.submittedExperiment.ExecutorTemplateRef != nil {
		t.Fatalf("prompt submit status=%d body=%s controller=%+v",
			response.Code, response.Body.String(), controller)
	}

	model := platformExperimentSubmitCommand(at.Add(2 * time.Second))
	model.Request.BatchID = "experiment-model-submit-1"
	model.Request.Variable = runmodel.ReplayVariableModel
	model.BudgetTimeoutMS = nil
	model.Model = "deepseek-reasoner"
	model.Mutation.IdempotencyKey = "submit-model-1"
	body, _ = json.Marshal(model)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted ||
		controller.submittedVariant.Variable != runmodel.ReplayVariableModel ||
		controller.submittedVariant.Model != model.Model ||
		controller.submittedVariant.ComponentRefs != nil {
		t.Fatalf("model submit status=%d body=%s controller=%+v",
			response.Code, response.Body.String(), controller)
	}
}

func TestEvaluationBatchSubmitAPIRejectsReadOnlyPathsAndDriftedIntent(t *testing.T) {
	controller := &evaluationBatchControllerStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "evaluation-reader",
		Roles:       []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions: []Permission{PermissionEvaluationRead}, ProfileRevision: "reader-v1",
	}
	handler, err := NewHandler(Services{EvaluationBatches: controller}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	command := platformExperimentSubmitCommand(time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC))
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only submit status=%d body=%s", response.Code, response.Body.String())
	}

	principal.Permissions = []Permission{PermissionEvaluationWrite}
	handler, err = NewHandler(Services{EvaluationBatches: controller}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	command.Request.CreatedAt = command.Request.CreatedAt.Add(time.Second)
	body, _ = json.Marshal(command)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusBadRequest || controller.submittedExperiment.BatchID != "" {
		t.Fatalf("drifted submit status=%d body=%s controller=%+v", response.Code, response.Body.String(), controller)
	}

	body = bytes.Replace(body, []byte(`"budget_timeout_ms":45000`),
		[]byte(`"budget_timeout_ms":45000,"prompt_bundle":"/tmp/untrusted.json"`), 1)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusBadRequest || controller.submittedExperiment.BatchID != "" {
		t.Fatalf("path injection status=%d body=%s controller=%+v", response.Code, response.Body.String(), controller)
	}

	command = platformExperimentSubmitCommand(time.Date(2026, 8, 26, 12, 5, 0, 0, time.UTC))
	command.ComponentRefs = []reviewconfig.VersionedRef{{
		ID: "pi-review-prompts", Revision: "prompt-v2",
		SHA256: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}}
	body, _ = json.Marshal(command)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/evaluation/experiment-batches", bytes.NewReader(body),
	))
	if response.Code != http.StatusBadRequest || controller.submittedExperiment.BatchID != "" {
		t.Fatalf("mixed budget/component submit status=%d body=%s controller=%+v",
			response.Code, response.Body.String(), controller)
	}
}

func TestEvaluationBatchResumeAPIInjectsPrincipalAndReturnsAccepted(t *testing.T) {
	controller := &evaluationBatchControllerStub{}
	principal := Principal{
		SchemaVersion:   PrincipalSchemaVersion,
		Actor:           "evaluation-operator",
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions:     []Permission{PermissionEvaluationWrite},
		ProfileRevision: "evaluation-operator-v1",
	}
	handler, err := NewHandler(Services{EvaluationBatches: controller}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	command := EvaluationBatchResumeCommand{
		SchemaVersion: EvaluationBatchResumeCommandSchemaVersion,
		Mutation: MutationInput{
			SchemaVersion:  MutationInputSchemaVersion,
			IdempotencyKey: "resume-experiment-batch",
			Audit:          "resume experiment batch from operator API",
			At:             at,
		},
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost,
		"/v1/evaluation/experiment-batch/resume?batch_id=experiment%2Fbatch%3A1",
		bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted || controller.experimentID != "experiment/batch:1" ||
		controller.experimentMutation.Actor != principal.Actor ||
		len(controller.experimentMutation.Roles) != 1 ||
		controller.experimentMutation.Roles[0] != evaluation.RoleDatasetCurator {
		t.Fatalf("experiment resume status=%d body=%s id=%q mutation=%+v",
			response.Code, response.Body.String(), controller.experimentID,
			controller.experimentMutation)
	}

	command.Mutation.IdempotencyKey = "resume-repeatability-batch"
	body, _ = json.Marshal(command)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost,
		"/v1/evaluation/repeatability-batch/resume?batch_id=repeatability-batch-1",
		bytes.NewReader(body),
	))
	if response.Code != http.StatusAccepted || controller.repeatabilityID != "repeatability-batch-1" ||
		controller.repeatMutation.Actor != principal.Actor {
		t.Fatalf("repeatability resume status=%d body=%s id=%q mutation=%+v",
			response.Code, response.Body.String(), controller.repeatabilityID,
			controller.repeatMutation)
	}
}

func TestEvaluationBatchResumeAPIRequiresWritePermissionAndStrictBody(t *testing.T) {
	controller := &evaluationBatchControllerStub{}
	readOnly := Principal{
		SchemaVersion:   PrincipalSchemaVersion,
		Actor:           "evaluation-reader",
		Roles:           []evaluation.Role{evaluation.RoleDatasetCurator},
		Permissions:     []Permission{PermissionEvaluationRead},
		ProfileRevision: "evaluation-reader-v1",
	}
	handler, err := NewHandler(Services{EvaluationBatches: controller}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost,
		"/v1/evaluation/experiment-batch/resume?batch_id=batch-1",
		bytes.NewBufferString(`{"schema_version":"argus.local_api_evaluation_batch_resume_command.v1alpha1","mutation":{"schema_version":"argus.local_api_mutation.v1alpha1","idempotency_key":"resume","audit":"resume batch","at":"2026-08-26T00:00:00Z"}}`),
	))
	if response.Code != http.StatusForbidden {
		t.Fatalf("read-only resume status=%d body=%s", response.Code, response.Body.String())
	}

	writer := readOnly
	writer.Permissions = []Permission{PermissionEvaluationWrite}
	handler, err = NewHandler(Services{EvaluationBatches: controller}, writer, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost,
		"/v1/evaluation/experiment-batch/resume?batch_id=batch-1",
		bytes.NewBufferString(`{"schema_version":"argus.local_api_evaluation_batch_resume_command.v1alpha1","actor":"spoofed","mutation":{"schema_version":"argus.local_api_mutation.v1alpha1","idempotency_key":"resume","audit":"resume batch","at":"2026-08-26T00:00:00Z"}}`),
	))
	if response.Code != http.StatusBadRequest || controller.experimentID != "" {
		t.Fatalf("spoofed resume status=%d body=%s controller=%+v",
			response.Code, response.Body.String(), controller)
	}
}
