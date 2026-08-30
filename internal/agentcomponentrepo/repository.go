// Package agentcomponentrepo provides the local governed registry used to
// resolve exact formal-agent components. It publishes immutable component
// bytes through artifactrepo and keeps a subject-scoped identity index; it is
// a local governance adapter, not a signature or runtime attestation service.
package agentcomponentrepo

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strings"
	"time"
	"unicode"

	"github.com/abietic/argus/internal/artifactrepo"
	"github.com/abietic/argus/internal/reviewconfig"
	"github.com/abietic/argus/internal/store/local"
	contractsv1alpha1 "github.com/abietic/argus/pkg/contracts/v1alpha1"
)

const (
	componentRecordSchemaVersion = "argus.agent_component_record.v1alpha1"
	defaultPublicationActor      = "argus-agent-component-bootstrap"
	defaultPublicationAudit      = "publish immutable formal agent component"
)

var (
	ErrComponentNotFound = errors.New("formal agent component is not published")
	ErrComponentConflict = errors.New("formal agent component identity conflicts with registry")
)

// Subject is fixed on a Resolver at composition time. Resolve requests must
// match it exactly, so a caller cannot turn the component registry into a
// cross-workspace existence oracle.
type Subject struct {
	TenantID       string `json:"tenant_id"`
	OrganizationID string `json:"organization_id"`
	WorkspaceID    string `json:"workspace_id"`
	RepositoryID   string `json:"repository_id"`
}

type Publication struct {
	Ref         reviewconfig.VersionedRef
	Contract    string
	Content     []byte
	PublishedAt time.Time
	Mutation    *Mutation
}

// Mutation is the authority-bearing publication audit supplied by a control
// plane. A nil mutation is reserved for trusted local bootstrap callers and
// is normalized to the historical system actor/idempotency contract.
type Mutation struct {
	IdempotencyKey string
	Actor          string
	Audit          string
	At             time.Time
}

// ResolvedPublication returns the exact governed binding together with the
// immutable bytes it names. Callers use this at admission time to freeze a
// replay template without reopening caller-controlled filesystem paths.
type ResolvedPublication struct {
	Binding contractsv1alpha1.AgentStageComponentBinding
	Content []byte
}

type componentRecord struct {
	SchemaVersion  string                            `json:"schema_version"`
	Subject        Subject                           `json:"subject"`
	Ref            reviewconfig.VersionedRef         `json:"ref"`
	Contract       string                            `json:"contract"`
	Artifact       contractsv1alpha1.ArtifactBinding `json:"artifact"`
	PublishedBy    string                            `json:"published_by"`
	Audit          string                            `json:"audit"`
	IdempotencyKey string                            `json:"idempotency_key"`
	PublishedAt    time.Time                         `json:"published_at"`
}

type Repository struct {
	store     *local.Store
	artifacts *artifactrepo.Repository
	authority string
}

func New(
	store *local.Store,
	artifacts *artifactrepo.Repository,
	authority string,
) (*Repository, error) {
	if store == nil || artifacts == nil {
		return nil, fmt.Errorf("local store and governed artifact repository are required")
	}
	if err := validateSafeID("authority", authority); err != nil {
		return nil, err
	}
	if authority != strings.ToLower(authority) {
		return nil, fmt.Errorf("authority must use lowercase canonical form")
	}
	return &Repository{store: store, artifacts: artifacts, authority: authority}, nil
}

