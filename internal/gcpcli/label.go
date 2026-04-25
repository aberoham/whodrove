package gcpcli

import (
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"teleport-ai/internal/labels"
	"teleport-ai/internal/store"
)

func newLabelCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "label", Short: "Manage Kubernetes-style session labels"}
	cmd.AddCommand(newLabelSetCmd())
	cmd.AddCommand(newLabelLsCmd())
	return cmd
}

func newLabelSetCmd() *cobra.Command {
	var sid, key, value, setBy string
	cmd := &cobra.Command{
		Use:   "set",
		Short: "Stamp a label on a session",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if sid == "" || key == "" {
				return errors.New("--session and --key are required")
			}
			dbPath, _ := cmd.Flags().GetString("db")
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()
			if setBy == "" {
				setBy = "manual:gcpcli"
			}
			return st.SetLabel(sid, key, value, setBy, time.Now().UTC().Format(time.RFC3339))
		},
	}
	cmd.Flags().StringVar(&sid, "session", "", "session id")
	cmd.Flags().StringVar(&key, "key", "", "label key (e.g. operator.type)")
	cmd.Flags().StringVar(&value, "value", "", "label value")
	cmd.Flags().StringVar(&setBy, "set-by", "", "who/what stamped the label (default 'manual:gcpcli')")
	return cmd
}

// newLabelLsCmd lists sessions matching a Kubernetes-style selector.
// Output columns are GCP-flavoured: principal, sample UA, bucket
// count, call count, denials, started_at. To list Teleport-side
// sessions, use teleport-analyze label ls.
func newLabelLsCmd() *cobra.Command {
	var selectorStr string
	cmd := &cobra.Command{
		Use:   "ls",
		Short: "List sessions matching a Kubernetes-style label selector (GCP-flavoured columns)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			sel, err := labels.ParseSelector(selectorStr)
			if err != nil {
				return err
			}
			dbPath, _ := cmd.Flags().GetString("db")
			st, err := store.Open(dbPath)
			if err != nil {
				return err
			}
			defer st.Close()

			rows, err := st.ListGCPSessionsBySelector(sel)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "SESSION_ID\tPRINCIPAL\tBUCKETS\tCALLS\tDENIES\tSTARTED")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%d\t%d\t%s\n",
					r.SessionID, r.User, r.GCPMinuteBuckets, r.GCPCallCount,
					r.GCPDeniedCalls, r.StartedAt)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().StringVar(&selectorStr, "selector", "", "k=v[,k=v...] selector (empty = all GCP sessions)")
	return cmd
}
