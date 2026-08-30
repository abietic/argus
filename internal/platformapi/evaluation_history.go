package platformapi

import (
	"net/http"

	"argus.local/argus/internal/evaluation"
)

// EvaluationHistoryReader is the read-only application port used by the local
// operator surface. The concrete evaluation repository satisfies it, while
// hosted deployments can supply a tenant-scoped adapter without granting HTTP
// handlers mutation access.
type EvaluationHistoryReader interface {
	ListEvaluationRuns(evaluation.Access) ([]evaluation.EvaluationRun, error)
	GetEvaluationRun(string, evaluation.Access) (evaluation.EvaluationRun, error)
	ListExperimentRuns(evaluation.Access) ([]evaluation.ExperimentRun, error)
	GetExperimentRun(string, evaluation.Access) (evaluation.ExperimentRun, error)
	ListRepeatabilityRuns(evaluation.Access) ([]evaluation.RepeatabilityRun, error)
	GetRepeatabilityRun(string, evaluation.Access) (evaluation.RepeatabilityRun, error)
	ListExperimentBatches(evaluation.Access) ([]evaluation.ExperimentBatchRecord, error)
	GetExperimentBatch(string, evaluation.Access) (evaluation.ExperimentBatchRecord, error)
	ListRepeatabilityBatches(evaluation.Access) ([]evaluation.RepeatabilityBatchRecord, error)
	GetRepeatabilityBatch(string, evaluation.Access) (evaluation.RepeatabilityBatchRecord, error)
}

// The evaluation history surface intentionally stays read-only. It reuses the
// domain repository's case-aware authorization instead of reconstructing ACLs
// in the HTTP adapter, and it keeps each immutable fact type distinct.

func (handler *Handler) handleEvaluationRuns(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	records, err := handler.evaluationHistory.ListEvaluationRuns(handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	items, next, err := paginate(records, limit, cursor, func(record evaluation.EvaluationRun) string {
		return record.EvaluationRunID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleEvaluationRun(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "evaluation_run_id")
	if !ok {
		return
	}
	record, err := handler.evaluationHistory.GetEvaluationRun(id, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleExperimentRuns(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	records, err := handler.evaluationHistory.ListExperimentRuns(handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	items, next, err := paginate(records, limit, cursor, func(record evaluation.ExperimentRun) string {
		return record.ExperimentRunID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleExperimentRun(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "experiment_run_id")
	if !ok {
		return
	}
	record, err := handler.evaluationHistory.GetExperimentRun(id, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleRepeatabilityRuns(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	records, err := handler.evaluationHistory.ListRepeatabilityRuns(handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	items, next, err := paginate(records, limit, cursor, func(record evaluation.RepeatabilityRun) string {
		return record.RepeatabilityRunID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleRepeatabilityRun(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "repeatability_run_id")
	if !ok {
		return
	}
	record, err := handler.evaluationHistory.GetRepeatabilityRun(id, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleExperimentBatches(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost {
		handler.handleExperimentBatchSubmit(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	records, err := handler.evaluationHistory.ListExperimentBatches(handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	items, next, err := paginate(records, limit, cursor, func(record evaluation.ExperimentBatchRecord) string {
		return record.Request.BatchID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleExperimentBatchSubmit(writer http.ResponseWriter, request *http.Request) {
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluationBatches != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command ExperimentBatchSubmitCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	mutation, err := command.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	variant, err := command.executionVariant()
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := handler.evaluationBatches.SubmitExperiment(
		request.Context(), command.Request, variant, mutation,
	)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusAccepted, record)
}

func (handler *Handler) handleExperimentBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "batch_id")
	if !ok {
		return
	}
	record, err := handler.evaluationHistory.GetExperimentBatch(id, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleExperimentBatchResume(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluationBatches != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "batch_id")
	if !ok {
		return
	}
	var command EvaluationBatchResumeCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	mutation, err := command.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := handler.evaluationBatches.ResumeExperiment(request.Context(), id, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusAccepted, record)
}

func (handler *Handler) handleRepeatabilityBatches(writer http.ResponseWriter, request *http.Request) {
	if request.Method == http.MethodPost {
		handler.handleRepeatabilityBatchSubmit(writer, request)
		return
	}
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
	if !ok {
		return
	}
	records, err := handler.evaluationHistory.ListRepeatabilityBatches(handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	items, next, err := paginate(records, limit, cursor, func(record evaluation.RepeatabilityBatchRecord) string {
		return record.Request.BatchID
	})
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
		return
	}
	handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
}

func (handler *Handler) handleRepeatabilityBatchSubmit(writer http.ResponseWriter, request *http.Request) {
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluationBatches != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command RepeatabilityBatchSubmitCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	mutation, err := command.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := handler.evaluationBatches.SubmitRepeatability(
		request.Context(), command.Request, mutation,
	)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusAccepted, record)
}

func (handler *Handler) handleRepeatabilityBatch(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationRead, handler.evaluationHistory != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "batch_id")
	if !ok {
		return
	}
	record, err := handler.evaluationHistory.GetRepeatabilityBatch(id, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleRepeatabilityBatchResume(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionEvaluationWrite, handler.evaluationBatches != nil) {
		return
	}
	id, ok := handler.parseExactLookup(writer, request.URL.Query(), "batch_id")
	if !ok {
		return
	}
	var command EvaluationBatchResumeCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	mutation, err := command.mutation(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := handler.evaluationBatches.ResumeRepeatability(request.Context(), id, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusAccepted, record)
}
