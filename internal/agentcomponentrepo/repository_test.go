package agentcomponentrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/abietic/argus/internal/application"
	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

func TestResolverPublishesAndClosesExactOrderedComponents(t *testing.T) {
	t.Parallel()
	repository, resolver, subject, publishedAt := newComponentFixture(t)
	request := application.AgentComponentResolutionRequest{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		WorkspaceID: subject.WorkspaceID, RepositoryID: subject.RepositoryID,
		PolicySHA256: componentDigest([]byte("policy")),
		Agent: publishComponent(t, repository, subject, publishedAt,
			contractsv1alpha1.AgentStagePlanAgentContract, "pi-agent", []byte("agent-v1")),
		Provider: publishComponent(t, repository, subject, publishedAt,
			contractsv1alpha1.AgentStagePlanProviderContract, "deepseek-anthropic", []byte("provider-v1")),
		Model: publishComponent(t, repository, subject, publishedAt,
			contractsv1alpha1.AgentStagePlanModelContract, "deepseek-model", []byte("model-v1")),
		Runtime: publishComponent(t, repository, subject, publishedAt,
			contractsv1alpha1.AgentStagePlanRuntimeContract, "pi-review-worker", []byte("runtime-v1")),
		Prompt: publishComponent(t, repository, subject, publishedAt,
			contractsv1alpha1.AgentStagePlanPromptContract, "pi-review-prompt", []byte("prompt-v1")),
		APIProtocol: publishComponent(t, repository, subject, publishedAt,
			contractsv1alpha1.AgentStagePlanAPIProtocolContract, "anthropic-messages", []byte("protocol-v1")),
	}
	for index, definition := range []struct {
		id      string
		phase   reviewconfig.AgentSkillPhase
		content string
	}{
		{"grouping", reviewconfig.AgentSkillPhaseGrouping, "grouping-skill"},
		{"context", reviewconfig.AgentSkillPhaseContext, "context-skill"},
		{"correctness", reviewconfig.AgentSkillPhaseReview, "correctness-skill"},
		{"verification", reviewconfig.AgentSkillPhaseVerification, "verification-skill"},
	} {
		ref := publishComponent(
			t,
			repository,
			subject,
			publishedAt.Add(time.Duration(index)*time.Second),
			contractsv1alpha1.AgentStagePlanSkillContract,
			definition.id,
			[]byte(definition.content),
		)
		request.Skills = append(request.Skills, reviewconfig.AgentSkillPackDefinition{
			ID: definition.id, Phase: definition.phase, Ref: ref,
		})
	}
	knowledgeRef := publishComponent(
		t, repository, subject, publishedAt.Add(10*time.Second),
		contractsv1alpha1.AgentStagePlanKnowledgeContract,
		"business-rules", []byte("knowledge-v1"),
	)
	request.Knowledge = []reviewconfig.AgentKnowledgePackDefinition{{
		ID: "business-rules", Ref: knowledgeRef,
	}}
	contextRef := publishComponent(
		t, repository, subject, publishedAt.Add(11*time.Second),
		contractsv1alpha1.AgentStagePlanContextProviderAdapterContract,
		"codegraph-adapter", []byte("context-adapter-v1"),
	)
	request.ContextProviders = []reviewconfig.ContextProviderDefinition{{
		ID: "codegraph", Revision: "v1", Kind: "codegraph", Adapter: contextRef,
	}}

	resolution := reviewconfig.ResolutionContext{
		TenantID: subject.TenantID, OrganizationID: subject.OrganizationID,
		RepositoryID: subject.RepositoryID, Path: "", InvocationID: "review-run-1",
	}
	components, err := resolver.ResolveAgentComponents(
		context.Background(), resolution, request,
	)
	if err != nil {
		t.Fatalf("ResolveAgentComponents() error = %v", err)
	}
	if components.Agent.Ref.ID != request.Agent.ID ||
		components.Provider.Ref.ID != request.Provider.ID ||
		components.Model.Ref.ID != request.Model.ID ||
		components.Runtime.Ref.ID != request.Runtime.ID ||
		components.Prompt.Ref.ID != request.Prompt.ID ||
		components.APIProtocol.Ref.ID != request.APIProtocol.ID ||
		len(components.Skills) != len(request.Skills) ||
		len(components.Knowledge) != 1 || len(components.ContextProviders) != 1 {
		t.Fatalf("resolved component closure is incomplete: %+v", components)
	}
	for index, skill := range components.Skills {
		if skill.ID != request.Skills[index].ID ||
			skill.Phase != contractsv1alpha1.AgentStageSkillPhase(request.Skills[index].Phase) ||
			skill.Ref.SHA256 != skill.Artifact.Ref.SHA256 ||
			skill.Artifact.Contract != contractsv1alpha1.AgentStagePlanSkillContract {
			t.Fatalf("skill[%d] lost governed order or closure: %+v", index, skill)
		}
	}

	changedSubject := request
	changedSubject.WorkspaceID = "workspace-2"
	if _, err := resolver.ResolveAgentComponents(
		context.Background(), resolution, changedSubject,
	); err == nil {
		t.Fatal("subject-confused component request was accepted")
	}
}

