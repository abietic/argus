package v1alpha1

import (
	"strings"
	"testing"
)

func TestModelProfilePreservesOpaqueProviderWireModel(t *testing.T) {
	profile, err := NewModelProfile("deepseek-anthropic", "deepseek-v4-pro[1m]")
	if err != nil {
		t.Fatal(err)
	}
	data, err := MarshalModelProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeModelProfile(data)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != profile || decoded.WireModel != "deepseek-v4-pro[1m]" {
		t.Fatalf("model profile changed provider wire identity: %+v", decoded)
	}
}

func TestModelProfileRejectsUnsafeOrAmbiguousWireModel(t *testing.T) {
	for _, model := range []string{"", " deepseek-chat", "deepseek-chat\n", "deepseek\x00chat", strings.Repeat("模", 172)} {
		t.Run(model, func(t *testing.T) {
			if _, err := NewModelProfile("deepseek-anthropic", model); err == nil {
				t.Fatalf("NewModelProfile() admitted %q", model)
			}
		})
	}
}

func TestDecodeModelProfileIsStrict(t *testing.T) {
	for _, data := range []string{
		`{"schema_version":"argus.model_profile.v1alpha1","provider_id":"deepseek-anthropic","wire_model":"deepseek-chat","extra":true}`,
		`{"schema_version":"argus.model_profile.v1alpha1","provider_id":"deepseek-anthropic","wire_model":null}`,
		`{"schema_version":"argus.model_profile.v1alpha1","provider_id":"deepseek-anthropic","wire_model":"deepseek-chat"} trailing`,
	} {
		if _, err := DecodeModelProfile([]byte(data)); err == nil {
			t.Fatalf("DecodeModelProfile() admitted %s", data)
		}
	}
}
