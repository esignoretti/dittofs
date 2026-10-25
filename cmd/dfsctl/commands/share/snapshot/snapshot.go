// Package snapshot implements share snapshot management commands.
package snapshot

import (
	"github.com/spf13/cobra"
)

// Cmd is the parent command for share snapshot management.
var Cmd = &cobra.Command{
	Use:   "snapshot",
	Short: "Manage share snapshots (create, list, show, usage, remove, restore, rehydrate)",
	Long: `Manage share snapshots.

A snapshot captures the full state of a share at a point in time. It can be
inspected, listed, removed, restored back onto a (disabled) share, or rehydrated
into a NEW writable volume that shares its chunks (no bytes copied).

Examples:
  # Create a snapshot and wait for it to be ready
  dfsctl share snapshot create /archive --name weekly

  # List snapshots for a share
  dfsctl share snapshot list /archive

  # Show details of a single snapshot
  dfsctl share snapshot show /archive snap-abc123

  # Show what the snapshots hold and what deleting one would free
  dfsctl share snapshot usage /archive

  # Remove a snapshot (prompts for confirmation)
  dfsctl share snapshot remove /archive snap-abc123

  # Create a new writable volume from a snapshot (no bytes copied)
  dfsctl share snapshot rehydrate /vm snap-abc123 --source boot --volume boot-2

  # Restore a snapshot onto a disabled share
  dfsctl share disable /archive
  dfsctl share snapshot restore /archive snap-abc123`,
}

func init() {
	Cmd.AddCommand(createCmd)
	Cmd.AddCommand(listCmd)
	Cmd.AddCommand(showCmd)
	Cmd.AddCommand(usageCmd)
	Cmd.AddCommand(removeCmd)
	Cmd.AddCommand(restoreCmd)
	Cmd.AddCommand(rehydrateCmd)
}
