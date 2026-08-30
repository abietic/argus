package main

import (
	"context"
	"fmt"

	"argus.local/argus/internal/agentcomponentrepo"
	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/platformapi"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

// localAgentComponentPublisher is a narrow Argus governance adapter. Its
// subject is derived from a fully committed ReviewRun, never accepted from an
// HTTP request or inferred from the process working directory.
type localAgentComponentPublisher struct {
	runs       *runrepo.Repository
	components *agentcomponentrepo.Repository
}

func newLocalAgentComponentPublisher(
	store *local.Store,
	runs *runrepo.Repository,
) (*localAgentComponentPublisher, error) {
	if store == nil || runs == nil {
		return nil, fmt.Errorf("component publisher store and run repository are required")
	}
	artifacts, err := artifactrepo.Open(store, nil)
	if err != nil {
		return nil, err
	}
	components, err := agentcomponentrepo.New(store, artifacts, artifactrepo.DefaultAuthority)
	if err != nil {
		return nil, err
	}
	return &localAgentComponentPublisher{runs: runs, components: components}, nil
}

func (publisher *localAgentComponentPublisher) PublishAgentComponent(
	ctx context.Context,
	request platformapi.AgentComponentPublicationRequest,
	mutation platformapi.AgentComponentPublicationMutation,
) (platformapi.AgentComponentPublicationRecord, error) {
	if publisher == nil || publisher.runs == nil || publisher.components == nil {
		return platformapi.AgentComponentPublicationRecord{}, fmt.Errorf("component publisher is unavailable")
	}
	result, err := publisher.runs.LoadCommittedRunResult(request.BaselineReviewRunID)
	if err != nil {
		return platformapi.AgentComponentPublicationRecord{}, err
	}
	snapshot, err := publisher.runs.LoadExecutionSnapshot(result.Run.ExecutionSnapshotID)
	if err != nil {
		return platformapi.AgentComponentPublicationRecord{}, err
	}
	var spec contractsv1alpha1.ReviewSpec
	if err := publisher.runs.ReadJSONArtifact(snapshot.ReviewSpecRef, &spec); err != nil {
		return platformapi.AgentComponentPublicationRecord{}, err
	}
	subject := agentcomponentrepo.Subject{
		TenantID: spec.TenantID, OrganizationID: "local",
		WorkspaceID: spec.WorkspaceID, RepositoryID: spec.Repository.RepositoryID,
	}
	binding, err := publisher.components.Publish(ctx, subject, agentcomponentrepo.Publication{
		Ref: request.Ref, Contract: request.Contract, Content: request.Content,
		PublishedAt: mutation.At,
		Mutation: &agentcomponentrepo.Mutation{
			IdempotencyKey: mutation.IdempotencyKey,
			Actor:          mutation.Actor, Audit: mutation.Audit, At: mutation.At,
		},
	})
	if err != nil {
		return platformapi.AgentComponentPublicationRecord{}, err
	}
	return platformapi.AgentComponentPublicationRecord{
		SchemaVersion:       platformapi.AgentComponentPublicationRecordSchemaVersion,
		BaselineReviewRunID: request.BaselineReviewRunID,
		Contract:            request.Contract, Binding: binding,
		PublishedBy: mutation.Actor, PublishedAt: mutation.At,
	}, nil
}
