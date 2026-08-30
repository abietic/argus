package platformapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/abietic/argus/internal/evaluation"
	"github.com/abietic/argus/internal/reviewconfig"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

type agentComponentPublisherStub struct {
	request  AgentComponentPublicationRequest
	mutation AgentComponentPublicationMutation
	calls    int
}

func (stub *agentComponentPublisherStub) PublishAgentComponent(
	_ context.Context,
	request AgentComponentPublicationRequest,
	mutation AgentComponentPublicationMutation,
) (AgentComponentPublicationRecord, error) {
	stub.calls++
	stub.request = request
	stub.request.Content = bytes.Clone(request.Content)
	stub.mutation = mutation
	return AgentComponentPublicationRecord{
		SchemaVersion:       AgentComponentPublicationRecordSchemaVersion,
		BaselineReviewRunID: request.BaselineReviewRunID,
		Contract:            request.Contract,
		Binding: contractsv1alpha1.AgentStageComponentBinding{
			Ref: contractsv1alpha1.VersionedRef{
				ID: request.Ref.ID, Revision: request.Ref.Revision, SHA256: request.Ref.SHA256,
			},
			Artifact: contractsv1alpha1.ArtifactBinding{
				Ref: contractsv1alpha1.ContentRef{
					URI:    "artifact://local/sha256/" + request.Ref.SHA256,
					SHA256: request.Ref.SHA256, SizeBytes: int64(len(request.Content)),
				}, Contract: request.Contract,
			},
		},
		PublishedBy: mutation.Actor, PublishedAt: mutation.At,
	}, nil
}

func TestAgentComponentPublishAPIInjectsPrincipalAndRejectsCallerScope(t *testing.T) {
	prompt := contractsv1alpha1.DefaultAgentReviewPromptBundle()
	prompt.Revision = "platform-prompt-v2"
	prompt.ReviewSystemPrompt += "\nCheck exact state-machine invariants."
	content, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	at := time.Date(2026, 8, 26, 14, 0, 0, 0, time.UTC)
	command := AgentComponentPublishCommand{
		SchemaVersion:       AgentComponentPublishCommandSchemaVersion,
		BaselineReviewRunID: "review-baseline-1",
		Contract:            contractsv1alpha1.AgentStagePlanPromptContract,
		Ref: reviewconfig.VersionedRef{
			ID: "pi-review-prompts", Revision: prompt.Revision,
			SHA256: hex.EncodeToString(digest[:]),
		},
		ContentBase64: base64.StdEncoding.EncodeToString(content),
		Mutation: MutationInput{
			SchemaVersion:  MutationInputSchemaVersion,
			IdempotencyKey: "publish-prompt-v2", Audit: "publish governed prompt", At: at,
		},
	}
	publisher := &agentComponentPublisherStub{}
	principal := Principal{
		SchemaVersion: PrincipalSchemaVersion, Actor: "component-governor",
		Roles: []evaluation.Role{}, Permissions: []Permission{PermissionComponentWrite},
		ProfileRevision: "component-governor-v1",
	}
	handler, err := NewHandler(Services{Components: publisher}, principal, testToken)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(command)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/agent-components", bytes.NewReader(body),
	))
	if response.Code != http.StatusOK || publisher.calls != 1 ||
		publisher.request.BaselineReviewRunID != command.BaselineReviewRunID ||
		publisher.request.Ref != command.Ref || !bytes.Equal(publisher.request.Content, content) ||
		publisher.mutation.Actor != principal.Actor || publisher.mutation.Audit != command.Mutation.Audit {
		t.Fatalf("publish status=%d body=%s publisher=%+v", response.Code, response.Body.String(), publisher)
	}

	callerScope := strings.Replace(string(body), `"contract":`,
		`"tenant_id":"attacker","repository_id":"other","contract":`, 1)
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/agent-components", strings.NewReader(callerScope),
	))
	if response.Code != http.StatusBadRequest || publisher.calls != 1 {
		t.Fatalf("caller scope status=%d body=%s calls=%d", response.Code, response.Body.String(), publisher.calls)
	}

	readOnly := principal
	readOnly.Permissions = []Permission{PermissionConfigRead}
	readHandler, err := NewHandler(Services{Components: publisher}, readOnly, testToken)
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	readHandler.ServeHTTP(response, authenticatedRequest(
		http.MethodPost, "/v1/agent-components", bytes.NewReader(body),
	))
	if response.Code != http.StatusForbidden || publisher.calls != 1 {
		t.Fatalf("read-only publish status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestAgentComponentPublishCommandRejectsUnboundOrMalformedContent(t *testing.T) {
	prompt := contractsv1alpha1.DefaultAgentReviewPromptBundle()
	content, err := json.Marshal(prompt)
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	command := AgentComponentPublishCommand{
		SchemaVersion:       AgentComponentPublishCommandSchemaVersion,
		BaselineReviewRunID: "review-baseline-1",
		Contract:            contractsv1alpha1.AgentStagePlanPromptContract,
		Ref: reviewconfig.VersionedRef{
			ID: "pi-review-prompts", Revision: "different-revision",
			SHA256: hex.EncodeToString(digest[:]),
		},
		ContentBase64: base64.StdEncoding.EncodeToString(content),
		Mutation: MutationInput{
			SchemaVersion: MutationInputSchemaVersion, IdempotencyKey: "invalid-prompt",
			Audit: "must be rejected", At: time.Date(2026, 8, 26, 14, 5, 0, 0, time.UTC),
		},
	}
	if err := command.Validate(); err == nil || !strings.Contains(err.Error(), "revision") {
		t.Fatalf("revision-mismatched prompt error=%v", err)
	}
	command.Ref.Revision = prompt.Revision
	command.ContentBase64 = base64.RawStdEncoding.EncodeToString(content)
	if err := command.Validate(); err == nil || !strings.Contains(err.Error(), "canonical") {
		t.Fatalf("non-canonical base64 error=%v", err)
	}
}
