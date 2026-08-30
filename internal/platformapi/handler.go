package platformapi

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/abietic/argus/internal/analyticsadapter"
	"github.com/abietic/argus/internal/calibration"
	"github.com/abietic/argus/internal/calibrationpromotion"
	"github.com/abietic/argus/internal/configrepo"
	"github.com/abietic/argus/internal/controlplane"
	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/findingdecision"
	"github.com/abietic/argus/internal/findinglineage"
	"github.com/abietic/argus/internal/normalizationpromotion"
	"github.com/abietic/argus/internal/promotionmonitor"
	"github.com/abietic/argus/internal/reviewjob"
	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/scheduling"
	"github.com/abietic/argus/internal/training"
)

const (
	maximumRequestBodyBytes = int64(4 << 20)
	defaultPageLimit        = 50
	maximumPageLimit        = 200
	reviewRunCursorSchema   = "argus.local_api_review_run_cursor.v1alpha1"
	impactCursorSchema      = "argus.local_api_review_run_impact_cursor.v1alpha1"
)

var errInvalidReviewRunCursor = errors.New("invalid review run cursor")

type reviewRunCursor struct {
	SchemaVersion string `json:"schema_version"`
	Watermark     uint64 `json:"watermark"`
	LastSequence  uint64 `json:"last_sequence"`
	RunID         string `json:"run_id"`
}

type impactCursor struct {
	SchemaVersion string                 `json:"schema_version"`
	Watermark     uint64                 `json:"watermark"`
	LastSequence  uint64                 `json:"last_sequence"`
	RunID         string                 `json:"run_id"`
	Selector      runrepo.ImpactSelector `json:"selector"`
}

type Handler struct {
	evaluation             *evaluation.Repository
	calibration            *calibration.Repository
	training               TrainingService
	trainingExports        TrainingExportService
	trainingJobs           TrainingJobService
	calibrationPromotion   CalibrationPromotionService
	promotionMonitor       PromotionMonitorService
	normalizationPromotion NormalizationPromotionService
	evaluationHistory      EvaluationHistoryReader
	evaluationBatches      EvaluationBatchController
	components             AgentComponentPublisher
	config                 *configrepo.Repository
	dashboard              *analyticsadapter.Adapter
	runs                   *runrepo.Repository
	findings               FindingService
	lineages               FindingLineageService
	reviewJobs             ReviewJobService
	workloads              WorkloadPressureReader
	principal              Principal
	token                  string
}

type FindingService interface {
	Finding(string, string) (controlplane.FindingDetail, error)
	RecordDecision(
		context.Context,
		findingdecision.Request,
		findingdecision.Mutation,
	) (findingdecision.Decision, error)
	RecordFeedback(context.Context, feedback.Feedback) (feedback.Feedback, error)
	RecordOutcome(context.Context, feedback.Outcome) (feedback.Outcome, error)
}

type FindingLineageService interface {
	Build(context.Context, findinglineage.BuildRequest) (findinglineage.Record, error)
	Get(string) (findinglineage.Record, error)
	List(findinglineage.ListFilter) ([]findinglineage.Record, error)
}

type ReviewJobService interface {
	Submit(context.Context, reviewjob.Request, reviewjob.Mutation) (reviewjob.Record, error)
	Get(string) (reviewjob.Record, error)
	List() ([]reviewjob.Record, error)
	Timeline(string) ([]scheduling.WorkloadTimelineEvent, error)
	Cancel(context.Context, string, reviewjob.CancelCommand) (reviewjob.Record, error)
}

// EvaluationBatchController is the narrow asynchronous execution port owned
// by the local adapter. Domain intent/checkpoint/lease state remains in the
// evaluation repository; the HTTP handler never calls a provider directly.
type EvaluationBatchController interface {
	SubmitExperiment(context.Context, evaluation.ExperimentBatchRequest, ExperimentBatchExecutionVariant, evaluation.Mutation) (evaluation.ExperimentBatchRecord, error)
	SubmitRepeatability(context.Context, evaluation.RepeatabilityBatchRequest, evaluation.Mutation) (evaluation.RepeatabilityBatchRecord, error)
	ResumeExperiment(context.Context, string, evaluation.Mutation) (evaluation.ExperimentBatchRecord, error)
	ResumeRepeatability(context.Context, string, evaluation.Mutation) (evaluation.RepeatabilityBatchRecord, error)
}

type CalibrationPromotionService interface {
	Prepare(context.Context, calibrationpromotion.PrepareRequest, evaluation.Mutation) (calibrationpromotion.Plan, error)
	RecordGate(context.Context, calibrationpromotion.GateRequest, evaluation.Mutation) (calibrationpromotion.Plan, error)
	Activate(context.Context, string, configrepo.Rollout, evaluation.Mutation) (calibrationpromotion.Plan, error)
	Rollback(context.Context, string, evaluation.Mutation) (calibrationpromotion.Plan, error)
	Get(string, evaluation.Access) (calibrationpromotion.Plan, error)
	List(evaluation.Access) ([]calibrationpromotion.Plan, error)
}

type NormalizationPromotionService interface {
	Prepare(context.Context, normalizationpromotion.PrepareRequest, evaluation.Mutation) (normalizationpromotion.Preparation, error)
	RecordQualityGate(context.Context, normalizationpromotion.GateRequest, evaluation.Mutation) (normalizationpromotion.GateOutput, error)
	RecordOperationalGate(context.Context, normalizationpromotion.OperationalGateRequest, evaluation.Mutation) (normalizationpromotion.OperationalGateOutput, error)
	StartCanary(context.Context, string, runmodel.ArtifactRef, configrepo.Rollout, evaluation.Mutation) (normalizationpromotion.LifecycleOutput, error)
	Activate(context.Context, string, evaluation.Mutation) (normalizationpromotion.LifecycleOutput, error)
	Rollback(context.Context, string, evaluation.Mutation) (normalizationpromotion.LifecycleOutput, error)
	Get(string, evaluation.Access) (evaluation.PromotionRecord, error)
	List(evaluation.Access) ([]evaluation.PromotionRecord, error)
}

type PromotionMonitorService interface {
	Build(context.Context, promotionmonitor.BuildRequest, evaluation.Mutation) (promotionmonitor.Observation, error)
	Get(string, evaluation.Access) (promotionmonitor.Observation, error)
	List(evaluation.Access) ([]promotionmonitor.Summary, error)
}

type TrainingService interface {
	Materialize(context.Context, training.MaterializationRequest, evaluation.Mutation) (training.Record, error)
	Get(string, evaluation.Access) (training.Record, error)
	List(evaluation.Access) ([]training.Record, error)
}

type TrainingExportService interface {
	Build(context.Context, training.ExportRequest, evaluation.Mutation) (training.ExportRecord, error)
	Get(string, evaluation.Access) (training.ExportRecord, error)
	List(evaluation.Access) ([]training.ExportRecord, error)
}

type TrainingJobService interface {
	Prepare(context.Context, training.JobPrepareRequest, evaluation.Mutation) (training.JobRecord, error)
	Observe(context.Context, training.JobObservationRequest, evaluation.Mutation) (training.JobRecord, error)
	Get(string, evaluation.Access) (training.JobRecord, error)
	List(evaluation.Access) ([]training.JobRecord, error)
}

// AgentComponentPublisher derives the only writable subject from a committed
// ReviewRun. The HTTP body cannot select tenant/workspace/repository scope.
type AgentComponentPublisher interface {
	PublishAgentComponent(
		context.Context,
		AgentComponentPublicationRequest,
		AgentComponentPublicationMutation,
	) (AgentComponentPublicationRecord, error)
}

// WorkloadPressureReader keeps the HTTP adapter dependent on the read-only
// scheduling projection rather than the concrete local repository.
type WorkloadPressureReader interface {
	Pressure(time.Time) (scheduling.PressureSnapshot, error)
}

type Services struct {
	Evaluation             *evaluation.Repository
	Calibration            *calibration.Repository
	Training               TrainingService
	TrainingExports        TrainingExportService
	TrainingJobs           TrainingJobService
	CalibrationPromotion   CalibrationPromotionService
	PromotionMonitor       PromotionMonitorService
	NormalizationPromotion NormalizationPromotionService
	EvaluationHistory      EvaluationHistoryReader
	EvaluationBatches      EvaluationBatchController
	Components             AgentComponentPublisher
	Config                 *configrepo.Repository
	Dashboard              *analyticsadapter.Adapter
	Runs                   *runrepo.Repository
	Findings               FindingService
	Lineages               FindingLineageService
	ReviewJobs             ReviewJobService
	Workloads              WorkloadPressureReader
}

