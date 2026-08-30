package runrepo

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"argus.local/argus/internal/reviewconfig"
	"argus.local/argus/internal/runmodel"
	"argus.local/argus/internal/store/local"
)

const ImpactSnapshotSchemaVersion = "argus.review_run_impact_snapshot.v1alpha1"

const (
	ImpactIndexFactSchemaVersion = "argus.review_run_impact_index_fact.v1alpha1"
	impactIndexStream            = "review-run-impact-index"
)

type ImpactComponentKind string

const (
	ImpactConfigRevision ImpactComponentKind = "config_revision"
	ImpactConfigBundle   ImpactComponentKind = "config_bundle"
	ImpactRulePack       ImpactComponentKind = "rule_pack"
	ImpactWorkflow       ImpactComponentKind = "workflow"
	ImpactModel          ImpactComponentKind = "model"
)

type ImpactSelector struct {
	Kind     ImpactComponentKind `json:"kind"`
	ID       string              `json:"id"`
	Revision string              `json:"revision"`
	SHA256   string              `json:"sha256,omitempty"`
}

func (selector ImpactSelector) Validate() error {
	switch selector.Kind {
	case ImpactConfigRevision, ImpactConfigBundle, ImpactRulePack, ImpactWorkflow, ImpactModel:
	default:
		return fmt.Errorf("unsupported impact component kind %q", selector.Kind)
	}
	for name, value := range map[string]string{"id": selector.ID, "revision": selector.Revision} {
		if value == "" || len(value) > 256 || value != strings.TrimSpace(value) ||
			!utf8.ValidString(value) || strings.ContainsAny(value, `/\\`) {
			return fmt.Errorf("%s must be a clean non-path value of at most 256 bytes", name)
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return fmt.Errorf("%s contains control characters", name)
			}
		}
	}
	if selector.SHA256 != "" {
		decoded, err := hex.DecodeString(selector.SHA256)
		if err != nil || len(decoded) != 32 || selector.SHA256 != strings.ToLower(selector.SHA256) {
			return fmt.Errorf("sha256 must be a lowercase 64-character digest")
		}
	}
	return nil
}

type ImpactBinding struct {
	Kind     ImpactComponentKind `json:"kind"`
	ID       string              `json:"id"`
	Revision string              `json:"revision"`
	SHA256   string              `json:"sha256,omitempty"`
	Source   string              `json:"source"`
}

// ImpactIndexFact is the durable reverse-index row for one immutable
// ExecutionSnapshot. The writer appends it before the authoritative
// run.created/terminal event. An orphan row is therefore harmless, while a
// process crash after this append can retry the same fact without ambiguity.
// Queries only admit rows whose RunID is present in the selected run-index
// watermark.
type ImpactIndexFact struct {
	SchemaVersion       string          `json:"schema_version"`
	RunID               string          `json:"run_id"`
	ExecutionSnapshotID string          `json:"execution_snapshot_id"`
	Bindings            []ImpactBinding `json:"bindings"`
}

func DecodeImpactIndexFact(data []byte) (ImpactIndexFact, error) {
	var fact ImpactIndexFact
	if err := decodeStrictJSON(data, &fact); err != nil {
		return ImpactIndexFact{}, err
	}
	if err := fact.Validate(); err != nil {
		return ImpactIndexFact{}, err
	}
	return fact, nil
}

func (fact ImpactIndexFact) Validate() error {
	if fact.SchemaVersion != ImpactIndexFactSchemaVersion {
		return fmt.Errorf("unsupported impact index fact schema %q", fact.SchemaVersion)
	}
	for name, value := range map[string]string{
		"run_id": fact.RunID, "execution_snapshot_id": fact.ExecutionSnapshotID,
	} {
		if value == "" || len(value) > 256 || value != strings.TrimSpace(value) ||
			!utf8.ValidString(value) || strings.ContainsAny(value, `/\\`) {
			return fmt.Errorf("%s must be a clean non-path value of at most 256 bytes", name)
		}
		for _, character := range value {
			if unicode.IsControl(character) {
				return fmt.Errorf("%s contains control characters", name)
			}
		}
	}
	if len(fact.Bindings) < 4 {
		return fmt.Errorf("impact index fact must contain frozen config, rule, workflow, and model bindings")
	}
	previous := ImpactBinding{}
	counts := make(map[ImpactComponentKind]int)
	for index, binding := range fact.Bindings {
		selector := ImpactSelector{
			Kind: binding.Kind, ID: binding.ID, Revision: binding.Revision, SHA256: binding.SHA256,
		}
		if err := selector.Validate(); err != nil {
			return fmt.Errorf("bindings[%d]: %w", index, err)
		}
		if err := validateImpactBindingSource(binding); err != nil {
			return fmt.Errorf("bindings[%d]: %w", index, err)
		}
		if index > 0 && compareImpactBindings(previous, binding) >= 0 {
			return fmt.Errorf("bindings must be sorted and unique")
		}
		counts[binding.Kind]++
		previous = binding
	}
	for _, kind := range []ImpactComponentKind{
		ImpactConfigBundle, ImpactRulePack, ImpactWorkflow, ImpactModel,
	} {
		if counts[kind] == 0 {
			return fmt.Errorf("impact index fact is missing %s binding", kind)
		}
	}
	if counts[ImpactConfigBundle] != 1 || counts[ImpactRulePack] != 1 || counts[ImpactWorkflow] != 1 {
		return fmt.Errorf("impact index fact contains ambiguous singleton bindings")
	}
	return nil
}