// Publish registers one immutable ID/revision. An exact retry is idempotent;
// the same identity can never be rebound to another digest, contract, subject,
// or publication time.
func (repository *Repository) Publish(
	ctx context.Context,
	subject Subject,
	publication Publication,
) (contractsv1alpha1.AgentStageComponentBinding, error) {
	if err := checkContext(ctx); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if err := subject.Validate(); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if err := publication.Ref.Validate(); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"validate component reference: %w", err,
		)
	}
	if !supportedComponentContract(publication.Contract) {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"unsupported formal agent component contract %q", publication.Contract,
		)
	}
	if len(publication.Content) == 0 {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"formal agent component content must be non-empty",
		)
	}
	if publication.PublishedAt.IsZero() || publication.PublishedAt.Location() != time.UTC {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"component published_at must be a non-zero UTC timestamp",
		)
	}
	mutation, err := publication.normalizedMutation(subject)
	if err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if !mutation.At.Equal(publication.PublishedAt) {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"component mutation at must equal published_at",
		)
	}
	digest := sha256.Sum256(publication.Content)
	if publication.Ref.SHA256 != hex.EncodeToString(digest[:]) {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"component reference does not bind exact published bytes",
		)
	}

	if existing, found, err := repository.lookupRecord(subject, publication.Contract, publication.Ref); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	} else if found {
		if existing.PublishedAt != publication.PublishedAt ||
			existing.PublishedBy != mutation.Actor || existing.Audit != mutation.Audit ||
			existing.IdempotencyKey != mutation.IdempotencyKey {
			return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
				"%w: exact retry changed published_at", ErrComponentConflict,
			)
		}
		return repository.resolveRecord(ctx, subject, existing)
	}

	artifactRef, _, err := repository.artifacts.Put(
		ctx,
		subject.artifactSubject(),
		artifactrepo.PutRequest{
			Authority: repository.authority,
			TenantID:  subject.TenantID, WorkspaceID: subject.WorkspaceID,
			Contract:    publication.Contract,
			AllowedUses: []artifactrepo.Use{artifactrepo.UseRead},
			Content:     publication.Content,
			Mutation:    mutation,
		},
	)
	if err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"publish governed component artifact: %w", err,
		)
	}
	record := componentRecord{
		SchemaVersion: componentRecordSchemaVersion,
		Subject:       subject,
		Ref:           publication.Ref,
		Contract:      publication.Contract,
		Artifact: contractsv1alpha1.ArtifactBinding{
			Ref: contractsv1alpha1.ContentRef{
				URI: artifactRef.URI, SHA256: artifactRef.SHA256,
				SizeBytes: artifactRef.SizeBytes,
			},
			Contract: artifactRef.Contract,
		},
		PublishedBy: mutation.Actor, Audit: mutation.Audit,
		IdempotencyKey: mutation.IdempotencyKey,
		PublishedAt:    publication.PublishedAt,
	}
	if err := record.Validate(); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	recordID := componentRecordID(subject, publication.Contract, publication.Ref)
	if err := repository.store.PutJSON(recordID, record); err != nil {
		if !errors.Is(err, local.ErrImmutableExists) {
			return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
				"persist component identity record: %w", err,
			)
		}
		var winner componentRecord
		if loadErr := repository.store.GetJSON(recordID, &winner); loadErr != nil {
			return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
				"reload concurrent component identity winner: %w", loadErr,
			)
		}
		if validateErr := winner.Validate(); validateErr != nil {
			return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
				"validate concurrent component identity winner: %w", validateErr,
			)
		}
		if !reflect.DeepEqual(winner, record) {
			return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
				"%w: immutable identity winner differs", ErrComponentConflict,
			)
		}
		record = winner
	}
	return repository.resolveRecord(ctx, subject, record)
}

func (publication Publication) normalizedMutation(subject Subject) (artifactrepo.Mutation, error) {
	if publication.Mutation == nil {
		return artifactrepo.Mutation{
			IdempotencyKey: componentMutationID(subject, publication.Contract, publication.Ref),
			Actor:          defaultPublicationActor, Audit: defaultPublicationAudit,
			At: publication.PublishedAt,
		}, nil
	}
	mutation := artifactrepo.Mutation{
		IdempotencyKey: publication.Mutation.IdempotencyKey,
		Actor:          publication.Mutation.Actor,
		Audit:          publication.Mutation.Audit,
		At:             publication.Mutation.At,
	}
	if err := mutation.Validate(); err != nil {
		return artifactrepo.Mutation{}, fmt.Errorf("validate component publication mutation: %w", err)
	}
	return mutation, nil
}

