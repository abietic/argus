package artifactrepo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"argus.local/argus/internal/store/local"
)

func TestAuthorityCanonicalURIAndEligibilityRolesFailClosed(t *testing.T) {
	repository, store := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	request := testPutRequest(subject, []Use{UseEvaluation, UseRead})
	if _, _, err := repository.Put(
		context.Background(),
		subject,
		request,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Put(evaluation without role) error = %v", err)
	}
	subject.Roles = []string{RoleEvaluationUse}
	ref, _, err := repository.Put(context.Background(), subject, request)
	if err != nil {
		t.Fatal(err)
	}
	noRole := subject
	noRole.Roles = []string{}
	if _, _, err := repository.Resolve(
		context.Background(),
		noRole,
		ref,
		UseEvaluation,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Resolve(evaluation without role) error = %v", err)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UseEvaluation,
	); err != nil {
		t.Fatalf("Resolve(evaluation with role) error = %v", err)
	}

	for name, uri := range map[string]string{
		"empty query":     ref.URI + "?",
		"empty fragment":  ref.URI + "#",
		"uppercase host":  strings.Replace(ref.URI, "argus-local", "ARGUS-LOCAL", 1),
		"escaped segment": strings.Replace(ref.URI, "/tenants/tenant-a", "/tenants/tenant%2Da", 1),
	} {
		t.Run(name, func(t *testing.T) {
			forged := ref
			forged.URI = uri
			if _, _, err := repository.Resolve(
				context.Background(),
				subject,
				forged,
				UseRead,
			); err == nil {
				t.Fatalf("Resolve() accepted non-canonical URI %q", uri)
			}
		})
	}

	otherAuthority, err := OpenForAuthority(store, "hailix-local", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := otherAuthority.Resolve(
		context.Background(),
		subject,
		ref,
		UseRead,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-authority Resolve() error = %v", err)
	}
	otherRequest := request
	otherRequest.Authority = "hailix-local"
	otherRequest.Mutation = testMutation("put-other-authority", testTime.Add(time.Second))
	otherRef, _, err := otherAuthority.Put(context.Background(), subject, otherRequest)
	if err != nil {
		t.Fatalf("Put(other authority) error = %v", err)
	}
	if !strings.HasPrefix(otherRef.URI, "artifact://hailix-local/") {
		t.Fatalf("other authority ref = %q", otherRef.URI)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		otherRef,
		UseRead,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("default authority accepted other ref: %v", err)
	}

	otherWorkspace := testSubject(subject.TenantID, "workspace-b")
	if _, _, err := repository.Resolve(
		context.Background(),
		otherWorkspace,
		ref,
		UseRead,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-workspace Resolve() error = %v", err)
	}
}

func TestMutationAuditFieldsRejectControlCharacters(t *testing.T) {
	mutation := testMutation("audit-control", testTime)
	for _, value := range []string{"actor\timpersonated", "audit\x01forged"} {
		test := mutation
		if strings.HasPrefix(value, "actor") {
			test.Actor = value
		} else {
			test.Audit = value
		}
		if err := test.Validate(); err == nil {
			t.Fatalf("Mutation.Validate() accepted control text %q", value)
		}
	}
}

func TestEverySensitiveUseRequiresItsExplicitRole(t *testing.T) {
	tests := []struct {
		use  Use
		role string
	}{
		{UsePublication, RolePublicationUse},
		{UseEvaluation, RoleEvaluationUse},
		{UseTraining, RoleTrainingUse},
		{UseExport, RoleExportUse},
	}
	for index, test := range tests {
		t.Run(string(test.use), func(t *testing.T) {
			repository, _ := newTestRepository(t)
			subject := testSubject("tenant-a", "workspace-a")
			uses := []Use{test.use, UseRead}
			if test.use == UseTraining {
				uses = []Use{UseRead, UseTraining}
			}
			request := testPutRequest(subject, uses)
			request.Mutation = testMutation(
				fmt.Sprintf("sensitive-put-%d", index),
				testTime.Add(time.Duration(index)*time.Second),
			)
			if _, _, err := repository.Put(
				context.Background(),
				subject,
				request,
			); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Put(%s without role) error = %v", test.use, err)
			}
			subject.Roles = []string{test.role}
			ref, _, err := repository.Put(context.Background(), subject, request)
			if err != nil {
				t.Fatalf("Put(%s with role) error = %v", test.use, err)
			}
			withoutRole := subject
			withoutRole.Roles = []string{}
			if _, _, err := repository.Resolve(
				context.Background(),
				withoutRole,
				ref,
				test.use,
			); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("Resolve(%s without role) error = %v", test.use, err)
			}
			if _, _, err := repository.Resolve(
				context.Background(),
				subject,
				ref,
				test.use,
			); err != nil {
				t.Fatalf("Resolve(%s with role) error = %v", test.use, err)
			}
		})
	}
}