func NewHandler(services Services, principal Principal, token string) (*Handler, error) {
	if services.Evaluation == nil && services.Calibration == nil && services.Training == nil && services.TrainingExports == nil && services.TrainingJobs == nil && services.CalibrationPromotion == nil && services.PromotionMonitor == nil && services.NormalizationPromotion == nil && services.Config == nil && services.Dashboard == nil &&
		services.Runs == nil && services.Findings == nil && services.Lineages == nil && services.ReviewJobs == nil &&
		services.Workloads == nil && services.EvaluationHistory == nil && services.EvaluationBatches == nil &&
		services.Components == nil {
		return nil, fmt.Errorf("at least one local API service is required")
	}
	if err := principal.Validate(); err != nil {
		return nil, fmt.Errorf("validate local API principal: %w", err)
	}
	if err := ValidateBearerToken(token); err != nil {
		return nil, err
	}
	history := services.EvaluationHistory
	if history == nil && services.Evaluation != nil {
		history = services.Evaluation
	}
	return &Handler{
		evaluation: services.Evaluation, evaluationHistory: history,
		calibration:            services.Calibration,
		training:               services.Training,
		trainingExports:        services.TrainingExports,
		trainingJobs:           services.TrainingJobs,
		calibrationPromotion:   services.CalibrationPromotion,
		promotionMonitor:       services.PromotionMonitor,
		normalizationPromotion: services.NormalizationPromotion,
		evaluationBatches:      services.EvaluationBatches,
		components:             services.Components,
		config:                 services.Config, dashboard: services.Dashboard,
		runs: services.Runs, findings: services.Findings, lineages: services.Lineages, reviewJobs: services.ReviewJobs,
		workloads: services.Workloads,
		principal: principal, token: token,
	}, nil
}

// ValidateBearerToken is exposed for composition roots so invalid credentials
// fail before any background service or local store is opened.
func ValidateBearerToken(token string) error {
	if len(token) < 32 || len(token) > 4096 {
		return fmt.Errorf("local API bearer token must contain between 32 and 4096 bytes")
	}
	for _, character := range token {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return fmt.Errorf("local API bearer token must not contain whitespace or control characters")
		}
	}
	return nil
}