func TestRepositoryExactRetryConcurrentAndChangedIdentityConflict(t *testing.T) {
	t.Parallel()
	repository, _, subject, publishedAt := newComponentFixture(t)
	content := []byte("immutable prompt bytes")
	publication := Publication{
		Ref: reviewconfig.VersionedRef{
			ID: "prompt", Revision: "v1", SHA256: componentDigest(content),
		},
		Contract:    contractsv1alpha1.AgentStagePlanPromptContract,
		Content:     content,
		PublishedAt: publishedAt,
	}
	const workers = 32
	results := make(chan contractsv1alpha1.AgentStageComponentBinding, workers)
	errorsSeen := make(chan error, workers)
	var group sync.WaitGroup
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			binding, err := repository.Publish(context.Background(), subject, publication)
			if err != nil {
				errorsSeen <- err
				return
			}
			results <- binding
		}()
	}
	group.Wait()
	close(results)
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatalf("concurrent exact Publish() error = %v", err)
	}
	var winner contractsv1alpha1.AgentStageComponentBinding
	count := 0
	for binding := range results {
		count++
		if winner.Ref.ID == "" {
			winner = binding
		} else if binding != winner {
			t.Fatalf("concurrent exact publish returned changed binding: %+v != %+v", binding, winner)
		}
	}
	if count != workers {
		t.Fatalf("successful exact publishes = %d, want %d", count, workers)
	}

	changed := publication
	changed.Content = []byte("different prompt bytes")
	changed.Ref.SHA256 = componentDigest(changed.Content)
	if _, err := repository.Publish(context.Background(), subject, changed); !errors.Is(err, ErrComponentConflict) {
		t.Fatalf("changed immutable identity error = %v", err)
	}
	if _, err := repository.Publish(
		context.Background(),
		subject,
		Publication{
			Ref: publication.Ref, Contract: publication.Contract,
			Content: publication.Content, PublishedAt: publishedAt.Add(time.Second),
		},
	); !errors.Is(err, ErrComponentConflict) {
		t.Fatalf("changed exact-retry timestamp error = %v", err)
	}
}

func TestRepositoryMissingAndQuarantinedComponentsFailClosed(t *testing.T) {
	t.Parallel()
	repository, _, subject, publishedAt := newComponentFixture(t)
	missing := reviewconfig.VersionedRef{
		ID: "missing", Revision: "v1", SHA256: componentDigest([]byte("missing")),
	}
	if _, err := repository.Resolve(
		context.Background(), subject,
		contractsv1alpha1.AgentStagePlanModelContract, missing,
	); !errors.Is(err, ErrComponentNotFound) {
		t.Fatalf("missing component error = %v", err)
	}
	if _, err := repository.Publish(
		context.Background(), subject,
		Publication{
			Ref: missing, Contract: contractsv1alpha1.AgentStagePlanModelContract,
			Content: []byte("not-the-declared-digest"), PublishedAt: publishedAt,
		},
	); err == nil {
		t.Fatal("component bytes not bound by declared digest were accepted")
	}
}

