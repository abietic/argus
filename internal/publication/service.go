package publication

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"argus.local/argus/internal/store/local"
)

const publicationLedger = "publication-ledger"

var (
	ErrDisabled       = errors.New("remote publication adapter is disabled")
	ErrConflict       = errors.New("publication request conflicts with durable state")
	ErrUnknownOutcome = errors.New("publication outcome is unknown; reconcile before any retry")
)

type DisabledProvider struct{}

func (DisabledProvider) Revalidate(context.Context, Request) (Revalidation, error) {
	return Revalidation{}, ErrDisabled
}

func (DisabledProvider) Publish(context.Context, ProviderRequest) (ProviderResult, error) {
	return ProviderResult{}, ErrDisabled
}

func (DisabledProvider) Lookup(context.Context, ProviderRequest) (ProviderResult, error) {
	return ProviderResult{}, ErrDisabled
}

type Repository struct {
	store *local.Store
	mu    *sync.Mutex
}

var publicationLocks sync.Map

func NewRepository(store *local.Store) (*Repository, error) {
	if store == nil {
		return nil, fmt.Errorf("local store is required")
	}
	value, _ := publicationLocks.LoadOrStore(store.Root(), &sync.Mutex{})
	repository := &Repository{store: store, mu: value.(*sync.Mutex)}
	if _, err := repository.load(); err != nil {
		return nil, err
	}
	return repository, nil
}

