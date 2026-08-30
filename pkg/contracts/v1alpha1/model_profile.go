package v1alpha1

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	ModelProfileSchemaVersion = AgentStagePlanModelContract
	ModelProfileMaxBytes      = 4 << 10
	modelWireIDMaxBytes       = 512
)

// ModelProfile separates Argus' safe, stable component identity from the
// provider-owned wire model identifier. WireModel is opaque provider input: it
// must be preserved exactly and must never be normalized into a different
// model name merely to satisfy an internal path/identifier grammar.
type ModelProfile struct {
	SchemaVersion string `json:"schema_version"`
	ProviderID    string `json:"provider_id"`
	WireModel     string `json:"wire_model"`
}

func NewModelProfile(providerID, wireModel string) (ModelProfile, error) {
	profile := ModelProfile{
		SchemaVersion: ModelProfileSchemaVersion,
		ProviderID:    providerID,
		WireModel:     wireModel,
	}
	if err := profile.Validate(); err != nil {
		return ModelProfile{}, err
	}
	return profile, nil
}

func DecodeModelProfile(data []byte) (ModelProfile, error) {
	if len(data) == 0 || len(data) > ModelProfileMaxBytes {
		return ModelProfile{}, fmt.Errorf(
			"model profile must be between 1 and %d bytes",
			ModelProfileMaxBytes,
		)
	}
	var profile ModelProfile
	if err := decodeAgentReviewShadowJSON(data, &profile); err != nil {
		return ModelProfile{}, fmt.Errorf("decode ModelProfile: %w", err)
	}
	if err := profile.Validate(); err != nil {
		return ModelProfile{}, err
	}
	return profile, nil
}

func MarshalModelProfile(profile ModelProfile) ([]byte, error) {
	if err := profile.Validate(); err != nil {
		return nil, err
	}
	data, err := json.Marshal(profile)
	if err != nil {
		return nil, fmt.Errorf("marshal ModelProfile: %w", err)
	}
	if len(data) > ModelProfileMaxBytes {
		return nil, fmt.Errorf("model profile exceeds %d bytes", ModelProfileMaxBytes)
	}
	return data, nil
}

func (profile ModelProfile) Validate() error {
	if profile.SchemaVersion != ModelProfileSchemaVersion {
		return fmt.Errorf("unsupported ModelProfile schema %q", profile.SchemaVersion)
	}
	if err := requireIdentifier("provider_id", profile.ProviderID); err != nil {
		return err
	}
	if profile.WireModel == "" || profile.WireModel != strings.TrimSpace(profile.WireModel) ||
		!utf8.ValidString(profile.WireModel) || len([]byte(profile.WireModel)) > modelWireIDMaxBytes {
		return fmt.Errorf(
			"wire_model must be non-empty valid UTF-8 without outer whitespace and bounded to %d bytes",
			modelWireIDMaxBytes,
		)
	}
	for _, value := range profile.WireModel {
		if value == 0 || value < 0x20 || value == 0x7f {
			return fmt.Errorf("wire_model must not contain control characters")
		}
	}
	return nil
}