func (handler *Handler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if serveOperatorUI(writer, request) {
		return
	}
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	if !handler.authenticate(request) {
		handler.writeError(writer, http.StatusUnauthorized, "unauthorized", "valid bearer authentication is required")
		return
	}
	if request.URL.RawPath != "" {
		handler.writeError(writer, http.StatusBadRequest, "invalid_path", "encoded request paths are not accepted")
		return
	}

	switch {
	case request.URL.Path == "/v1/health":
		handler.handleHealth(writer, request)
	case request.URL.Path == "/v1/evaluation/cases":
		handler.handleCases(writer, request)
	case request.URL.Path == "/v1/calibration/runs":
		handler.handleCalibrationRuns(writer, request)
	case request.URL.Path == "/v1/calibration/run":
		handler.handleCalibrationRun(writer, request)
	case request.URL.Path == "/v1/training/manifests":
		handler.handleTrainingManifests(writer, request)
	case request.URL.Path == "/v1/training/manifest":
		handler.handleTrainingManifest(writer, request)
	case request.URL.Path == "/v1/training/exports":
		handler.handleTrainingExports(writer, request)
	case request.URL.Path == "/v1/training/export":
		handler.handleTrainingExport(writer, request)
	case request.URL.Path == "/v1/training/jobs":
		handler.handleTrainingJobs(writer, request)
	case request.URL.Path == "/v1/training/job":
		handler.handleTrainingJob(writer, request)
	case request.URL.Path == "/v1/training/job/observations":
		handler.handleTrainingJobObservations(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/plans":
		handler.handleCalibrationPromotionPlans(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/plan":
		handler.handleCalibrationPromotionPlan(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/gates":
		handler.handleCalibrationPromotionGate(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/activate":
		handler.handleCalibrationPromotionActivate(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/rollback":
		handler.handleCalibrationPromotionRollback(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/observations":
		handler.handleCalibrationPromotionObservations(writer, request)
	case request.URL.Path == "/v1/calibration/promotion/observation":
		handler.handleCalibrationPromotionObservation(writer, request)
	case request.URL.Path == "/v1/evaluation/normalization/promotions":
		handler.handleNormalizationPromotions(writer, request)
	case request.URL.Path == "/v1/evaluation/normalization/promotion":
		handler.handleNormalizationPromotion(writer, request)
	case request.URL.Path == "/v1/evaluation/normalization/promotion/gates":
		handler.handleNormalizationPromotionGate(writer, request)
	case request.URL.Path == "/v1/evaluation/normalization/promotion/operational-gates":
		handler.handleNormalizationPromotionOperationalGate(writer, request)
	case request.URL.Path == "/v1/evaluation/normalization/promotion/canary":
		handler.handleNormalizationPromotionCanary(writer, request)
	case request.URL.Path == "/v1/evaluation/normalization/promotion/activate":
		handler.handleNormalizationPromotionLifecycle(writer, request, "activate")
	case request.URL.Path == "/v1/evaluation/normalization/promotion/rollback":
		handler.handleNormalizationPromotionLifecycle(writer, request, "rollback")
	case request.URL.Path == "/v1/evaluation/case":
		handler.handleCaseQuery(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/evaluation/cases/"):
		handler.handleCase(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/evaluation/cases/"))
	case request.URL.Path == "/v1/evaluation/case-imports":
		handler.handleCaseImport(writer, request)
	case request.URL.Path == "/v1/evaluation/governance-batches":
		handler.handleGovernanceBatches(writer, request)
	case request.URL.Path == "/v1/evaluation/governance-batch":
		handler.handleGovernanceBatchQuery(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/evaluation/governance-batches/"):
		handler.handleGovernanceBatch(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/evaluation/governance-batches/"))
	case request.URL.Path == "/v1/evaluation/trust-keys":
		handler.handleTrustKeys(writer, request)
	case request.URL.Path == "/v1/evaluation/trust-key-revocations":
		handler.handleTrustKeyRevocation(writer, request)
	case request.URL.Path == "/v1/evaluation/evaluation-runs":
		handler.handleEvaluationRuns(writer, request)
	case request.URL.Path == "/v1/evaluation/evaluation-run":
		handler.handleEvaluationRun(writer, request)
	case request.URL.Path == "/v1/evaluation/experiment-runs":
		handler.handleExperimentRuns(writer, request)
	case request.URL.Path == "/v1/evaluation/experiment-run":
		handler.handleExperimentRun(writer, request)
	case request.URL.Path == "/v1/evaluation/repeatability-runs":
		handler.handleRepeatabilityRuns(writer, request)
	case request.URL.Path == "/v1/evaluation/repeatability-run":
		handler.handleRepeatabilityRun(writer, request)
	case request.URL.Path == "/v1/evaluation/experiment-batches":
		handler.handleExperimentBatches(writer, request)
	case request.URL.Path == "/v1/evaluation/experiment-batch":
		handler.handleExperimentBatch(writer, request)
	case request.URL.Path == "/v1/evaluation/experiment-batch/resume":
		handler.handleExperimentBatchResume(writer, request)
	case request.URL.Path == "/v1/evaluation/repeatability-batches":
		handler.handleRepeatabilityBatches(writer, request)
	case request.URL.Path == "/v1/evaluation/repeatability-batch":
		handler.handleRepeatabilityBatch(writer, request)
	case request.URL.Path == "/v1/evaluation/repeatability-batch/resume":
		handler.handleRepeatabilityBatchResume(writer, request)
	case request.URL.Path == "/v1/agent-components":
		handler.handleAgentComponentPublish(writer, request)
	case request.URL.Path == "/v1/config/revisions":
		handler.handleConfigRevisions(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/config/revisions/"):
		handler.handleConfigRevisionPath(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/config/revisions/"))
	case request.URL.Path == "/v1/config/resolutions":
		handler.handleConfigResolution(writer, request)
	case request.URL.Path == "/v1/dashboard/snapshots":
		handler.handleDashboardSnapshots(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/dashboard/snapshots/"):
		handler.handleDashboardSnapshot(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/dashboard/snapshots/"))
	case request.URL.Path == "/v1/review-runs":
		handler.handleReviewRuns(writer, request)
	case request.URL.Path == "/v1/review-run-impacts":
		handler.handleReviewRunImpacts(writer, request)
	case request.URL.Path == "/v1/finding-lineages":
		handler.handleFindingLineages(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/finding-lineages/"):
		handler.handleFindingLineage(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/finding-lineages/"))
	case strings.HasPrefix(request.URL.Path, "/v1/review-runs/"):
		handler.handleReviewRunPath(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/review-runs/"))
	case request.URL.Path == "/v1/review-jobs":
		handler.handleReviewJobs(writer, request)
	case strings.HasPrefix(request.URL.Path, "/v1/review-jobs/"):
		handler.handleReviewJobPath(writer, request, strings.TrimPrefix(request.URL.Path, "/v1/review-jobs/"))
	case request.URL.Path == "/v1/workloads/pressure":
		handler.handleWorkloadPressure(writer, request)
	default:
		handler.writeError(writer, http.StatusNotFound, "route_not_found", "route not found")
	}
}

func (handler *Handler) handleWorkloadPressure(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionReviewRead, handler.workloads != nil) {
		return
	}
	raw, ok := handler.parseExactLookup(writer, request.URL.Query(), "at")
	if !ok {
		return
	}
	observedAt, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || observedAt.IsZero() || observedAt.Location() != time.UTC || !strings.HasSuffix(raw, "Z") {
		handler.writeError(writer, http.StatusBadRequest, "invalid_query", "at must be a non-zero RFC3339 UTC timestamp ending in Z")
		return
	}
	snapshot, err := handler.workloads.Pressure(observedAt)
	if err != nil {
		if errors.Is(err, scheduling.ErrCorrupt) {
			handler.writeError(writer, http.StatusInternalServerError, "workload_state_corrupt", "workload scheduling state failed integrity validation")
		} else {
			handler.writeError(writer, http.StatusBadRequest, "invalid_observation", err.Error())
		}
		return
	}
	handler.writeResponse(writer, http.StatusOK, snapshot)
}

func (handler *Handler) handleReviewRunPath(writer http.ResponseWriter, request *http.Request, suffix string) {
	segments := strings.Split(suffix, "/")
	switch {
	case len(segments) == 1:
		handler.handleReviewRun(writer, request, segments[0])
	case len(segments) == 3 && segments[1] == "findings":
		handler.handleReviewFinding(writer, request, segments[0], segments[2])
	case len(segments) == 4 && segments[1] == "findings" && segments[3] == "decisions":
		handler.handleFindingDecision(writer, request, segments[0], segments[2])
	case len(segments) == 4 && segments[1] == "findings" && segments[3] == "feedback":
		handler.handleFindingFeedback(writer, request, segments[0], segments[2])
	case len(segments) == 4 && segments[1] == "findings" && segments[3] == "outcomes":
		handler.handleFindingOutcome(writer, request, segments[0], segments[2])
	default:
		handler.writeError(writer, http.StatusBadRequest, "invalid_path", "review run path is invalid")
	}
}

func (handler *Handler) validateFindingWrite(
	writer http.ResponseWriter,
	request *http.Request,
	runID string,
	findingID string,
) bool {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return false
	}
	if !handler.authorize(writer, PermissionReviewWrite, handler.findings != nil) {
		return false
	}
	if !validPathID(runID) || !validPathID(findingID) {
		handler.writeError(
			writer, http.StatusBadRequest, "invalid_path",
			"run and finding IDs must be unencoded path segments",
		)
		return false
	}
	return handler.rejectQuery(writer, request.URL.Query())
}

func (handler *Handler) handleFindingDecision(
	writer http.ResponseWriter,
	request *http.Request,
	runID string,
	findingID string,
) {
	if !handler.validateFindingWrite(writer, request, runID, findingID) {
		return
	}
	var command FindingDecisionWriteCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if err := command.Validate(runID, findingID, handler.principal); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	decision, err := handler.findings.RecordDecision(
		request.Context(),
		findingdecision.Request{
			SchemaVersion: findingdecision.RequestSchemaVersion,
			RunID:         runID, FindingID: findingID, Action: command.Action,
			ReasonCode: command.ReasonCode, EvidenceRefs: slices.Clone(command.EvidenceRefs),
			OccurredAt: command.OccurredAt,
		},
		findingdecision.Mutation{
			SchemaVersion:  findingdecision.MutationSchemaVersion,
			IdempotencyKey: command.Mutation.IdempotencyKey,
			Actor: findingdecision.Actor{
				Kind: handler.principal.ActorKind, ID: handler.principal.Actor,
			},
			Roles: slices.Clone(handler.principal.FindingRoles),
			Audit: command.Mutation.Audit, At: command.Mutation.At,
		},
	)
	if err != nil {
		handler.writeFindingMutationError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusCreated, decision)
}

func (handler *Handler) handleFindingFeedback(
	writer http.ResponseWriter,
	request *http.Request,
	runID string,
	findingID string,
) {
	if !handler.validateFindingWrite(writer, request, runID, findingID) {
		return
	}
	var command FeedbackWriteCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	fact, err := command.Fact(runID, findingID, handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	recorded, err := handler.findings.RecordFeedback(request.Context(), fact)
	if err != nil {
		handler.writeFindingMutationError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusCreated, FeedbackWriteResult{
		Feedback: recorded, Eligibility: recorded.EvaluationCandidateEligibility(),
	})
}

func (handler *Handler) handleFindingOutcome(
	writer http.ResponseWriter,
	request *http.Request,
	runID string,
	findingID string,
) {
	if !handler.validateFindingWrite(writer, request, runID, findingID) {
		return
	}
	var command OutcomeWriteCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	fact, err := command.Fact(runID, findingID, handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	recorded, err := handler.findings.RecordOutcome(request.Context(), fact)
	if err != nil {
		handler.writeFindingMutationError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusCreated, recorded)
}

func (handler *Handler) handleReviewJobs(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionReviewRead, handler.reviewJobs != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.reviewJobs.List()
		if err != nil {
			handler.writeReviewJobError(writer, err)
			return
		}
		start := 0
		if cursor != "" {
			found := false
			for index := range records {
				if records[index].JobID == cursor {
					start = index + 1
					found = true
					break
				}
			}
			if !found {
				handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", "review job cursor is malformed or unavailable")
				return
			}
		}
		end := min(start+limit, len(records))
		next := ""
		if end < len(records) && end > start {
			next = records[end-1].JobID
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: records[start:end], NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionReviewExecute, handler.reviewJobs != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command ReviewJobSubmitCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record, err := handler.reviewJobs.Submit(request.Context(), command.Request, reviewjob.Mutation{
			IdempotencyKey: command.Mutation.IdempotencyKey,
			Actor:          handler.principal.Actor, Audit: command.Mutation.Audit, At: command.Mutation.At,
		})
		if err != nil {
			handler.writeReviewJobError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusAccepted, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleReviewJobPath(
	writer http.ResponseWriter,
	request *http.Request,
	suffix string,
) {
	segments := strings.Split(suffix, "/")
	switch {
	case len(segments) == 1 && request.Method == http.MethodGet:
		if !handler.authorize(writer, PermissionReviewRead, handler.reviewJobs != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		record, err := handler.reviewJobs.Get(segments[0])
		if err != nil {
			handler.writeReviewJobError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, record)
	case len(segments) == 2 && segments[1] == "cancel" && request.Method == http.MethodPost:
		if !handler.authorize(writer, PermissionReviewExecute, handler.reviewJobs != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command ReviewJobCancelCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record, err := handler.reviewJobs.Cancel(request.Context(), segments[0], reviewjob.CancelCommand{
			Reason: command.Reason,
			Mutation: reviewjob.Mutation{
				IdempotencyKey: command.Mutation.IdempotencyKey,
				Actor:          handler.principal.Actor, Audit: command.Mutation.Audit, At: command.Mutation.At,
			},
		})
		if err != nil {
			handler.writeReviewJobError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, record)
	case len(segments) == 2 && segments[1] == "timeline" && request.Method == http.MethodGet:
		if !handler.authorize(writer, PermissionReviewRead, handler.reviewJobs != nil) {
			return
		}
		pageQuery := url.Values{}
		for _, key := range []string{"limit", "cursor"} {
			if value, exists := request.URL.Query()[key]; exists {
				pageQuery[key] = value
			}
		}
		limit, cursor, ok := handler.parsePage(writer, pageQuery)
		if !ok {
			return
		}
		events, err := handler.reviewJobs.Timeline(segments[0])
		if err != nil {
			handler.writeReviewJobError(writer, err)
			return
		}
		page, next, err := paginate(events, limit, cursor, func(event scheduling.WorkloadTimelineEvent) string {
			return fmt.Sprintf("%020d", event.Sequence)
		})
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: page, NextCursor: next})
	case len(segments) == 1:
		handler.methodNotAllowed(writer, http.MethodGet)
	case len(segments) == 2 && segments[1] == "cancel":
		handler.methodNotAllowed(writer, http.MethodPost)
	case len(segments) == 2 && segments[1] == "timeline":
		handler.methodNotAllowed(writer, http.MethodGet)
	default:
		handler.writeError(writer, http.StatusBadRequest, "invalid_path", "review job path is invalid")
	}
}

func (handler *Handler) handleReviewRuns(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionReviewRead, handler.runs != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	page, err := handler.reviewRunPage(limit, cursor)
	if err != nil {
		if errors.Is(err, errInvalidReviewRunCursor) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", "review run cursor is malformed or unavailable")
		} else {
			handler.writeRunError(writer, err)
		}
		return
	}
	handler.writeResponse(writer, http.StatusOK, page)
}

func (handler *Handler) handleFindingLineages(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionReviewRead, handler.lineages != nil) {
			return
		}
		allowed := map[string]bool{"run_id": true, "repository_id": true, "limit": true, "cursor": true}
		for key, values := range request.URL.Query() {
			if !allowed[key] || len(values) != 1 {
				handler.writeError(writer, http.StatusBadRequest, "invalid_query", "finding lineage query contains an unsupported or repeated parameter")
				return
			}
		}
		pageQuery := url.Values{}
		for _, key := range []string{"limit", "cursor"} {
			if value, exists := request.URL.Query()[key]; exists {
				pageQuery[key] = value
			}
		}
		limit, cursor, ok := handler.parsePage(writer, pageQuery)
		if !ok {
			return
		}
		records, err := handler.lineages.List(findinglineage.ListFilter{
			RunID: request.URL.Query().Get("run_id"), RepositoryID: request.URL.Query().Get("repository_id"),
		})
		if err != nil {
			handler.writeFindingLineageError(writer, err)
			return
		}
		page, next, err := paginate(records, limit, cursor, func(record findinglineage.Record) string { return record.Lineage.LineageID })
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: page, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionReviewWrite, handler.lineages != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command findinglineage.BuildRequest
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		record, err := handler.lineages.Build(request.Context(), command)
		if err != nil {
			handler.writeFindingLineageError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleFindingLineage(writer http.ResponseWriter, request *http.Request, lineageID string) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionReviewRead, handler.lineages != nil) {
		return
	}
	if !validPathID(lineageID) || !handler.rejectQuery(writer, request.URL.Query()) {
		if !validPathID(lineageID) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_path", "lineage ID must be one unencoded path segment")
		}
		return
	}
	record, err := handler.lineages.Get(lineageID)
	if err != nil {
		handler.writeFindingLineageError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) writeFindingLineageError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, findinglineage.ErrNotFound):
		handler.writeError(writer, http.StatusNotFound, "finding_lineage_not_found", "finding lineage was not found")
	case errors.Is(err, findinglineage.ErrConflict):
		handler.writeError(writer, http.StatusConflict, "finding_lineage_conflict", err.Error())
	case errors.Is(err, findinglineage.ErrCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "finding_lineage_corrupt", "finding lineage state failed integrity validation")
	default:
		handler.writeError(writer, http.StatusBadRequest, "invalid_finding_lineage", err.Error())
	}
}

func (handler *Handler) handleReviewRunImpacts(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionReviewRead, handler.runs != nil) {
		return
	}
	selector, limit, encodedCursor, ok := handler.parseImpactQuery(writer, request.URL.Query())
	if !ok {
		return
	}
	var cursor impactCursor
	if encodedCursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(encodedCursor)
		if err != nil || len(data) == 0 || len(data) > 4096 ||
			decodeStrictInto(data, &cursor) != nil ||
			cursor.SchemaVersion != impactCursorSchema || cursor.Watermark == 0 ||
			cursor.LastSequence == 0 || cursor.LastSequence > cursor.Watermark ||
			cursor.RunID == "" || cursor.Selector != selector {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", "review run impact cursor is malformed or belongs to another selector")
			return
		}
	}
	impact, err := handler.runs.ImpactAt(selector, cursor.Watermark)
	if err != nil {
		if encodedCursor != "" && errors.Is(err, os.ErrNotExist) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", "review run impact watermark is unavailable")
		} else {
			handler.writeRunError(writer, err)
		}
		return
	}
	start := 0
	if encodedCursor != "" {
		found := false
		for index, match := range impact.Matches {
			if match.History.LastSequence == cursor.LastSequence &&
				match.History.RunID == cursor.RunID {
				start = index + 1
				found = true
				break
			}
		}
		if !found {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", "review run impact cursor no longer identifies a match")
			return
		}
	}
	end := min(start+limit, len(impact.Matches))
	items := impact.Matches[start:end]
	next := ""
	if end < len(impact.Matches) && len(items) != 0 {
		last := items[len(items)-1].History
		data, err := json.Marshal(impactCursor{
			SchemaVersion: impactCursorSchema, Watermark: impact.Watermark,
			LastSequence: last.LastSequence, RunID: last.RunID, Selector: selector,
		})
		if err != nil {
			handler.writeRunError(writer, err)
			return
		}
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	handler.writeResponse(writer, http.StatusOK, ReviewRunImpactPage{
		SchemaVersion: impact.SchemaVersion, Watermark: impact.Watermark,
		Selector: selector, Items: items, Coverage: impact.Coverage, NextCursor: next,
	})
}

func (handler *Handler) parseImpactQuery(
	writer http.ResponseWriter,
	query url.Values,
) (runrepo.ImpactSelector, int, string, bool) {
	allowed := map[string]bool{
		"kind": true, "id": true, "revision": true, "sha256": true,
		"limit": true, "cursor": true,
	}
	for key, values := range query {
		if !allowed[key] {
			handler.writeError(writer, http.StatusBadRequest, "invalid_query", "unsupported query parameter "+key)
			return runrepo.ImpactSelector{}, 0, "", false
		}
		if len(values) != 1 {
			handler.writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters must not be repeated")
			return runrepo.ImpactSelector{}, 0, "", false
		}
	}
	selector := runrepo.ImpactSelector{
		Kind: runrepo.ImpactComponentKind(query.Get("kind")), ID: query.Get("id"),
		Revision: query.Get("revision"), SHA256: query.Get("sha256"),
	}
	if err := selector.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_query", err.Error())
		return runrepo.ImpactSelector{}, 0, "", false
	}
	limit := defaultPageLimit
	if text := query.Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > maximumPageLimit {
			handler.writeError(writer, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 200")
			return runrepo.ImpactSelector{}, 0, "", false
		}
		limit = parsed
	}
	return selector, limit, query.Get("cursor"), true
}

func (handler *Handler) reviewRunPage(limit int, encodedCursor string) (Page, error) {
	var cursor reviewRunCursor
	if encodedCursor != "" {
		data, err := base64.RawURLEncoding.DecodeString(encodedCursor)
		if err != nil || len(data) == 0 || len(data) > 4096 {
			return Page{}, errInvalidReviewRunCursor
		}
		if err := decodeStrictInto(data, &cursor); err != nil ||
			cursor.SchemaVersion != reviewRunCursorSchema || cursor.Watermark == 0 ||
			cursor.LastSequence == 0 || cursor.LastSequence > cursor.Watermark ||
			cursor.RunID == "" {
			return Page{}, errInvalidReviewRunCursor
		}
	}
	snapshot, err := handler.runs.HistoryAt(cursor.Watermark)
	if err != nil {
		if encodedCursor != "" && errors.Is(err, os.ErrNotExist) {
			return Page{}, errInvalidReviewRunCursor
		}
		return Page{}, err
	}
	start := 0
	if encodedCursor != "" {
		found := false
		for index, entry := range snapshot.Entries {
			if entry.LastSequence == cursor.LastSequence && entry.RunID == cursor.RunID {
				start = index + 1
				found = true
				break
			}
		}
		if !found {
			return Page{}, errInvalidReviewRunCursor
		}
	}
	end := min(start+limit, len(snapshot.Entries))
	items := snapshot.Entries[start:end]
	next := ""
	if end < len(snapshot.Entries) && len(items) > 0 {
		last := items[len(items)-1]
		data, err := json.Marshal(reviewRunCursor{
			SchemaVersion: reviewRunCursorSchema,
			Watermark:     snapshot.Watermark, LastSequence: last.LastSequence, RunID: last.RunID,
		})
		if err != nil {
			return Page{}, err
		}
		next = base64.RawURLEncoding.EncodeToString(data)
	}
	return Page{Items: items, NextCursor: next}, nil
}

func (handler *Handler) handleReviewRun(writer http.ResponseWriter, request *http.Request, runID string) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionReviewRead, handler.runs != nil) {
		return
	}
	if !validPathID(runID) || !handler.rejectQuery(writer, request.URL.Query()) {
		if !validPathID(runID) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_path", "run ID must be one unencoded path segment")
		}
		return
	}
	detail, err := handler.loadReviewRunDetail(runID)
	if err != nil {
		handler.writeRunError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, detail)
}

func (handler *Handler) handleReviewFinding(
	writer http.ResponseWriter,
	request *http.Request,
	runID string,
	findingID string,
) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionReviewRead, handler.findings != nil) {
		return
	}
	if !validPathID(runID) || !validPathID(findingID) || !handler.rejectQuery(writer, request.URL.Query()) {
		if !validPathID(runID) || !validPathID(findingID) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_path", "run and finding IDs must be unencoded path segments")
		}
		return
	}
	detail, err := handler.findings.Finding(runID, findingID)
	if err != nil {
		handler.writeFindingError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, findingView(detail))
}

func (handler *Handler) loadReviewRunDetail(runID string) (ReviewRunDetail, error) {
	snapshot, err := handler.runs.HistoryAt(0)
	if err != nil {
		return ReviewRunDetail{}, err
	}
	var history *runrepo.HistoryEntry
	for index := range snapshot.Entries {
		if snapshot.Entries[index].RunID == runID {
			entry := snapshot.Entries[index]
			history = &entry
			break
		}
	}
	if history == nil {
		return ReviewRunDetail{}, os.ErrNotExist
	}
	detail := ReviewRunDetail{History: *history}
	if history.Status == runmodel.RunStatusPending || history.Status == runmodel.RunStatusRunning {
		return detail, nil
	}
	result, err := handler.runs.LoadCommittedRunResult(runID)
	if err != nil {
		return ReviewRunDetail{}, err
	}
	detail.Committed = true
	detail.Run = &result.Run
	detail.Report = result.Report
	detail.CandidateSet = result.CandidateSet
	detail.VerificationLedger = result.VerificationLedger
	detail.CalibrationLedger = result.CalibrationLedger
	detail.SuppressionLedger = result.SuppressionLedger
	detail.GovernedReport = result.GovernedReport
	return detail, nil
}

func (handler *Handler) handleConfigRevisions(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionConfigRead, handler.config != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.config.List()
		if err != nil {
			handler.writeConfigError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, configRecordIdentity)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionConfigWrite, handler.config != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command ConfigCreateCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.configMutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record, err := handler.config.Create(request.Context(), command.Revision, mutation)
		if err != nil {
			handler.writeConfigError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleConfigResolution(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionConfigRead, handler.config != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var query ConfigResolutionQuery
	if !handler.decodeRequest(writer, request, &query) {
		return
	}
	if err := query.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	bundle, receipt, err := handler.config.ResolvePublishedWithReceipt(request.Context(), query.Context)
	if err != nil {
		handler.writeConfigError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, ConfigResolutionView{Bundle: bundle, Receipt: receipt})
}

func (handler *Handler) handleConfigRevisionPath(writer http.ResponseWriter, request *http.Request, suffix string) {
	segments := strings.Split(suffix, "/")
	if len(segments) < 2 || len(segments) > 3 || !validPathID(segments[0]) || !validPathID(segments[1]) {
		handler.writeError(writer, http.StatusBadRequest, "invalid_path", "config revision path must contain ID and revision")
		return
	}
	id, revision := segments[0], segments[1]
	if len(segments) == 2 {
		if request.Method != http.MethodGet {
			handler.methodNotAllowed(writer, http.MethodGet)
			return
		}
		if !handler.authorize(writer, PermissionConfigRead, handler.config != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		detail, err := handler.config.GetWithHistory(id, revision)
		if err != nil {
			handler.writeConfigError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, detail)
		return
	}
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionConfigWrite, handler.config != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command ConfigTransitionCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if err := command.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.configMutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	var record configrepo.Record
	switch segments[2] {
	case "validate":
		if command.Rollout != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "validate transition must not contain rollout")
			return
		}
		record, err = handler.config.ValidateRevision(request.Context(), id, revision, mutation)
	case "publish":
		if command.Rollout == nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "publish transition requires rollout")
			return
		}
		record, err = handler.config.Publish(request.Context(), id, revision, *command.Rollout, mutation)
	case "advance":
		if command.Rollout == nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "advance transition requires rollout")
			return
		}
		record, err = handler.config.AdvanceRollout(request.Context(), id, revision, *command.Rollout, mutation)
	case "rollback":
		if command.Rollout != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "rollback transition must not contain rollout")
			return
		}
		record, err = handler.config.Rollback(request.Context(), id, revision, mutation)
	default:
		handler.writeError(writer, http.StatusNotFound, "route_not_found", "config transition route not found")
		return
	}
	if err != nil {
		handler.writeConfigError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleDashboardSnapshots(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionDashboardRead, handler.dashboard != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	summaries, err := handler.dashboard.ListSnapshots()
	if err != nil {
		handler.writeDashboardError(writer, err)
		return
	}
	items, next, err := paginate(summaries, limit, cursor, func(summary analyticsadapter.SnapshotSummary) string {
		return summary.SnapshotID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleDashboardSnapshot(writer http.ResponseWriter, request *http.Request, snapshotID string) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionDashboardRead, handler.dashboard != nil) {
		return
	}
	if !validPathID(snapshotID) || !handler.rejectQuery(writer, request.URL.Query()) {
		if !validPathID(snapshotID) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_path", "snapshot ID must be one unencoded path segment")
		}
		return
	}
	snapshot, err := handler.dashboard.Query(snapshotID)
	if err != nil {
		handler.writeDashboardError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, dashboardView(snapshot))
}

func (handler *Handler) authorize(writer http.ResponseWriter, permission Permission, available bool) bool {
	if !handler.principal.HasPermission(permission) {
		handler.writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for this operation")
		return false
	}
	if !available {
		handler.writeError(writer, http.StatusServiceUnavailable, "service_unavailable", "local API service is not configured")
		return false
	}
	return true
}

func (handler *Handler) authenticate(request *http.Request) bool {
	values := request.Header.Values("Authorization")
	if len(values) != 1 || !strings.HasPrefix(values[0], "Bearer ") {
		return false
	}
	presented := strings.TrimPrefix(values[0], "Bearer ")
	if len(presented) != len(handler.token) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(presented), []byte(handler.token)) == 1
}

func (handler *Handler) handleHealth(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	handler.writeResponse(writer, http.StatusOK, struct {
		Status          string `json:"status"`
		ProfileRevision string `json:"profile_revision"`
	}{Status: "ok", ProfileRevision: handler.principal.ProfileRevision})
}

func (handler *Handler) handleCalibrationRuns(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionEvaluationRead, handler.calibration != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		runs, err := handler.calibration.List(handler.principal.Access())
		if err != nil {
			handler.writeCalibrationError(writer, err)
			return
		}
		items, next, err := paginate(runs, limit, cursor, func(run calibration.Run) string { return run.RunID })
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionEvaluationWrite, handler.calibration != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command CalibrationFitCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		run, err := handler.calibration.Fit(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeCalibrationError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, run)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleCalibrationRun(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.calibration != nil) {
		return
	}
	runID, ok := handler.parseExactLookup(writer, request.URL.Query(), "run_id")
	if !ok {
		return
	}
	run, err := handler.calibration.Get(runID, handler.principal.Access())
	if err != nil {
		handler.writeCalibrationError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, run)
}

func (handler *Handler) handleTrainingManifests(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionEvaluationRead, handler.training != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.training.List(handler.principal.Access())
		if err != nil {
			handler.writeTrainingError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, func(record training.Record) string {
			return record.Manifest.ManifestID
		})
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionEvaluationWrite, handler.training != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command TrainingMaterializeCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record, err := handler.training.Materialize(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeTrainingError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleTrainingManifest(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.training != nil) {
		return
	}
	manifestID, ok := handler.parseExactLookup(writer, request.URL.Query(), "manifest_id")
	if !ok {
		return
	}
	record, err := handler.training.Get(manifestID, handler.principal.Access())
	if err != nil {
		handler.writeTrainingError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleTrainingExports(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionEvaluationRead, handler.trainingExports != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.trainingExports.List(handler.principal.Access())
		if err != nil {
			handler.writeTrainingError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, func(record training.ExportRecord) string { return record.Bundle.ExportID })
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionEvaluationWrite, handler.trainingExports != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command TrainingExportBuildCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record, err := handler.trainingExports.Build(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeTrainingError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleTrainingExport(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.trainingExports != nil) {
		return
	}
	exportID, ok := handler.parseExactLookup(writer, request.URL.Query(), "export_id")
	if !ok {
		return
	}
	record, err := handler.trainingExports.Get(exportID, handler.principal.Access())
	if err != nil {
		handler.writeTrainingError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleTrainingJobs(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionEvaluationRead, handler.trainingJobs != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.trainingJobs.List(handler.principal.Access())
		if err != nil {
			handler.writeTrainingError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, func(record training.JobRecord) string { return record.Plan.Request.JobID })
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionEvaluationWrite, handler.trainingJobs != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command TrainingJobPrepareCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		record, err := handler.trainingJobs.Prepare(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeTrainingError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleTrainingJob(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.trainingJobs != nil) {
		return
	}
	jobID, ok := handler.parseExactLookup(writer, request.URL.Query(), "job_id")
	if !ok {
		return
	}
	record, err := handler.trainingJobs.Get(jobID, handler.principal.Access())
	if err != nil {
		handler.writeTrainingError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleTrainingJobObservations(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.trainingJobs != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command TrainingJobObserveCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if err := command.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := handler.trainingJobs.Observe(request.Context(), command.Request, mutation)
	if err != nil {
		handler.writeTrainingError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusCreated, record)
}

func (handler *Handler) authorizeCalibrationPromotion(writer http.ResponseWriter, write bool) bool {
	if handler.calibrationPromotion == nil {
		handler.writeError(writer, http.StatusServiceUnavailable, "service_unavailable", "calibration promotion service is unavailable")
		return false
	}
	evaluationPermission, configPermission := PermissionEvaluationRead, PermissionConfigRead
	if write {
		evaluationPermission, configPermission = PermissionEvaluationWrite, PermissionConfigWrite
	}
	if !handler.authorize(writer, evaluationPermission, true) {
		return false
	}
	return handler.authorize(writer, configPermission, true)
}

func (handler *Handler) handleCalibrationPromotionPlans(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorizeCalibrationPromotion(writer, false) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		plans, err := handler.calibrationPromotion.List(handler.principal.Access())
		if err != nil {
			handler.writeCalibrationPromotionError(writer, err)
			return
		}
		items, next, err := paginate(plans, limit, cursor, func(plan calibrationpromotion.Plan) string { return plan.Request.PlanID })
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorizeCalibrationPromotion(writer, true) || !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command CalibrationPromotionPrepareCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		plan, err := handler.calibrationPromotion.Prepare(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeCalibrationPromotionError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, plan)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleCalibrationPromotionPlan(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorizeCalibrationPromotion(writer, false) {
		return
	}
	planID, ok := handler.parseExactLookup(writer, request.URL.Query(), "plan_id")
	if !ok {
		return
	}
	plan, err := handler.calibrationPromotion.Get(planID, handler.principal.Access())
	if err != nil {
		handler.writeCalibrationPromotionError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, plan)
}

func (handler *Handler) handleCalibrationPromotionGate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeCalibrationPromotion(writer, true) || !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command CalibrationPromotionGateCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if err := command.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plan, err := handler.calibrationPromotion.RecordGate(request.Context(), command.Request, mutation)
	if err != nil {
		handler.writeCalibrationPromotionError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, plan)
}

func (handler *Handler) handleCalibrationPromotionActivate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeCalibrationPromotion(writer, true) || !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command CalibrationPromotionActivateCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if err := command.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plan, err := handler.calibrationPromotion.Activate(request.Context(), command.PlanID, command.Rollout, mutation)
	if err != nil {
		handler.writeCalibrationPromotionError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, plan)
}

func (handler *Handler) handleCalibrationPromotionRollback(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeCalibrationPromotion(writer, true) || !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command CalibrationPromotionRollbackCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if err := command.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	plan, err := handler.calibrationPromotion.Rollback(request.Context(), command.PlanID, mutation)
	if err != nil {
		handler.writeCalibrationPromotionError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, plan)
}

func (handler *Handler) authorizePromotionMonitor(writer http.ResponseWriter, write bool) bool {
	if handler.promotionMonitor == nil {
		handler.writeError(writer, http.StatusServiceUnavailable, "service_unavailable", "calibration promotion monitor is unavailable")
		return false
	}
	evaluationPermission, configPermission := PermissionEvaluationRead, PermissionConfigRead
	if write {
		evaluationPermission, configPermission = PermissionEvaluationWrite, PermissionConfigWrite
	}
	if !handler.authorize(writer, evaluationPermission, true) || !handler.authorize(writer, configPermission, true) {
		return false
	}
	return handler.authorize(writer, PermissionDashboardRead, true)
}

func (handler *Handler) handleCalibrationPromotionObservations(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorizePromotionMonitor(writer, false) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		items, err := handler.promotionMonitor.List(handler.principal.Access())
		if err != nil {
			handler.writePromotionMonitorError(writer, err)
			return
		}
		page, next, err := paginate(items, limit, cursor, func(item promotionmonitor.Summary) string { return item.ObservationID })
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: page, NextCursor: next})
	case http.MethodPost:
		if !handler.authorizePromotionMonitor(writer, true) || !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command CalibrationPromotionObserveCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if err := command.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		observation, err := handler.promotionMonitor.Build(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writePromotionMonitorError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, observation)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleCalibrationPromotionObservation(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorizePromotionMonitor(writer, false) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "observation_id")
	if !ok {
		return
	}
	observation, err := handler.promotionMonitor.Get(id, handler.principal.Access())
	if err != nil {
		handler.writePromotionMonitorError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, observation)
}

func (handler *Handler) handleCases(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	records, err := handler.evaluation.ListCases(handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	items, next, err := paginate(records, limit, cursor, func(record evaluation.CaseRecord) string {
		return record.Case.CaseID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleCase(writer http.ResponseWriter, request *http.Request, caseID string) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
		return
	}
	if !validPathID(caseID) || !handler.rejectQuery(writer, request.URL.Query()) {
		if !validPathID(caseID) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_path", "case ID must be one unencoded path segment")
		}
		return
	}
	record, err := handler.evaluation.GetCase(caseID, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleCaseQuery(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
		return
	}
	caseID, ok := handler.parseExactLookup(writer, request.URL.Query(), "case_id")
	if !ok {
		return
	}
	record, err := handler.evaluation.GetCase(caseID, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleCaseImport(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluation != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command CaseImportCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if command.SchemaVersion != CaseImportCommandSchemaVersion {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", "unsupported case import command schema")
		return
	}
	if err := command.Request.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !command.Request.ImportedAt.Equal(mutation.At.UTC()) {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", "request imported_at must equal mutation at")
		return
	}
	record, err := handler.evaluation.ImportGovernedCase(request.Context(), command.Request, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleGovernanceBatches(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.evaluation.ListGovernanceBatches(handler.principal.Access())
		if err != nil {
			handler.writeDomainError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, func(record evaluation.GovernanceBatchRecord) string {
			return record.Request.BatchID
		})
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluation != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command GovernanceBatchCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if command.SchemaVersion != GovernanceBatchCommandSchemaVersion {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "unsupported governance batch command schema")
			return
		}
		if err := command.Request.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if !command.Request.SubmittedAt.Equal(mutation.At.UTC()) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "request submitted_at must equal mutation at")
			return
		}
		record, err := handler.evaluation.RunGovernanceBatch(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeDomainError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleGovernanceBatch(writer http.ResponseWriter, request *http.Request, batchID string) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
		return
	}
	if !validPathID(batchID) || !handler.rejectQuery(writer, request.URL.Query()) {
		if !validPathID(batchID) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_path", "batch ID must be one unencoded path segment")
		}
		return
	}
	record, err := handler.evaluation.GetGovernanceBatch(batchID, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleGovernanceBatchQuery(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
		return
	}
	batchID, ok := handler.parseExactLookup(writer, request.URL.Query(), "batch_id")
	if !ok {
		return
	}
	record, err := handler.evaluation.GetGovernanceBatch(batchID, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleTrustKeys(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluation != nil) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.evaluation.ListGovernanceTrustKeys(handler.principal.Access())
		if err != nil {
			handler.writeDomainError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, trustKeyIdentity)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluation != nil) {
			return
		}
		if !handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command TrustKeyCommand
		if !handler.decodeRequest(writer, request, &command) {
			return
		}
		if command.SchemaVersion != TrustKeyCommandSchemaVersion {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "unsupported trust key command schema")
			return
		}
		if err := command.Registration.Validate(); err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		mutation, err := command.Mutation.mutation(handler.principal)
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		if !command.Registration.RegisteredAt.Equal(mutation.At.UTC()) {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "registration registered_at must equal mutation at")
			return
		}
		record, err := handler.evaluation.RegisterGovernanceTrustKey(request.Context(), command.Registration, mutation)
		if err != nil {
			handler.writeDomainError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusOK, record)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleTrustKeyRevocation(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluation != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command TrustKeyRevocationCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	if command.SchemaVersion != TrustKeyRevocationCommandSchemaVersion {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", "unsupported trust key revocation command schema")
		return
	}
	if err := command.Revocation.Validate(); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	mutation, err := command.Mutation.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	if !command.Revocation.RevokedAt.Equal(mutation.At.UTC()) {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", "revocation revoked_at must equal mutation at")
		return
	}
	record, err := handler.evaluation.RevokeGovernanceTrustKey(request.Context(), command.Revocation, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) decodeRequest(writer http.ResponseWriter, request *http.Request, target any) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		handler.writeError(writer, http.StatusUnsupportedMediaType, "unsupported_media_type", "Content-Type must be application/json")
		return false
	}
	request.Body = http.MaxBytesReader(writer, request.Body, maximumRequestBodyBytes)
	data, err := io.ReadAll(request.Body)
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			handler.writeError(writer, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds 4 MiB")
		} else {
			handler.writeError(writer, http.StatusBadRequest, "invalid_request", "read request body")
		}
		return false
	}
	if len(data) == 0 {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", "request body is required")
		return false
	}
	if err := decodeStrictInto(data, target); err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return false
	}
	return true
}

func decodeStrictInto(data []byte, target any) error {
	if err := rejectDuplicateFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return nil
}

func (handler *Handler) parsePage(writer http.ResponseWriter, query url.Values) (int, string, bool) {
	for key, values := range query {
		if key != "limit" && key != "cursor" {
			handler.writeError(writer, http.StatusBadRequest, "invalid_query", "unsupported query parameter "+key)
			return 0, "", false
		}
		if len(values) != 1 {
			handler.writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters must not be repeated")
			return 0, "", false
		}
	}
	limit := defaultPageLimit
	if text := query.Get("limit"); text != "" {
		parsed, err := strconv.Atoi(text)
		if err != nil || parsed < 1 || parsed > maximumPageLimit {
			handler.writeError(writer, http.StatusBadRequest, "invalid_query", "limit must be between 1 and 200")
			return 0, "", false
		}
		limit = parsed
	}
	return limit, query.Get("cursor"), true
}

func (handler *Handler) parseExactLookup(
	writer http.ResponseWriter,
	query url.Values,
	key string,
) (string, bool) {
	if len(query) != 1 {
		handler.writeError(writer, http.StatusBadRequest, "invalid_query", "exact lookup requires only "+key)
		return "", false
	}
	values, exists := query[key]
	if !exists || len(values) != 1 || values[0] == "" {
		handler.writeError(writer, http.StatusBadRequest, "invalid_query", key+" must appear exactly once and be non-empty")
		return "", false
	}
	return values[0], true
}

func paginate[T any](items []T, limit int, cursor string, key func(T) string) ([]T, string, error) {
	marker := ""
	if cursor != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || len(decoded) == 0 {
			return nil, "", fmt.Errorf("cursor is malformed")
		}
		marker = string(decoded)
	}
	start := 0
	for start < len(items) && key(items[start]) <= marker {
		start++
	}
	end := min(start+limit, len(items))
	page := items[start:end]
	next := ""
	if end < len(items) && len(page) > 0 {
		next = base64.RawURLEncoding.EncodeToString([]byte(key(page[len(page)-1])))
	}
	return page, next, nil
}

func trustKeyIdentity(record evaluation.GovernanceTrustKeyRecord) string {
	return record.Key.Authority + "\x00" + record.Key.KeyID + "\x00" + record.Key.Revision
}

func configRecordIdentity(record configrepo.Record) string {
	return record.Revision.ID + "\x00" + record.Revision.Revision
}

func validPathID(value string) bool {
	return value != "" && value != "." && value != ".." && !strings.Contains(value, "/")
}

func (handler *Handler) rejectQuery(writer http.ResponseWriter, query url.Values) bool {
	if len(query) != 0 {
		handler.writeError(writer, http.StatusBadRequest, "invalid_query", "query parameters are not accepted")
		return false
	}
	return true
}

func (handler *Handler) methodNotAllowed(writer http.ResponseWriter, allowed string) {
	writer.Header().Set("Allow", allowed)
	handler.writeError(writer, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
}

func (handler *Handler) writeDomainError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, evaluation.ErrUnauthorized):
		handler.writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for this operation")
	case errors.Is(err, evaluation.ErrNotFound):
		handler.writeError(writer, http.StatusNotFound, "not_found", "evaluation record not found")
	case errors.Is(err, evaluation.ErrConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, evaluation.ErrInvalidTransition), errors.Is(err, evaluation.ErrContaminated):
		handler.writeError(writer, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, evaluation.ErrCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "evaluation state is unavailable")
	default:
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "local API operation failed")
	}
}

func (handler *Handler) writeCalibrationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, calibration.ErrUnauthorized), errors.Is(err, evaluation.ErrUnauthorized):
		handler.writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for calibration")
	case errors.Is(err, calibration.ErrNotFound), errors.Is(err, evaluation.ErrNotFound), errors.Is(err, os.ErrNotExist):
		handler.writeError(writer, http.StatusNotFound, "not_found", "calibration source or run not found")
	case errors.Is(err, calibration.ErrConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, calibration.ErrContaminated), errors.Is(err, evaluation.ErrContaminated):
		handler.writeError(writer, http.StatusUnprocessableEntity, "calibration_contaminated", err.Error())
	case errors.Is(err, calibration.ErrCorrupt), errors.Is(err, runrepo.ErrArtifactQuarantined), errors.Is(err, runrepo.ErrArtifactTombstoned), errors.Is(err, runrepo.ErrArtifactIntegrity):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "calibration evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusBadRequest, "calibration_rejected", err.Error())
	}
}

