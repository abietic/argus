// Package formalevidence governs user disclosure and revocation of exact task
// evidence attached to a committed formal ReviewRun. It resolves authority
// only from runrepo's whole-closure terminal facts; callers cannot provide an
// artifact ref or tenant/workspace scope.
package formalevidence

import (
	"context"
	"fmt"
	"sort"
	"time"

	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

type Service struct {
	runs      *runrepo.Repository
	artifacts *artifactrepo.Repository
}

type Summary struct {
	GovernedRef        contractsv1alpha1.ArtifactBinding  `json:"governed_ref"`
	LocalRef           runmodel.ArtifactRef               `json:"local_ref"`
	ReceiptGovernedRef *contractsv1alpha1.ArtifactBinding `json:"receipt_governed_ref,omitempty"`
	ReceiptLocalRef    *runmodel.ArtifactRef              `json:"receipt_local_ref,omitempty"`
	ContentPolicy      string                             `json:"content_policy"`
	Scope              string                             `json:"scope"`
	Authority          string                             `json:"authority"`
	Provenance         string                             `json:"provenance_class"`
	Disposition        string                             `json:"disposition"`
	State              string                             `json:"state"`
	ReceiptState       string                             `json:"receipt_state,omitempty"`
	ExportPolicy       string                             `json:"export_policy"`
	RetentionPolicy    string                             `json:"retention_policy"`
	DeletionSemantics  string                             `json:"deletion_semantics"`
	Diagnostics        *DiagnosticSummary                 `json:"diagnostics,omitempty"`
}

type DiagnosticSummary struct {
	Completeness       string               `json:"completeness"`
	ReasonCodes        []string             `json:"reason_codes"`
	TaskCount          int                  `json:"task_count"`
	ContextTasks       int                  `json:"context_tasks"`
	ReviewTasks        int                  `json:"review_tasks"`
	VerificationTasks  int                  `json:"verification_tasks"`
	TasksSucceeded     int                  `json:"tasks_succeeded"`
	TasksFailed        int                  `json:"tasks_failed"`
	TasksCanceled      int                  `json:"tasks_canceled"`
	FailureReasonCodes []FailureReasonCount `json:"failure_reason_codes"`
}

type FailureReasonCount struct {
	Code  string `json:"code"`
	Count int    `json:"count"`
}

type ReadRequest struct {
	FormalRunID string
	Access      artifactrepo.SensitiveAccessRequest
}

type ReadResult struct {
	FormalRunID string                                              `json:"formal_run_id"`
	Evidence    contractsv1alpha1.AgentReviewTaskEvidenceCollection `json:"evidence"`
	Receipts    contractsv1alpha1.AgentExecutionReceiptCollection   `json:"receipts"`
	Proof       artifactrepo.SensitiveAccessProof                   `json:"access_proof"`
}

type RevokeRequest struct {
	FormalRunID    string
	IdempotencyKey string
	Actor          string
	Reason         string
	At             time.Time
}

func New(runs *runrepo.Repository, artifacts *artifactrepo.Repository) (*Service, error) {
	if runs == nil || artifacts == nil {
		return nil, fmt.Errorf("formal run and governed artifact repositories are required")
	}
	return &Service{runs: runs, artifacts: artifacts}, nil
}

func (service *Service) Describe(
	ctx context.Context,
	formalRunID string,
) (Summary, bool, error) {
	if err := contextError(ctx); err != nil {
		return Summary{}, false, err
	}
	binding, subject, found, err := service.runs.FormalAgentTaskEvidenceBinding(formalRunID)
	if err != nil || !found {
		return Summary{}, found, err
	}
	receiptBinding, receiptSubject, receiptFound, err :=
		service.runs.FormalAgentExecutionReceiptBinding(formalRunID)
	if err != nil {
		return Summary{}, false, err
	}
	if !receiptFound || receiptSubject != subject {
		return Summary{}, false, fmt.Errorf("formal task evidence has no exact receipt pair")
	}
	record, err := service.artifacts.Inspect(
		ctx,
		processingSubject(subject),
		ref(binding),
	)
	if err != nil {
		return Summary{}, false, fmt.Errorf("inspect formal task evidence: %w", err)
	}
	receiptRecord, err := service.artifacts.Inspect(
		ctx,
		processingSubject(subject),
		ref(receiptBinding),
	)
	if err != nil {
		return Summary{}, false, fmt.Errorf("inspect formal execution receipts: %w", err)
	}
	run, err := service.runs.LoadRun(formalRunID)
	if err != nil {
		return Summary{}, false, err
	}
	result := summary(binding, receiptBinding, record.State, receiptRecord.State, run.Status)
	if record.State == artifactrepo.StateActive && receiptRecord.State == artifactrepo.StateActive {
		diagnostics, diagnosticsErr := service.describeDiagnostics(
			ctx, subject, binding, receiptBinding,
		)
		if diagnosticsErr != nil {
			return Summary{}, false, diagnosticsErr
		}
		result.Diagnostics = &diagnostics
	}
	return result, true, nil
}

func (service *Service) Read(
	ctx context.Context,
	request ReadRequest,
) (ReadResult, error) {
	if err := contextError(ctx); err != nil {
		return ReadResult{}, err
	}
	if err := request.Access.Validate(); err != nil {
		return ReadResult{}, err
	}
	binding, subject, found, err := service.runs.FormalAgentTaskEvidenceBinding(request.FormalRunID)
	if err != nil {
		return ReadResult{}, err
	}
	if !found {
		return ReadResult{}, fmt.Errorf("formal ReviewRun has no agent task evidence")
	}
	data, _, proof, err := service.artifacts.ResolveSensitive(
		ctx,
		disclosureSubject(subject),
		ref(binding),
		request.Access,
	)
	if err != nil {
		return ReadResult{}, fmt.Errorf("disclose formal task evidence: %w", err)
	}
	evidence, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(data)
	if err != nil {
		return ReadResult{}, fmt.Errorf("strictly decode formal task evidence: %w", err)
	}
	if evidence.SourceRunID != request.FormalRunID ||
		evidence.ReviewRunID != request.FormalRunID ||
		evidence.ExecutionID == "" || evidence.PlanID == "" {
		return ReadResult{}, fmt.Errorf("formal task evidence escaped its committed run identity")
	}
	receiptBinding, receiptSubject, found, err :=
		service.runs.FormalAgentExecutionReceiptBinding(request.FormalRunID)
	if err != nil {
		return ReadResult{}, err
	}
	if !found || receiptSubject != subject {
		return ReadResult{}, fmt.Errorf("formal task evidence has no exact receipt pair")
	}
	receiptData, _, err := service.artifacts.Resolve(
		ctx,
		processingSubject(subject),
		ref(receiptBinding),
		artifactrepo.UseRead,
	)
	if err != nil {
		return ReadResult{}, fmt.Errorf("resolve formal execution receipts: %w", err)
	}
	receipts, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptData)
	if err != nil {
		return ReadResult{}, fmt.Errorf("strictly decode formal execution receipts: %w", err)
	}
	if err := validateDiagnosticPair(evidence, receipts); err != nil {
		return ReadResult{}, err
	}
	return ReadResult{
		FormalRunID: request.FormalRunID, Evidence: evidence,
		Receipts: receipts, Proof: proof,
	}, nil
}

