package configrepo

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"argus.local/argus/internal/configdefaults"
	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/workflow"
)

func TestResolvePublishedWithReceiptBindsAtomicLedgerSelection(t *testing.T) {
	t.Parallel()
	repository, _ := newConfigRepository(t)
	baseline := completePublishedRevision(t, "platform", "1")
	createValidatePublish(t, repository, baseline, Rollout{Percentage: 100}, 1)
	resolutionContext := testResolutionContext("receipt-invocation")

	bundle, receipt, err := repository.ResolvePublishedWithReceipt(
		context.Background(),
		resolutionContext,
	)
	if err != nil {
		t.Fatalf("ResolvePublishedWithReceipt() error = %v", err)
	}
	if err := receipt.ValidateAgainst(bundle); err != nil {
		t.Fatalf("receipt.ValidateAgainst() error = %v", err)
	}
	if len(receipt.Revisions) != 1 ||
		receipt.Revisions[0].Source != bundle.AppliedRevisions[0] ||
		receipt.Revisions[0].PublishSequence == 0 ||
		receipt.Revisions[0].PublishEventID == "" {
		t.Fatalf("resolution receipt does not close the applied revision: %+v", receipt)
	}
	legacy, err := repository.ResolvePublished(context.Background(), resolutionContext)
	if err != nil || !reflect.DeepEqual(legacy, bundle) {
		t.Fatalf("ResolvePublished() = %+v, %v; want receipt bundle", legacy, err)
	}

	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := reviewconfig.DecodeConfigResolutionReceipt(data)
	if err != nil {
		t.Fatalf("DecodeConfigResolutionReceipt() error = %v", err)
	}
	if err := decoded.ValidateAgainst(bundle); err != nil {
		t.Fatalf("decoded receipt validation error = %v", err)
	}
	tampered := decoded
	tampered.Revisions = append([]reviewconfig.PublishedRevisionBinding{}, decoded.Revisions...)
	tampered.Revisions[0].AssignmentSHA256 = strings.Repeat("f", 64)
	if err := tampered.ValidateAgainst(bundle); err == nil ||
		!strings.Contains(err.Error(), "identity") {
		t.Fatalf("tampered receipt validation = %v, want self-digest rejection", err)
	}
}

func TestConfigResolutionReceiptStrictDecode(t *testing.T) {
	for name, data := range map[string][]byte{
		"null":      []byte("null"),
		"unknown":   []byte(`{"unknown":true}`),
		"duplicate": []byte(`{"schema_version":"a","schema_version":"b"}`),
		"trailing":  []byte(`{} {}`),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := reviewconfig.DecodeConfigResolutionReceipt(data); err == nil {
				t.Fatal("DecodeConfigResolutionReceipt() accepted invalid JSON")
			}
		})
	}
}

func TestResolvePublishedUsesOnlyApplicablePublishedRevisions(t *testing.T) {
	t.Parallel()
	repository, _ := newConfigRepository(t)
	baseline := completePublishedRevision(t, "platform", "1")
	createValidatePublish(
		t,
		repository,
		baseline,
		Rollout{Percentage: 100},
		1,
	)
	draft := testRevision("draft-override", "1", 7)
	createRevision(t, repository, draft, 4)

	resolutionContext := testResolutionContext("invocation-1")
	bundle, err := repository.ResolvePublished(context.Background(), resolutionContext)
	if err != nil {
		t.Fatalf("ResolvePublished() error = %v", err)
	}
	if bundle.Context != resolutionContext ||
		len(bundle.AppliedRevisions) != 1 ||
		bundle.AppliedRevisions[0].ID != baseline.ID ||
		bundle.Target.MaxFiles == 7 {
		t.Fatalf("resolved bundle included non-published revision: %+v", bundle)
	}
}

func TestResolvePublishedFailsClosedWithoutApplicablePublication(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		prepare func(*testing.T, *Repository)
	}{
		{
			name: "draft only",
			prepare: func(t *testing.T, repository *Repository) {
				createRevision(t, repository, testRevision("draft", "1", 10), 1)
			},
		},
		{
			name: "selector mismatch",
			prepare: func(t *testing.T, repository *Repository) {
				revision := testRevision("path-only", "1", 10)
				revision.Scope = reviewconfig.ScopePath
				revision.Selector = reviewconfig.Selector{
					TenantID:       "tenant-1",
					OrganizationID: "organization-1",
					RepositoryID:   "repository-1",
					PathPrefix:     "another",
				}
				createValidatePublish(
					t,
					repository,
					revision,
					Rollout{Percentage: 100},
					1,
				)
			},
		},
	}
	for _, test := range tests {
		test := test
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			repository, _ := newConfigRepository(t)
			test.prepare(t, repository)
			_, err := repository.ResolvePublished(
				context.Background(),
				testResolutionContext("invocation-1"),
			)
			if !errors.Is(err, ErrNoPublishedConfig) {
				t.Fatalf("ResolvePublished() error = %v, want ErrNoPublishedConfig", err)
			}
		})
	}
}

func TestResolvePublishedRejectsIncompletePublishedLayers(t *testing.T) {
	t.Parallel()
	repository, _ := newConfigRepository(t)
	incomplete := testRevision("incomplete", "1", 10)
	createValidatePublish(
		t,
		repository,
		incomplete,
		Rollout{Percentage: 100},
		1,
	)
	_, err := repository.ResolvePublished(
		context.Background(),
		testResolutionContext("invocation-1"),
	)
	if err == nil || !strings.Contains(err.Error(), "required config field") {
		t.Fatalf("ResolvePublished(incomplete) error = %v", err)
	}
}

func TestResolvePublishedHonorsCancellation(t *testing.T) {
	t.Parallel()
	repository, _ := newConfigRepository(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := repository.ResolvePublished(ctx, testResolutionContext("invocation-1"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ResolvePublished(canceled) error = %v", err)
	}
}

func completePublishedRevision(
	t *testing.T,
	id string,
	revision string,
) reviewconfig.Revision {
	t.Helper()
	value, err := configdefaults.Revision(configdefaults.Options{
		ID:             id,
		Revision:       revision,
		MaxFiles:       100,
		MaxPatchBytes:  1 << 20,
		MaxInputBytes:  1 << 20,
		MaxOutputBytes: 1 << 20,
		MaxAttempts:    2,
		AllowedModes:   []string{"diff", "scope", "selection"},
		TargetInclude:  []string{"**"},
		TargetExclude:  []string{},
	}, workflow.DefaultReviewDefinition())
	if err != nil {
		t.Fatalf("configdefaults.Revision() error = %v", err)
	}
	return value
}