func validateImpactBindingSource(binding ImpactBinding) error {
	wantSHA := true
	valid := false
	switch binding.Kind {
	case ImpactConfigRevision:
		valid = binding.Source == "config_bundle.applied_revisions"
		wantSHA = false
	case ImpactConfigBundle:
		valid = binding.Source == "execution_snapshot.config"
	case ImpactRulePack:
		valid = binding.Source == "config_bundle.rule_pack"
	case ImpactWorkflow:
		valid = binding.Source == "execution_snapshot.workflow"
	case ImpactModel:
		valid = binding.Source == "config_bundle.execution.model_profile" ||
			binding.Source == "config_bundle.agent_review.model"
	}
	if !valid {
		return fmt.Errorf("binding source %q is invalid for %s", binding.Source, binding.Kind)
	}
	if wantSHA && binding.SHA256 == "" {
		return fmt.Errorf("%s binding requires sha256", binding.Kind)
	}
	if !wantSHA && binding.SHA256 != "" {
		return fmt.Errorf("%s binding must not contain sha256", binding.Kind)
	}
	return nil
}

type ImpactMatch struct {
	History             HistoryEntry    `json:"history"`
	ExecutionSnapshotID string          `json:"execution_snapshot_id"`
	Bindings            []ImpactBinding `json:"bindings"`
}

type ImpactGap struct {
	RunID      string `json:"run_id"`
	ReasonCode string `json:"reason_code"`
}

type ImpactCoverage struct {
	HistoryEntries int         `json:"history_entries"`
	RunsResolved   int         `json:"runs_resolved"`
	Complete       bool        `json:"complete"`
	Gaps           []ImpactGap `json:"gaps"`
}

type ImpactSnapshot struct {
	SchemaVersion string         `json:"schema_version"`
	Watermark     uint64         `json:"watermark"`
	Selector      ImpactSelector `json:"selector"`
	Matches       []ImpactMatch  `json:"matches"`
	Coverage      ImpactCoverage `json:"coverage"`
}

type ImpactIndexRebuild struct {
	SchemaVersion  string      `json:"schema_version"`
	Watermark      uint64      `json:"watermark"`
	HistoryEntries int         `json:"history_entries"`
	ExistingFacts  int         `json:"existing_facts"`
	AppendedFacts  int         `json:"appended_facts"`
	Complete       bool        `json:"complete"`
	Gaps           []ImpactGap `json:"gaps"`
}

const ImpactIndexRebuildSchemaVersion = "argus.review_run_impact_index_rebuild.v1alpha1"

