package analyticsadapter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/abietic/argus/internal/analytics"
	feedbackdomain "github.com/abietic/argus/internal/feedback"
	"github.com/abietic/argus/internal/publication"
	"github.com/abietic/argus/internal/runrepo"
	"github.com/abietic/argus/internal/store/local"
)

var (
	ErrProjectionConflict = errors.New("analytics projection snapshot conflict")
	ErrProjectionCorrupt  = errors.New("analytics projection snapshot corrupt")
)

type Adapter struct {
	runs        *runrepo.Repository
	feedback    *feedbackdomain.Repository
	store       *local.Store
	evaluations EvaluationSource
	lineages    FindingLineageSource
	publication PublicationSource
}

// PublicationSource exposes only the immutable channel ledger query required
// by analytics. A FindingDecision is eligibility evidence, not proof that a
// provider comment exists.
type PublicationSource interface {
	ByFinding(runID string, findingID string) ([]publication.Record, error)
}

// projectionManifest is the immutable, digest-bound lookup record for a
// materialized snapshot. Query paths load this object and its content-addressed
// artifact only; authoritative JSONL ledgers are never consulted.
type projectionManifest struct {
	SchemaVersion string                    `json:"schema_version"`
	SnapshotID    string                    `json:"snapshot_id"`
	SnapshotRef   local.ArtifactRef         `json:"snapshot_ref"`
	Scope         Scope                     `json:"scope"`
	Window        analytics.TimeWindow      `json:"window"`
	GroupBy       []analytics.DimensionName `json:"group_by"`
	BuiltAt       time.Time                 `json:"built_at"`
}

// New creates a rebuild-capable adapter. Feedback may be nil, but Rebuild then
// records feedback/outcome as unknown and marks the FactSet partial.
func New(
	runs *runrepo.Repository,
	feedback *feedbackdomain.Repository,
	store *local.Store,
	evaluations EvaluationSource,
	publicationSources ...PublicationSource,
) (*Adapter, error) {
	return newAdapter(runs, feedback, store, evaluations, nil, publicationSources...)
}

func NewWithFindingLineages(
	runs *runrepo.Repository,
	feedback *feedbackdomain.Repository,
	store *local.Store,
	evaluations EvaluationSource,
	lineages FindingLineageSource,
	publicationSources ...PublicationSource,
) (*Adapter, error) {
	return newAdapter(runs, feedback, store, evaluations, lineages, publicationSources...)
}

func newAdapter(
	runs *runrepo.Repository,
	feedback *feedbackdomain.Repository,
	store *local.Store,
	evaluations EvaluationSource,
	lineages FindingLineageSource,
	publicationSources ...PublicationSource,
) (*Adapter, error) {
	if runs == nil || store == nil {
		return nil, fmt.Errorf("run repository and projection store are required")
	}
	if len(publicationSources) > 1 {
		return nil, fmt.Errorf("at most one publication source may be configured")
	}
	var publicationSource PublicationSource
	if len(publicationSources) == 1 {
		publicationSource = publicationSources[0]
	}
	return &Adapter{
		runs:        runs,
		feedback:    feedback,
		store:       store,
		evaluations: evaluations,
		lineages:    lineages,
		publication: publicationSource,
	}, nil
}

// Open creates a query/export-only adapter. It cannot rebuild and therefore
// cannot accidentally scan authoritative source ledgers.
func Open(store *local.Store) (*Adapter, error) {
	if store == nil {
		return nil, fmt.Errorf("projection store is required")
	}
	return &Adapter{store: store}, nil
}

func (adapter *Adapter) Query(snapshotID string) (ProjectionSnapshot, error) {
	if adapter == nil || adapter.store == nil {
		return ProjectionSnapshot{}, fmt.Errorf("projection adapter is not initialized")
	}
	if err := validateID("snapshot_id", snapshotID); err != nil {
		return ProjectionSnapshot{}, err
	}
	manifest, err := adapter.loadManifest(snapshotID)
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	return adapter.loadSnapshot(manifest)
}