func (service *Service) Revoke(
	ctx context.Context,
	request RevokeRequest,
) (Summary, error) {
	if err := contextError(ctx); err != nil {
		return Summary{}, err
	}
	binding, subject, found, err := service.runs.FormalAgentTaskEvidenceBinding(request.FormalRunID)
	if err != nil {
		return Summary{}, err
	}
	if !found {
		return Summary{}, fmt.Errorf("formal ReviewRun has no agent task evidence")
	}
	record, err := service.artifacts.Tombstone(
		ctx,
		operatorSubject(subject),
		ref(binding),
		request.Reason,
		artifactrepo.Mutation{
			IdempotencyKey: request.IdempotencyKey,
			Actor:          request.Actor,
			Audit:          "revoke formal exact local task evidence",
			At:             request.At,
		},
	)
	if err != nil {
		return Summary{}, fmt.Errorf("revoke formal task evidence: %w", err)
	}
	receiptBinding, _, receiptFound, err :=
		service.runs.FormalAgentExecutionReceiptBinding(request.FormalRunID)
	if err != nil {
		return Summary{}, err
	}
	if !receiptFound {
		return Summary{}, fmt.Errorf("formal task evidence has no exact receipt pair")
	}
	receiptRecord, err := service.artifacts.Inspect(
		ctx, processingSubject(subject), ref(receiptBinding),
	)
	if err != nil {
		return Summary{}, fmt.Errorf("inspect formal execution receipts: %w", err)
	}
	run, err := service.runs.LoadRun(request.FormalRunID)
	if err != nil {
		return Summary{}, err
	}
	return summary(binding, receiptBinding, record.State, receiptRecord.State, run.Status), nil
}