// ByFinding returns immutable publication-channel facts for one exact run and
// Finding. A DecisionPublish action is deliberately insufficient: only ledger
// records returned here can prove whether a provider side effect happened.
func (repository *Repository) ByFinding(runID string, findingID string) ([]Record, error) {
	if err := validateID("run_id", runID); err != nil {
		return nil, err
	}
	if err := validateID("finding_id", findingID); err != nil {
		return nil, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	state, err := repository.load()
	if err != nil {
		return nil, err
	}
	result := make([]Record, 0)
	for _, record := range state {
		if record.Request.RunID == runID && record.Request.FindingID == findingID {
			result = append(result, cloneRecord(record))
		}
	}
	slices.SortFunc(result, func(left, right Record) int {
		return strings.Compare(left.Request.PublicationID, right.Request.PublicationID)
	})
	return result, nil
}

func (repository *Repository) Get(publicationID string) (Record, error) {
	if err := validateID("publication_id", publicationID); err != nil {
		return Record{}, err
	}
	repository.mu.Lock()
	defer repository.mu.Unlock()
	return repository.loadOne(publicationID)
}

type Service struct {
	repository *Repository
	provider   Provider
	authority  RequestAuthorizer
	now        func() time.Time
}

func NewService(
	repository *Repository,
	provider Provider,
	authority RequestAuthorizer,
	now func() time.Time,
) (*Service, error) {
	if repository == nil || provider == nil || authority == nil {
		return nil, fmt.Errorf("publication repository, provider, and request authorizer are required")
	}
	if now == nil {
		now = time.Now
	}
	return &Service{
		repository: repository, provider: provider, authority: authority, now: now,
	}, nil
}

// Publish performs a durable write-ahead transition before crossing the
// provider boundary. Once dispatch has started, every retry uses Lookup only.
func (service *Service) Publish(ctx context.Context, request Request) (Record, error) {
	if ctx == nil {
		return Record{}, fmt.Errorf("context is required")
	}
	if err := request.Validate(); err != nil {
		return Record{}, err
	}
	digest, err := DigestRequest(request)
	if err != nil {
		return Record{}, err
	}

	service.repository.mu.Lock()
	defer service.repository.mu.Unlock()

	state, err := service.repository.load()
	if err != nil {
		return Record{}, err
	}
	alreadyRequested := false
	if existing, ok := state[request.PublicationID]; ok {
		if existing.RequestSHA256 != digest ||
			existing.Request.IdempotencyKey != request.IdempotencyKey {
			return Record{}, fmt.Errorf("%w: publication_id %q", ErrConflict,
				request.PublicationID)
		}
		switch existing.State {
		case StatePublished, StateRejected, StateNotFound:
			return cloneRecord(existing), nil
		case StateRequested:
			// The provider boundary has not been crossed. Retrying a transient
			// revalidation failure is therefore safe.
			alreadyRequested = true
		case StateDispatching, StateUnknown:
			return service.reconcileLocked(ctx, existing)
		default:
			return Record{}, fmt.Errorf("%w: unsupported durable state %q",
				ErrConflict, existing.State)
		}
	}
	if err := contextErr(ctx); err != nil {
		return Record{}, err
	}
	// Authority gates only a request that has not crossed the provider
	// boundary. Terminal reads and lookup-only unknown-outcome reconciliation
	// must remain available even when authority is later revoked.
	if err := service.authority.AuthorizePublication(ctx, request); err != nil {
		return Record{}, fmt.Errorf("authorize publication request: %w", err)
	}
	if !alreadyRequested {
		requested := LedgerEvent{
			SchemaVersion: LedgerEventSchemaVersion,
			EventID:       request.PublicationID + "-requested",
			Type:          EventRequested,
			PublicationID: request.PublicationID,
			RequestSHA256: digest,
			Request:       &request,
			OccurredAt:    service.timestamp(),
		}
		if err := service.repository.append(requested); err != nil {
			return Record{}, err
		}
	}

	revalidation, err := service.provider.Revalidate(ctx, request)
	if err != nil {
		return Record{}, fmt.Errorf("revalidate publication: %w", err)
	}
	if err := validateExactRevalidation(request, revalidation); err != nil {
		result := ProviderResult{
			SchemaVersion:  ProviderResultSchema,
			Status:         ProviderResultRejected,
			IdempotencyKey: request.IdempotencyKey,
			ReasonCode:     "preflight_revalidation_failed",
			ObservedAt:     service.timestamp(),
		}
		event := resultEvent(request, digest, EventRejected, result)
		if appendErr := service.repository.append(event); appendErr != nil {
			return Record{}, appendErr
		}
		record, loadErr := service.repository.loadOne(request.PublicationID)
		if loadErr != nil {
			return Record{}, loadErr
		}
		return record, err
	}
	// Recheck Argus-owned Decision/config authority immediately before the
	// durable dispatch boundary. Provider-owned head/permission/anchor facts
	// were independently checked above.
	if err := service.authority.AuthorizePublication(ctx, request); err != nil {
		return Record{}, fmt.Errorf("reauthorize publication request: %w", err)
	}
	providerRequest := buildProviderRequest(request, digest)
	started := LedgerEvent{
		SchemaVersion:   LedgerEventSchemaVersion,
		EventID:         request.PublicationID + "-dispatch-started",
		Type:            EventDispatchStarted,
		PublicationID:   request.PublicationID,
		RequestSHA256:   digest,
		Revalidation:    &revalidation,
		ProviderRequest: &providerRequest,
		OccurredAt:      service.timestamp(),
	}
	if err := service.repository.append(started); err != nil {
		return Record{}, err
	}

	result, publishErr := service.provider.Publish(ctx, providerRequest)
	if publishErr != nil {
		result = ProviderResult{
			SchemaVersion:  ProviderResultSchema,
			Status:         ProviderResultUnknown,
			IdempotencyKey: request.IdempotencyKey,
			ReasonCode:     "provider_call_outcome_unknown",
			ObservedAt:     service.timestamp(),
		}
	}
	if err := validateProviderBinding(providerRequest, result); err != nil {
		result = ProviderResult{
			SchemaVersion:  ProviderResultSchema,
			Status:         ProviderResultUnknown,
			IdempotencyKey: request.IdempotencyKey,
			ReasonCode:     "invalid_provider_result",
			ObservedAt:     service.timestamp(),
		}
		publishErr = err
	}
	if result.Status != ProviderResultPublished &&
		result.Status != ProviderResultRejected &&
		result.Status != ProviderResultUnknown {
		result = ProviderResult{
			SchemaVersion:  ProviderResultSchema,
			Status:         ProviderResultUnknown,
			IdempotencyKey: request.IdempotencyKey,
			ReasonCode:     "invalid_initial_provider_status",
			ObservedAt:     service.timestamp(),
		}
		publishErr = fmt.Errorf("initial provider result status is not definitive")
	}
	eventType, err := initialResultEventType(result.Status)
	if err != nil {
		return Record{}, err
	}
	if err := service.repository.append(resultEvent(request, digest, eventType, result)); err != nil {
		return Record{}, err
	}
	record, err := service.repository.loadOne(request.PublicationID)
	if err != nil {
		return Record{}, err
	}
	if result.Status == ProviderResultUnknown {
		if publishErr != nil {
			return record, fmt.Errorf("%w: %v", ErrUnknownOutcome, publishErr)
		}
		return record, ErrUnknownOutcome
	}
	return record, publishErr
}

// Reconcile queries the provider by the original idempotency identity. It
// never issues a second remote write.
func (service *Service) Reconcile(ctx context.Context, publicationID string) (Record, error) {
	if ctx == nil {
		return Record{}, fmt.Errorf("context is required")
	}
	if err := validateID("publication_id", publicationID); err != nil {
		return Record{}, err
	}
	service.repository.mu.Lock()
	defer service.repository.mu.Unlock()
	record, err := service.repository.loadOne(publicationID)
	if err != nil {
		return Record{}, err
	}
	return service.reconcileLocked(ctx, record)
}

func (service *Service) reconcileLocked(ctx context.Context, record Record) (Record, error) {
	if record.State == StatePublished || record.State == StateRejected ||
		record.State == StateNotFound {
		return cloneRecord(record), nil
	}
	if record.State != StateDispatching && record.State != StateUnknown {
		return Record{}, fmt.Errorf("publication %q is not reconcilable from state %q",
			record.Request.PublicationID, record.State)
	}
	if record.ProviderRequest == nil {
		return Record{}, fmt.Errorf("%w: reconcilable publication has no provider request",
			ErrConflict)
	}
	if err := contextErr(ctx); err != nil {
		return Record{}, err
	}
	result, err := service.provider.Lookup(ctx, *record.ProviderRequest)
	if err != nil {
		return cloneRecord(record), fmt.Errorf("%w: lookup: %v", ErrUnknownOutcome, err)
	}
	if err := validateProviderBinding(*record.ProviderRequest, result); err != nil {
		return cloneRecord(record), fmt.Errorf("%w: invalid lookup result: %v",
			ErrUnknownOutcome, err)
	}
	var eventType EventType
	switch result.Status {
	case ProviderResultPublished:
		eventType = EventReconciledPublished
	case ProviderResultNotFound:
		eventType = EventReconciledNotFound
	case ProviderResultUnknown:
		return cloneRecord(record), ErrUnknownOutcome
	default:
		return cloneRecord(record), fmt.Errorf("%w: lookup returned non-reconciling status %q",
			ErrUnknownOutcome, result.Status)
	}
	event := resultEvent(record.Request, record.RequestSHA256, eventType, result)
	event.EventID = record.Request.PublicationID + "-" + string(eventType)
	if err := service.repository.append(event); err != nil {
		return Record{}, err
	}
	return service.repository.loadOne(record.Request.PublicationID)
}

func validateExactRevalidation(request Request, observation Revalidation) error {
	if err := observation.Validate(); err != nil {
		return fmt.Errorf("invalid publication revalidation: %w", err)
	}
	switch {
	case observation.ProviderBaseRevision != request.BaseRevision:
		return fmt.Errorf("provider base moved")
	case observation.ProviderHeadRevision != request.ExpectedHeadRevision:
		return fmt.Errorf("provider head moved")
	case observation.CheckedAt.Before(request.CreatedAt):
		return fmt.Errorf("publication revalidation predates the request")
	case !observation.CheckedAt.Before(request.GrantExpiresAt):
		return fmt.Errorf("publication grant expired before dispatch")
	case !observation.PermissionGranted:
		return fmt.Errorf("publication permission was revoked")
	case observation.PermissionRevision != request.ExpectedPermissionRevision:
		return fmt.Errorf("publication permission revision changed")
	case observation.ConfigBundleSHA256 != request.ConfigBundleRef.SHA256:
		return fmt.Errorf("publication config changed")
	case observation.FindingSourceSHA256 != request.FindingSourceRef.SHA256:
		return fmt.Errorf("finding source changed")
	case !observation.FindingCurrent:
		return fmt.Errorf("finding is no longer current")
	case !observation.AnchorCurrent:
		return fmt.Errorf("finding anchor is stale")
	default:
		return nil
	}
}

func buildProviderRequest(request Request, digest string) ProviderRequest {
	return ProviderRequest{
		SchemaVersion:      ProviderRequestSchema,
		PublicationID:      request.PublicationID,
		IdempotencyKey:     request.IdempotencyKey,
		GrantID:            request.GrantID,
		Provider:           request.Provider,
		RepositoryID:       request.RepositoryID,
		ChangeKind:         request.ChangeKind,
		ChangeID:           request.ChangeID,
		BaseRevision:       request.BaseRevision,
		HeadRevision:       request.ExpectedHeadRevision,
		FindingID:          request.FindingID,
		Fingerprint:        request.Fingerprint,
		DecisionID:         request.DecisionID,
		PermissionRevision: request.ExpectedPermissionRevision,
		Anchor:             request.Anchor,
		Channel:            request.Channel,
		Message:            request.Message,
		RequestSHA256:      digest,
	}
}

func validateProviderBinding(request ProviderRequest, result ProviderResult) error {
	if err := request.Validate(); err != nil {
		return err
	}
	if err := result.Validate(); err != nil {
		return err
	}
	if result.IdempotencyKey != request.IdempotencyKey {
		return fmt.Errorf("provider result idempotency key does not match request")
	}
	return nil
}

func initialResultEventType(status ProviderResultStatus) (EventType, error) {
	switch status {
	case ProviderResultPublished:
		return EventPublished, nil
	case ProviderResultRejected:
		return EventRejected, nil
	case ProviderResultUnknown:
		return EventOutcomeUnknown, nil
	default:
		return "", fmt.Errorf("initial publish returned unsupported status %q", status)
	}
}

func resultEvent(request Request, digest string, eventType EventType,
	result ProviderResult) LedgerEvent {
	return LedgerEvent{
		SchemaVersion:  LedgerEventSchemaVersion,
		EventID:        request.PublicationID + "-" + string(eventType),
		Type:           eventType,
		PublicationID:  request.PublicationID,
		RequestSHA256:  digest,
		ProviderResult: &result,
		OccurredAt:     result.ObservedAt,
	}
}

func (repository *Repository) append(event LedgerEvent) error {
	if err := event.Validate(); err != nil {
		return err
	}
	if _, err := repository.store.AppendJSONL(publicationLedger, local.Event{
		ID:      event.EventID,
		Schema:  LedgerEventSchemaVersion,
		Time:    event.OccurredAt,
		Payload: event,
	}); err != nil {
		if errors.Is(err, local.ErrEventConflict) {
			return fmt.Errorf("%w: event %q", ErrConflict, event.EventID)
		}
		return fmt.Errorf("append publication event: %w", err)
	}
	return nil
}

func (repository *Repository) loadOne(publicationID string) (Record, error) {
	state, err := repository.load()
	if err != nil {
		return Record{}, err
	}
	record, ok := state[publicationID]
	if !ok {
		return Record{}, fmt.Errorf("publication %q not found", publicationID)
	}
	return cloneRecord(record), nil
}

func (repository *Repository) load() (map[string]Record, error) {
	envelopes, err := repository.store.ReadJSONL(publicationLedger)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]Record{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read publication ledger: %w", err)
	}
	state := make(map[string]Record)
	for _, envelope := range envelopes {
		if envelope.Schema != LedgerEventSchemaVersion {
			return nil, fmt.Errorf("publication ledger contains unsupported schema %q",
				envelope.Schema)
		}
		event, err := decodeEvent(envelope.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode publication event sequence %d: %w",
				envelope.Sequence, err)
		}
		if event.EventID != envelope.ID || !event.OccurredAt.Equal(envelope.Time) {
			return nil, fmt.Errorf("publication event envelope does not match payload")
		}
		record := state[event.PublicationID]
		if err := applyEvent(&record, event); err != nil {
			return nil, fmt.Errorf("apply publication event %q: %w", event.EventID, err)
		}
		state[event.PublicationID] = record
	}
	return state, nil
}

