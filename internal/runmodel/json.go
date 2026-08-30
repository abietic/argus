package runmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeExecutionSnapshot strictly decodes the immutable snapshot contract.
// Unknown, duplicate, null, and trailing fields fail closed before validation.
func DecodeExecutionSnapshot(data []byte) (ExecutionSnapshot, error) {
	if err := rejectDuplicateSnapshotFields(data); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf("decode ExecutionSnapshot structure: %w", err)
	}
	var snapshot ExecutionSnapshot
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&snapshot); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf("decode ExecutionSnapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return ExecutionSnapshot{}, fmt.Errorf("ExecutionSnapshot contains multiple JSON values")
		}
		return ExecutionSnapshot{}, fmt.Errorf("decode trailing ExecutionSnapshot data: %w", err)
	}
	if err := snapshot.Validate(); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf("validate ExecutionSnapshot: %w", err)
	}
	return snapshot, nil
}

// DecodeReviewRun strictly decodes the immutable terminal run contract.
func DecodeReviewRun(data []byte) (ReviewRun, error) {
	var run ReviewRun
	if err := decodeStrictRunModel(data, "ReviewRun", &run); err != nil {
		return ReviewRun{}, err
	}
	if err := run.Validate(); err != nil {
		return ReviewRun{}, fmt.Errorf("validate ReviewRun: %w", err)
	}
	return run, nil
}

// DecodeReplayChangeSet strictly decodes the immutable replay declaration.
func DecodeReplayChangeSet(data []byte) (ReplayChangeSet, error) {
	var change ReplayChangeSet
	if err := decodeStrictRunModel(data, "ReplayChangeSet", &change); err != nil {
		return ReplayChangeSet{}, err
	}
	if err := change.Validate(); err != nil {
		return ReplayChangeSet{}, fmt.Errorf("validate ReplayChangeSet: %w", err)
	}
	return change, nil
}

func decodeStrictRunModel(data []byte, name string, out any) error {
	if err := rejectDuplicateSnapshotFields(data); err != nil {
		return fmt.Errorf("decode %s structure: %w", name, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return fmt.Errorf("decode %s: %w", name, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("%s contains multiple JSON values", name)
		}
		return fmt.Errorf("decode trailing %s data: %w", name, err)
	}
	return nil
}

func rejectDuplicateSnapshotFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectSnapshotJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("unexpected trailing JSON token")
	}
	return nil
}

func inspectSnapshotJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	if token == nil {
		return fmt.Errorf("explicit JSON null is not allowed")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectSnapshotJSON(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("invalid JSON object terminator")
		}
	case '[':
		for decoder.More() {
			if err := inspectSnapshotJSON(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("invalid JSON array terminator")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