// ImpactAt resolves exact frozen component bindings from the same point-in-time
// run history used by pagination. Terminal runs must pass whole-run closure;
// nonterminal runs are included only when their authoritative created lineage
// resolves one unambiguous ExecutionSnapshot. Missing nonterminal evidence is
// returned as an explicit gap rather than silently omitted.
func (repository *Repository) ImpactAt(
	selector ImpactSelector,
	watermark uint64,
) (ImpactSnapshot, error) {
	if err := selector.Validate(); err != nil {
		return ImpactSnapshot{}, err
	}
	history, err := repository.HistoryAt(watermark)
	if err != nil {
		return ImpactSnapshot{}, err
	}
	result := ImpactSnapshot{
		SchemaVersion: ImpactSnapshotSchemaVersion,
		Watermark:     history.Watermark,
		Selector:      selector,
		Matches:       []ImpactMatch{},
		Coverage: ImpactCoverage{
			HistoryEntries: len(history.Entries), Complete: true, Gaps: []ImpactGap{},
		},
	}
	index, err := repository.loadImpactIndex()
	if err != nil {
		return ImpactSnapshot{}, err
	}
	for _, entry := range history.Entries {
		fact, resolved := index[entry.RunID]
		if !resolved {
			reason, err := repository.classifyMissingImpactIndex(entry)
			if err != nil {
				return ImpactSnapshot{}, fmt.Errorf("resolve impact index for run %q: %w", entry.RunID, err)
			}
			result.Coverage.Complete = false
			result.Coverage.Gaps = append(result.Coverage.Gaps, ImpactGap{
				RunID: entry.RunID, ReasonCode: reason,
			})
			continue
		}
		result.Coverage.RunsResolved++
		bindings := matchingIndexedBindings(selector, fact.Bindings)
		if len(bindings) != 0 {
			resolved, err := repository.validateIndexedImpactMatch(entry, fact)
			if err != nil {
				return ImpactSnapshot{}, fmt.Errorf("validate indexed impact run %q: %w", entry.RunID, err)
			}
			if !resolved {
				result.Coverage.RunsResolved--
				result.Coverage.Complete = false
				result.Coverage.Gaps = append(result.Coverage.Gaps, ImpactGap{
					RunID: entry.RunID, ReasonCode: "execution_snapshot_unavailable",
				})
				continue
			}
			result.Matches = append(result.Matches, ImpactMatch{
				History: entry, ExecutionSnapshotID: fact.ExecutionSnapshotID,
				Bindings: bindings,
			})
		}
	}
	return result, nil
}

