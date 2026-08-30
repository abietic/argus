package local

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

var errJSONLLineTooLong = errors.New("JSONL line exceeds limit")

func readEnvelopes(reader io.Reader) ([]Envelope, error) {
	buffered := bufio.NewReaderSize(reader, 64<<10)
	envelopes := make([]Envelope, 0)
	seenIDs := make(map[string]struct{})
	var expected uint64 = 1
	for lineNumber := 1; ; lineNumber++ {
		line, err := readBoundedJSONLLine(buffered, maxEventBytes)
		if errors.Is(err, errJSONLLineTooLong) {
			return nil, fmt.Errorf(
				"%w: stream line %d exceeds %d bytes",
				ErrCorrupt,
				lineNumber,
				maxEventBytes,
			)
		}
		if errors.Is(err, io.EOF) {
			if len(line) != 0 {
				return nil, fmt.Errorf("%w: stream line %d has no terminating newline", ErrCorrupt, lineNumber)
			}
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: read stream line %d: %v", ErrCorrupt, lineNumber, err)
		}
		line = line[:len(line)-1]
		if len(line) == 0 {
			return nil, fmt.Errorf("%w: stream line %d is empty", ErrCorrupt, lineNumber)
		}
		var envelope Envelope
		if err := decodeStrictJSON(line, &envelope); err != nil {
			return nil, fmt.Errorf("%w: decode stream line %d: %v", ErrCorrupt, lineNumber, err)
		}
		if err := validateEnvelope(envelope, expected); err != nil {
			return nil, fmt.Errorf("%w: stream line %d: %v", ErrCorrupt, lineNumber, err)
		}
		if _, duplicate := seenIDs[envelope.ID]; duplicate {
			return nil, fmt.Errorf("%w: duplicate event id %q", ErrCorrupt, envelope.ID)
		}
		seenIDs[envelope.ID] = struct{}{}
		envelopes = append(envelopes, envelope)
		expected++
	}
	return envelopes, nil
}

func readBoundedJSONLLine(
	reader *bufio.Reader,
	limit int,
) ([]byte, error) {
	line := make([]byte, 0, min(limit, 64<<10))
	for {
		fragment, err := reader.ReadSlice('\n')
		if len(line) > limit-len(fragment) {
			return nil, errJSONLLineTooLong
		}
		line = append(line, fragment...)
		switch {
		case err == nil:
			return line, nil
		case errors.Is(err, bufio.ErrBufferFull):
			continue
		default:
			return line, err
		}
	}
}

func validateEnvelope(envelope Envelope, expectedSequence uint64) error {
	if err := validateLabel("event id", envelope.ID); err != nil {
		return err
	}
	if err := validateLabel("event schema", envelope.Schema); err != nil {
		return err
	}
	if envelope.Sequence != expectedSequence {
		return fmt.Errorf("sequence is %d, want %d", envelope.Sequence, expectedSequence)
	}
	if envelope.Time.IsZero() || envelope.Time.Location() != time.UTC {
		return fmt.Errorf("event time must be a non-zero UTC timestamp")
	}
	if len(envelope.Payload) == 0 || !json.Valid(envelope.Payload) {
		return fmt.Errorf("event payload is invalid JSON")
	}
	return nil
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
	if err := rejectTrailingJSON(decoder); err != nil {
		return err
	}
	return nil
}

func rejectTrailingJSON(decoder *json.Decoder) error {
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
	token, err := decoder.Token()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return fmt.Errorf("unexpected trailing JSON token %v", token)
}

func inspectJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
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
		if err != nil {
			return fmt.Errorf("close JSON object: %w", err)
		}
		if end != json.Delim('}') {
			return fmt.Errorf("JSON object has invalid closing token")
		}
	case '[':
		for decoder.More() {
			if err := inspectJSONValue(decoder); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil {
			return fmt.Errorf("close JSON array: %w", err)
		}
		if end != json.Delim(']') {
			return fmt.Errorf("JSON array has invalid closing token")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
	return nil
}

func compactJSON(data []byte) []byte {
	var compact bytes.Buffer
	if err := json.Compact(&compact, data); err != nil {
		return data
	}
	return compact.Bytes()
}
