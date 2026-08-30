package platformapi

import (
	"errors"
	"net/http"
	"os"

	"argus.local/argus/internal/agentcomponentrepo"
)

func (handler *Handler) handleAgentComponentPublish(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		handler.methodNotAllowed(writer, http.MethodPost)
		return
	}
	if !handler.authorize(writer, PermissionComponentWrite, handler.components != nil) {
		return
	}
	if !handler.rejectQuery(writer, request.URL.Query()) {
		return
	}
	var command AgentComponentPublishCommand
	if !handler.decodeRequest(writer, request, &command) {
		return
	}
	publication, mutation, err := command.publication(handler.principal)
	if err != nil {
		handler.writeError(writer, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	record, err := handler.components.PublishAgentComponent(request.Context(), publication, mutation)
	if err != nil {
		switch {
		case errors.Is(err, os.ErrNotExist):
			handler.writeError(writer, http.StatusNotFound, "baseline_review_run_not_found", "baseline ReviewRun was not found")
		case errors.Is(err, agentcomponentrepo.ErrComponentConflict):
			handler.writeError(writer, http.StatusConflict, "component_conflict", err.Error())
		default:
			handler.writeError(writer, http.StatusConflict, "component_publication_rejected", err.Error())
		}
		return
	}
	handler.writeResponse(writer, http.StatusOK, record)
}
