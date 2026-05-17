package linear

import "testing"

func TestNormalizeIssueMetaAppliesFallbackOwner(t *testing.T) {
	meta, err := NormalizeIssueMeta(&IssueMeta{GithubRepo: "api", DefaultBranch: "develop"}, "acme")
	if err != nil {
		t.Fatalf("NormalizeIssueMeta returned error: %v", err)
	}
	if meta.GithubRepo != "acme/api" {
		t.Fatalf("GithubRepo = %q, want %q", meta.GithubRepo, "acme/api")
	}
	if meta.DefaultBranch != "develop" {
		t.Fatalf("DefaultBranch = %q, want %q", meta.DefaultBranch, "develop")
	}
}

func TestParseIssueMetaRelaxedWithoutMetadata(t *testing.T) {
	meta, err := ParseIssueMetaRelaxed("Investigate bug in backend-api checkout flow")
	if err != nil {
		t.Fatalf("ParseIssueMetaRelaxed returned error: %v", err)
	}
	if meta.GithubRepo != "" {
		t.Fatalf("GithubRepo = %q, want empty", meta.GithubRepo)
	}
	if meta.DefaultBranch != "main" {
		t.Fatalf("DefaultBranch = %q, want %q", meta.DefaultBranch, "main")
	}
}

func TestParseIssueMetaRequiresRepo(t *testing.T) {
	if _, err := ParseIssueMeta("Just some prose without metadata"); err == nil {
		t.Fatal("ParseIssueMeta returned nil error, want error")
	}
}

func TestParseIssueMetaRelaxedReturnsMalformedFrontmatterError(t *testing.T) {
	_, err := ParseIssueMetaRelaxed("---\ngithub_repo: [broken\n---\nBody")
	if err == nil {
		t.Fatal("ParseIssueMetaRelaxed returned nil error, want YAML parse error")
	}
}

func TestParseIssueMetaRelaxedReturnsMalformedJSONError(t *testing.T) {
	_, err := ParseIssueMetaRelaxed("Please use {broken json} for this issue")
	if err == nil {
		t.Fatal("ParseIssueMetaRelaxed returned nil error, want JSON parse error")
	}
}
