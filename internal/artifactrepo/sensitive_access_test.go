package artifactrepo

import (
	"context"
	"errors"
	"testing"
)

func TestSensitiveArtifactRequiresSeparatedRolesAndAuditedDisclosure(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	request := testPutRequest(subject, []Use{UseSensitiveProcess, UseSensitiveRead})
	if _, _, err := repository.Put(t.Context(), subject, request); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Put(sensitive without roles) error = %v, want ErrUnauthorized", err)
	}
	publisher := subject
	publisher.Roles = []string{RoleSensitiveProcessor, RoleSensitiveReader}
	ref, _, err := repository.Put(t.Context(), publisher, request)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := repository.Resolve(t.Context(), publisher, ref, UseRead); !errors.Is(err, ErrUseDenied) {
		t.Fatalf("regular read error = %v, want ErrUseDenied", err)
	}
	if _, _, err := repository.Resolve(t.Context(), publisher, ref, UseSensitiveRead); !errors.Is(err, ErrUseDenied) {
		t.Fatalf("unaudited sensitive read error = %v, want ErrUseDenied", err)
	}
	processor := subject
	processor.Roles = []string{RoleSensitiveProcessor}
	content, _, err := repository.Resolve(t.Context(), processor, ref, UseSensitiveProcess)
	if err != nil || string(content) != "frozen source\n" {
		t.Fatalf("sensitive process = %q, %v", content, err)
	}
	reader := subject
	reader.Roles = []string{RoleSensitiveReader}
	access := SensitiveAccessRequest{
		RequestID: "sensitive-read-1", Actor: "review-debugger",
		Purpose: SensitiveAccessLocalDebug, At: testTime.Add(2),
	}
	content, _, first, err := repository.ResolveSensitive(
		context.Background(), reader, ref, access,
	)
	if err != nil || string(content) != "frozen source\n" ||
		first.AuditSequence != 1 || first.Receipt.ReceiptID == "" {
		t.Fatalf("ResolveSensitive() = %q, %+v, %v", content, first, err)
	}
	_, _, replay, err := repository.ResolveSensitive(t.Context(), reader, ref, access)
	if err != nil || replay != first {
		t.Fatalf("idempotent ResolveSensitive() = %+v, %v, want %+v", replay, err, first)
	}
	conflict := access
	conflict.Purpose = SensitiveAccessEvaluationReplay
	if _, _, _, err := repository.ResolveSensitive(
		t.Context(), reader, ref, conflict,
	); !errors.Is(err, ErrAuditConflict) {
		t.Fatalf("conflicting audit error = %v, want ErrAuditConflict", err)
	}
}

func TestSensitiveAllowedUsesCannotMixRegularBypass(t *testing.T) {
	repository, _ := newTestRepository(t)
	subject := testSubject("tenant-a", "workspace-a")
	subject.Roles = []string{RoleSensitiveProcessor, RoleSensitiveReader}
	for _, uses := range [][]Use{
		{UseSensitiveProcess},
		{UseSensitiveRead},
		{UseRead, UseSensitiveProcess, UseSensitiveRead},
	} {
		request := testPutRequest(subject, uses)
		request.Mutation.IdempotencyKey = "invalid-sensitive-read-class"
		if _, _, err := repository.Put(t.Context(), subject, request); err == nil {
			t.Fatalf("Put() accepted bypassable or incomplete uses %v", uses)
		}
	}
}