func (adapter *Adapter) QueryLatest(
	selector Selector,
) (ProjectionSnapshot, error) {
	if adapter == nil || adapter.store == nil {
		return ProjectionSnapshot{}, fmt.Errorf("projection adapter is not initialized")
	}
	selector.GroupBy = canonicalGroupBy(selector.GroupBy)
	if err := selector.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	manifests, err := adapter.loadManifests()
	if err != nil {
		return ProjectionSnapshot{}, err
	}
	var selected *projectionManifest
	for index := range manifests {
		manifest := manifests[index]
		if manifest.Scope != selector.Scope || manifest.Window != selector.Window ||
			!slices.Equal(manifest.GroupBy, selector.GroupBy) {
			continue
		}
		if selected == nil || manifest.BuiltAt.After(selected.BuiltAt) ||
			manifest.BuiltAt.Equal(selected.BuiltAt) &&
				manifest.SnapshotID > selected.SnapshotID {
			copy := manifest
			selected = &copy
		}
	}
	if selected == nil {
		return ProjectionSnapshot{}, fmt.Errorf(
			"projection selector has no snapshot: %w",
			os.ErrNotExist,
		)
	}
	return adapter.loadSnapshot(*selected)
}

func (adapter *Adapter) ListSnapshots() ([]SnapshotSummary, error) {
	if adapter == nil || adapter.store == nil {
		return nil, fmt.Errorf("projection adapter is not initialized")
	}
	manifests, err := adapter.loadManifests()
	if err != nil {
		return nil, err
	}
	summaries := make([]SnapshotSummary, 0, len(manifests))
	for _, manifest := range manifests {
		summaries = append(summaries, SnapshotSummary{
			SnapshotID: manifest.SnapshotID,
			Scope:      manifest.Scope,
			Window:     manifest.Window,
			GroupBy:    slices.Clone(manifest.GroupBy),
			BuiltAt:    manifest.BuiltAt,
		})
	}
	sort.Slice(summaries, func(left, right int) bool {
		return summaries[left].SnapshotID < summaries[right].SnapshotID
	})
	return summaries, nil
}

func (adapter *Adapter) Export(
	snapshotID string,
	target ExportTarget,
	format string,
) (analytics.ExportBundle, error) {
	snapshot, err := adapter.Query(snapshotID)
	if err != nil {
		return analytics.ExportBundle{}, err
	}
	switch target {
	case ExportFacts:
		switch format {
		case analytics.CanonicalJSONExportFormat:
			return analytics.ExportFactSetJSON(snapshot.Facts)
		case analytics.CanonicalCSVExportFormat:
			return analytics.ExportFactSetCSV(snapshot.Facts)
		case analytics.CanonicalParquetExportFormat:
			return ExportFactSetParquet(snapshot.Facts)
		}
	case ExportDashboard:
		switch format {
		case analytics.CanonicalJSONExportFormat:
			return analytics.ExportDashboardJSON(snapshot.Dashboard)
		case analytics.CanonicalCSVExportFormat:
			return analytics.ExportDashboardCSV(snapshot.Dashboard)
		case analytics.CanonicalParquetExportFormat:
			return analytics.ExportBundle{}, fmt.Errorf(
				"Parquet export is available only for the facts target",
			)
		}
	default:
		return analytics.ExportBundle{}, fmt.Errorf("unsupported export target %q", target)
	}
	return analytics.ExportBundle{}, fmt.Errorf("unsupported export format %q", format)
}