func (repository *Repository) Resolve(
	ctx context.Context,
	subject Subject,
	contract string,
	reference reviewconfig.VersionedRef,
) (contractsv1alpha1.AgentStageComponentBinding, error) {
	if err := checkContext(ctx); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if err := subject.Validate(); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if err := reference.Validate(); err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if !supportedComponentContract(contract) {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"unsupported formal agent component contract %q", contract,
		)
	}
	record, found, err := repository.lookupRecord(subject, contract, reference)
	if err != nil {
		return contractsv1alpha1.AgentStageComponentBinding{}, err
	}
	if !found {
		return contractsv1alpha1.AgentStageComponentBinding{}, fmt.Errorf(
			"%w: %s@%s", ErrComponentNotFound, reference.ID, reference.Revision,
		)
	}
	resolved, err := repository.resolveRecordWithContent(ctx, subject, record)
	return resolved.Binding, err
}

func (repository *Repository) ResolveWithContent(
	ctx context.Context,
	subject Subject,
	contract string,
	reference reviewconfig.VersionedRef,
) (ResolvedPublication, error) {
	if err := checkContext(ctx); err != nil {
		return ResolvedPublication{}, err
	}
	if err := subject.Validate(); err != nil {
		return ResolvedPublication{}, err
	}
	if err := reference.Validate(); err != nil {
		return ResolvedPublication{}, err
	}
	if !supportedComponentContract(contract) {
		return ResolvedPublication{}, fmt.Errorf(
			"unsupported formal agent component contract %q", contract,
		)
	}
	record, found, err := repository.lookupRecord(subject, contract, reference)
	if err != nil {
		return ResolvedPublication{}, err
	}
	if !found {
		return ResolvedPublication{}, fmt.Errorf(
			"%w: %s@%s", ErrComponentNotFound, reference.ID, reference.Revision,
		)
	}
	return repository.resolveRecordWithContent(ctx, subject, record)
}

func (repository *Repository) lookupRecord(
	subject Subject,
	contract string,
	reference reviewconfig.VersionedRef,
) (componentRecord, bool, error) {
	var record componentRecord
	err := repository.store.GetJSON(componentRecordID(subject, contract, reference), &record)
	if errors.Is(err, os.ErrNotExist) {
		return componentRecord{}, false, nil
	}
	if err != nil {
		return componentRecord{}, false, fmt.Errorf("load component identity record: %w", err)
	}
	if err := record.Validate(); err != nil {
		return componentRecord{}, false, fmt.Errorf("validate component identity record: %w", err)
	}
	if record.Subject != subject || record.Contract != contract || record.Ref != reference {
		return componentRecord{}, false, fmt.Errorf(
			"%w: component identity record does not match lookup", ErrComponentConflict,
		)
	}
	return record, true, nil
}

func (repository *Repository) resolveRecord(
	ctx context.Context,
	subject Subject,
	record componentRecord,
) (contractsv1alpha1.AgentStageComponentBinding, error) {
	resolved, err := repository.resolveRecordWithContent(ctx, subject, record)
	return resolved.Binding, err
}

func (repository *Repository) resolveRecordWithContent(
	ctx context.Context,
	subject Subject,
	record componentRecord,
) (ResolvedPublication, error) {
	ref := artifactrepo.Ref{
		URI: record.Artifact.Ref.URI, SHA256: record.Artifact.Ref.SHA256,
		SizeBytes: record.Artifact.Ref.SizeBytes, Contract: record.Artifact.Contract,
	}
	content, metadata, err := repository.artifacts.Resolve(
		ctx,
		subject.artifactSubject(),
		ref,
		artifactrepo.UseRead,
	)
	if err != nil {
		return ResolvedPublication{}, fmt.Errorf(
			"resolve governed component artifact: %w", err,
		)
	}
	digest := sha256.Sum256(content)
	if hex.EncodeToString(digest[:]) != record.Ref.SHA256 ||
		metadata.State != artifactrepo.StateActive ||
		metadata.Metadata.TenantID != subject.TenantID ||
		metadata.Metadata.WorkspaceID != subject.WorkspaceID ||
		metadata.Metadata.Contract != record.Contract {
		return ResolvedPublication{}, fmt.Errorf(
			"governed component artifact does not match immutable identity record",
		)
	}
	return ResolvedPublication{
		Binding: contractsv1alpha1.AgentStageComponentBinding{
			Ref: contractsv1alpha1.VersionedRef{
				ID: record.Ref.ID, Revision: record.Ref.Revision, SHA256: record.Ref.SHA256,
			},
			Artifact: record.Artifact,
		},
		Content: bytes.Clone(content),
	}, nil
}

