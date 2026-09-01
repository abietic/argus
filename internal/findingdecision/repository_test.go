package findingdecision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/abietic/argus/internal/runmodel"
	"github.com/abietic/argus/internal/store/local"
)

func TestRepositoryRecordsAuthorizedContiguousChainAndRestores(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, root := decisionFixture(ActionHumanReview)
	first, err := repository.Record(context.Background(), request, mutation, root)
	if err != nil {
		t.Fatalf("record first decision: %v", err)
	}
	if first.Sequence != 2 || first.PriorDecisionID != root.InitialDecisionID {
		t.Fatalf("first decision does not extend initial decision: %+v", first)
	}

	request.Action = ActionReject
	request.ReasonCode = "reviewer_rejected"
	request.OccurredAt = request.OccurredAt.Add(time.Minute)
	mutation.IdempotencyKey = "decision-key-2"
	mutation.At = mutation.At.Add(time.Minute)
	second, err := repository.Record(context.Background(), request, mutation, root)
	if err != nil {
		t.Fatalf("record second decision: %v", err)
	}
	if second.Sequence != 3 || second.PriorDecisionID != first.DecisionID {
		t.Fatalf("second decision does not extend ledger: %+v", second)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatalf("restore repository: %v", err)
	}
	chain, err := restarted.List(root.RunID, root.FindingID)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != 2 || chain[0].DecisionID != first.DecisionID ||
		chain[1].DecisionID != second.DecisionID {
		t.Fatalf("restored chain = %+v", chain)
	}
	retry, err := restarted.Record(context.Background(), request, mutation, root)
	if err != nil || retry.DecisionID != second.DecisionID {
		t.Fatalf("exact retry = %+v, %v", retry, err)
	}
}

func TestRepositoryRequiresPublicationApproverForPublish(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, root := decisionFixture(ActionPublish)
	if _, err := repository.Record(context.Background(), request, mutation, root); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reviewer publish error = %v, want unauthorized", err)
	}
	mutation.Roles = []Role{RolePublicationApprover}
	if _, err := repository.Record(context.Background(), request, mutation, root); err != nil {
		t.Fatalf("approver publish: %v", err)
	}
}

func TestRepositoryRejectsIdempotencyAndRootConflicts(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, root := decisionFixture(ActionReject)
	if _, err := repository.Record(context.Background(), request, mutation, root); err != nil {
		t.Fatal(err)
	}
	changed := request
	changed.ReasonCode = "different_reason"
	if _, err := repository.Record(context.Background(), changed, mutation, root); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed retry error = %v, want conflict", err)
	}
	mutation.IdempotencyKey = "decision-key-2"
	root.SourceSHA256 = digestOf('b')
	if _, err := repository.Record(context.Background(), request, mutation, root); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("changed root error = %v, want invalid transition", err)
	}
}

func TestRepositoryRejectsCorruptRestoredEvent(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, root := decisionFixture(ActionReject)
	decision, err := repository.Record(context.Background(), request, mutation, root)
	if err != nil {
		t.Fatal(err)
	}
	mutation.IdempotencyKey = "decision-key-corrupt"
	mutation.At = mutation.At.Add(time.Minute)
	request.OccurredAt = request.OccurredAt.Add(time.Minute)
	decision.IdempotencyKey = mutation.IdempotencyKey
	decision.RecordedAt = mutation.At
	decision.OccurredAt = request.OccurredAt
	decision.Sequence = 99
	decision.PriorDecisionID = "not-the-prior-decision"
	decision.DecisionID, err = decisionID(decision)
	if err != nil {
		t.Fatal(err)
	}
	bad := ledgerEvent{
		SchemaVersion: LedgerEventSchemaVersion,
		Request:       request, Mutation: mutation, Root: root, Decision: decision,
	}
	if _, err := store.AppendJSONL(ledgerStream, local.Event{
		ID: mutation.IdempotencyKey, Schema: LedgerEventSchemaVersion,
		Time: mutation.At, Payload: bad,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := New(store); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("restore error = %v, want corrupt", err)
	}
}

func TestRepositoryDoesNotWriteCanceledRequest(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	request, mutation, root := decisionFixture(ActionReject)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repository.Record(ctx, request, mutation, root); !errors.Is(err, context.Canceled) {
		t.Fatalf("record error = %v, want canceled", err)
	}
	chain, err := repository.List(root.RunID, root.FindingID)
	if err != nil || len(chain) != 0 {
		t.Fatalf("chain after canceled write = %+v, %v", chain, err)
	}
}

func TestStrictRequestAndMutationDecoders(t *testing.T) {
	request, mutation, _ := decisionFixture(ActionReject)
	requestJSON := []byte(`{"schema_version":"argus.finding_decision_request.v1alpha1","run_id":"run-1","finding_id":"finding-1","action":"reject","reason_code":"not_actionable","evidence_refs":[{"authority":"argus","id":"artifact-1","sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}],"occurred_at":"2026-08-25T06:00:00Z"}`)
	if decoded, err := DecodeRequestJSON(requestJSON); err != nil || !exactJSONEqual(decoded, request) {
		t.Fatalf("decode request = %+v, %v", decoded, err)
	}
	mutationJSON := []byte(`{"schema_version":"argus.finding_decision_mutation.v1alpha1","idempotency_key":"decision-key-1","actor":{"kind":"human","id":"reviewer-1"},"roles":["finding_reviewer"],"audit":"manual review in Argus CLI","at":"2026-08-25T06:01:00Z"}`)
	if decoded, err := DecodeMutationJSON(mutationJSON); err != nil || !exactJSONEqual(decoded, mutation) {
		t.Fatalf("decode mutation = %+v, %v", decoded, err)
	}
	duplicate := []byte(`{"schema_version":"argus.finding_decision_request.v1alpha1","run_id":"run-1","run_id":"run-2"}`)
	if _, err := DecodeRequestJSON(duplicate); err == nil {
		t.Fatal("duplicate JSON field was accepted")
	}
	unknown := append(requestJSON[:len(requestJSON)-1], []byte(`,"unknown":true}`)...)
	if _, err := DecodeRequestJSON(unknown); err == nil {
		t.Fatal("unknown JSON field was accepted")
	}
}

func decisionFixture(action Action) (Request, Mutation, Root) {
	return Request{
			SchemaVersion: RequestSchemaVersion,
			RunID:         "run-1", FindingID: "finding-1", Action: action,
			ReasonCode: "not_actionable",
			EvidenceRefs: []EvidenceRef{{
				Authority: "argus", ID: "artifact-1", SHA256: digestOf('a'),
			}},
			OccurredAt: time.Date(2026, 8, 25, 6, 0, 0, 0, time.UTC),
		}, Mutation{
			SchemaVersion:  MutationSchemaVersion,
			IdempotencyKey: "decision-key-1",
			Actor:          Actor{Kind: ActorHuman, ID: "reviewer-1"},
			Roles:          []Role{RoleFindingReviewer},
			Audit:          "manual review in Argus CLI",
			At:             time.Date(2026, 8, 25, 6, 1, 0, 0, time.UTC),
		}, Root{
			SchemaVersion: RootSchemaVersion,
			RunID:         "run-1", FindingID: "finding-1",
			InitialDecisionID: "initial-decision-1",
			SourceContract:    runmodel.ContractGovernedReviewReport,
			SourceSHA256:      digestOf('a'),
		}
}

func digestOf(character byte) string {
	value := make([]byte, 64)
	for index := range value {
		value[index] = character
	}
	return string(value)
}