func (handler *Handler) writeTrainingError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, training.ErrUnauthorized), errors.Is(err, evaluation.ErrUnauthorized):
		handler.writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for training data materialization")
	case errors.Is(err, training.ErrNotFound), errors.Is(err, training.ErrExportNotFound), errors.Is(err, training.ErrJobNotFound), errors.Is(err, evaluation.ErrNotFound), errors.Is(err, os.ErrNotExist):
		handler.writeError(writer, http.StatusNotFound, "not_found", "training source or manifest not found")
	case errors.Is(err, training.ErrConflict), errors.Is(err, training.ErrExportConflict), errors.Is(err, training.ErrJobConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, training.ErrContaminated), errors.Is(err, evaluation.ErrContaminated):
		handler.writeError(writer, http.StatusUnprocessableEntity, "training_contaminated", err.Error())
	case errors.Is(err, training.ErrJobInvalidTransition):
		handler.writeError(writer, http.StatusUnprocessableEntity, "training_job_transition_rejected", err.Error())
	case errors.Is(err, training.ErrCorrupt), errors.Is(err, training.ErrExportCorrupt), errors.Is(err, training.ErrJobCorrupt), errors.Is(err, runrepo.ErrArtifactQuarantined), errors.Is(err, runrepo.ErrArtifactTombstoned), errors.Is(err, runrepo.ErrArtifactIntegrity):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "training evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusBadRequest, "training_rejected", err.Error())
	}
}

