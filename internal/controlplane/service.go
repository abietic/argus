// Package controlplane joins Argus' authoritative ledgers at application
// boundaries. It does not merge their facts: FindingDecision, Feedback and
// Outcome remain independently owned and append-only.
package controlplane

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"argus.local/argus/internal/evaluation"
	"argus.local/argus/internal/feedback"
	"argus.local/argus/internal/findingdecision"
	"argus.local/argus/internal/findinglineage"
	"argus.local/argus/internal/publication"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/reviewcore"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/runrepo"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

var (
	ErrFindingNotFound         = errors.New("finding not found")
	ErrNoAuthoritativeFindings = errors.New("run has no authoritative findings")
)

type RunReader interface {
	History(int) ([]runrepo.HistoryEntry, error)
	LoadRun(string) (runmodel.ReviewRun, error)
	LoadExecutionSnapshot(string) (runmodel.ExecutionSnapshot, error)
	ReadArtifact(runmodel.ArtifactRef) ([]byte, error)
	CheckArtifactEligibility(runmodel.ArtifactRef, runrepo.ArtifactUse) error
}

type EvaluationSourceReader interface {
	ResolveMissedDefectIncident(string) (evaluation.MissedDefectIncident, error)
	ResolveEvaluationProbe(string) (evaluation.EvaluationProbe, error)
}

type FeedbackLedger interface {
	AppendFeedback(context.Context, feedback.Feedback) (feedback.Feedback, error)
	AppendOutcome(context.Context, feedback.Outcome) (feedback.Outcome, error)
	ByFinding(string) ([]feedback.Entry, error)
}

type PublicationLedger interface {
	ByFinding(string, string) ([]publication.Record, error)
}

type FindingLineageReader interface {
	List(findinglineage.ListFilter) ([]findinglineage.Record, error)
}

type DecisionLedger interface {
	Record(context.Context, findingdecision.Request, findingdecision.Mutation, findingdecision.Root) (findingdecision.Decision, error)
	List(string, string) ([]findingdecision.Decision, error)
}

type PublicationGrantLedger interface {
	Record(context.Context, publication.GrantRequest, publication.GrantMutation, publication.GrantBinding) (publication.Grant, error)
	Get(string) (publication.Grant, error)
	Reserve(context.Context, string, string, string, string, time.Time) (publication.GrantReservation, error)
}

type Service struct {
	runs              RunReader
	feedback          FeedbackLedger
	publications      PublicationLedger
	decisions         DecisionLedger
	grants            PublicationGrantLedger
	evaluationSources EvaluationSourceReader
	lineages          FindingLineageReader
}

func (service *Service) WithFindingLineages(lineages FindingLineageReader) (*Service, error) {
	if service == nil || lineages == nil {
		return nil, fmt.Errorf("control plane and finding lineage reader are required")
	}
	copy := *service
	copy.lineages = lineages
	return &copy, nil
}

func (service *Service) WithEvaluationSources(sources EvaluationSourceReader) (*Service, error) {
	if service == nil || sources == nil {
		return nil, fmt.Errorf("control plane and evaluation source reader are required")
	}
	copy := *service
	copy.evaluationSources = sources
	return &copy, nil
}

func NewWithPublicationGrants(
	runs RunReader,
	ledger FeedbackLedger,
	publications PublicationLedger,
	decisions DecisionLedger,
	grants PublicationGrantLedger,
) (*Service, error) {
	service, err := New(runs, ledger, publications, decisions)
	if err != nil {
		return nil, err
	}
	if grants == nil {
		return nil, fmt.Errorf("publication grant ledger is required")
	}
	service.grants = grants
	return service, nil
}

func New(
	runs RunReader,
	ledger FeedbackLedger,
	publications PublicationLedger,
	decisionLedgers ...DecisionLedger,
) (*Service, error) {
	if runs == nil || ledger == nil || publications == nil {
		return nil, fmt.Errorf(
			"run reader, feedback ledger, and publication ledger are required",
		)
	}
	if len(decisionLedgers) > 1 {
		return nil, fmt.Errorf("at most one finding decision ledger may be configured")
	}
	var decisions DecisionLedger
	if len(decisionLedgers) == 1 {
		if decisionLedgers[0] == nil {
			return nil, fmt.Errorf("finding decision ledger must not be nil")
		}
		decisions = decisionLedgers[0]
	}
	return &Service{
		runs: runs, feedback: ledger, publications: publications, decisions: decisions,
	}, nil
}

