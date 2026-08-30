package hailixexecution

import (
	"context"
	"fmt"
	"os"
)

const (
	BearerTokenEnvironment        = "ARGUS_HAILIX_BEARER_TOKEN"
	CredentialRevisionEnvironment = "ARGUS_HAILIX_CREDENTIAL_REVISION"
)

// EnvironmentCredentialSource resolves the Hailix credential at request time.
// The environment variable names are fixed so CLI configuration can never
// smuggle a bearer token into a persisted execution command or snapshot.
type EnvironmentCredentialSource struct {
	lookup func(string) (string, bool)
}

func NewEnvironmentCredentialSource() *EnvironmentCredentialSource {
	return &EnvironmentCredentialSource{lookup: os.LookupEnv}
}

func (source *EnvironmentCredentialSource) Resolve(
	ctx context.Context,
	_ Subject,
) (Credential, error) {
	if ctx == nil {
		return Credential{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	if source == nil || source.lookup == nil {
		return Credential{}, fmt.Errorf("Hailix environment credential source is not initialized")
	}
	token, tokenExists := source.lookup(BearerTokenEnvironment)
	revision, revisionExists := source.lookup(CredentialRevisionEnvironment)
	if !tokenExists || !revisionExists || token == "" || revision == "" {
		return Credential{}, fmt.Errorf(
			"Hailix credentials require %s and %s",
			BearerTokenEnvironment,
			CredentialRevisionEnvironment,
		)
	}
	credential := Credential{BearerToken: token, Revision: revision}
	if err := validateCredential(credential); err != nil {
		return Credential{}, err
	}
	return credential, nil
}
