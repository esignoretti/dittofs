package snapshot

import (
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var usageID string

var usageCmd = &cobra.Command{
	Use:   "usage <share>",
	Short: "Show what snapshots hold and what deleting one would free",
	Long: `Show how much of a share's snapshot set each snapshot actually pins.

Snapshots share chunks with each other — an hourly snapshot of a slowly-changing
volume re-references almost everything the previous one held — so neither the
snapshot list nor the size of the snapshot directories answers "how much space do
my snapshots use, and what would deleting this one free".

This reports both:

  CHUNKS  distinct chunks this snapshot references
  UNIQUE  chunks no OTHER snapshot references: what deleting it would free
  SHARED  the rest, still held by something else

The footer gives the union across all snapshots and the sum of the per-snapshot
counts. The gap between them is the sharing, which is why retaining many
snapshots costs far less than many volumes.

Counts, not bytes: a manifest line is a content hash with no size, so converting
to bytes would need one remote request per distinct chunk. For bytes, read
bytes_freed from 'dfsctl store block gc-status' after a real delete.

A snapshot held open by a running export or a rehydrate reports OPEN yes; a
delete would be refused with 409 until that holder releases it.

Examples:
  # Every snapshot on the share
  dfsctl share snapshot usage /archive

  # One snapshot, still measured against the others
  dfsctl share snapshot usage /archive --id snap-abc123

  # JSON, for scripting
  dfsctl share snapshot usage /archive -o json`,
	Args: cobra.ExactArgs(1),
	RunE: runUsage,
}

func init() {
	usageCmd.Flags().StringVar(&usageID, "id", "", "Report one snapshot instead of the whole share")
}

func runUsage(cmd *cobra.Command, args []string) error {
	share := args[0]

	client, err := getClient()
	if err != nil {
		return err
	}

	// Resolve a partial id the same way show/remove do, so a short id works here
	// too. Only when one was given: the whole-share report needs no resolution.
	id := usageID
	if id != "" {
		id, err = resolveSnapshotID(client, share, id)
		if err != nil {
			return err
		}
	}

	report, err := client.SnapshotUsage(share, id)
	if err != nil {
		return fmt.Errorf("failed to get snapshot usage: %w", err)
	}

	format, err := cmdutil.GetOutputFormatParsed()
	if err != nil {
		return err
	}

	switch format {
	case output.FormatJSON:
		return output.PrintJSON(os.Stdout, report)
	case output.FormatYAML:
		return output.PrintYAML(os.Stdout, report)
	default:
		return printUsageTable(share, report)
	}
}

// printUsageTable renders the report for a human.
//
// Written with tabwriter rather than the shared SimpleTable because this is a
// multi-row table with a footer, and the footer carries the interpretation: the
// union and the sum are the two numbers the operator came for, and the gap
// between them is the answer.
func printUsageTable(share string, report *apiclient.SnapshotUsageReport) error {
	if len(report.Snapshots) == 0 {
		if len(report.Skipped) > 0 {
			fmt.Printf("No readable snapshots on %s. Skipped (no manifest): %v\n", share, report.Skipped)
			return nil
		}
		fmt.Printf("No snapshots on %s.\n", share)
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "ID\tCREATED\tCHUNKS\tUNIQUE\tSHARED\tOPEN")
	for _, u := range report.Snapshots {
		open := "-"
		if u.Open {
			open = fmt.Sprintf("yes (%d)", u.Holders)
		}
		// A short ID, because the full UUID pushes the columns off a terminal.
		// The full one is in -o json and in 'snapshot list'.
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%s\n",
			shortID(u.ID), u.CreatedAt, u.Chunks, u.Unique, u.Shared, open)
	}
	if err := w.Flush(); err != nil {
		return err
	}

	fmt.Printf("\n%d snapshot(s) hold %d distinct chunk(s) between them (%d counting each separately).\n",
		len(report.Snapshots), report.TotalChunks, report.SumOfSnapshotChunks)
	if report.SumOfSnapshotChunks > report.TotalChunks && report.TotalChunks > 0 {
		shared := report.SumOfSnapshotChunks - report.TotalChunks
		fmt.Printf("Sharing saves %d chunk reference(s): retaining these snapshots costs %.1f%% of the sum.\n",
			shared, 100*float64(report.TotalChunks)/float64(report.SumOfSnapshotChunks))
	}
	fmt.Printf("%d chunk(s) are unique to a single snapshot, so deleting that one frees them.\n",
		report.UniqueToSomeSnapshot)
	if len(report.Skipped) > 0 {
		fmt.Printf("\nSkipped %d snapshot(s) with no readable manifest, so their cost is unknown: %v\n",
			len(report.Skipped), report.Skipped)
	}
	fmt.Println("\nCounts, not bytes: a manifest records hashes, not sizes. For bytes, read")
	fmt.Println("bytes_freed from 'dfsctl store block gc-status' after a real delete.")
	return nil
}

// shortID trims a UUID to the 8-character prefix the list command prints, so
// the two commands agree and an operator can paste one into the other.
func shortID(id string) string {
	if len(id) <= 8 {
		return id
	}
	return id[:8]
}