func (subject Subject) Validate() error {
	if err := (artifactrepo.Subject{
		TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID, Roles: []string{},
	}).Validate(); err != nil {
		return fmt.Errorf("validate component artifact subject: %w", err)
	}
	if err := validateSafeID("organization_id", subject.OrganizationID); err != nil {
		return err
	}
	return validateSafeID("repository_id", subject.RepositoryID)
}

func (subject Subject) artifactSubject() artifactrepo.Subject {
	return artifactrepo.Subject{
		TenantID: subject.TenantID, WorkspaceID: subject.WorkspaceID, Roles: []string{},
	}
}

func (record componentRecord) Validate() error {
	if record.SchemaVersion != componentRecordSchemaVersion {
		return fmt.Errorf("unsupported component record schema %q", record.SchemaVersion)
	}
	if err := record.Subject.Validate(); err != nil {
		return err
	}
	if err := record.Ref.Validate(); err != nil {
		return err
	}
	if !supportedComponentContract(record.Contract) ||
		record.Artifact.Contract != record.Contract ||
		record.Artifact.Ref.SHA256 != record.Ref.SHA256 ||
		record.Artifact.Ref.SizeBytes <= 0 {
		return fmt.Errorf("component artifact does not close exact contract and reference")
	}
	if record.PublishedBy == "" || record.Audit == "" || record.IdempotencyKey == "" {
		return fmt.Errorf("component publication audit is incomplete")
	}
	parsed, err := url.Parse(record.Artifact.Ref.URI)
	if err != nil || parsed.Scheme != "artifact" || parsed.Host == "" ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("component artifact URI is invalid")
	}
	if record.PublishedAt.IsZero() || record.PublishedAt.Location() != time.UTC {
		return fmt.Errorf("component published_at must be a non-zero UTC timestamp")
	}
	return nil
}

func componentRecordID(
	subject Subject,
	contract string,
	reference reviewconfig.VersionedRef,
) string {
	payload, _ := json.Marshal(struct {
		Subject  Subject `json:"subject"`
		Contract string  `json:"contract"`
		ID       string  `json:"id"`
		Revision string  `json:"revision"`
	}{subject, contract, reference.ID, reference.Revision})
	digest := sha256.Sum256(payload)
	return "agent-components/" + hex.EncodeToString(digest[:])
}

func componentMutationID(
	subject Subject,
	contract string,
	reference reviewconfig.VersionedRef,
) string {
	path := componentRecordID(subject, contract, reference)
	return "agent-component-put-" + path[len("agent-components/"):len("agent-components/")+24]
}

func supportedComponentContract(contract string) bool {
	switch contract {
	case contractsv1alpha1.AgentStagePlanAgentContract,
		contractsv1alpha1.AgentStagePlanProviderContract,
		contractsv1alpha1.AgentStagePlanModelContract,
		contractsv1alpha1.AgentStagePlanRuntimeContract,
		contractsv1alpha1.AgentStagePlanPromptContract,
		contractsv1alpha1.AgentStagePlanSkillContract,
		contractsv1alpha1.AgentStagePlanKnowledgeContract,
		contractsv1alpha1.AgentStagePlanAPIProtocolContract,
		contractsv1alpha1.AgentStagePlanContextProviderAdapterContract:
		return true
	default:
		return false
	}
}

func validateSafeID(name, value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 128 ||
		value == "." || value == ".." {
		return fmt.Errorf("%s is invalid", name)
	}
	for _, character := range value {
		if unicode.IsLetter(character) || unicode.IsDigit(character) ||
			strings.ContainsRune("._~-", character) {
			continue
		}
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func checkContext(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