func (handler *Handler) writeCalibrationPromotionError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, calibrationpromotion.ErrUnauthorized), errors.Is(err, calibration.ErrUnauthorized), errors.Is(err, evaluation.ErrUnauthorized):
		handler.writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for calibration promotion")
	case errors.Is(err, calibrationpromotion.ErrNotFound), errors.Is(err, calibration.ErrNotFound), errors.Is(err, evaluation.ErrNotFound), errors.Is(err, configrepo.ErrNotFound), errors.Is(err, os.ErrNotExist):
		handler.writeError(writer, http.StatusNotFound, "not_found", "calibration promotion source or plan not found")
	case errors.Is(err, calibrationpromotion.ErrConflict), errors.Is(err, calibration.ErrConflict), errors.Is(err, evaluation.ErrConflict), errors.Is(err, configrepo.ErrConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, calibrationpromotion.ErrInvalidTransition), errors.Is(err, calibrationpromotion.ErrEvidenceMismatch), errors.Is(err, evaluation.ErrInvalidTransition), errors.Is(err, evaluation.ErrContaminated), errors.Is(err, configrepo.ErrInvalidTransition):
		handler.writeError(writer, http.StatusUnprocessableEntity, "promotion_rejected", err.Error())
	case errors.Is(err, calibrationpromotion.ErrCorrupt), errors.Is(err, calibration.ErrCorrupt), errors.Is(err, evaluation.ErrCorrupt), errors.Is(err, configrepo.ErrCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "calibration promotion evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusBadRequest, "promotion_rejected", err.Error())
	}
}

