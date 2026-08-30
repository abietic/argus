package training

import (
	"bytes"
	"strings"
	"testing"
)

func TestStrictRedactionLeakageCorpus(t *testing.T) {
	anthropicToken := strings.Join([]string{"sk", "ant", "api03", "abcdefghijklmnopqrstuvwxyz"}, "-")
	githubToken := "gh" + "p_abcdefghijklmnopqrstuvwxyz123456"
	slackToken := "xo" + "xb-1234567890-abcdefghijklmnop"
	awsAccessKey := "AK" + "IAABCDEFGHIJKLMNOP"
	jwt := "eyJhbGciOiJIUzI1NiJ9." + "eyJzdWIiOiIxMjM0NTY3ODkwIn0.abcdefghijklmnopqrstuvwxyz"
	privateKeyHeader := "-----BE" + "GIN PRIVATE " + "KEY-----"
	privateKeyFooter := "-----EN" + "D PRIVATE " + "KEY-----"
	secrets := []string{
		anthropicToken,
		githubToken,
		slackToken,
		awsAccessKey,
		jwt,
		"hunter2-password-value",
		"basic-user-password",
		"private-key-body-value",
		"developer@example.invalid",
		"192.0.2.10",
		"example-user",
	}
	input := strings.Join([]string{
		"ANTHROPIC_API_KEY=" + anthropicToken,
		"github=" + githubToken,
		"slack=" + slackToken,
		"aws=" + awsAccessKey,
		"Authorization: Bearer " + jwt,
		"password: hunter2-password-value",
		"https://basic-user:basic-user-password@example.invalid/path",
		privateKeyHeader + "\nprivate-key-body-value\n" + privateKeyFooter,
		"owner=developer@example.invalid host=192.0.2.10 path=/Users/example-user/project/main.go",
	}, "\n")
	result, err := RedactStrictText([]byte(input), DefaultStrictRedactionPolicy())
	if err != nil {
		t.Fatalf("RedactStrictText() error = %v", err)
	}
	if len(result.Hits) < 8 {
		t.Fatalf("hits = %+v", result.Hits)
	}
	for _, secret := range secrets {
		if bytes.Contains(result.Content, []byte(secret)) {
			t.Fatalf("redacted content retained %q: %s", secret, result.Content)
		}
	}
	second, err := RedactStrictText(result.Content, DefaultStrictRedactionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(second.Content, result.Content) || len(second.Hits) != 0 {
		t.Fatalf("redaction is not idempotent: hits=%+v", second.Hits)
	}
}

func TestStrictRedactionPreservesOrdinaryCodeAndRejectsUnsafeInputs(t *testing.T) {
	input := []byte("package review\n\nfunc add(left, right int) int { return left + right }\n")
	result, err := RedactStrictText(input, DefaultStrictRedactionPolicy())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(result.Content, input) || len(result.Hits) != 0 {
		t.Fatalf("ordinary code changed: %q hits=%+v", result.Content, result.Hits)
	}
	if _, err := RedactStrictText([]byte{0xff}, DefaultStrictRedactionPolicy()); err == nil {
		t.Fatal("invalid UTF-8 was accepted")
	}
	if _, err := RedactStrictText([]byte{'a', 0, 'b'}, DefaultStrictRedactionPolicy()); err == nil {
		t.Fatal("NUL text was accepted")
	}
	oversized := bytes.Repeat([]byte{'a'}, int(maximumRedactionArtifactBytes)+1)
	if _, err := RedactStrictText(oversized, DefaultStrictRedactionPolicy()); err == nil {
		t.Fatal("oversized text was accepted")
	}
	tampered := DefaultStrictRedactionPolicy()
	tampered.RuleIDs = tampered.RuleIDs[:len(tampered.RuleIDs)-1]
	if _, err := RedactStrictText(input, tampered); err == nil {
		t.Fatal("tampered policy was accepted")
	}
}