func TestRepositoryResolveWithContentReturnsExactDefensiveCopy(t *testing.T) {
	t.Parallel()
	repository, _, subject, publishedAt := newComponentFixture(t)
	content := []byte("immutable governed prompt bytes")
	ref := publishComponent(
		t,
		repository,
		subject,
		publishedAt,
		contractsv1alpha1.AgentStagePlanPromptContract,
		"pi-review-prompts",
		content,
	)

	resolved, err := repository.ResolveWithContent(
		context.Background(),
		subject,
		contractsv1alpha1.AgentStagePlanPromptContract,
		ref,
	)
	if err != nil {
		t.Fatalf("ResolveWithContent() error = %v", err)
	}
	if resolved.Binding.Ref.ID != ref.ID ||
		resolved.Binding.Ref.Revision != ref.Revision ||
		resolved.Binding.Ref.SHA256 != ref.SHA256 ||
		resolved.Binding.Artifact.Ref.SHA256 != ref.SHA256 ||
		resolved.Binding.Artifact.Contract != contractsv1alpha1.AgentStagePlanPromptContract ||
		string(resolved.Content) != string(content) {
		t.Fatalf("ResolveWithContent() = %+v", resolved)
	}

	resolved.Content[0] = 'X'
	reloaded, err := repository.ResolveWithContent(
		context.Background(),
		subject,
		contractsv1alpha1.AgentStagePlanPromptContract,
		ref,
	)
	if err != nil {
		t.Fatalf("second ResolveWithContent() error = %v", err)
	}
	if string(reloaded.Content) != string(content) {
		t.Fatalf("returned bytes mutated durable component: got %q want %q", reloaded.Content, content)
	}
}

func TestRepositoryPrincipalBoundPublicationRetryCannotChangeAudit(t *testing.T) {
	t.Parallel()
	repository, _, subject, publishedAt := newComponentFixture(t)
	content := []byte("# Governed skill\n\nVerify exact state transitions.\n")
	publication := Publication{
		Ref: reviewconfig.VersionedRef{
			ID: "correctness", Revision: "platform-v2", SHA256: componentDigest(content),
		},
		Contract: contractsv1alpha1.AgentStagePlanSkillContract,
		Content:  content, PublishedAt: publishedAt,
		Mutation: &Mutation{
			IdempotencyKey: "publish-correctness-platform-v2",
			Actor:          "component-governor", Audit: "publish reviewed correctness skill", At: publishedAt,
		},
	}
	first, err := repository.Publish(t.Context(), subject, publication)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := repository.Publish(t.Context(), subject, publication)
	if err != nil || retry != first {
		t.Fatalf("exact principal-bound retry=(%+v,%v), want %+v", retry, err, first)
	}
	changed := publication
	changed.Mutation = &Mutation{
		IdempotencyKey: publication.Mutation.IdempotencyKey,
		Actor:          "different-actor", Audit: publication.Mutation.Audit, At: publishedAt,
	}
	if _, err := repository.Publish(t.Context(), subject, changed); !errors.Is(err, ErrComponentConflict) {
		t.Fatalf("changed publication actor error=%v", err)
	}
}

func newComponentFixture(
	t *testing.T,
) (*Repository, *Resolver, Subject, time.Time) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	publishedAt := time.Date(2026, 8, 24, 8, 0, 0, 0, time.UTC)
	artifacts, err := artifactrepo.OpenForAuthority(
		store,
		artifactrepo.DefaultAuthority,
		func() time.Time { return publishedAt },
	)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store, artifacts, artifactrepo.DefaultAuthority)
	if err != nil {
		t.Fatal(err)
	}
	subject := Subject{
		TenantID: "tenant-1", OrganizationID: "organization-1",
		WorkspaceID: "workspace-1", RepositoryID: "repository-1",
	}
	resolver, err := NewResolver(repository, subject)
	if err != nil {
		t.Fatal(err)
	}
	return repository, resolver, subject, publishedAt
}

func publishComponent(
	t *testing.T,
	repository *Repository,
	subject Subject,
	publishedAt time.Time,
	contract string,
	id string,
	content []byte,
) reviewconfig.VersionedRef {
	t.Helper()
	ref := reviewconfig.VersionedRef{
		ID: id, Revision: "v1", SHA256: componentDigest(content),
	}
	binding, err := repository.Publish(context.Background(), subject, Publication{
		Ref: ref, Contract: contract, Content: content, PublishedAt: publishedAt,
	})
	if err != nil {
		t.Fatalf("publish %s: %v", id, err)
	}
	if binding.Ref.ID != id || binding.Ref.SHA256 != ref.SHA256 ||
		binding.Artifact.Contract != contract {
		t.Fatalf("published component binding mismatch: %+v", binding)
	}
	// Prove exact retry does not create a second identity or mutate publication.
	retry, err := repository.Publish(context.Background(), subject, Publication{
		Ref: ref, Contract: contract, Content: content, PublishedAt: publishedAt,
	})
	if err != nil || retry != binding {
		t.Fatalf("retry publish %s = (%+v, %v), want %+v", id, retry, err, binding)
	}
	return ref
}

func componentDigest(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}