func (handler *Handler) writePromotionMonitorError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, promotionmonitor.ErrUnauthorized), errors.Is(err, calibrationpromotion.ErrUnauthorized), errors.Is(err, evaluation.ErrUnauthorized):
		handler.writeError(writer, http.StatusForbidden, "forbidden", "principal is not authorized for calibration promotion monitoring")
	case errors.Is(err, promotionmonitor.ErrNotFound), errors.Is(err, calibrationpromotion.ErrNotFound), errors.Is(err, os.ErrNotExist):
		handler.writeError(writer, http.StatusNotFound, "not_found", "promotion observation or source not found")
	case errors.Is(err, promotionmonitor.ErrConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, promotionmonitor.ErrEvidenceMismatch):
		handler.writeError(writer, http.StatusUnprocessableEntity, "observation_rejected", err.Error())
	case errors.Is(err, promotionmonitor.ErrCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "promotion observation evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusBadRequest, "observation_rejected", err.Error())
	}
}

func (handler *Handler) writeConfigError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, configrepo.ErrNoPublishedConfig):
		handler.writeError(writer, http.StatusNotFound, "no_published_config", "no published configuration applies to the requested context")
	case errors.Is(err, configrepo.ErrNotFound):
		handler.writeError(writer, http.StatusNotFound, "not_found", "config revision not found")
	case errors.Is(err, configrepo.ErrConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, configrepo.ErrInvalidTransition):
		handler.writeError(writer, http.StatusUnprocessableEntity, "invalid_transition", err.Error())
	case errors.Is(err, configrepo.ErrCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "configuration state is unavailable")
	default:
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "configuration operation failed")
	}
}