func (repository *Repository) validateIndexedImpactMatch(
	entry HistoryEntry,
	fact ImpactIndexFact,
) (bool, error) {
	if entry.Status == runmodel.RunStatusSucceeded ||
		entry.Status == runmodel.RunStatusFailed ||
		entry.Status == runmodel.RunStatusCanceled {
		run, err := repository.LoadRun(entry.RunID)
		if err != nil {
			return false, err
		}
		if run.ExecutionSnapshotID != fact.ExecutionSnapshotID {
			return false, fmt.Errorf("terminal run execution snapshot differs from impact index")
		}
		return true, nil
	}
	snapshot, err := repository.ExecutionSnapshotForRun(entry.RunID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if snapshot.ExecutionSnapshotID != fact.ExecutionSnapshotID {
		return false, fmt.Errorf("nonterminal run execution snapshot differs from impact index")
	}
	return true, nil
}

func (repository *Repository) classifyMissingImpactIndex(
	entry HistoryEntry,
) (string, error) {
	snapshot, resolved, err := repository.impactSnapshotForEntry(entry)
	if err != nil {
		return "", err
	}
	if !resolved {
		return "execution_snapshot_unavailable", nil
	}
	_, err = repository.buildImpactIndexFact(entry.RunID, snapshot)
	if err != nil {
		return "", err
	}
	return "impact_index_unavailable", nil
}

// RebuildImpactIndex is an explicit, idempotent read-model repair. It is
// intended for process startup and offline maintenance, not request handlers:
// GET impact queries remain side-effect free. Complete historical closures are
// appended once; lineage without an ExecutionSnapshot remains an explicit gap.
func (repository *Repository) RebuildImpactIndex(
	ctx context.Context,
) (ImpactIndexRebuild, error) {
	if ctx == nil {
		return ImpactIndexRebuild{}, fmt.Errorf("context is required")
	}
	history, err := repository.HistoryAt(0)
	if err != nil {
		return ImpactIndexRebuild{}, err
	}
	index, err := repository.loadImpactIndex()
	if err != nil {
		return ImpactIndexRebuild{}, err
	}
	report := ImpactIndexRebuild{
		SchemaVersion: ImpactIndexRebuildSchemaVersion, Watermark: history.Watermark,
		HistoryEntries: len(history.Entries), ExistingFacts: len(index),
		Complete: true, Gaps: []ImpactGap{},
	}
	for _, entry := range history.Entries {
		if err := ctx.Err(); err != nil {
			return ImpactIndexRebuild{}, err
		}
		if _, exists := index[entry.RunID]; exists {
			continue
		}
		snapshot, resolved, err := repository.impactSnapshotForEntry(entry)
		if err != nil {
			return ImpactIndexRebuild{}, fmt.Errorf("resolve impact run %q: %w", entry.RunID, err)
		}
		if !resolved {
			report.Complete = false
			report.Gaps = append(report.Gaps, ImpactGap{
				RunID: entry.RunID, ReasonCode: "execution_snapshot_unavailable",
			})
			continue
		}
		fact, err := repository.buildImpactIndexFact(entry.RunID, snapshot)
		if err != nil {
			return ImpactIndexRebuild{}, fmt.Errorf("build impact run %q: %w", entry.RunID, err)
		}
		if err := repository.appendImpactIndexFact(fact, snapshot.CreatedAt); err != nil {
			return ImpactIndexRebuild{}, err
		}
		index[entry.RunID] = fact
		report.AppendedFacts++
	}
	return report, nil
}

func (repository *Repository) buildImpactIndexFact(
	runID string,
	snapshot runmodel.ExecutionSnapshot,
) (ImpactIndexFact, error) {
	data, err := repository.ReadArtifact(snapshot.ConfigBundleRef)
	if err != nil {
		return ImpactIndexFact{}, fmt.Errorf("read impact config bundle: %w", err)
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return ImpactIndexFact{}, fmt.Errorf("decode impact config bundle: %w", err)
	}
	return newImpactIndexFact(runID, snapshot, bundle)
}

func newImpactIndexFact(
	runID string,
	snapshot runmodel.ExecutionSnapshot,
	bundle reviewconfig.ConfigBundle,
) (ImpactIndexFact, error) {
	if bundle.BundleID != snapshot.Config.ID || bundle.SHA256 != snapshot.Config.SHA256 {
		return ImpactIndexFact{}, fmt.Errorf("impact config bundle does not match execution snapshot")
	}
	fact := ImpactIndexFact{
		SchemaVersion: ImpactIndexFactSchemaVersion, RunID: runID,
		ExecutionSnapshotID: snapshot.ExecutionSnapshotID,
		Bindings:            allImpactBindings(snapshot, bundle),
	}
	if err := fact.Validate(); err != nil {
		return ImpactIndexFact{}, err
	}
	return fact, nil
}

// opportunisticImpactIndexFact is used only beside run-ledger writes. The
// index is derived and must not make it impossible to persist deliberately
// incomplete legacy/failure lineage facts. It bypasses lifecycle side effects
// while probing: ImpactAt still uses the governed artifact read and fails
// closed when it later backfills a missing row.
func (repository *Repository) opportunisticImpactIndexFact(
	runID string,
	executionSnapshotID string,
) (ImpactIndexFact, time.Time, bool) {
	snapshot, err := repository.LoadExecutionSnapshot(executionSnapshotID)
	if err != nil || snapshot.ExecutionSnapshotID != executionSnapshotID {
		return ImpactIndexFact{}, time.Time{}, false
	}
	data, err := repository.readArtifactWithoutLifecycle(snapshot.ConfigBundleRef)
	if err != nil {
		return ImpactIndexFact{}, time.Time{}, false
	}
	bundle, err := reviewconfig.DecodeBundle(data)
	if err != nil {
		return ImpactIndexFact{}, time.Time{}, false
	}
	fact, err := newImpactIndexFact(runID, snapshot, bundle)
	if err != nil {
		return ImpactIndexFact{}, time.Time{}, false
	}
	return fact, snapshot.CreatedAt, true
}

func (repository *Repository) appendImpactIndexFact(
	fact ImpactIndexFact,
	at time.Time,
) error {
	if err := fact.Validate(); err != nil {
		return err
	}
	if at.IsZero() {
		return fmt.Errorf("impact index fact time is required")
	}
	_, err := repository.store.AppendJSONL(impactIndexStream, local.Event{
		ID: fact.RunID + "-impact-index", Schema: ImpactIndexFactSchemaVersion,
		Time: at.UTC(), Payload: fact,
	})
	if err != nil {
		return fmt.Errorf("append impact index fact: %w", err)
	}
	return nil
}

func (repository *Repository) loadImpactIndex() (map[string]ImpactIndexFact, error) {
	envelopes, err := repository.store.ReadJSONL(impactIndexStream)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]ImpactIndexFact), nil
		}
		return nil, fmt.Errorf("read impact index: %w", err)
	}
	index := make(map[string]ImpactIndexFact, len(envelopes))
	for _, envelope := range envelopes {
		if envelope.Schema != ImpactIndexFactSchemaVersion {
			return nil, fmt.Errorf("impact index sequence %d has unsupported schema %q", envelope.Sequence, envelope.Schema)
		}
		fact, err := DecodeImpactIndexFact(envelope.Payload)
		if err != nil {
			return nil, fmt.Errorf("decode impact index sequence %d: %w", envelope.Sequence, err)
		}
		if envelope.ID != fact.RunID+"-impact-index" {
			return nil, fmt.Errorf("impact index sequence %d has mismatched event identity", envelope.Sequence)
		}
		if _, duplicate := index[fact.RunID]; duplicate {
			return nil, fmt.Errorf("impact index contains duplicate run %q", fact.RunID)
		}
		index[fact.RunID] = fact
	}
	return index, nil
}

