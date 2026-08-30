package githubadapter

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

func TestEnvironmentCredentialSourceUsesOnlyFixedNamesAndRedactsToken(t *testing.T) {
	values := map[string]string{
		TokenEnvironmentVariable:              "secret-token",
		PrincipalEnvironmentVariable:          "user-1",
		CredentialRevisionEnvironmentVariable: "credential-1",
		"UNRELATED_SECRET":                    "must-not-read",
	}
	read := make([]string, 0, 3)
	source, err := newEnvironmentCredentialSource(func(name string) (string, bool) {
		read = append(read, name)
		value, exists := values[name]
		return value, exists
	})
	if err != nil {
		t.Fatal(err)
	}
	credential, err := source.Resolve(context.Background(), "org/repo")
	if err != nil {
		t.Fatal(err)
	}
	if credential.Token != "secret-token" || len(read) != 3 {
		t.Fatalf("credential/read names = %s, %v", credential, read)
	}
	for _, formatted := range []string{
		credential.String(), credential.GoString(),
		fmt.Sprintf("%v", credential), fmt.Sprintf("%+v", credential), fmt.Sprintf("%#v", credential),
	} {
		if strings.Contains(formatted, credential.Token) {
			t.Fatalf("credential formatting leaked token: %q", formatted)
		}
	}
}

func TestEnvironmentCredentialSourceFailsClosedWithoutEveryIdentityField(t *testing.T) {
	for _, missing := range []string{
		TokenEnvironmentVariable,
		PrincipalEnvironmentVariable,
		CredentialRevisionEnvironmentVariable,
	} {
		source, err := newEnvironmentCredentialSource(func(name string) (string, bool) {
			if name == missing {
				return "", false
			}
			return "value-1", true
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := source.Resolve(context.Background(), "org/repo"); err == nil ||
			!strings.Contains(err.Error(), missing) {
			t.Fatalf("missing %s error = %v", missing, err)
		}
	}
}

func TestSingleUserLocalPermissionBrokerRequiresExactVisibleRevision(t *testing.T) {
	broker := SingleUserLocalPermissionBroker{}
	request := PermissionRequest{
		Provider: ProviderID, RepositoryID: "org/repo",
		ChangeKind: "pull_request", ChangeID: "42", Channel: "pull_request_inline",
		PrincipalID: "user-1", CredentialRevision: "credential-1",
		ExpectedPermissionRevision: SingleUserLocalPermissionRevision,
	}
	decision, err := broker.Check(context.Background(), request)
	if err != nil || !decision.Granted || decision.Revision != SingleUserLocalPermissionRevision {
		t.Fatalf("local permission = %+v, %v", decision, err)
	}
	request.ExpectedPermissionRevision = "caller-invented"
	decision, err = broker.Check(context.Background(), request)
	if err != nil || decision.Granted || decision.Revision != SingleUserLocalPermissionRevision {
		t.Fatalf("drifted local permission = %+v, %v", decision, err)
	}
}
