package snapshot

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/marmos91/dittofs/cmd/dfsctl/cmdutil"
	"github.com/marmos91/dittofs/internal/cli/output"
	"github.com/marmos91/dittofs/pkg/apiclient"
	"github.com/spf13/cobra"
)

var (
	rehydrateSource  string
	rehydrateDest    string
	rehydrateWait    bool
	rehydratePoll    time.Duration
	rehydrateTimeout time.Duration
)

var rehydrateCmd = &cobra.Command{
	Use:   "rehydrate <share> <id>",
	Short: "Create a new writable volume from a snapshot",
	Long: `Create a new writable volume whose content is a snapshot's.

No bytes are copied. Chunks are immutable and content-addressed, so the new
volume shares every chunk with the snapshot it came from — a reflink. The cost
is the metadata: one chunk reference per block, which is why this runs as a job
rather than a request and why a large volume takes a few seconds.

The snapshot is NOT modified and stays bootable. This is a copy out of it, never
a promotion of it, so a snapshot can be rehydrated any number of times — the
usual pattern is to keep one golden snapshot and rehydrate a fresh volume from it
per test run or per restore.

A snapshot is share-scoped: it holds every volume in the share, so the volume to
copy must be named with --source. --volume names the new one.

Failure is a rollback: the destination is removed and the job reports why. The
snapshot is untouched, so retrying is just retrying.

Examples:
  # Rehydrate the boot volume from the newest snapshot, and wait for it
  dfsctl share snapshot rehydrate /vm snap-abc123 --source boot --volume boot-2

  # Start the job and return immediately (poll with 'rehydrate status')
  dfsctl share snapshot rehydrate /vm snap-abc123 --source boot --volume boot-2 --wait=false

  # Report a job's state
  dfsctl share snapshot rehydrate status /vm job-3f2a1b4c`,
	Args: cobra.ExactArgs(2),
	RunE: runRehydrate,
}

var rehydrateStatusCmd = &cobra.Command{
	Use:   "status <share> <job-id>",
	Short: "Report a rehydrate job's state",
	Args:  cobra.ExactArgs(2),
	RunE:  runRehydrateStatus,
}

func init() {
	rehydrateCmd.Flags().StringVar(&rehydrateSource, "source", "", "Volume inside the snapshot to copy (required)")
	rehydrateCmd.Flags().StringVar(&rehydrateDest, "volume", "", "Name of the new volume (required)")
	rehydrateCmd.Flags().BoolVar(&rehydrateWait, "wait", true, "Wait for the job to finish")
	rehydrateCmd.Flags().DurationVar(&rehydratePoll, "poll", 500*time.Millisecond, "Poll interval while waiting")
	rehydrateCmd.Flags().DurationVar(&rehydrateTimeout, "timeout", 30*time.Minute, "How long to wait for the job")
	_ = rehydrateCmd.MarkFlagRequired("source")
	_ = rehydrateCmd.MarkFlagRequired("volume")
	rehydrateCmd.AddCommand(rehydrateStatusCmd)
}

func runRehydrate(cmd *cobra.Command, args []string) error {
	share, id := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}
	id, err = resolveSnapshotID(client, share, id)
	if err != nil {
		return err
	}

	job, err := client.RehydrateSnapshot(share, id, apiclient.RehydrateSnapshotRequest{
		SourceVolume: rehydrateSource,
		Volume:       rehydrateDest,
	})
	if err != nil {
		return fmt.Errorf("failed to start rehydrate: %w", err)
	}

	if !rehydrateWait {
		printJob(job)
		fmt.Printf("\nJob %s started. Check it with:\n  dfsctl share snapshot rehydrate status %s %s\n",
			job.ID, share, job.ID)
		return nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), rehydrateTimeout)
	defer cancel()
	done, err := client.WaitForRehydrateJob(ctx, share, job.ID, rehydratePoll)
	if err != nil {
		return fmt.Errorf("waiting for rehydrate job %s: %w (the job continues server-side; "+
			"check it with 'dfsctl share snapshot rehydrate status %s %s')", job.ID, err, share, job.ID)
	}
	printJob(done)
	if done.State == "failed" {
		// A failed job is a real failure of the command, but the message is the
		// server's, so return it rather than a generic one.
		return fmt.Errorf("rehydrate failed: %s", done.Error)
	}
	fmt.Printf("\nVolume %q created from snapshot %s.\n", done.Volume, shortID(done.SnapshotID))
	return nil
}

func runRehydrateStatus(cmd *cobra.Command, args []string) error {
	share, jobID := args[0], args[1]

	client, err := getClient()
	if err != nil {
		return err
	}
	job, err := client.GetRehydrateJob(share, jobID)
	if err != nil {
		return fmt.Errorf("failed to get rehydrate job: %w", err)
	}
	printJob(job)
	return nil
}

// printJob renders a job for a human or, when -o json/yaml was given, verbatim.
func printJob(job *apiclient.RehydrateJob) {
	format, err := cmdutil.GetOutputFormatParsed()
	if err == nil {
		switch format {
		case output.FormatJSON:
			_ = output.PrintJSON(os.Stdout, job)
			return
		case output.FormatYAML:
			_ = output.PrintYAML(os.Stdout, job)
			return
		}
	}
	pairs := [][2]string{
		{"JOB", job.ID},
		{"STATE", job.State},
		{"SHARE", job.Share},
		{"SNAPSHOT", shortID(job.SnapshotID)},
		{"SOURCE VOLUME", job.SourceVolume},
		{"NEW VOLUME", job.Volume},
		{"BLOCKS", fmt.Sprintf("%d", job.Blocks)},
		{"BYTES", fmt.Sprintf("%d", job.Bytes)},
		{"STARTED", job.StartedAt},
		{"FINISHED", cmdutil.EmptyOr(job.FinishedAt, "-")},
		{"ERROR", cmdutil.EmptyOr(job.Error, "-")},
	}
	_ = output.SimpleTable(os.Stdout, pairs)
}
