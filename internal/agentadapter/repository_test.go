package agentadapter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"argus.local/argus/internal/application"
	"argus.local/argus/internal/artifactrepo"
	"argus.local/argus/internal/runrepo"
	"argus.local/argus/internal/store/local"
	contractsv1alpha1 "argus.local/argus/pkg/contracts/v1alpha1"
)

func TestRepositoryProjectsExactReadOnlyArtifactAndRetriesIdempotently(t *testing.T) {
	repository, runs, governed := newTestRepository(t)
	content := []byte(`{"schema_version":"argus.test.v1alpha1"}`)
	localRef, err := runs.PutArtifact("argus.test.v1alpha1", content)
	if err != nil {
		t.Fatal(err)
	}
	subject := testSubject()
	request := application.AgentArtifactProjectionRequest{
		Role: "test-input", ReviewRunID: "run-1", StageID: "agent_hypothesize",
		Source: localRef, ExactContent: content,
		FrozenAt: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
	}
	first, err := repository.ProjectReadOnly(context.Background(), subject, request)
	if err != nil {
		t.Fatalf("ProjectReadOnly() error = %v", err)
	}
	second, err := repository.ProjectReadOnly(context.Background(), subject, request)
	if err != nil {
		t.Fatalf("ProjectReadOnly(retry) error = %v", err)
	}
	if first != second || first.Ref.SHA256 != localRef.SHA256 ||
		first.Ref.SizeBytes != localRef.SizeBytes || first.Contract != localRef.Contract ||
		strings.HasPrefix(first.Ref.URI, "artifact://local/") {
		t.Fatalf("projection = %+v / %+v, local = %+v", first, second, localRef)
	}
	resolved, err := repository.VerifyRead(context.Background(), subject, first)
	if err != nil || string(resolved) != string(content) {
		t.Fatalf("VerifyRead() = %q, %v", resolved, err)
	}
	record, err := governed.Inspect(
		context.Background(),
		subjectForArtifacts(subject),
		artifactrepo.Ref{
			URI: first.Ref.URI, SHA256: first.Ref.SHA256,
			SizeBytes: first.Ref.SizeBytes, Contract: first.Contract,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != artifactrepo.StateActive ||
		len(record.Metadata.AllowedUses) != 1 ||
		record.Metadata.AllowedUses[0] != artifactrepo.UseRead {
		t.Fatalf("governed metadata = %+v", record)
	}
}

func TestRepositoryProjectionFailsClosedBeforeGovernedPut(t *testing.T) {
	repository, runs, _ := newTestRepository(t)
	content := []byte("exact")
	localRef, err := runs.PutArtifact("argus.test.v1alpha1", content)
	if err != nil {
		t.Fatal(err)
	}
	base := application.AgentArtifactProjectionRequest{
		Role: "input", ReviewRunID: "run-1", StageID: "agent_hypothesize",
		Source: localRef, ExactContent: content,
		FrozenAt: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
	}
	tests := []struct {
		name    string
		subject application.AgentPlanningSubject
		mutate  func(*application.AgentArtifactProjectionRequest)
	}{
		{
			name: "content mismatch", subject: testSubject(),
			mutate: func(request *application.AgentArtifactProjectionRequest) {
				request.ExactContent = []byte("other")
			},
		},
		{
			name: "non UTC frozen time", subject: testSubject(),
			mutate: func(request *application.AgentArtifactProjectionRequest) {
				request.FrozenAt = request.FrozenAt.In(time.FixedZone("offset", 3600))
			},
		},
		{
			name: "artifact incompatible subject",
			subject: application.AgentPlanningSubject{
				TenantID: "租户", OrganizationID: "org-1",
				WorkspaceID: "workspace-1", RepositoryID: "repo-1",
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := base
			if test.mutate != nil {
				test.mutate(&request)
			}
			if _, err := repository.ProjectReadOnly(
				context.Background(), test.subject, request,
			); err == nil {
				t.Fatal("ProjectReadOnly() accepted invalid input")
			}
		})
	}
}

func TestRepositoryVerifyReadEnforcesHostSubjectAndCanonicalGovernedURI(t *testing.T) {
	repository, runs, _ := newTestRepository(t)
	content := []byte("context")
	localRef, err := runs.PutArtifact("argus.context.test.v1alpha1", content)
	if err != nil {
		t.Fatal(err)
	}
	subject := testSubject()
	binding, err := repository.ProjectReadOnly(
		context.Background(),
		subject,
		application.AgentArtifactProjectionRequest{
			Role: "context", ReviewRunID: "run-1", StageID: "agent_hypothesize",
			Source: localRef, ExactContent: content,
			FrozenAt: time.Date(2026, 8, 24, 1, 2, 3, 0, time.UTC),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	other := subject
	other.WorkspaceID = "workspace-2"
	if _, err := repository.VerifyRead(
		context.Background(), other, binding,
	); !errors.Is(err, artifactrepo.ErrUnauthorized) {
		t.Fatalf("cross-workspace VerifyRead() error = %v", err)
	}
	generic := contractsv1alpha1.ArtifactBinding{
		Ref: contractsv1alpha1.ContentRef{
			URI: "artifact://argus/contexts/1", SHA256: localRef.SHA256,
			SizeBytes: localRef.SizeBytes,
		},
		Contract: localRef.Contract,
	}
	if _, err := repository.VerifyRead(context.Background(), subject, generic); err == nil {
		t.Fatal("VerifyRead() accepted a generic non-governed URI")
	}
}

func newTestRepository(
	t *testing.T,
) (*Repository, *runrepo.Repository, *artifactrepo.Repository) {
	t.Helper()
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runrepo.New(store)
	if err != nil {
		t.Fatal(err)
	}
	governed, err := artifactrepo.Open(store, nil)
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(runs, governed, artifactrepo.DefaultAuthority)
	if err != nil {
		t.Fatal(err)
	}
	return repository, runs, governed
}

func testSubject() application.AgentPlanningSubject {
	return application.AgentPlanningSubject{
		TenantID: "tenant-1", OrganizationID: "org-1",
		WorkspaceID: "workspace-1", RepositoryID: "repo-1",
	}
}
