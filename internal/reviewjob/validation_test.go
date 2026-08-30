package reviewjob

import "testing"

func TestFormalRequestIsSourceOnlyAndProfileIsExplicit(t *testing.T) {
	valid := Request{
		SchemaVersion: RequestSchemaVersion, ExecutionProfile: FormalPiExecutionProfile,
		SourceRunID: "run-source", ExecutionTimeoutSeconds: 1800,
	}
	if err := valid.Validate(); err != nil {
		t.Fatalf("formal request rejected: %v", err)
	}
	mixed := valid
	mixed.RepositoryPath = "/tmp/repository"
	if err := mixed.Validate(); err == nil {
		t.Fatal("formal request accepted a second target authority")
	}
	missingProfile := valid
	missingProfile.ExecutionProfile = ""
	if err := missingProfile.Validate(); err == nil {
		t.Fatal("review request accepted an implicit execution profile")
	}
}
