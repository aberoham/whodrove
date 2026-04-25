package uafingerprint

import "testing"

func TestClassify(t *testing.T) {
	cases := []struct {
		ua   string
		want string
	}{
		{"", ToolUnknown},
		{"google-cloud-sdk/482.0.0 command/gcloud.compute.ssh", ToolGcloud},
		{"google-cloud-sdk/482.0.0 command/gcloud.compute.ssh.tunnel-through-iap", ToolGcloud},
		{"gcloud/482.0.0 (linux/amd64)", ToolGcloud},
		{"Terraform/1.6.0 (+https://www.terraform.io) terraform-provider-google/4.50.0", ToolTerraform},
		{"terraform/1.0.0", ToolTerraform},
		{"pulumi/3.x", ToolPulumi},
		{"pulumi-resource-google-native/0.32.0", ToolPulumi},
		{"kubectl/v1.28.0 (linux/amd64) kubernetes/3.0", ToolKubectl},
		{"client-go/v0.28.0", ToolClientGo},
		{"Boto3/1.34.10 Python/3.11", ToolBoto3},
		{"botocore/1.34.10", ToolBoto3},
		{"claude-code/1.0.0", ToolClaudeCode},
		{"Mozilla/5.0 claude-code/v0.5", ToolClaudeCode},
		{"codex-cli/1.0", ToolCodexCLI},
		{"codex/0.5", ToolCodexCLI},
		{"cursor/0.40 (darwin)", ToolCursor},
		{"cursor-agent/0.1", ToolCursor},
		{"random unknown user agent", ToolUnknown},
		{"PostmanRuntime/7.32.3", ToolUnknown},
	}
	for _, tc := range cases {
		if got := Classify(tc.ua); got != tc.want {
			t.Errorf("Classify(%q) = %q, want %q", tc.ua, got, tc.want)
		}
	}
}

func TestClassify_AgentBeatsUnderlyingSDK(t *testing.T) {
	// Some agents wrap gcloud and pass through the gcloud UA but
	// prepend their own marker. The agent should win.
	got := Classify("claude-code/v0.5 google-cloud-sdk/482.0.0 command/gcloud.compute.ssh")
	if got != ToolClaudeCode {
		t.Errorf("agent should win over wrapped gcloud; got %q", got)
	}
}

func TestIsHumanInteractiveTool(t *testing.T) {
	if !IsHumanInteractiveTool(ToolGcloud) {
		t.Errorf("gcloud should be treated as interactive")
	}
	if IsHumanInteractiveTool(ToolTerraform) {
		t.Errorf("terraform is not interactive")
	}
}

func TestIsAgentTool(t *testing.T) {
	if !IsAgentTool(ToolClaudeCode) {
		t.Errorf("claude-code should be agent")
	}
	if IsAgentTool(ToolGcloud) {
		t.Errorf("gcloud is not an agent tool")
	}
}
