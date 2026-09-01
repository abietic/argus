package controlplane

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/abietic/argus/internal/reviewcore"
)

const findingSetSchemaVersion = "argus.finding_set.v1alpha1"

type findingSet struct {
	SchemaVersion string                       `json:"schema_version"`
	TargetDigest  string                       `json:"target_digest"`
	Findings      []reviewcore.Finding         `json:"findings"`
	Decisions     []reviewcore.FindingDecision `json:"decisions"`
}

func decodeFindingSet(data []byte) (findingSet, error) {
	var set findingSet
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&set); err != nil {
		return findingSet{}, fmt.Errorf("decode finding set: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return findingSet{}, fmt.Errorf("finding set contains trailing JSON")
		}
		return findingSet{}, fmt.Errorf("decode finding set trailing JSON: %w", err)
	}
	if set.SchemaVersion != findingSetSchemaVersion || set.TargetDigest == "" ||
		set.Findings == nil || set.Decisions == nil {
		return findingSet{}, fmt.Errorf("finding set is incomplete")
	}
	for index, finding := range set.Findings {
		if err := finding.Validate(); err != nil {
			return findingSet{}, fmt.Errorf("findings[%d]: %w", index, err)
		}
	}
	for index, decision := range set.Decisions {
		if err := decision.Validate(); err != nil {
			return findingSet{}, fmt.Errorf("decisions[%d]: %w", index, err)
		}
	}
	return set, nil
}
