package hailixexecution

import (
	"context"
	"strings"
	"testing"
)

func TestEnvironmentCredentialSourceResolvesAtRequestTimeWithoutLeakingToken(t *testing.T) {
	t.Setenv(BearerTokenEnvironment, "first-secret-token")
	t.Setenv(CredentialRevisionEnvironment, "revision-1")
	source := NewEnvironmentCredentialSource()

	first, err := source.Resolve(context.Background(), Subject{})
	if err != nil {
		t.Fatal(err)
	}
	if first.BearerToken != "first-secret-token" || first.Revision != "revision-1" {
		t.Fatalf("first credential = %s", first)
	}

	t.Setenv(BearerTokenEnvironment, "rotated-secret-token")
	t.Setenv(CredentialRevisionEnvironment, "revision-2")
	second, err := source.Resolve(context.Background(), Subject{})
	if err != nil {
		t.Fatal(err)
	}
	if second.BearerToken != "rotated-secret-token" || second.Revision != "revision-2" {
		t.Fatalf("second credential = %s", second)
	}
	if strings.Contains(second.String(), second.BearerToken) ||
		strings.Contains(second.GoString(), second.BearerToken) {
		t.Fatalf("credential formatting leaked bearer token: %s", second)
	}
}

func TestEnvironmentCredentialSourceFailsClosedWhenEitherVariableIsMissing(t *testing.T) {
	t.Setenv(BearerTokenEnvironment, "")
	t.Setenv(CredentialRevisionEnvironment, "revision-1")
	_, err := NewEnvironmentCredentialSource().Resolve(context.Background(), Subject{})
	if err == nil || strings.Contains(err.Error(), "revision-1") {
		t.Fatalf("Resolve() error = %v", err)
	}
}
