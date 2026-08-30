package agentplan

import (
	"strings"
	"testing"

	"github.com/abietic/argus/internal/runmodel"
)

func TestGovernedArtifactProjectionValidatesExactDualReference(t *testing.T) {
	local := contentAddressedRef(
		runmodel.ContractReviewInput,
		digestLabel("review-input-projection"),
		128,
	)
	valid := governedProjection(local)
	if err := valid.Validate(); err != nil {
		t.Fatalf("GovernedArtifactProjection.Validate() error = %v", err)
	}

	tests := []struct {
		name   string
		mutate func(*GovernedArtifactProjection)
		want   string
	}{
		{
			name: "missing local",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Local = runmodel.ArtifactRef{}
			},
			want: "local ref",
		},
		{
			name: "non content addressed local",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Local.URI = "artifact://local/sha256/" + digestLabel("other")
			},
			want: "does not match digest",
		},
		{
			name: "local uri masquerades as governed",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Governed.Ref.URI = projection.Local.URI
			},
			want: "tenants/<tenant>/workspaces/<workspace>/objects/<object>",
		},
		{
			name: "governed digest mismatch",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Governed.Ref.SHA256 = digestLabel("other-governed-content")
			},
			want: "exact same digest, size, and contract",
		},
		{
			name: "governed size mismatch",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Governed.Ref.SizeBytes++
			},
			want: "exact same digest, size, and contract",
		},
		{
			name: "governed contract mismatch",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Governed.Contract = runmodel.ContractReviewSpec
			},
			want: "exact same digest, size, and contract",
		},
		{
			name: "non canonical uppercase authority",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Governed.Ref.URI = strings.Replace(
					projection.Governed.Ref.URI,
					"artifact://argus-local/",
					"artifact://ARGUS-LOCAL/",
					1,
				)
			},
			want: "invalid authority",
		},
		{
			name: "query-bearing governed URI",
			mutate: func(projection *GovernedArtifactProjection) {
				projection.Governed.Ref.URI += "?credential=forbidden"
			},
			want: "canonical governed artifact URI",
		},
		{
			name: "non digest governed object",
			mutate: func(projection *GovernedArtifactProjection) {
				parts := strings.Split(projection.Governed.Ref.URI, "/")
				parts[len(parts)-1] = "mutable-latest"
				projection.Governed.Ref.URI = strings.Join(parts, "/")
			},
			want: "invalid authority, tenant, workspace, or object identity",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			projection := valid
			test.mutate(&projection)
			if err := projection.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompileFailsClosedOnMissingCrossedOrLocalGovernedProjection(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*CompileInput)
		want   string
	}{
		{
			name: "missing execution snapshot projection",
			mutate: func(input *CompileInput) {
				input.ExecutionSnapshotProjection = GovernedArtifactProjection{}
			},
			want: "execution snapshot governed projection local ref",
		},
		{
			name: "missing config bundle projection",
			mutate: func(input *CompileInput) {
				input.ConfigBundleProjection = GovernedArtifactProjection{}
			},
			want: "config bundle governed projection local ref",
		},
		{
			name: "missing config receipt projection",
			mutate: func(input *CompileInput) {
				input.ConfigResolutionReceiptProjection = GovernedArtifactProjection{}
			},
			want: "config resolution receipt governed projection local ref",
		},
		{
			name: "missing workflow projection",
			mutate: func(input *CompileInput) {
				input.WorkflowProjection = GovernedArtifactProjection{}
			},
			want: "workflow governed projection local ref",
		},
		{
			name: "missing review spec projection",
			mutate: func(input *CompileInput) {
				input.ReviewSpecProjection = GovernedArtifactProjection{}
			},
			want: "review spec governed projection local ref",
		},
		{
			name: "missing review input projection",
			mutate: func(input *CompileInput) {
				input.ReviewInputProjection = GovernedArtifactProjection{}
			},
			want: "review input governed projection local ref",
		},
		{
			name: "crossed artifact projection",
			mutate: func(input *CompileInput) {
				input.WorkflowProjection = input.ReviewSpecProjection
			},
			want: "workflow governed projection does not bind its exact frozen local artifact",
		},
		{
			name: "local URI masquerades as governed binding",
			mutate: func(input *CompileInput) {
				input.ReviewInputProjection.Governed.Ref.URI =
					input.ReviewInputProjection.Local.URI
			},
			want: "review input governed projection governed binding URI must use",
		},
		{
			name: "cross workspace projection",
			mutate: func(input *CompileInput) {
				input.WorkflowProjection.Governed.Ref.URI = strings.Replace(
					input.WorkflowProjection.Governed.Ref.URI,
					"/workspaces/workspace-1/",
					"/workspaces/workspace-2/",
					1,
				)
			},
			want: "cross authority, tenant, or workspace namespaces",
		},
		{
			name: "dual ref digest mismatch",
			mutate: func(input *CompileInput) {
				input.ReviewSpecProjection.Governed.Ref.SHA256 = digestLabel("forged")
			},
			want: "local and governed refs must bind the exact same digest",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := newCompileFixture(t, fixtureOptions{})
			test.mutate(&input)
			if _, err := Compile(input); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Compile() error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCompileGovernedPublicationURIChangesFullButNotBehaviorIdentity(t *testing.T) {
	input := newCompileFixture(t, fixtureOptions{})
	base, err := Compile(input)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(input.WorkflowProjection.Governed.Ref.URI, "/")
	parts[len(parts)-1] = digestLabel("republished-workflow-alias")
	input.WorkflowProjection.Governed.Ref.URI = strings.Join(parts, "/")
	republished, err := Compile(input)
	if err != nil {
		t.Fatalf("Compile(republished governed alias) error = %v", err)
	}
	if republished.BehaviorSHA256 != base.BehaviorSHA256 {
		t.Fatal("governed publication URI changed behavior identity")
	}
	if republished.SHA256 == base.SHA256 || republished.PlanID == base.PlanID {
		t.Fatal("governed publication URI did not change full plan identity")
	}
}
