package runrepo

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/abietic/argus/internal/store/local"
)

func TestArtifactIntegrityLifecycleGatesEveryUseAndPreservesSharedBytes(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 25, 1, 0, 0, 0, time.UTC)
	repository.now = func() time.Time { return now }
	content := []byte("immutable review evidence\n")
	ref, err := repository.PutArtifact("argus.test.review_evidence.v1", content)
	if err != nil {
		t.Fatal(err)
	}
	alias, err := repository.PutArtifact("argus.test.alias.v1", content)
	if err != nil {
		t.Fatal(err)
	}

	active, err := repository.InspectArtifactIntegrity(ref)
	if err != nil || active.State != ArtifactIntegrityActive || active.Sequence != 0 {
		t.Fatalf("initial integrity = %+v, %v", active, err)
	}
	quarantineMutation := integrityMutation("manual-quarantine", now.Add(time.Minute))
	quarantined, err := repository.QuarantineArtifact(
		context.Background(), ref, "operator investigation", quarantineMutation,
	)
	if err != nil {
		t.Fatalf("QuarantineArtifact() error = %v", err)
	}
	if quarantined.State != ArtifactIntegrityQuarantined || quarantined.Sequence != 1 {
		t.Fatalf("quarantined record = %+v", quarantined)
	}
	for _, use := range []ArtifactUse{
		ArtifactUseRead, ArtifactUseCandidatePool, ArtifactUsePublication, ArtifactUseEvaluation,
		ArtifactUseTraining, ArtifactUseExport,
	} {
		if err := repository.CheckArtifactEligibility(ref, use); !errors.Is(err, ErrArtifactQuarantined) {
			t.Fatalf("CheckArtifactEligibility(%s) error = %v", use, err)
		}
	}
	if _, err := repository.ReadArtifact(ref); !errors.Is(err, ErrArtifactQuarantined) {
		t.Fatalf("ReadArtifact(quarantined) error = %v", err)
	}

	released, err := repository.ReleaseArtifactQuarantine(
		context.Background(), ref, "checksum independently verified",
		integrityMutation("release-quarantine", now.Add(2*time.Minute)),
	)
	if err != nil || released.State != ArtifactIntegrityActive || released.Sequence != 2 {
		t.Fatalf("released record = %+v, %v", released, err)
	}

	contentPath := filepath.Join(
		store.Root(), "artifacts", "sha256", ref.SHA256[:2], ref.SHA256,
	)
	if err := os.Chmod(contentPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(contentPath, []byte("corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReadArtifact(ref); !errors.Is(err, ErrArtifactQuarantined) || !errors.Is(err, local.ErrCorrupt) {
		t.Fatalf("ReadArtifact(corrupt) error = %v", err)
	}
	automatic, err := repository.InspectArtifactIntegrity(ref)
	if err != nil || automatic.State != ArtifactIntegrityQuarantined ||
		automatic.StateReason != "content_verification_failed" || automatic.Sequence != 3 {
		t.Fatalf("automatic quarantine = %+v, %v", automatic, err)
	}
	if _, err := repository.ReleaseArtifactQuarantine(
		context.Background(), ref, "unsafe early release",
		integrityMutation("early-release", now.Add(3*time.Minute)),
	); err == nil {
		t.Fatal("ReleaseArtifactQuarantine() accepted corrupt bytes")
	}
	if err := os.WriteFile(contentPath, content, 0o444); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.ReleaseArtifactQuarantine(
		context.Background(), ref, "content restored from verified copy",
		integrityMutation("verified-release", now.Add(4*time.Minute)),
	); err != nil {
		t.Fatalf("ReleaseArtifactQuarantine(restored) error = %v", err)
	}
	tombstoneMutation := integrityMutation("retention-tombstone", now.Add(5*time.Minute))
	tombstoned, err := repository.TombstoneArtifact(
		context.Background(), ref, "retention authority revoked", tombstoneMutation,
	)
	if err != nil || tombstoned.State != ArtifactIntegrityTombstoned || tombstoned.Sequence != 5 {
		t.Fatalf("tombstoned record = %+v, %v", tombstoned, err)
	}
	if retry, err := repository.TombstoneArtifact(
		context.Background(), ref, "retention authority revoked", tombstoneMutation,
	); err != nil || retry.Sequence != tombstoned.Sequence {
		t.Fatalf("idempotent tombstone = %+v, %v", retry, err)
	}
	if _, err := repository.TombstoneArtifact(
		context.Background(), ref, "different reason", tombstoneMutation,
	); !errors.Is(err, ErrArtifactTransition) {
		t.Fatalf("conflicting tombstone error = %v", err)
	}
	if _, err := repository.ReadArtifact(ref); !errors.Is(err, ErrArtifactTombstoned) {
		t.Fatalf("ReadArtifact(tombstoned) error = %v", err)
	}
	if _, err := os.Stat(contentPath); err != nil {
		t.Fatalf("shared content bytes were physically deleted: %v", err)
	}
	aliasBytes, err := repository.ReadArtifact(alias)
	if err != nil || string(aliasBytes) != string(content) {
		t.Fatalf("logical alias should remain active: %q, %v", aliasBytes, err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	if record, err := restarted.InspectArtifactIntegrity(ref); err != nil || record.State != ArtifactIntegrityTombstoned || record.Sequence != 5 {
		t.Fatalf("restarted integrity record = %+v, %v", record, err)
	}
}

func TestArtifactIntegrityLedgerCorruptionFailsClosed(t *testing.T) {
	store, err := local.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repository, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	ref, err := repository.PutArtifact("argus.test.v1", []byte("evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repository.QuarantineArtifact(
		context.Background(), ref, "test quarantine",
		integrityMutation("quarantine", time.Date(2026, 8, 25, 2, 0, 0, 0, time.UTC)),
	); err != nil {
		t.Fatal(err)
	}
	streamPath := filepath.Join(
		store.Root(), "streams", "runrepo", "artifact-integrity",
		artifactIntegrityIdentity(ref)+".jsonl",
	)
	data, err := os.ReadFile(streamPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(streamPath, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(streamPath, append(data, []byte("{}\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := repository.CheckArtifactEligibility(ref, ArtifactUseEvaluation); !errors.Is(err, ErrArtifactIntegrity) {
		t.Fatalf("corrupt ledger eligibility error = %v", err)
	}
}

func integrityMutation(key string, at time.Time) ArtifactIntegrityMutation {
	return ArtifactIntegrityMutation{
		IdempotencyKey: key, Actor: "integrity-operator", Audit: "test integrity mutation", At: at,
	}
}
