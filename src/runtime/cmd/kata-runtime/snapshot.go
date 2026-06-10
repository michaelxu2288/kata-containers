// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"os"
	"path/filepath"

	containerdshim "github.com/kata-containers/kata-containers/src/runtime/pkg/containerd-shim-v2"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/utils/shimclient"
	"github.com/urfave/cli"
)

var snapshotSubCmds = []cli.Command{
	deleteSnapshotCommand,
}

var snapshotCLICommand = cli.Command{
	Name:  "snapshot",
	Usage: "snapshot a running Kata Containers sandbox VM",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:        "sandbox-id",
			Usage:       "the target sandbox to snapshot",
			Destination: &sandboxID,
		},
		cli.StringFlag{
			Name:  "name",
			Usage: "name the snapshot directory (default: the sandbox id)",
		},
	},
	// delete is a subcommand; with no matching subcommand this Action runs.
	Subcommands: snapshotSubCmds,
	Action: func(c *cli.Context) error {
		// sandbox-id is required for the take-snapshot action. it is NOT marked
		// Required on the flag because that check would also fire on the
		// `snapshot delete` subcommand path (cli v1 validates parent flags
		// before dispatching), so we enforce it here instead.
		if sandboxID == "" {
			return fmt.Errorf("--sandbox-id is required")
		}
		if err := katautils.VerifyContainerID(sandboxID); err != nil {
			return err
		}

		// the snapshot always lives under SnapshotBaseDir. --name only changes
		// the leaf directory name; empty lets the shim default to <base>/<sbid>.
		var destDir string
		if name := c.String("name"); name != "" {
			destDir = filepath.Join(containerdshim.SnapshotBaseDir, name)
		}

		// the shim does the pause/save/snapshot/resume work; we send the
		// destination dir as the request body and print the path it used.
		if err := shimclient.DoPut(sandboxID, defaultTimeout, containerdshim.SnapshotUrl,
			"application/octet-stream", []byte(destDir)); err != nil {
			return fmt.Errorf("Error observed when making snapshot request: %s", err)
		}

		out := destDir
		if out == "" {
			out = filepath.Join(containerdshim.SnapshotBaseDir, sandboxID)
		}
		fmt.Fprintln(defaultOutputFile, out)

		return nil
	},
}

var deleteSnapshotCommand = cli.Command{
	Name:  "delete",
	Usage: "delete a snapshot directory by sandbox id or name",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:        "sandbox-id",
			Usage:       "delete the snapshot taken for this sandbox id",
			Destination: &sandboxID,
		},
		cli.StringFlag{
			Name:  "name",
			Usage: "delete the snapshot with this name",
		},
	},
	Action: func(c *cli.Context) error {
		name := c.String("name")

		// exactly one selector is required.
		if (sandboxID == "") == (name == "") {
			return fmt.Errorf("specify exactly one of --sandbox-id or --name")
		}

		target := sandboxID
		if name != "" {
			target = name
		}

		// delete is a node-local directory removal; a snapshot can outlive its
		// sandbox, so there is no shim round-trip.
		dir := filepath.Join(containerdshim.SnapshotBaseDir, target)
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("failed to delete snapshot %s: %s", dir, err)
		}

		fmt.Fprintln(defaultOutputFile, dir)

		return nil
	},
}
