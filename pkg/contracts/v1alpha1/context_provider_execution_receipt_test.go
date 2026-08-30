package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestContextProviderExecutionReceiptSealDecodeAndRejectMutation(t *testing.T) {
	started := time.Date(2026, 8, 25, 2, 3, 4, 0, time.UTC)
	receipt, err := SealContextProviderExecutionReceipt(ContextProviderExecutionReceipt{
		ProviderID: "go-ast-exact", ProviderRevision: "1", Kind: "go_ast",
		Adapter:       VersionedRef{ID: "argus-go-ast", Revision: "1", SHA256: testDigest},
		RequestSHA256: testDigest, RepositoryID: "local-repository",
		CommitOID:        "1111111111111111111111111111111111111111",
		ObservedRevision: "1111111111111111111111111111111111111111",
		TargetPaths:      []string{"review/handler.go"}, Status: ContextProviderReceiptSucceeded,
		TargetRanges: []ContextProviderTargetRange{},
		ReasonCode:   "", ContextID: "go-ast-exact-1111111111111111",
		ContextDigest: testDigest, ContextContract: "argus.context.go_ast.v1alpha1",
		TimeoutMS: 30_000, StartedAt: started, CompletedAt: started.Add(125 * time.Millisecond),
		DurationMS: 125, Authority: "local_host_observation",
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeContextProviderExecutionReceipt(data)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(decoded, receipt) {
		t.Fatalf("decoded receipt changed: %+v", decoded)
	}

	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	object["unknown"] = true
	mutated, _ := json.Marshal(object)
	if _, err := DecodeContextProviderExecutionReceipt(mutated); err == nil {
		t.Fatal("unknown receipt field was accepted")
	}
	delete(object, "unknown")
	object["status"] = "gap"
	mutated, _ = json.Marshal(object)
	if _, err := DecodeContextProviderExecutionReceipt(mutated); err == nil {
		t.Fatal("status substitution was accepted")
	}
}

func TestContextProviderExecutionReceiptRequiresBoundedRevisionMismatchEvidence(t *testing.T) {
	started := time.Date(2026, 8, 25, 2, 3, 4, 0, time.UTC)
	receipt := ContextProviderExecutionReceipt{
		ProviderID: "go-ast-exact", ProviderRevision: "1", Kind: "go_ast",
		Adapter:       VersionedRef{ID: "argus-go-ast", Revision: "2", SHA256: testDigest},
		RequestSHA256: testDigest, RepositoryID: "local-repository",
		CommitOID:        "1111111111111111111111111111111111111111",
		ObservedRevision: "2222222222222222222222222222222222222222",
		TargetPaths:      []string{"review/handler.go"}, Status: ContextProviderReceiptGap,
		TargetRanges: []ContextProviderTargetRange{},
		ReasonCode:   ContextProviderRevisionMismatch, ContextID: "go-ast-exact-gap-111111111111",
		ContextDigest: testDigest, ContextContract: "",
		TimeoutMS: 30_000, StartedAt: started, CompletedAt: started.Add(125 * time.Millisecond),
		DurationMS: 125, Authority: "local_host_observation",
	}
	sealed, err := SealContextProviderExecutionReceipt(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if sealed.ObservedRevision != receipt.ObservedRevision {
		t.Fatalf("observed revision changed: %+v", sealed)
	}

	invalid := receipt
	invalid.ObservedRevision = invalid.CommitOID
	if _, err := SealContextProviderExecutionReceipt(invalid); err == nil {
		t.Fatal("revision mismatch receipt accepted the requested revision as observed")
	}
	invalid = receipt
	invalid.ReasonCode = "provider_timeout"
	if _, err := SealContextProviderExecutionReceipt(invalid); err == nil {
		t.Fatal("non-revision gap accepted an observed revision")
	}
	invalid = receipt
	invalid.ObservedRevision = "main"
	if _, err := SealContextProviderExecutionReceipt(invalid); err == nil {
		t.Fatal("revision mismatch receipt accepted a mutable observed revision")
	}
	invalid = receipt
	invalid.TargetRanges = []ContextProviderTargetRange{{
		Path: "other.go", StartLine: 1, EndLine: 2,
	}}
	if _, err := SealContextProviderExecutionReceipt(invalid); err == nil {
		t.Fatal("receipt accepted a target range outside target_paths")
	}
}
