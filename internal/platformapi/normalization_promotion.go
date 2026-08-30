package platformapi

import (
	"fmt"
	"net/http"

	"github.com/abietic/argus/internal/evaluation"
)

func (handler *Handler) authorizeNormalizationPromotion(writer http.ResponseWriter, write bool) bool {
	if handler.normalizationPromotion == nil {
		handler.writeError(writer, http.StatusServiceUnavailable, "service_unavailable", "normalization promotion service is unavailable")
		return false
	}
	permission := PermissionEvaluationRead
	if write {
		permission = PermissionEvaluationWrite
	}
	return handler.authorize(writer, permission, true)
}

func (handler *Handler) authorizeNormalizationPromotionConfig(writer http.ResponseWriter, write bool) bool {
	permission := PermissionConfigRead
	if write {
		permission = PermissionConfigWrite
	}
	return handler.authorize(writer, permission, handler.config != nil)
}

func (handler *Handler) handleNormalizationPromotions(writer http.ResponseWriter, request *http.Request) {
	switch request.Method {
	case http.MethodGet:
		if !handler.authorizeNormalizationPromotion(writer, false) {
			return
		}
		limit, cursor, ok := handler.parsePage(writer, request.URL.Query())
		if !ok {
			return
		}
		records, err := handler.normalizationPromotion.List(handler.principal.Access())
		if err != nil {
			handler.writeDomainError(writer, err)
			return
		}
		items, next, err := paginate(records, limit, cursor, func(record evaluation.PromotionRecord) string {
			return record.Variant.VariantID
		})
		if err != nil {
			handler.writeError(writer, http.StatusBadRequest, "invalid_cursor", err.Error())
			return
		}
		handler.writeResponse(writer, http.StatusOK, Page{Items: items, NextCursor: next})
	case http.MethodPost:
		if !handler.authorizeNormalizationPromotion(writer, true) ||
			!handler.authorizeNormalizationPromotionConfig(writer, true) ||
			!handler.rejectQuery(writer, request.URL.Query()) {
			return
		}
		var command NormalizationPromotionPrepareCommand
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
		preparation, err := handler.normalizationPromotion.Prepare(request.Context(), command.Request, mutation)
		if err != nil {
			handler.writeDomainError(writer, err)
			return
		}
		handler.writeResponse(writer, http.StatusCreated, preparation)
	default:
		handler.methodNotAllowed(writer, http.MethodGet+", "+http.MethodPost)
	}
}

func (handler *Handler) handleNormalizationPromotion(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		handler.methodNotAllowed(writer, http.MethodGet)
		return
	}
	if !handler.authorizeNormalizationPromotion(writer, false) {
		return
	}
	variantID, ok := handler.parseExactLookup(writer, request.URL.Query(), "variant_id")
	if !ok {
		return
	}
	record, err := handler.normalizationPromotion.Get(variantID, handler.principal.Access())
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}

func (handler *Handler) handleNormalizationPromotionGate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeNormalizationPromotion(writer, true) || !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command NormalizationPromotionGateCommand
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
	output, err := handler.normalizationPromotion.RecordQualityGate(request.Context(), command.Request, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, output)
}

func (handler *Handler) handleNormalizationPromotionOperationalGate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeNormalizationPromotion(writer, true) ||
		!handler.authorizeNormalizationPromotionConfig(writer, false) ||
		!handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command NormalizationPromotionOperationalGateCommand
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
	output, err := handler.normalizationPromotion.RecordOperationalGate(request.Context(), command.Request, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, output)
}

func (handler *Handler) handleNormalizationPromotionCanary(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeNormalizationPromotion(writer, true) ||
		!handler.authorizeNormalizationPromotionConfig(writer, true) ||
		!handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command NormalizationPromotionCanaryCommand
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
	output, err := handler.normalizationPromotion.StartCanary(request.Context(), command.VariantID, command.PolicyRef, command.Rollout, mutation)
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, output)
}

func (handler *Handler) handleNormalizationPromotionLifecycle(writer http.ResponseWriter, request *http.Request, action string) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorizeNormalizationPromotion(writer, true) ||
		!handler.authorizeNormalizationPromotionConfig(writer, true) ||
		!handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command NormalizationPromotionLifecycleCommand
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
	var output any
	switch action {
	case "activate":
		output, err = handler.normalizationPromotion.Activate(request.Context(), command.VariantID, mutation)
	case "rollback":
		output, err = handler.normalizationPromotion.Rollback(request.Context(), command.VariantID, mutation)
	default:
		err = fmt.Errorf("unsupported normalization promotion lifecycle action %q", action)
	}
	if err != nil {
		handler.writeDomainError(writer, err)
		return
	}
	handler.writeResponse(writer, http.StatusOK, output)
}