func summary(
	binding contractsv1alpha1.ArtifactBinding,
	receiptBinding contractsv1alpha1.ArtifactBinding,
	state artifactrepo.State,
	receiptState artifactrepo.State,
	runStatus runmodel.RunStatus,
) Summary {
	receiptLocal := localRef(receiptBinding)
	scope := "review_run_supporting_evidence"
	if runStatus != runmodel.RunStatusSucceeded {
		scope = "terminal_failure_diagnostics"
	}
	return Summary{
		GovernedRef:        binding,
		LocalRef:           localRef(binding),
		ReceiptGovernedRef: &receiptBinding,
		ReceiptLocalRef:    &receiptLocal,
		ContentPolicy:      contractsv1alpha1.AgentReviewTaskEvidenceContentPolicy,
		Scope:              scope,
		Authority:          contractsv1alpha1.AgentReviewTaskEvidenceAuthority,
		Provenance:         contractsv1alpha1.AgentReviewTaskEvidenceProvenance,
		Disposition:        contractsv1alpha1.AgentReviewTaskEvidenceDisposition,
		State:              string(state), ReceiptState: string(receiptState), ExportPolicy: "deny",
		RetentionPolicy:   "until_explicit_revocation",
		DeletionSemantics: "logical_tombstone_shared_content_gc_deferred",
	}
}

func localRef(binding contractsv1alpha1.ArtifactBinding) runmodel.ArtifactRef {
	return runmodel.ArtifactRef{
		URI:    "artifact://local/sha256/" + binding.Ref.SHA256,
		SHA256: binding.Ref.SHA256, SizeBytes: binding.Ref.SizeBytes,
		Contract: binding.Contract,
	}
}

