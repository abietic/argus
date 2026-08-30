package githubadapter

import (
	"context"
	"fmt"
	"os"
)

const (
	TokenEnvironmentVariable              = "ARGUS_GITHUB_TOKEN"
	PrincipalEnvironmentVariable          = "ARGUS_GITHUB_PRINCIPAL_ID"
	CredentialRevisionEnvironmentVariable = "ARGUS_GITHUB_CREDENTIAL_REVISION"
	SingleUserLocalPermissionRevision     = "single-user-local-v1"
)

type EnvironmentCredentialSource struct {
	lookup func(string) (string, bool)
}

func NewEnvironmentCredentialSource() *EnvironmentCredentialSource {
	return &EnvironmentCredentialSource{lookup: os.LookupEnv}
}

func newEnvironmentCredentialSource(
	lookup func(string) (string, bool),
) (*EnvironmentCredentialSource, error) {
	if lookup == nil {
		return nil, fmt.Errorf("environment lookup is required")
	}
	return &EnvironmentCredentialSource{lookup: lookup}, nil
}

func (source *EnvironmentCredentialSource) Resolve(
	ctx context.Context,
	repositoryID string,
) (Credential, error) {
	if ctx == nil {
		return Credential{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return Credential{}, err
	}
	parts := splitRepository(repositoryID)
	if len(parts) != 2 || !validGitHubName(parts[0]) || !validGitHubName(parts[1]) {
		return Credential{}, fmt.Errorf("GitHub repository_id must be owner/name")
	}
	values := make(map[string]string, 3)
	for _, name := range []string{
		TokenEnvironmentVariable,
		PrincipalEnvironmentVariable,
		CredentialRevisionEnvironmentVariable,
	} {
		value, exists := source.lookup(name)
		if !exists || value == "" {
			return Credential{}, fmt.Errorf("required GitHub environment variable %s is unavailable", name)
		}
		values[name] = value
	}
	credential := Credential{
		Token:       values[TokenEnvironmentVariable],
		PrincipalID: values[PrincipalEnvironmentVariable],
		Revision:    values[CredentialRevisionEnvironmentVariable],
	}
	if err := credential.Validate(); err != nil {
		return Credential{}, err
	}
	return credential, nil
}

// SingleUserLocalPermissionBroker is deliberately not a production IAM
// implementation. It admits only an exact, visible local profile revision;
// the PublicationGrant remains the human authorization and GitHub remains the
// authority for whether the supplied credential can perform the write.
type SingleUserLocalPermissionBroker struct{}

func (SingleUserLocalPermissionBroker) Check(
	ctx context.Context,
	request PermissionRequest,
) (PermissionDecision, error) {
	if ctx == nil {
		return PermissionDecision{}, fmt.Errorf("context is required")
	}
	if err := ctx.Err(); err != nil {
		return PermissionDecision{}, err
	}
	if request.Provider != ProviderID ||
		validateDestination(request.RepositoryID, request.ChangeKind, request.ChangeID) != nil ||
		(request.Channel != "pull_request_inline" && request.Channel != "pull_request_summary") ||
		request.PrincipalID == "" || request.CredentialRevision == "" {
		return PermissionDecision{}, fmt.Errorf("single-user local permission request is invalid")
	}
	return PermissionDecision{
		Granted:  request.ExpectedPermissionRevision == SingleUserLocalPermissionRevision,
		Revision: SingleUserLocalPermissionRevision,
	}, nil
}

func splitRepository(value string) []string {
	for index, character := range value {
		if character == '/' {
			return []string{value[:index], value[index+1:]}
		}
	}
	return []string{value}
}

var _ CredentialSource = (*EnvironmentCredentialSource)(nil)
var _ PermissionBroker = SingleUserLocalPermissionBroker{}
