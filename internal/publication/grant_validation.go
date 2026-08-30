package publication

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
)

func (request GrantRequest) Validate() error {
	if request.SchemaVersion != GrantRequestSchemaVersion {
		return fmt.Errorf("unsupported publication grant request schema %q", request.SchemaVersion)
	}
	for name, value := range map[string]string{
		"grant_id":                     request.GrantID,
		"publication_id":               request.PublicationID,
		"publication_idempotency_key":  request.PublicationIdempotencyKey,
		"run_id":                       request.RunID,
		"finding_id":                   request.FindingID,
		"provider":                     request.Provider,
		"repository_id":                request.RepositoryID,
		"change_id":                    request.ChangeID,
		"channel":                      request.Channel,
		"expected_permission_revision": request.ExpectedPermissionRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if request.ChangeKind != "pull_request" {
		return fmt.Errorf("unsupported publication change_kind %q", request.ChangeKind)
	}
	switch request.Channel {
	case "pull_request_inline", "pull_request_summary":
	default:
		return fmt.Errorf("unsupported publication grant channel %q", request.Channel)
	}
	if err := validateUTC("occurred_at", request.OccurredAt); err != nil {
		return err
	}
	if err := validateUTC("expires_at", request.ExpiresAt); err != nil {
		return err
	}
	if !request.ExpiresAt.After(request.OccurredAt) {
		return fmt.Errorf("expires_at must follow occurred_at")
	}
	if request.ExpiresAt.Sub(request.OccurredAt) > GrantMaxLifetime {
		return fmt.Errorf("publication grant lifetime exceeds %s", GrantMaxLifetime)
	}
	return nil
}

func (mutation GrantMutation) Validate() error {
	if mutation.SchemaVersion != GrantMutationSchemaVersion {
		return fmt.Errorf("unsupported publication grant mutation schema %q", mutation.SchemaVersion)
	}
	if err := validateID("idempotency_key", mutation.IdempotencyKey); err != nil {
		return err
	}
	if err := mutation.Actor.Validate(); err != nil {
		return fmt.Errorf("actor: %w", err)
	}
	if len(mutation.Roles) != 1 || mutation.Roles[0] != GrantRolePublicationApprover {
		return fmt.Errorf("roles must contain exactly publication_approver")
	}
	if !slices.IsSorted(mutation.Roles) {
		return fmt.Errorf("roles must be canonical sorted")
	}
	if err := validateText("audit", mutation.Audit, 4096, false); err != nil {
		return err
	}
	return validateUTC("at", mutation.At)
}

func (actor GrantActor) Validate() error {
	switch actor.Kind {
	case GrantActorHuman, GrantActorService:
	default:
		return fmt.Errorf("unsupported grant actor kind %q", actor.Kind)
	}
	return validateID("actor.id", actor.ID)
}

func (binding GrantBinding) Validate() error {
	for name, value := range map[string]string{
		"tenant_id":              binding.TenantID,
		"workspace_id":           binding.WorkspaceID,
		"decision_id":            binding.DecisionID,
		"provider":               binding.Provider,
		"repository_id":          binding.RepositoryID,
		"base_revision":          binding.BaseRevision,
		"expected_head_revision": binding.ExpectedHeadRevision,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if binding.BaseRevision == binding.ExpectedHeadRevision {
		return fmt.Errorf("publication grant requires distinct base and head revisions")
	}
	for name, ref := range map[string]ContentRef{
		"target_snapshot_ref": binding.TargetSnapshotRef,
		"config_bundle_ref":   binding.ConfigBundleRef,
		"finding_source_ref":  binding.FindingSourceRef,
	} {
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
	}
	switch binding.FindingSourceContract {
	case FindingSourceFindingSet, FindingSourceGovernedReviewReport:
	default:
		return fmt.Errorf("unsupported finding_source_contract %q", binding.FindingSourceContract)
	}
	return nil
}

func (grant Grant) Validate() error {
	if grant.SchemaVersion != GrantSchemaVersion {
		return fmt.Errorf("unsupported publication grant schema %q", grant.SchemaVersion)
	}
	request := grant.Request()
	if err := request.Validate(); err != nil {
		return err
	}
	mutation := grant.Mutation()
	if err := mutation.Validate(); err != nil {
		return err
	}
	if grant.GrantedAt.Before(grant.OccurredAt) {
		return fmt.Errorf("granted_at must not precede occurred_at")
	}
	if !grant.ExpiresAt.After(grant.GrantedAt) {
		return fmt.Errorf("expires_at must follow granted_at")
	}
	if err := grant.Binding().Validate(); err != nil {
		return err
	}
	digest, err := DigestGrant(grant)
	if err != nil {
		return err
	}
	if grant.GrantSHA256 != digest {
		return fmt.Errorf("grant_sha256 does not match canonical grant facts")
	}
	return nil
}

func (grant Grant) Request() GrantRequest {
	return GrantRequest{
		SchemaVersion: GrantRequestSchemaVersion,
		GrantID:       grant.GrantID, PublicationID: grant.PublicationID,
		PublicationIdempotencyKey: grant.PublicationIdempotencyKey,
		RunID:                     grant.RunID, FindingID: grant.FindingID, Channel: grant.Channel,
		Provider: grant.Provider, RepositoryID: grant.RepositoryID,
		ChangeKind: grant.ChangeKind, ChangeID: grant.ChangeID,
		ExpectedPermissionRevision: grant.ExpectedPermissionRevision,
		OccurredAt:                 grant.OccurredAt, ExpiresAt: grant.ExpiresAt,
	}
}

func (grant Grant) Mutation() GrantMutation {
	return GrantMutation{
		SchemaVersion:  GrantMutationSchemaVersion,
		IdempotencyKey: grant.IdempotencyKey, Actor: grant.GrantedBy,
		Roles: slices.Clone(grant.Roles), Audit: grant.Audit, At: grant.GrantedAt,
	}
}

func (grant Grant) Binding() GrantBinding {
	return GrantBinding{
		TenantID: grant.TenantID, WorkspaceID: grant.WorkspaceID,
		DecisionID: grant.DecisionID, Provider: grant.Provider,
		RepositoryID: grant.RepositoryID, BaseRevision: grant.BaseRevision,
		ExpectedHeadRevision: grant.ExpectedHeadRevision,
		TargetSnapshotRef:    grant.TargetSnapshotRef, ConfigBundleRef: grant.ConfigBundleRef,
		FindingSourceRef:      grant.FindingSourceRef,
		FindingSourceContract: grant.FindingSourceContract,
	}
}

func DigestGrant(grant Grant) (string, error) {
	copy := grant
	copy.GrantSHA256 = ""
	data, err := json.Marshal(copy)
	if err != nil {
		return "", fmt.Errorf("marshal publication grant: %w", err)
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}

func (reservation GrantReservation) Validate() error {
	if reservation.SchemaVersion != GrantReservationSchemaVersion {
		return fmt.Errorf("unsupported grant reservation schema %q", reservation.SchemaVersion)
	}
	for name, value := range map[string]string{
		"grant_id":        reservation.GrantID,
		"publication_id":  reservation.PublicationID,
		"idempotency_key": reservation.IdempotencyKey,
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if err := validateSHA256("grant_sha256", reservation.GrantSHA256); err != nil {
		return err
	}
	return validateUTC("reserved_at", reservation.ReservedAt)
}
