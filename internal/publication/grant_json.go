package publication

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

func DecodeGrantRequestJSON(data []byte) (GrantRequest, error) {
	var request GrantRequest
	if err := decodeStrictPublicationJSON(data, &request); err != nil {
		return GrantRequest{}, fmt.Errorf("decode publication grant request: %w", err)
	}
	if err := request.Validate(); err != nil {
		return GrantRequest{}, err
	}
	return request, nil
}

func DecodeGrantMutationJSON(data []byte) (GrantMutation, error) {
	var mutation GrantMutation
	if err := decodeStrictPublicationJSON(data, &mutation); err != nil {
		return GrantMutation{}, fmt.Errorf("decode publication grant mutation: %w", err)
	}
	if err := mutation.Validate(); err != nil {
		return GrantMutation{}, err
	}
	return mutation, nil
}

func DecodeGrantJSON(data []byte) (Grant, error) {
	var grant Grant
	if err := decodeStrictPublicationJSON(data, &grant); err != nil {
		return Grant{}, fmt.Errorf("decode publication grant: %w", err)
	}
	if err := grant.Validate(); err != nil {
		return Grant{}, err
	}
	return grant, nil
}

func DecodeGrantReservationJSON(data []byte) (GrantReservation, error) {
	var reservation GrantReservation
	if err := decodeStrictPublicationJSON(data, &reservation); err != nil {
		return GrantReservation{}, fmt.Errorf("decode publication grant reservation: %w", err)
	}
	if err := reservation.Validate(); err != nil {
		return GrantReservation{}, err
	}
	return reservation, nil
}

func decodeStrictPublicationJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONFields(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values are not allowed")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}