type FindingDetail struct {
	Run               runmodel.ReviewRun                          `json:"run"`
	Finding           *reviewcore.Finding                         `json:"finding,omitempty"`
	Decisions         []reviewcore.FindingDecision                `json:"decisions"`
	GovernedFinding   *contractsv1alpha1.GovernedReviewFinding    `json:"governed_finding,omitempty"`
	GovernedDecisions []contractsv1alpha1.GovernedFindingDecision `json:"governed_decisions"`
	HumanDecisions    []findingdecision.Decision                  `json:"human_decisions"`
	Publications      []publication.Record                        `json:"publications"`
	Feedback          []feedback.Feedback                         `json:"feedback"`
	Outcomes          []feedback.Outcome                          `json:"outcomes"`
}

// RecordDecision resolves the exact immutable Finding source server-side.
// Callers can assert an action and mutation authority, but cannot choose or
// forge the root artifact that the decision chain extends.
func (service *Service) RecordDecision(
	ctx context.Context,
	request findingdecision.Request,
	mutation findingdecision.Mutation,
) (findingdecision.Decision, error) {
	if service.decisions == nil {
		return findingdecision.Decision{}, fmt.Errorf("finding decision ledger is not configured")
	}
	detail, err := service.finding(request.RunID, request.FindingID)
	if err != nil {
		return findingdecision.Decision{}, err
	}
	if detail.Run.Kind == runmodel.RunKindReplay && request.Action == findingdecision.ActionPublish {
		return findingdecision.Decision{}, fmt.Errorf("replay runs cannot authorize remote publication")
	}
	if detail.Run.CompletedAt != nil && request.OccurredAt.Before(*detail.Run.CompletedAt) {
		return findingdecision.Decision{}, fmt.Errorf("finding decision predates run completion")
	}
	root, err := decisionRoot(detail)
	if err != nil {
		return findingdecision.Decision{}, err
	}
	return service.decisions.Record(ctx, request, mutation, root)
}

func (detail FindingDetail) FindingID() string {
	if detail.Finding != nil {
		return detail.Finding.ID
	}
	if detail.GovernedFinding != nil {
		return detail.GovernedFinding.FindingID
	}
	return ""
}

// RecordFeedback first proves that the asserted Finding belongs to the exact
// run. A local UI/API observation does not imply remote publication; a
// code-host observation must additionally bind an actually published provider
// comment from the publication ledger.
func (service *Service) RecordFeedback(
	ctx context.Context,
	fact feedback.Feedback,
) (feedback.Feedback, error) {
	_, err := service.finding(fact.RunID, fact.FindingID)
	if err != nil {
		return feedback.Feedback{}, err
	}
	publications, err := service.publications.ByFinding(fact.RunID, fact.FindingID)
	if err != nil {
		return feedback.Feedback{}, fmt.Errorf("load publication facts: %w", err)
	}
	if err := validateObservationChannel(fact.Source, fact.SourceRefs, publications); err != nil {
		return feedback.Feedback{}, err
	}
	return service.feedback.AppendFeedback(ctx, fact)
}

func (service *Service) RecordOutcome(
	ctx context.Context,
	fact feedback.Outcome,
) (feedback.Outcome, error) {
	_, err := service.finding(fact.RunID, fact.FindingID)
	if err != nil {
		return feedback.Outcome{}, err
	}
	publications, err := service.publications.ByFinding(fact.RunID, fact.FindingID)
	if err != nil {
		return feedback.Outcome{}, fmt.Errorf("load publication facts: %w", err)
	}
	if err := validateObservationChannel(fact.Source, fact.SourceRefs, publications); err != nil {
		return feedback.Outcome{}, err
	}
	return service.feedback.AppendOutcome(ctx, fact)
}

