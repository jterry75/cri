package lcow

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	"github.com/containerd/containerd/snapshots"
	"github.com/containerd/containerd/snapshots/storage"
	"github.com/pkg/errors"
)

func handleScratchLabels(
	ctx context.Context,
	s *snapshotter,
	snapshotInfo snapshots.Info,
	snDir string) error {

	containerType, ok := snapshotInfo.Labels[reuseScratchContainerTypeLabel]
	// Using the kubernetes titles here to discern between the two different
	// codepaths as that is the main use case for this. This could easily
	// be named something different (parent etc.), but as the snapshotters already
	// use the term parent for something else I did not want to overload the term
	// and cause confusion.
	//
	// If the container type is `sandbox` that means this is the container whose
	// scratch space future containers will be sharing. The flow for this is as follows.
	//
	// 1. Create or copy a scratch vhd into the snapshot as usual.
	// 2. Update the labels of this snapshot to include a `reuseScratchLabelSandboxFormat` entry
	// with the value being the current snapshot directory.
	// 3. Write this to disk.
	if ok && containerType == "sandbox" {
		id, ok := snapshotInfo.Labels[reuseScratchLabelSandboxIDLabel]
		if !ok {
			return errors.New("no sandbox ID present when asked to reuse scratch space")
		}
		if err := handleCreateScratch(ctx, s, snDir, snapshotInfo.Labels); err != nil {
			return err
		}
		// Save the sandboxes snapshot directory to a label and write this to disk.
		snapshotInfo.Labels[fmt.Sprintf(reuseScratchLabelSandboxFormat, id)] = snDir
		if _, err := storage.UpdateInfo(ctx, snapshotInfo); err != nil {
			return errors.Wrap(err, "failed to update snapshot")
		}
	} else if ok && containerType == "container" {
		// Grab the snapshot info saved on disk if the ID is present in the
		// labels.
		id, ok := snapshotInfo.Labels[reuseScratchLabelSandboxIDLabel]
		if !ok {
			return errors.New("no sandbox ID present when asked to reuse scratch space")
		}
		// Grab the snapshot from disk which will include the labels from the
		// snapshot with key `id`. This should be the container we're asking to shares
		// final snapshot (eg. where the containers sandbox.vhd is located).
		savedSnapshotInfo, err := s.Stat(context.Background(), id)
		if err != nil {
			return errors.Wrap(err, "failed to find snapshot to share")
		}

		// Grab the path of the sandbox snapshot saved to disk (if it exists).
		path, ok := savedSnapshotInfo.Labels[fmt.Sprintf(reuseScratchLabelSandboxFormat, id)]
		if !ok {
			return fmt.Errorf("reuse scratch specified but no sandbox snapshot can be found")
		}
		// We've found a path from a previous containers snapshot that we'd like to reuse, now stat
		// the dir to make sure it actually still exists and also if it contains a sandbox.vhd.
		if _, err := os.Stat(path); err != nil {
			return errors.Wrap(err, "failed to find snapshot directory")
		}

		sandboxPath := filepath.Join(path, "sandbox.vhd")
		linkPath := filepath.Join(snDir, "sandbox.vhd")
		if _, err := os.Stat(sandboxPath); err != nil {
			return errors.Wrap(err, "failed to find sandbox.vhd in snapshot directory")
		}
		// We've found everything we need, now just make a symlink in our new snapshot to the
		// sandbox.vhd in the scratch we're asking to share.
		if err := os.Symlink(sandboxPath, linkPath); err != nil {
			return errors.Wrap(err, "failed to create symlink for sandbox scratch space")
		}
	} else {
		return errors.New("reuse scratch label specified but unknown container type found")
	}
	return nil
}

func handleCreateScratch(ctx context.Context, s *snapshotter, snDir string, labels map[string]string) error {
	var sizeGB int
	if sizeGBstr, ok := labels[rootfsSizeLabel]; ok {
		i64, _ := strconv.ParseInt(sizeGBstr, 10, 32)
		sizeGB = int(i64)
	}

	var scratchLocation string
	scratchLocation, _ = labels[rootfsLocLabel]

	scratchSource, err := s.openOrCreateScratch(ctx, sizeGB, scratchLocation)
	if err != nil {
		return err
	}
	defer scratchSource.Close()

	// TODO: JTERRY75 - This has to be called sandbox.vhdx for the time
	// being but it really is the scratch.vhdx. Using this naming convention
	// for now but this is not the kubernetes sandbox.
	//
	// Create the sandbox.vhdx for this snapshot from the cache.
	destPath := filepath.Join(snDir, "sandbox.vhdx")
	dest, err := os.OpenFile(destPath, os.O_RDWR|os.O_CREATE, 0700)
	if err != nil {
		return errors.Wrap(err, "failed to create sandbox.vhdx in snapshot")
	}
	defer dest.Close()
	if _, err := io.Copy(dest, scratchSource); err != nil {
		dest.Close()
		os.Remove(destPath)
		return errors.Wrap(err, "failed to copy cached scratch.vhdx to sandbox.vhdx in snapshot")
	}
	return nil
}