func (service *Service) describeDiagnostics(
	ctx context.Context,
	subject runmodel.AgentPlanningSubject,
	taskBinding contractsv1alpha1.ArtifactBinding,
	receiptBinding contractsv1alpha1.ArtifactBinding,
) (DiagnosticSummary, error) {
	taskData, _, err := service.artifacts.Resolve(
		ctx, processingSubject(subject), ref(taskBinding), artifactrepo.UseSensitiveProcess,
	)
	if err != nil {
		return DiagnosticSummary{}, fmt.Errorf("process formal task evidence summary: %w", err)
	}
	receiptData, _, err := service.artifacts.Resolve(
		ctx, processingSubject(subject), ref(receiptBinding), artifactrepo.UseRead,
	)
	if err != nil {
		return DiagnosticSummary{}, fmt.Errorf("process formal receipt summary: %w", err)
	}
	evidence, err := contractsv1alpha1.DecodeAgentReviewTaskEvidenceCollection(taskData)
	if err != nil {
		return DiagnosticSummary{}, fmt.Errorf("decode formal task evidence summary: %w", err)
	}
	receipts, err := contractsv1alpha1.DecodeAgentExecutionReceiptCollection(receiptData)
	if err != nil {
		return DiagnosticSummary{}, fmt.Errorf("decode formal receipt summary: %w", err)
	}
	if err := validateDiagnosticPair(evidence, receipts); err != nil {
		return DiagnosticSummary{}, err
	}
	result := DiagnosticSummary{
		Completeness: evidence.Completeness,
		ReasonCodes:  append([]string(nil), evidence.ReasonCodes...),
		TaskCount:    len(receipts.Receipts),
	}
	failureCounts := make(map[string]int)
	for _, receipt := range receipts.Receipts {
		switch receipt.TaskRole {
		case contractsv1alpha1.AgentTaskContext:
			result.ContextTasks++
		case contractsv1alpha1.AgentTaskReview:
			result.ReviewTasks++
		case contractsv1alpha1.AgentTaskVerification:
			result.VerificationTasks++
		}
		switch receipt.Status {
		case contractsv1alpha1.AgentTaskSucceeded:
			result.TasksSucceeded++
		case contractsv1alpha1.AgentTaskFailed:
			result.TasksFailed++
		case contractsv1alpha1.AgentTaskCanceled:
			result.TasksCanceled++
		}
		if receipt.FailureReasonCode != nil {
			failureCounts[*receipt.FailureReasonCode]++
		}
	}
	codes := make([]string, 0, len(failureCounts))
	for code := range failureCounts {
		codes = append(codes, code)
	}
	sort.Strings(codes)
	for _, code := range codes {
		result.FailureReasonCodes = append(result.FailureReasonCodes, FailureReasonCount{
			Code: code, Count: failureCounts[code],
		})
	}
	if result.FailureReasonCodes == nil {
		result.FailureReasonCodes = []FailureReasonCount{}
	}
	return result, nil
}

func validateDiagnosticPair(
	evidence contractsv1alpha1.AgentReviewTaskEvidenceCollection,
	receipts contractsv1alpha1.AgentExecutionReceiptCollection,
) error {
	if evidence.PlanID != receipts.PlanID || evidence.SourceRunID != receipts.SourceRunID ||
		evidence.ExecutionID != receipts.ExecutionID || evidence.ReviewRunID != receipts.ReviewRunID {
		return fmt.Errorf("formal task evidence and receipts escaped their exact execution lineage")
	}
	byTask := make(map[string]contractsv1alpha1.AgentExecutionReceipt, len(receipts.Receipts))
	for _, receipt := range receipts.Receipts {
		byTask[receipt.TaskID] = receipt
	}
	for _, task := range evidence.TaskExecutions {
		receipt, found := byTask[task.TaskID]
		if !found || receipt.TaskRole != task.TaskRole || receipt.GroupID != task.GroupID ||
			receipt.Status != task.TerminalStatus {
			return fmt.Errorf("formal task evidence does not bind receipt %q", task.TaskID)
		}
	}
	return nil
}

func ref(binding contractsv1alpha1.ArtifactBinding) artifactrepo.Ref {
	return artifactrepo.Ref{
		URI: binding.Ref.URI, SHA256: binding.Ref.SHA256,
		SizeBytes: binding.Ref.SizeBytes, Contract: binding.Contract,
	}
}

func processingSubject(subject runmodel.AgentPlanningSubject) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID,
		Roles: []string{artifactrepo.RoleSensitiveProcessor},
	}
}

func disclosureSubject(subject runmodel.AgentPlanningSubject) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID,
		Roles: []string{artifactrepo.RoleSensitiveReader},
	}
}

func operatorSubject(subject runmodel.AgentPlanningSubject) artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID,
		Roles: []string{artifactrepo.RoleIntegrityOperator},
	}
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
