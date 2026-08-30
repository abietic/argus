package workflow

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

const maxDefinitionBytes = 4 << 20

// DecodeDefinition is the only supported JSON admission path for a workflow
// artifact. It rejects duplicate and unknown fields as well as trailing JSON.
func DecodeDefinition(data []byte) (Definition, error) {
	if len(data) == 0 || len(data) > maxDefinitionBytes {
		return Definition{}, fmt.Errorf(
			"WorkflowDefinition must contain between 1 and %d bytes",
			maxDefinitionBytes,
		)
	}
	if err := rejectDuplicateFields(data); err != nil {
		return Definition{}, fmt.Errorf("decode WorkflowDefinition: %w", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var definition Definition
	if err := decoder.Decode(&definition); err != nil {
		return Definition{}, fmt.Errorf("decode WorkflowDefinition: %w", err)
	}
	var trailing any
	switch err := decoder.Decode(&trailing); {
	case errors.Is(err, io.EOF):
	case err == nil:
		return Definition{}, fmt.Errorf("decode WorkflowDefinition: multiple JSON values")
	default:
		return Definition{}, fmt.Errorf("decode WorkflowDefinition trailing JSON: %w", err)
	}
	if err := definition.Validate(); err != nil {
		return Definition{}, fmt.Errorf("validate WorkflowDefinition: %w", err)
	}
	return definition, nil
}

func rejectDuplicateFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectValue(decoder); err != nil {
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

func inspectValue(decoder *json.Decoder) error {
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
			token, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := token.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectValue(decoder); err != nil {
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
			if err := inspectValue(decoder); err != nil {
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