func (handler *Handler) writeDashboardError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		handler.writeError(writer, http.StatusNotFound, "not_found", "dashboard snapshot not found")
	case errors.Is(err, analyticsadapter.ErrProjectionConflict):
		handler.writeError(writer, http.StatusConflict, "conflict", "dashboard snapshot conflicts with existing projection")
	case errors.Is(err, analyticsadapter.ErrProjectionCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "dashboard projection is unavailable")
	default:
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "dashboard query failed")
	}
}

func (handler *Handler) writeRunError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist):
		handler.writeError(writer, http.StatusNotFound, "not_found", "review run not found")
	case errors.Is(err, runrepo.ErrArtifactQuarantined),
		errors.Is(err, runrepo.ErrArtifactTombstoned),
		errors.Is(err, runrepo.ErrArtifactIntegrity):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "review run evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "review run operation failed")
	}
}

func (handler *Handler) writeFindingError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, controlplane.ErrFindingNotFound):
		handler.writeError(writer, http.StatusNotFound, "not_found", "review finding not found")
	case errors.Is(err, controlplane.ErrNoAuthoritativeFindings):
		handler.writeError(writer, http.StatusUnprocessableEntity, "findings_unavailable", "review run has no authoritative findings")
	case errors.Is(err, runrepo.ErrArtifactQuarantined),
		errors.Is(err, runrepo.ErrArtifactTombstoned),
		errors.Is(err, runrepo.ErrArtifactIntegrity):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "review finding evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "review finding operation failed")
	}
}