func (adapter *Adapter) persist(
	ctx context.Context,
	snapshot ProjectionSnapshot,
) (ProjectionSnapshot, error) {
	if err := contextErr(ctx); err != nil {
		return ProjectionSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("validate projection snapshot: %w", err)
	}
	data, err := json.Marshal(snapshot)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("marshal projection snapshot: %w", err)
	}
	ref, err := adapter.store.PutArtifact(data)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf("persist projection artifact: %w", err)
	}
	manifest := projectionManifest{
		SchemaVersion: ProjectionManifestSchemaVersion,
		SnapshotID:    snapshot.SnapshotID,
		SnapshotRef:   ref,
		Scope:         snapshot.Scope,
		Window:        snapshot.Window,
		GroupBy:       slices.Clone(snapshot.GroupBy),
		BuiltAt:       snapshot.BuiltAt,
	}
	if err := manifest.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	objectID := projectionManifestID(snapshot.SnapshotID)
	var existing projectionManifest
	getErr := adapter.store.GetJSON(objectID, &existing)
	switch {
	case getErr == nil:
		if !reflect.DeepEqual(existing, manifest) {
			return ProjectionSnapshot{}, fmt.Errorf(
				"%w: snapshot id %q already binds different facts",
				ErrProjectionConflict,
				snapshot.SnapshotID,
			)
		}
	case errors.Is(getErr, os.ErrNotExist):
		if err := contextErr(ctx); err != nil {
			return ProjectionSnapshot{}, err
		}
		if putErr := adapter.store.PutJSON(objectID, manifest); putErr != nil {
			if !errors.Is(putErr, local.ErrImmutableExists) {
				return ProjectionSnapshot{}, fmt.Errorf(
					"persist projection manifest: %w",
					putErr,
				)
			}
			if loadErr := adapter.store.GetJSON(objectID, &existing); loadErr != nil {
				return ProjectionSnapshot{}, fmt.Errorf("load concurrent manifest: %w", loadErr)
			}
			if !reflect.DeepEqual(existing, manifest) {
				return ProjectionSnapshot{}, fmt.Errorf(
					"%w: snapshot id %q raced with different facts",
					ErrProjectionConflict,
					snapshot.SnapshotID,
				)
			}
		}
	default:
		return ProjectionSnapshot{}, fmt.Errorf(
			"%w: read projection manifest: %v",
			ErrProjectionCorrupt,
			getErr,
		)
	}
	return snapshot, nil
}

func (adapter *Adapter) loadManifest(snapshotID string) (projectionManifest, error) {
	var manifest projectionManifest
	if err := adapter.store.GetJSON(projectionManifestID(snapshotID), &manifest); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return projectionManifest{}, fmt.Errorf(
				"projection snapshot %q: %w",
				snapshotID,
				os.ErrNotExist,
			)
		}
		return projectionManifest{}, fmt.Errorf(
			"%w: load manifest for %q: %v",
			ErrProjectionCorrupt,
			snapshotID,
			err,
		)
	}
	if manifest.SnapshotID != snapshotID {
		return projectionManifest{}, fmt.Errorf(
			"%w: manifest lookup does not bind snapshot %q",
			ErrProjectionCorrupt,
			snapshotID,
		)
	}
	if err := manifest.Validate(); err != nil {
		return projectionManifest{}, fmt.Errorf(
			"%w: manifest for %q: %v",
			ErrProjectionCorrupt,
			snapshotID,
			err,
		)
	}
	return manifest, nil
}

