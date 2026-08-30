package calibration

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

func DecodeFitRequest(data []byte) (FitRequest, error) {
	var request FitRequest
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		return FitRequest{}, fmt.Errorf("decode CalibrationFitRequest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return FitRequest{}, fmt.Errorf("decode CalibrationFitRequest: multiple JSON values are not allowed")
		}
		return FitRequest{}, fmt.Errorf("decode CalibrationFitRequest trailing JSON: %w", err)
	}
	if err := request.Validate(); err != nil {
		return FitRequest{}, fmt.Errorf("validate CalibrationFitRequest: %w", err)
	}
	return request, nil
}