func (handler *Handler) writeFindingMutationError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, os.ErrNotExist), errors.Is(err, controlplane.ErrFindingNotFound):
		handler.writeError(writer, http.StatusNotFound, "not_found", "review finding not found")
	case errors.Is(err, controlplane.ErrNoAuthoritativeFindings):
		handler.writeError(writer, http.StatusUnprocessableEntity, "findings_unavailable", "review run has no authoritative findings")
	case errors.Is(err, findingdecision.ErrUnauthorized):
		handler.writeError(writer, http.StatusForbidden, "finding_write_forbidden", err.Error())
	case errors.Is(err, findingdecision.ErrConflict), errors.Is(err, feedback.ErrConflict),
		errors.Is(err, findingdecision.ErrInvalidTransition),
		errors.Is(err, feedback.ErrInvalidCorrection):
		handler.writeError(writer, http.StatusConflict, "finding_write_conflict", err.Error())
	case errors.Is(err, findingdecision.ErrCorrupt), errors.Is(err, feedback.ErrCorrupt),
		errors.Is(err, runrepo.ErrArtifactQuarantined),
		errors.Is(err, runrepo.ErrArtifactTombstoned),
		errors.Is(err, runrepo.ErrArtifactIntegrity):
		handler.writeError(writer, http.StatusInternalServerError, "internal_error", "review finding evidence is unavailable")
	default:
		handler.writeError(writer, http.StatusBadRequest, "finding_write_rejected", err.Error())
	}
}

func (handler *Handler) writeReviewJobError(writer http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, reviewjob.ErrNotFound), errors.Is(err, scheduling.ErrNotFound):
		handler.writeError(writer, http.StatusNotFound, "review_job_not_found", err.Error())
	case errors.Is(err, reviewjob.ErrConflict), errors.Is(err, scheduling.ErrConflict),
		errors.Is(err, scheduling.ErrInvalidTransition),
		errors.Is(err, configrepo.ErrNoPublishedConfig):
		handler.writeError(writer, http.StatusConflict, "review_job_conflict", err.Error())
	case errors.Is(err, reviewjob.ErrCorrupt), errors.Is(err, scheduling.ErrCorrupt):
		handler.writeError(writer, http.StatusInternalServerError, "review_job_corrupt", "review job state failed integrity validation")
	default:
		handler.writeError(writer, http.StatusBadRequest, "review_job_rejected", err.Error())
	}
}

func (handler *Handler) writeResponse(writer http.ResponseWriter, status int, data any) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(Response{SchemaVersion: ResponseSchemaVersion, Data: data})
}

func (handler *Handler) writeError(writer http.ResponseWriter, status int, code, message string) {
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(ErrorResponse{SchemaVersion: ErrorSchemaVersion, Code: code, Message: message})
}