func TestMutationKeyIsBoundAcrossObjectsAndOperations(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	firstRequest := testPutRequest(subject, []Use{UseRead})
	firstRequest.Mutation = testMutation("shared-put", testTime)
	firstRef, _, err := repository.Put(context.Background(), subject, firstRequest)
	if err != nil {
		t.Fatal(err)
	}
	conflict := firstRequest
	conflict.Content = []byte("different frozen source\n")
	if _, _, err := repository.Put(
		context.Background(),
		subject,
		conflict,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("reused Put mutation error = %v", err)
	}

	secondRequest := firstRequest
	secondRequest.Contract = "argus.second_artifact.v1"
	secondRequest.Mutation = testMutation("put-second", testTime.Add(time.Second))
	secondRef, _, err := repository.Put(context.Background(), subject, secondRequest)
	if err != nil {
		t.Fatal(err)
	}
	operator := subject
	operator.Roles = []string{RoleIntegrityOperator}
	stateMutation := testMutation("shared-state", testTime.Add(2*time.Second))
	if _, err := repository.Quarantine(
		context.Background(),
		operator,
		firstRef,
		"integrity hold",
		stateMutation,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Quarantine(
		context.Background(),
		operator,
		secondRef,
		"integrity hold",
		stateMutation,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-object mutation reuse error = %v", err)
	}
	if _, err := repository.Tombstone(
		context.Background(),
		operator,
		secondRef,
		"retention",
		firstRequest.Mutation,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("cross-operation mutation reuse error = %v", err)
	}
}

func TestMissingContentAndRepeatedCorruptionAutoQuarantine(t *testing.T) {
	repository, store := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	ref, _, err := repository.Put(
		context.Background(),
		subject,
		testPutRequest(subject, []Use{UseRead}),
	)
	if err != nil {
		t.Fatal(err)
	}
	path := artifactContentPath(store, ref)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UseRead,
	); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("missing content Resolve() error = %v", err)
	}

	original := []byte("frozen source\n")
	if err := os.WriteFile(path, original, 0o444); err != nil {
		t.Fatal(err)
	}
	operator := subject
	operator.Roles = []string{RoleIntegrityOperator}
	released, err := repository.ReleaseQuarantine(
		context.Background(),
		operator,
		ref,
		"content restored",
		testMutation("release-after-missing", testTime.Add(2*time.Minute)),
	)
	if err != nil || released.State != StateActive {
		t.Fatalf("ReleaseQuarantine() record = %+v, error = %v", released, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("corrupt-again"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Resolve(
		context.Background(),
		subject,
		ref,
		UseRead,
	); !errors.Is(err, ErrQuarantined) {
		t.Fatalf("second corruption Resolve() error = %v", err)
	}
	record, err := repository.Inspect(context.Background(), subject, ref)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != StateQuarantined {
		t.Fatalf("second quarantine record = %+v", record)
	}
}

func TestBackdatedMutationRejectedWithoutCorruptingObjectLedger(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	ref, _, err := repository.Put(
		context.Background(),
		subject,
		testPutRequest(subject, []Use{UseRead}),
	)
	if err != nil {
		t.Fatal(err)
	}
	operator := subject
	operator.Roles = []string{RoleIntegrityOperator}
	if _, err := repository.Quarantine(
		context.Background(),
		operator,
		ref,
		"backdated hold",
		testMutation("backdated", testTime.Add(-time.Second)),
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("backdated mutation error = %v", err)
	}
	record, err := repository.Inspect(context.Background(), subject, ref)
	if err != nil {
		t.Fatalf("Inspect() after rejected mutation error = %v", err)
	}
	if record.State != StateActive {
		t.Fatalf("rejected mutation changed state: %+v", record)
	}
}

func TestObjectLedgerBindsEnvelopeIDTimeAndStreamIdentity(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*local.Event, *artifactEvent)
	}{
		{
			name: "envelope id",
			mutate: func(event *local.Event, _ *artifactEvent) {
				event.ID = "wrong-envelope-id"
			},
		},
		{
			name: "envelope time",
			mutate: func(event *local.Event, _ *artifactEvent) {
				event.Time = testTime.Add(3 * time.Minute)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, store := newTestRepository(t)
			subject := testSubject("tenant-a", "workspace-a")
			ref, _, err := repository.Put(
				context.Background(),
				subject,
				testPutRequest(subject, []Use{UseRead}),
			)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := validateRef(ref)
			if err != nil {
				t.Fatal(err)
			}
			mutation := testMutation("manual-corrupt-event", testTime.Add(2*time.Minute))
			payload := artifactEvent{
				SchemaVersion: eventSchemaVersion,
				Type:          eventQuarantined,
				Reason:        "manual corruption fixture",
				Mutation:      mutation,
			}
			event := local.Event{
				ID: mutation.IdempotencyKey, Schema: eventSchemaVersion,
				Time: mutation.At, Payload: payload,
			}
			test.mutate(&event, &payload)
			event.Payload = payload
			if _, err := store.AppendJSONLAtSequence(
				objectStream(identity.objectID),
				1,
				event,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.Inspect(
				context.Background(),
				subject,
				ref,
			); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Inspect() error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestCreationEnvelopeBindsMutationIdempotencyKey(t *testing.T) {
	repository, store := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	request := testPutRequest(subject, []Use{UseRead})
	request.Mutation = testMutation("creation-binding", testTime)
	ref, _, err := repository.Put(context.Background(), subject, request)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := validateRef(ref)
	if err != nil {
		t.Fatal(err)
	}
	envelopes, err := store.ReadJSONL(objectStream(identity.objectID))
	if err != nil {
		t.Fatal(err)
	}
	if len(envelopes) != 1 || envelopes[0].ID != request.Mutation.IdempotencyKey {
		t.Fatalf("creation envelope = %+v", envelopes)
	}
}

func TestObjectLedgerRejectsStrictJSONAndCreationMutationMismatch(t *testing.T) {
	tests := []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{
			name: "duplicate payload field",
			mutate: func(data []byte) []byte {
				needle := []byte(
					`"payload":{"schema_version":"argus.artifact_event.v1alpha1",`,
				)
				replacement := []byte(
					`"payload":{"schema_version":"argus.artifact_event.v1alpha1",` +
						`"schema_version":"argus.artifact_event.v1alpha1",`,
				)
				return bytes.Replace(data, needle, replacement, 1)
			},
		},
		{
			name: "created actor mismatch",
			mutate: func(data []byte) []byte {
				return bytes.Replace(
					data,
					[]byte(`"created_by":"artifact-operator"`),
					[]byte(`"created_by":"forged-actor"`),
					1,
				)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			repository, store := newTestRepository(t)
			subject := testSubject("tenant-a", "workspace-a")
			ref, _, err := repository.Put(
				context.Background(),
				subject,
				testPutRequest(subject, []Use{UseRead}),
			)
			if err != nil {
				t.Fatal(err)
			}
			identity, err := validateRef(ref)
			if err != nil {
				t.Fatal(err)
			}
			streamPath := filepath.Join(
				store.Root(),
				"streams",
				filepath.FromSlash(objectStream(identity.objectID))+".jsonl",
			)
			data, err := os.ReadFile(streamPath)
			if err != nil {
				t.Fatal(err)
			}
			mutated := test.mutate(data)
			if bytes.Equal(mutated, data) {
				t.Fatal("corruption fixture did not mutate stream")
			}
			if err := os.WriteFile(streamPath, mutated, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := repository.Inspect(
				context.Background(),
				subject,
				ref,
			); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("Inspect() error = %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestTombstoneIsTerminalAcrossLifecycleOperations(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	ref, _, err := repository.Put(
		context.Background(),
		subject,
		testPutRequest(subject, []Use{UseRead}),
	)
	if err != nil {
		t.Fatal(err)
	}
	operator := subject
	operator.Roles = []string{RoleIntegrityOperator}
	if _, err := repository.Tombstone(
		context.Background(),
		operator,
		ref,
		"retention",
		testMutation("terminal-tombstone", testTime.Add(time.Minute)),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.Quarantine(
		context.Background(),
		operator,
		ref,
		"too late",
		testMutation("post-tombstone-quarantine", testTime.Add(2*time.Minute)),
	); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("Quarantine(tombstoned) error = %v", err)
	}
	if _, err := repository.ReleaseQuarantine(
		context.Background(),
		operator,
		ref,
		"too late",
		testMutation("post-tombstone-release", testTime.Add(2*time.Minute)),
	); !errors.Is(err, ErrTombstoned) {
		t.Fatalf("Release(tombstoned) error = %v", err)
	}
}

func TestPutCASAcrossStoreAndRepositoryInstances(t *testing.T) {
	root := filepath.Join(t.TempDir(), "state")
	firstStore, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	secondStore, err := local.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	first, err := Open(firstStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Open(secondStore, nil)
	if err != nil {
		t.Fatal(err)
	}
	subject := testSubject("tenant-a", "workspace-a")
	const writers = 24
	var wait sync.WaitGroup
	start := make(chan struct{})
	results := make(chan Ref, writers)
	failures := make(chan error, writers)
	for index := range writers {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			<-start
			request := testPutRequest(subject, []Use{UseRead})
			request.Mutation = testMutation(
				fmt.Sprintf("multi-instance-put-%d", index),
				testTime.Add(time.Duration(index)*time.Second),
			)
			target := first
			if index%2 == 1 {
				target = second
			}
			ref, _, err := target.Put(context.Background(), subject, request)
			if err != nil {
				failures <- err
				return
			}
			results <- ref
		}(index)
	}
	close(start)
	wait.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Errorf("Put() error = %v", err)
	}
	var expected Ref
	count := 0
	for ref := range results {
		if count == 0 {
			expected = ref
		} else if ref != expected {
			t.Errorf("ref = %+v, want %+v", ref, expected)
		}
		count++
	}
	if count != writers {
		t.Fatalf("successful writers = %d, want %d", count, writers)
	}
}

func artifactContentPath(store *local.Store, ref Ref) string {
	return filepath.Join(
		store.Root(),
		"artifacts",
		"sha256",
		ref.SHA256[:2],
		ref.SHA256,
	)
}