func (repository *Repository) impactSnapshotForEntry(
	entry HistoryEntry,
) (runmodel.ExecutionSnapshot, bool, error) {
	run, terminalErr := repository.LoadRun(entry.RunID)
	if terminalErr == nil {
		snapshot, err := repository.LoadExecutionSnapshot(run.ExecutionSnapshotID)
		return snapshot, err == nil, err
	}
	if entry.Status == runmodel.RunStatusSucceeded ||
		entry.Status == runmodel.RunStatusFailed ||
		entry.Status == runmodel.RunStatusCanceled ||
		!errors.Is(terminalErr, os.ErrNotExist) {
		return runmodel.ExecutionSnapshot{}, false, terminalErr
	}
	snapshot, err := repository.ExecutionSnapshotForRun(entry.RunID)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return runmodel.ExecutionSnapshot{}, false, nil
		}
		return runmodel.ExecutionSnapshot{}, false, err
	}
	return snapshot, true, nil
}

func matchingImpactBindings(
	selector ImpactSelector,
	snapshot runmodel.ExecutionSnapshot,
	bundle reviewconfig.ConfigBundle,
) []ImpactBinding {
	return matchingIndexedBindings(selector, allImpactBindings(snapshot, bundle))
}

func matchingIndexedBindings(
	selector ImpactSelector,
	indexed []ImpactBinding,
) []ImpactBinding {
	bindings := make([]ImpactBinding, 0, 2)
	for _, binding := range indexed {
		if binding.Kind != selector.Kind || binding.ID != selector.ID ||
			binding.Revision != selector.Revision ||
			(selector.SHA256 != "" && binding.SHA256 != selector.SHA256) {
			continue
		}
		bindings = append(bindings, binding)
	}
	return bindings
}

func allImpactBindings(
	snapshot runmodel.ExecutionSnapshot,
	bundle reviewconfig.ConfigBundle,
) []ImpactBinding {
	bindings := make([]ImpactBinding, 0, len(bundle.AppliedRevisions)+5)
	for _, revision := range bundle.AppliedRevisions {
		bindings = append(bindings, ImpactBinding{
			Kind: ImpactConfigRevision, ID: revision.ID, Revision: revision.Revision,
			Source: "config_bundle.applied_revisions",
		})
	}
	bindings = append(bindings,
		ImpactBinding{
			Kind: ImpactConfigBundle, ID: snapshot.Config.ID, Revision: snapshot.Config.Revision,
			SHA256: snapshot.Config.SHA256, Source: "execution_snapshot.config",
		},
		ImpactBinding{
			Kind: ImpactRulePack, ID: bundle.RulePack.ID, Revision: bundle.RulePack.Revision,
			SHA256: bundle.RulePack.SHA256, Source: "config_bundle.rule_pack",
		},
		ImpactBinding{
			Kind: ImpactWorkflow, ID: snapshot.Workflow.ID, Revision: snapshot.Workflow.Revision,
			SHA256: snapshot.Workflow.SHA256, Source: "execution_snapshot.workflow",
		},
		versionedImpactBinding(
			ImpactModel, bundle.Execution.ModelProfile,
			"config_bundle.execution.model_profile",
		),
	)
	if bundle.AgentReview != nil {
		bindings = append(bindings, versionedImpactBinding(
			ImpactModel, bundle.AgentReview.Model, "config_bundle.agent_review.model",
		))
	}
	slices.SortFunc(bindings, compareImpactBindings)
	return slices.Compact(bindings)
}

func compareImpactBindings(left, right ImpactBinding) int {
	for _, values := range [][2]string{
		{string(left.Kind), string(right.Kind)},
		{left.ID, right.ID},
		{left.Revision, right.Revision},
		{left.SHA256, right.SHA256},
		{left.Source, right.Source},
	} {
		if comparison := strings.Compare(values[0], values[1]); comparison != 0 {
			return comparison
		}
	}
	return 0
}

func versionedImpactBinding(
	kind ImpactComponentKind,
	ref reviewconfig.VersionedRef,
	source string,
) ImpactBinding {
	return ImpactBinding{
		Kind: kind, ID: ref.ID, Revision: ref.Revision, SHA256: ref.SHA256, Source: source,
	}
}
