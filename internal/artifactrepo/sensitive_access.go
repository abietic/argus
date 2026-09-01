package artifactrepo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/abietic/argus/internal/store/local"
)

const sensitiveAccessSchemaVersion = "argus.artifact_sensitive_access_receipt.v1alpha1"

// ResolveSensitive durably accepts an exact, purpose-bound authorization
// receipt before returning any sensitive bytes. Retrying the same request ID
// with the same binding is idempotent; substitution fails closed.
func (repository *Repository) ResolveSensitive(
	ctx context.Context,
	subject Subject,
	ref Ref,
	access SensitiveAccessRequest,
) ([]byte, Record, SensitiveAccessProof, error) {
	if err := checkContext(ctx); err != nil {
		return nil, Record{}, SensitiveAccessProof{}, err
	}
	if err := access.Validate(); err != nil {
		return nil, Record{}, SensitiveAccessProof{}, err
	}
	record, identity, err := repository.authorize(subject, ref)
	if err != nil {
		return nil, Record{}, SensitiveAccessProof{}, err
	}
	if record.State == StateQuarantined {
		return nil, record, SensitiveAccessProof{}, ErrQuarantined
	}
	if record.State == StateTombstoned {
		return nil, record, SensitiveAccessProof{}, ErrTombstoned
	}
	if record.State != StateActive {
		return nil, Record{}, SensitiveAccessProof{}, fmt.Errorf(
			"%w: unsupported state %q", ErrCorrupt, record.State,
		)
	}
	now := repository.now().UTC()
	if now.IsZero() {
		return nil, record, SensitiveAccessProof{}, fmt.Errorf("repository clock returned zero")
	}
	if access.At.Before(record.Metadata.CreatedAt) || access.At.After(now) {
		return nil, record, SensitiveAccessProof{}, fmt.Errorf(
			"sensitive access time must be within artifact lifetime and host observation time",
		)
	}
	if !containsUse(record.Metadata.AllowedUses, UseSensitiveRead) {
		return nil, record, SensitiveAccessProof{}, ErrUseDenied
	}
	if err := authorizeUseRole(subject, UseSensitiveRead); err != nil {
		return nil, record, SensitiveAccessProof{}, err
	}
	receipt := SensitiveAccessReceipt{
		SchemaVersion: sensitiveAccessSchemaVersion,
		RequestID:     access.RequestID, Authority: identity.authority,
		TenantID: identity.tenantID, WorkspaceID: identity.workspaceID,
		ObjectID: identity.objectID, Contract: record.Metadata.Contract,
		ContentSHA256: record.Metadata.Content.SHA256,
		SizeBytes:     record.Metadata.Content.SizeBytes,
		Actor:         access.Actor, Purpose: access.Purpose, AuthorizedAt: access.At,
	}
	receipt.ReceiptID = sensitiveAccessReceiptID(receipt)
	if err := receipt.Validate(); err != nil {
		return nil, Record{}, SensitiveAccessProof{}, err
	}
	stream := sensitiveAccessStream(identity)
	envelope, err := repository.store.AppendJSONL(stream, local.Event{
		ID: access.RequestID, Schema: sensitiveAccessSchemaVersion,
		Time: access.At, Payload: receipt,
	})
	if err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return nil, record, SensitiveAccessProof{}, fmt.Errorf(
				"%w: request_id %q is bound to another sensitive access",
				ErrAuditConflict,
				access.RequestID,
			)
		}
		return nil, record, SensitiveAccessProof{}, fmt.Errorf(
			"persist sensitive access receipt: %w", err,
		)
	}
	proof := SensitiveAccessProof{
		Receipt: receipt, AuditStream: stream, AuditSequence: envelope.Sequence,
	}
	content, current, err := repository.resolveAuthorized(
		ctx,
		subject,
		ref,
		UseSensitiveRead,
	)
	if err != nil {
		return nil, current, proof, err
	}
	return content, current, proof, nil
}

func sensitiveAccessReceiptID(receipt SensitiveAccessReceipt) string {
	digest := sha256.New()
	for _, value := range []string{
		receipt.SchemaVersion,
		receipt.RequestID,
		receipt.Authority,
		receipt.TenantID,
		receipt.WorkspaceID,
		receipt.ObjectID,
		receipt.Contract,
		receipt.ContentSHA256,
		strconv.FormatInt(receipt.SizeBytes, 10),
		receipt.Actor,
		string(receipt.Purpose),
		receipt.AuthorizedAt.Format("2006-01-02T15:04:05.999999999Z07:00"),
	} {
		_, _ = digest.Write([]byte(strconv.Itoa(len(value))))
		_, _ = digest.Write([]byte{':'})
		_, _ = digest.Write([]byte(value))
	}
	return hex.EncodeToString(digest.Sum(nil))
}

func sensitiveAccessStream(identity refIdentity) string {
	return "artifactrepo/sensitive-access/" + identity.authority + "/" +
		identity.tenantID + "/" + identity.workspaceID
}

func containsUse(uses []Use, want Use) bool {
	for _, use := range uses {
		if use == want {
			return true
		}
	}
	return false
}
