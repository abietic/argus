package evaluation

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestGovernanceTrustRegistryRequiresIndependentAdminAndBlocksRevokedKeys(t *testing.T) {
	repository, store := newEvaluationRepository(t)
	firstCase := testActiveCase("trust-first", "repo-trust", SplitTest, testEpoch)
	firstImport := testGovernedCaseImport(firstCase)
	registeredAt := firstImport.ImportedAt.Add(-time.Minute)
	registration := GovernanceTrustKeyRegistration{
		SchemaVersion: GovernanceTrustKeyRegistrationSchemaVersion,
		Key:           firstImport.TrustedKey,
		RegisteredAt:  registeredAt,
	}
	unauthorized := testMutation("register-trust-unauthorized", registeredAt, RoleDatasetCurator)
	if _, err := repository.RegisterGovernanceTrustKey(context.Background(), registration, unauthorized); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized registration error = %v", err)
	}
	registerMutation := testMutation("register-trust-first", registeredAt, RoleGovernanceTrustAdmin)
	registerMutation.Actor = "independent-trust-admin"
	record, err := repository.RegisterGovernanceTrustKey(context.Background(), registration, registerMutation)
	if err != nil {
		t.Fatal(err)
	}
	if retry, err := repository.RegisterGovernanceTrustKey(context.Background(), registration, registerMutation); err != nil ||
		!reflect.DeepEqual(retry, record) {
		t.Fatalf("idempotent trust registration = %+v, %v", retry, err)
	}
	if _, err := repository.GetGovernanceTrustKey(
		registration.Key.Authority, registration.Key.KeyID, registration.Key.Revision,
		Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}},
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unauthorized key read error = %v", err)
	}

	selfImport := testMutation("import-trust-self", firstCase.CreatedAt, RoleDatasetCurator)
	selfImport.Actor = registerMutation.Actor
	if _, err := repository.ImportGovernedCase(context.Background(), firstImport, selfImport); !errors.Is(err, ErrUnauthorized) ||
		!strings.Contains(err.Error(), "trust administrator must be independent") {
		t.Fatalf("trust admin self-import error = %v", err)
	}
	mismatched := firstImport
	mismatched.TrustedKey.ValidUntil = mismatched.TrustedKey.ValidUntil.Add(time.Minute)
	mismatchMutation := testMutation("import-trust-mismatch", firstCase.CreatedAt, RoleDatasetCurator)
	if _, err := repository.ImportGovernedCase(context.Background(), mismatched, mismatchMutation); !errors.Is(err, ErrUnauthorized) ||
		!strings.Contains(err.Error(), "does not match registered revision") {
		t.Fatalf("mismatched registered key error = %v", err)
	}
	importMutation := testMutation("import-trust-first", firstCase.CreatedAt, RoleDatasetCurator)
	importMutation.Actor = "independent-import-operator"
	if _, err := repository.ImportGovernedCase(context.Background(), firstImport, importMutation); err != nil {
		t.Fatal(err)
	}

	revokedAt := firstCase.CreatedAt.Add(time.Minute)
	revocation := GovernanceTrustKeyRevocation{
		SchemaVersion: GovernanceTrustKeyRevocationSchemaVersion,
		Authority:     registration.Key.Authority, KeyID: registration.Key.KeyID,
		Revision: registration.Key.Revision, Reason: "external authority rotation", RevokedAt: revokedAt,
	}
	revokeMutation := testMutation("revoke-trust-first", revokedAt, RoleGovernanceTrustAdmin)
	revokeMutation.Actor = "second-trust-admin"
	revoked, err := repository.RevokeGovernanceTrustKey(context.Background(), revocation, revokeMutation)
	if err != nil || revoked.RevokedAt == nil || revoked.RevokedEventID != revokeMutation.IdempotencyKey {
		t.Fatalf("revoked record = %+v, %v", revoked, err)
	}
	if retry, err := repository.RevokeGovernanceTrustKey(context.Background(), revocation, revokeMutation); err != nil ||
		!reflect.DeepEqual(retry, revoked) {
		t.Fatalf("idempotent revocation = %+v, %v", retry, err)
	}

	secondCase := testActiveCase("trust-second", "repo-trust", SplitTest, revokedAt.Add(time.Minute))
	secondImport := testGovernedCaseImportWithTrustedKey(secondCase, registration.Key)
	secondMutation := testMutation("import-trust-second", secondCase.CreatedAt, RoleDatasetCurator)
	if _, err := repository.ImportGovernedCase(context.Background(), secondImport, secondMutation); !errors.Is(err, ErrUnauthorized) ||
		!strings.Contains(err.Error(), "is revoked") {
		t.Fatalf("revoked key import error = %v", err)
	}

	restarted, err := New(store)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := restarted.ListGovernanceTrustKeys(Access{
		Actor: "trust-auditor", Roles: []Role{RoleGovernanceTrustAdmin},
	})
	if err != nil || len(keys) != 1 || keys[0].RevokedAt == nil {
		t.Fatalf("restored trust keys = %+v, %v", keys, err)
	}
	if _, err := restarted.GetCase(firstCase.CaseID, Access{Actor: "curator", Roles: []Role{RoleDatasetCurator}}); err != nil {
		t.Fatalf("revocation retroactively invalidated historical import: %v", err)
	}
}
