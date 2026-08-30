package targetmodel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// DecodeMaterializedTarget is the strict admission path for a frozen target
// artifact. Unknown fields, duplicate object keys, trailing JSON, and an
// invalid semantic closure all fail closed.
func DecodeMaterializedTarget(data []byte) (MaterializedTarget, error) {
	if err := rejectDuplicateTargetFields(data); err != nil {
		return MaterializedTarget{}, fmt.Errorf(
			"decode MaterializedTarget structure: %w",
			err,
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var target MaterializedTarget
	if err := decoder.Decode(&target); err != nil {
		return MaterializedTarget{}, fmt.Errorf("decode MaterializedTarget: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return MaterializedTarget{}, fmt.Errorf(
				"MaterializedTarget contains multiple JSON values",
			)
		}
		return MaterializedTarget{}, fmt.Errorf(
			"decode trailing MaterializedTarget data: %w",
			err,
		)
	}
	if err := target.Validate(); err != nil {
		return MaterializedTarget{}, fmt.Errorf("validate MaterializedTarget: %w", err)
	}
	return target, nil
}

func rejectDuplicateTargetFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectTargetJSON(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		if err != nil {
			return err
		}
		return fmt.Errorf("multiple JSON values")
	}
	return nil
}

func inspectTargetJSON(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
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
			if err := inspectTargetJSON(decoder); err != nil {
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
			if err := inspectTargetJSON(decoder); err != nil {
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