func applyEvent(record *Record, event LedgerEvent) error {
	if event.Type == EventRequested {
		if len(record.Events) != 0 {
			return fmt.Errorf("duplicate requested event")
		}
		digest, err := DigestRequest(*event.Request)
		if err != nil {
			return err
		}
		if digest != event.RequestSHA256 ||
			event.PublicationID != event.Request.PublicationID {
			return fmt.Errorf("requested event identity changed")
		}
		record.RequestSHA256 = digest
		record.Request = *event.Request
		record.State = StateRequested
		record.Events = []LedgerEvent{event}
		return nil
	}
	if len(record.Events) == 0 || record.RequestSHA256 != event.RequestSHA256 {
		return fmt.Errorf("event has no matching requested fact")
	}
	switch event.Type {
	case EventDispatchStarted:
		if record.State != StateRequested {
			return fmt.Errorf("dispatch requires requested state, got %q", record.State)
		}
		if event.ProviderRequest.RequestSHA256 != record.RequestSHA256 ||
			event.ProviderRequest.IdempotencyKey != record.Request.IdempotencyKey {
			return fmt.Errorf("provider request does not bind the immutable request")
		}
		record.State = StateDispatching
		record.Revalidation = cloneRevalidation(event.Revalidation)
		record.ProviderRequest = cloneProviderRequest(event.ProviderRequest)
	case EventPublished:
		if record.State != StateDispatching {
			return fmt.Errorf("published requires dispatching state, got %q", record.State)
		}
		record.State = StatePublished
		record.ProviderResult = cloneProviderResult(event.ProviderResult)
	case EventRejected:
		if record.State != StateRequested && record.State != StateDispatching {
			return fmt.Errorf("rejected transition is invalid from %q", record.State)
		}
		record.State = StateRejected
		record.ProviderResult = cloneProviderResult(event.ProviderResult)
	case EventOutcomeUnknown:
		if record.State != StateDispatching {
			return fmt.Errorf("unknown outcome requires dispatching state, got %q", record.State)
		}
		record.State = StateUnknown
		record.ProviderResult = cloneProviderResult(event.ProviderResult)
	case EventReconciledPublished:
		if record.State != StateDispatching && record.State != StateUnknown {
			return fmt.Errorf("reconciled publication is invalid from %q", record.State)
		}
		record.State = StatePublished
		record.ProviderResult = cloneProviderResult(event.ProviderResult)
	case EventReconciledNotFound:
		if record.State != StateDispatching && record.State != StateUnknown {
			return fmt.Errorf("reconciled absence is invalid from %q", record.State)
		}
		record.State = StateNotFound
		record.ProviderResult = cloneProviderResult(event.ProviderResult)
	default:
		return fmt.Errorf("unsupported event type %q", event.Type)
	}
	record.Events = append(record.Events, event)
	return nil
}

func cloneRecord(record Record) Record {
	record.Events = append([]LedgerEvent(nil), record.Events...)
	record.Revalidation = cloneRevalidation(record.Revalidation)
	record.ProviderRequest = cloneProviderRequest(record.ProviderRequest)
	record.ProviderResult = cloneProviderResult(record.ProviderResult)
	return record
}

func cloneRevalidation(value *Revalidation) *Revalidation {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneProviderRequest(value *ProviderRequest) *ProviderRequest {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneProviderResult(value *ProviderResult) *ProviderResult {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (service *Service) timestamp() time.Time {
	return service.now().UTC()
}

func contextErr(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
		return nil
	}
}
