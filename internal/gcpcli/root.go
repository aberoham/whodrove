// Package gcpcli wires the cobra command tree for shellscope-gcp.
// Sibling of internal/cli (the Teleport-side tree); both write into
// the same sessions.sqlite via internal/store.
package gcpcli

import "github.com/spf13/cobra"

const (
	defaultDB             = "sessions.sqlite"
	defaultIdleSeconds    = 600
	defaultAuditTable     = "cloudaudit_googleapis_com_activity"
	parserVersionGCP      = "shellscope-gcp/v0.1"
)

func Root() *cobra.Command {
	root := &cobra.Command{
		Use:   "shellscope-gcp",
		Short: "GCP-side ad-hoc privileged-user activity analyzer",
		Long: "Queries the FFF aggregated Cloud Audit Log BigQuery dataset for " +
			"per-(principal, minute) features, synthesises sessions, and writes " +
			"per-session features and Kubernetes-style labels into a local SQLite " +
			"file shared with the Teleport-side teleport-analyze.",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	root.PersistentFlags().String("db", defaultDB, "path to sessions.sqlite")

	root.AddCommand(newPullCmd())
	root.AddCommand(newLabelCmd())
	return root
}