func (service *Service) Finding(
	runID string,
	findingID string,
) (FindingDetail, error) {
	detail, err := service.finding(runID, findingID)
	if err != nil {
		return FindingDetail{}, err
	}
	entries, err := service.feedback.ByFinding(findingID)
	if err != nil {
		return FindingDetail{}, fmt.Errorf("load feedback and outcomes: %w", err)
	}
	publications, err := service.publications.ByFinding(runID, findingID)
	if err != nil {
		return FindingDetail{}, fmt.Errorf("load publication facts: %w", err)
	}
	detail.Publications = publications
	if service.decisions != nil {
		detail.HumanDecisions, err = service.decisions.List(runID, findingID)
		if err != nil {
			return FindingDetail{}, fmt.Errorf("load human decision history: %w", err)
		}
	}
	for _, entry := range entries {
		switch {
		case entry.Feedback != nil:
			if entry.Feedback.RunID != runID {
				return FindingDetail{}, fmt.Errorf(
					"feedback %q binds finding %q to a different run",
					entry.Feedback.FeedbackID, findingID,
				)
			}
			detail.Feedback = append(detail.Feedback, *entry.Feedback)
		case entry.Outcome != nil:
			if entry.Outcome.RunID != runID {
				return FindingDetail{}, fmt.Errorf(
					"outcome %q binds finding %q to a different run",
					entry.Outcome.OutcomeID, findingID,
				)
			}
			detail.Outcomes = append(detail.Outcomes, *entry.Outcome)
		}
	}
	return detail, nil
}

func (service *Service) finding(runID, findingID string) (FindingDetail, error) {
	if runID == "" || findingID == "" {
		return FindingDetail{}, fmt.Errorf("run_id and finding_id are required")
	}
	run, err := service.runs.LoadRun(runID)
	if err != nil {
		return FindingDetail{}, fmt.Errorf("load run %q: %w", runID, err)
	}
	if run.Status != runmodel.RunStatusSucceeded {
		return FindingDetail{}, fmt.Errorf("%w: run %q is %s", ErrNoAuthoritativeFindings, runID, run.Status)
	}
	if run.FindingSetRef != nil {
		return service.legacyFinding(run, findingID)
	}
	if run.GovernedReportRef != nil {
		return service.governedFinding(run, findingID)
	}
	return FindingDetail{}, fmt.Errorf("%w: run %q", ErrNoAuthoritativeFindings, runID)
}

func (service *Service) legacyFinding(
	run runmodel.ReviewRun,
	findingID string,
) (FindingDetail, error) {
	if run.FindingSetRef == nil {
		return FindingDetail{}, fmt.Errorf("run %q has no authoritative finding set", run.RunID)
	}
	data, err := service.runs.ReadArtifact(*run.FindingSetRef)
	if err != nil {
		return FindingDetail{}, fmt.Errorf("read finding set: %w", err)
	}
	set, err := decodeFindingSet(data)
	if err != nil {
		return FindingDetail{}, err
	}
	var finding *reviewcore.Finding
	for index := range set.Findings {
		if set.Findings[index].ID == findingID {
			copy := set.Findings[index]
			finding = &copy
			break
		}
	}
	if finding == nil {
		return FindingDetail{}, fmt.Errorf(
			"%w: finding %q does not belong to run %q", ErrFindingNotFound, findingID, run.RunID,
		)
	}
	decisions := make([]reviewcore.FindingDecision, 0)
	for _, decision := range set.Decisions {
		if decision.FindingID == findingID {
			decisions = append(decisions, decision)
		}
	}
	if len(decisions) == 0 {
		return FindingDetail{}, fmt.Errorf("finding %q has no decision history", findingID)
	}
	findingCopy := *finding
	return FindingDetail{
		Run: run, Finding: &findingCopy, Decisions: decisions,
		GovernedDecisions: []contractsv1alpha1.GovernedFindingDecision{},
		HumanDecisions:    []findingdecision.Decision{},
		Publications:      []publication.Record{},
		Feedback:          []feedback.Feedback{}, Outcomes: []feedback.Outcome{},
	}, nil
}

