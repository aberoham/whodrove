// Package uafingerprint maps a callerSuppliedUserAgent string to a
// best-guess "tool" label. Used to stamp gcp.ua.tool on synthesised
// sessions during phase-1 classification.
//
// callerSuppliedUserAgent is client-controlled and trivially
// spoofable; treat the result as a strong but not authoritative
// signal. Spoofing is rare in practice.
//
// The returned tool string is the canonical label value:
// gcloud / terraform / kubectl / client-go / boto3 / pulumi /
// claude-code / claude-cli / codex-cli / cursor / unknown.
package uafingerprint

import "strings"

const (
	ToolGcloud     = "gcloud"
	ToolTerraform  = "terraform"
	ToolKubectl    = "kubectl"
	ToolClientGo   = "client-go"
	ToolBoto3      = "boto3"
	ToolPulumi     = "pulumi"
	ToolClaudeCode = "claude-code"
	ToolClaudeCLI  = "claude-cli"
	ToolCodexCLI   = "codex-cli"
	ToolCursor     = "cursor"
	ToolUnknown    = "unknown"
)

// Classify returns the canonical tool label for a UA string.
// Empty UA returns "unknown".
//
// Order of checks matters: the most specific patterns are checked
// first (so e.g. an agent UA that wraps gcloud is detected as the
// agent, not as gcloud).
func Classify(ua string) string {
	if ua == "" {
		return ToolUnknown
	}
	u := strings.ToLower(ua)

	// Coding agents (most specific first)
	switch {
	case strings.Contains(u, "claude-code"):
		return ToolClaudeCode
	case strings.Contains(u, "claudecli") || strings.Contains(u, "claude-cli"):
		return ToolClaudeCLI
	case strings.Contains(u, "codex-cli") || strings.Contains(u, "codex/"):
		return ToolCodexCLI
	case strings.Contains(u, "cursor/") || strings.Contains(u, "cursor-"):
		return ToolCursor
	}

	// IaC tools
	switch {
	case strings.HasPrefix(u, "terraform/") || strings.Contains(u, "terraform-provider-google"):
		return ToolTerraform
	case strings.Contains(u, "pulumi/") || strings.Contains(u, "pulumi-"):
		return ToolPulumi
	}

	// SDK / lib UAs (kubectl before client-go: the kubectl UA is
	// `kubectl/<ver>` and is typically used by humans; bare
	// `client-go/<ver>` is the SDK / agent pattern.)
	switch {
	case strings.HasPrefix(u, "kubectl/"):
		return ToolKubectl
	case strings.Contains(u, "client-go/"):
		return ToolClientGo
	case strings.HasPrefix(u, "boto3/") || strings.Contains(u, "botocore/"):
		return ToolBoto3
	}

	// gcloud and the Google Cloud SDK family. The canonical UA is
	// `google-cloud-sdk/<ver> command/gcloud.<subcommand>`; some calls
	// have the form `gcloud/<ver>`.
	switch {
	case strings.Contains(u, "google-cloud-sdk"),
		strings.HasPrefix(u, "gcloud/"),
		strings.Contains(u, " gcloud/"):
		return ToolGcloud
	}
	return ToolUnknown
}

// IsHumanInteractiveTool returns true for UAs that are typically
// driven by a human at a terminal (gcloud, kubectl). Used as an
// early phase-1 hint, not a final decision.
func IsHumanInteractiveTool(tool string) bool {
	switch tool {
	case ToolGcloud, ToolKubectl:
		return true
	}
	return false
}

// IsAgentTool returns true for UAs that indicate a coding agent.
func IsAgentTool(tool string) bool {
	switch tool {
	case ToolClaudeCode, ToolClaudeCLI, ToolCodexCLI, ToolCursor:
		return true
	}
	return false
}