func (adapter *Adapter) loadManifests() ([]projectionManifest, error) {
	directory := filepath.Join(
		adapter.store.Root(),
		"immutable",
		filepath.FromSlash(strings.TrimSuffix(projectionManifestRoot, "/")),
	)
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return []projectionManifest{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%w: inspect manifest directory: %v", ErrProjectionCorrupt, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("%w: manifest directory is unsafe", ErrProjectionCorrupt)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("%w: read manifest directory: %v", ErrProjectionCorrupt, err)
	}
	manifests := make([]projectionManifest, 0, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() ||
			filepath.Ext(name) != ".json" {
			return nil, fmt.Errorf(
				"%w: unexpected manifest directory entry %q",
				ErrProjectionCorrupt,
				name,
			)
		}
		digest := strings.TrimSuffix(name, ".json")
		if err := validateSHA256("manifest object id", digest); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrProjectionCorrupt, err)
		}
		var manifest projectionManifest
		if err := adapter.store.GetJSON(projectionManifestRoot+digest, &manifest); err != nil {
			return nil, fmt.Errorf(
				"%w: load manifest object %q: %v",
				ErrProjectionCorrupt,
				digest,
				err,
			)
		}
		if projectionManifestID(manifest.SnapshotID) != projectionManifestRoot+digest {
			return nil, fmt.Errorf(
				"%w: manifest object %q does not bind snapshot id",
				ErrProjectionCorrupt,
				digest,
			)
		}
		if err := manifest.Validate(); err != nil {
			return nil, fmt.Errorf("%w: manifest %q: %v", ErrProjectionCorrupt, digest, err)
		}
		manifests = append(manifests, manifest)
	}
	return manifests, nil
}

func (adapter *Adapter) loadSnapshot(
	manifest projectionManifest,
) (ProjectionSnapshot, error) {
	data, err := adapter.store.ReadArtifact(manifest.SnapshotRef)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf(
			"%w: load projection %q: %v",
			ErrProjectionCorrupt,
			manifest.SnapshotID,
			err,
		)
	}
	snapshot, err := decodeProjectionSnapshot(data)
	if err != nil {
		return ProjectionSnapshot{}, fmt.Errorf(
			"%w: decode projection %q: %v",
			ErrProjectionCorrupt,
			manifest.SnapshotID,
			err,
		)
	}
	if snapshot.SnapshotID != manifest.SnapshotID ||
		snapshot.Scope != manifest.Scope ||
		snapshot.Window != manifest.Window ||
		!slices.Equal(snapshot.GroupBy, manifest.GroupBy) ||
		!snapshot.BuiltAt.Equal(manifest.BuiltAt) {
		return ProjectionSnapshot{}, fmt.Errorf(
			"%w: projection %q does not match immutable manifest",
			ErrProjectionCorrupt,
			manifest.SnapshotID,
		)
	}
	return snapshot, nil
}

func decodeProjectionSnapshot(data []byte) (ProjectionSnapshot, error) {
	var snapshot ProjectionSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ProjectionSnapshot{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return ProjectionSnapshot{}, fmt.Errorf("multiple JSON values")
		}
		return ProjectionSnapshot{}, err
	}
	if err := snapshot.Validate(); err != nil {
		return ProjectionSnapshot{}, err
	}
	return snapshot, nil
}

func (manifest projectionManifest) Validate() error {
	if manifest.SchemaVersion != ProjectionManifestSchemaVersion {
		return fmt.Errorf("unsupported projection manifest schema %q", manifest.SchemaVersion)
	}
	request := RebuildRequest{
		SnapshotID: manifest.SnapshotID,
		Scope:      manifest.Scope,
		Window:     manifest.Window,
		GroupBy:    manifest.GroupBy,
		BuiltAt:    manifest.BuiltAt,
	}
	if err := request.Validate(); err != nil {
		return err
	}
	if err := validateSHA256("snapshot_ref.sha256", manifest.SnapshotRef.SHA256); err != nil {
		return err
	}
	if manifest.SnapshotRef.URI !=
		"artifact://local/sha256/"+manifest.SnapshotRef.SHA256 {
		return fmt.Errorf("snapshot_ref.uri does not bind its digest")
	}
	if manifest.SnapshotRef.SizeBytes <= 0 {
		return fmt.Errorf("snapshot_ref.size_bytes must be positive")
	}
	return nil
}

func projectionManifestID(snapshotID string) string {
	return projectionManifestRoot + digestBytes([]byte(snapshotID))
}

func digestBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("context is required")
	}
	return ctx.Err()
}