func (service *Service) governedFinding(
	run runmodel.ReviewRun,
	findingID string,
) (FindingDetail, error) {
	if run.GovernedReportRef == nil {
		return FindingDetail{}, fmt.Errorf("run %q has no governed review report", run.RunID)
	}
	data, err := service.runs.ReadArtifact(*run.GovernedReportRef)
	if err != nil {
		return FindingDetail{}, fmt.Errorf("read governed review report: %w", err)
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(data)
	if err != nil {
		return FindingDetail{}, fmt.Errorf("decode governed review report: %w", err)
	}
	if report.ReviewRunID != run.RunID {
		return FindingDetail{}, fmt.Errorf("governed review report belongs to a different run")
	}
	var finding *contractsv1alpha1.GovernedReviewFinding
	for index := range report.Findings {
		if report.Findings[index].FindingID == findingID {
			copy := report.Findings[index]
			finding = &copy
			break
		}
	}
	if finding == nil {
		return FindingDetail{}, fmt.Errorf(
			"%w: finding %q does not belong to run %q", ErrFindingNotFound, findingID, run.RunID,
		)
	}
	decisions := make([]contractsv1alpha1.GovernedFindingDecision, 0)
	for _, decision := range report.Decisions {
		if decision.FindingID == findingID {
			decisions = append(decisions, decision)
		}
	}
	if len(decisions) == 0 {
		return FindingDetail{}, fmt.Errorf("finding %q has no governed decision history", findingID)
	}
	return FindingDetail{
		Run: run, Finding: nil, Decisions: []reviewcore.FindingDecision{},
		GovernedFinding: finding, GovernedDecisions: decisions,
		HumanDecisions: []findingdecision.Decision{},
		Publications:   []publication.Record{}, Feedback: []feedback.Feedback{},
		Outcomes: []feedback.Outcome{},
	}, nil
}

func decisionRoot(detail FindingDetail) (findingdecision.Root, error) {
	root := findingdecision.Root{
		SchemaVersion: findingdecision.RootSchemaVersion,
		RunID:         detail.Run.RunID,
		FindingID:     detail.FindingID(),
	}
	switch {
	case detail.Finding != nil && detail.Run.FindingSetRef != nil:
		initialID, err := legacyInitialDecisionID(detail.Decisions)
		if err != nil {
			return findingdecision.Root{}, err
		}
		root.InitialDecisionID = initialID
		root.SourceContract = runmodel.ContractFindingSet
		root.SourceSHA256 = detail.Run.FindingSetRef.SHA256
	case detail.GovernedFinding != nil && detail.Run.GovernedReportRef != nil:
		initialID, err := governedInitialDecisionID(detail.GovernedDecisions)
		if err != nil {
			return findingdecision.Root{}, err
		}
		root.InitialDecisionID = initialID
		root.SourceContract = runmodel.ContractGovernedReviewReport
		root.SourceSHA256 = detail.Run.GovernedReportRef.SHA256
	default:
		return findingdecision.Root{}, fmt.Errorf("finding source is ambiguous")
	}
	if err := root.Validate(); err != nil {
		return findingdecision.Root{}, fmt.Errorf("resolve finding decision root: %w", err)
	}
	return root, nil
}

func legacyInitialDecisionID(decisions []reviewcore.FindingDecision) (string, error) {
	var initial string
	for _, decision := range decisions {
		if decision.Sequence != 1 {
			continue
		}
		if initial != "" {
			return "", fmt.Errorf("finding has multiple initial decisions")
		}
		initial = decision.ID
	}
	if initial == "" {
		return "", fmt.Errorf("finding has no initial decision")
	}
	return initial, nil
}

func governedInitialDecisionID(decisions []contractsv1alpha1.GovernedFindingDecision) (string, error) {
	var initial string
	for _, decision := range decisions {
		if decision.Sequence != 1 {
			continue
		}
		if initial != "" {
			return "", fmt.Errorf("finding has multiple governed initial decisions")
		}
		initial = decision.DecisionID
	}
	if initial == "" {
		return "", fmt.Errorf("finding has no governed initial decision")
	}
	return initial, nil
}

type ImpactQuery struct {
	ConfigBundleSHA256 string `json:"config_bundle_sha256,omitempty"`
	WorkflowSHA256     string `json:"workflow_sha256,omitempty"`
	RuleID             string `json:"rule_id,omitempty"`
	AgentProfileID     string `json:"agent_profile_id,omitempty"`
	ModelProfileID     string `json:"model_profile_id,omitempty"`
}

type ImpactedRun struct {
	RunID              string   `json:"run_id"`
	ConfigBundleSHA256 string   `json:"config_bundle_sha256"`
	WorkflowSHA256     string   `json:"workflow_sha256"`
	RuleIDs            []string `json:"rule_ids"`
	FindingIDs         []string `json:"finding_ids"`
}

// AffectedRuns is the reverse provenance lookup used before rollback,
// migration or evaluation-case correction. All non-empty query fields are
// conjunctive and are matched against frozen execution snapshots.
func (service *Service) AffectedRuns(query ImpactQuery) ([]ImpactedRun, error) {
	if query == (ImpactQuery{}) {
		return nil, fmt.Errorf("at least one impact selector is required")
	}
	history, err := service.runs.History(0)
	if err != nil {
		return nil, err
	}
	result := make([]ImpactedRun, 0)
	for _, entry := range history {
		if entry.Status != runmodel.RunStatusSucceeded {
			continue
		}
		run, err := service.runs.LoadRun(entry.RunID)
		if err != nil {
			return nil, fmt.Errorf("load impacted run %q: %w", entry.RunID, err)
		}
		snapshot, err := service.runs.LoadExecutionSnapshot(run.ExecutionSnapshotID)
		if err != nil {
			return nil, fmt.Errorf("load impacted snapshot %q: %w",
				run.ExecutionSnapshotID, err)
		}
		configData, err := service.runs.ReadArtifact(snapshot.ConfigBundleRef)
		if err != nil {
			return nil, fmt.Errorf("read impacted ConfigBundle: %w", err)
		}
		bundle, err := reviewconfig.DecodeBundle(configData)
		if err != nil {
			return nil, fmt.Errorf("decode impacted ConfigBundle: %w", err)
		}
		if !matchesImpact(query, snapshot, bundle) {
			continue
		}
		ruleIDs := make([]string, len(bundle.RulePack.Rules))
		for index, rule := range bundle.RulePack.Rules {
			ruleIDs[index] = rule.ID
		}
		slices.Sort(ruleIDs)
		findingIDs, err := service.findingIDs(run)
		if err != nil {
			return nil, err
		}
		result = append(result, ImpactedRun{
			RunID:              run.RunID,
			ConfigBundleSHA256: bundle.SHA256,
			WorkflowSHA256:     snapshot.Workflow.SHA256,
			RuleIDs:            ruleIDs,
			FindingIDs:         findingIDs,
		})
	}
	return result, nil
}

func matchesImpact(
	query ImpactQuery,
	snapshot runmodel.ExecutionSnapshot,
	bundle reviewconfig.ConfigBundle,
) bool {
	if query.ConfigBundleSHA256 != "" && query.ConfigBundleSHA256 != bundle.SHA256 {
		return false
	}
	if query.WorkflowSHA256 != "" && query.WorkflowSHA256 != snapshot.Workflow.SHA256 {
		return false
	}
	if query.AgentProfileID != "" &&
		query.AgentProfileID != bundle.Execution.AgentProfile.ID {
		return false
	}
	if query.ModelProfileID != "" &&
		query.ModelProfileID != bundle.Execution.ModelProfile.ID {
		return false
	}
	if query.RuleID != "" {
		found := false
		for _, rule := range bundle.RulePack.Rules {
			if rule.ID == query.RuleID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func (service *Service) findingIDs(run runmodel.ReviewRun) ([]string, error) {
	if run.FindingSetRef != nil {
		data, err := service.runs.ReadArtifact(*run.FindingSetRef)
		if err != nil {
			return nil, fmt.Errorf("read impacted finding set: %w", err)
		}
		set, err := decodeFindingSet(data)
		if err != nil {
			return nil, err
		}
		ids := make([]string, len(set.Findings))
		for index, finding := range set.Findings {
			ids[index] = finding.ID
		}
		slices.Sort(ids)
		return ids, nil
	}
	if run.GovernedReportRef == nil {
		return []string{}, nil
	}
	data, err := service.runs.ReadArtifact(*run.GovernedReportRef)
	if err != nil {
		return nil, fmt.Errorf("read impacted governed review report: %w", err)
	}
	report, err := contractsv1alpha1.DecodeGovernedReviewReport(data)
	if err != nil {
		return nil, fmt.Errorf("decode impacted governed review report: %w", err)
	}
	if report.ReviewRunID != run.RunID {
		return nil, fmt.Errorf("impacted governed review report belongs to a different run")
	}
	ids := make([]string, len(report.Findings))
	for index, finding := range report.Findings {
		ids[index] = finding.FindingID
	}
	slices.Sort(ids)
	return ids, nil
}

func validateObservationChannel(
	source feedback.Source,
	refs []feedback.SourceRef,
	records []publication.Record,
) error {
	if source.Kind != feedback.SourceCodeHost {
		return nil
	}
	for _, record := range records {
		if record.State != publication.StatePublished || record.ProviderResult == nil ||
			record.ProviderResult.CommentID == "" {
			continue
		}
		for _, ref := range refs {
			if ref.Kind == feedback.SourceRefComment &&
				ref.Authority == record.Request.Provider &&
				ref.ID == record.ProviderResult.CommentID {
				return nil
			}
		}
	}
	return fmt.Errorf(
		"code-host observation has no matching provider-published comment fact",
	)
}
