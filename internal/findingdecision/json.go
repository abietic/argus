package findingdecision

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func DecodeRequestJSON(data []byte) (Request, error) {
	var request Request
	if err := decodeStrictJSON(data, &request); err != nil {
		return Request{}, fmt.Errorf("decode finding decision request JSON: %w", err)
	}
	if err := request.Validate(); err != nil {
		return Request{}, fmt.Errorf("validate finding decision request: %w", err)
	}
	return request, nil
}

func DecodeMutationJSON(data []byte) (Mutation, error) {
	var mutation Mutation
	if err := decodeStrictJSON(data, &mutation); err != nil {
		return Mutation{}, fmt.Errorf("decode finding decision mutation JSON: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return Mutation{}, fmt.Errorf("validate finding decision mutation: %w", err)
	}
	return mutation, nil
}

func DecodeDecisionJSON(data []byte) (Decision, error) {
	var decision Decision
	if err := decodeStrictJSON(data, &decision); err != nil {
		return Decision{}, fmt.Errorf("decode finding decision JSON: %w", err)
	}
	if err := decision.Validate(); err != nil {
		return Decision{}, fmt.Errorf("validate finding decision: %w", err)
	}
	return decision, nil
}

func decodeStrictJSON(data []byte, out any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	var trailing any
	err := decoder.Decode(&trailing)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return fmt.Errorf("multiple JSON values are not allowed")
	}
	return fmt.Errorf("invalid trailing JSON: %w", err)
}

func rejectDuplicateJSONFields(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := inspectJSONValue(decoder); err != nil {
		return err
	}
	if _, err := decoder.Token(); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return fmt.Errorf("unexpected trailing JSON token")
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
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
				return fmt.Errorf("read JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("JSON object key is not a string")
			}
			if _, duplicate := seen[key]; duplicate {
				return fmt.Errorf("duplicate JSON field %q", key)
			}
			seen[key] = struct{}{}
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}
