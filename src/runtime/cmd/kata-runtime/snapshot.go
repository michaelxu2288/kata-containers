// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"

	containerdshim "github.com/kata-containers/kata-containers/src/runtime/pkg/containerd-shim-v2"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/utils/shimclient"
	"github.com/urfave/cli"
)

var snapshotSubCmds = []cli.Command{
	saveSnapshotCommand,
}

var snapshotCLICommand = cli.Command{
	Name:        "snapshot",
	Usage:       "snapshot a running Kata Containers sandbox VM",
	Subcommands: snapshotSubCmds,
	Action: func(context *cli.Context) {
		cli.ShowSubcommandHelp(context)
	},
}

var saveSnapshotCommand = cli.Command{
	Name:      "save",
	Usage:     "save a snapshot of the sandbox VM to a destination directory",
	ArgsUsage: "[destination-directory]",
	Flags: []cli.Flag{
		cli.StringFlag{
			Name:        "sandbox-id",
			Usage:       "the target sandbox for the snapshot",
			Required:    true,
			Destination: &sandboxID,
		},
	},
	Action: func(c *cli.Context) error {
		// optional positional arg: where to write the snapshot. empty -> shim default.
		destDir := c.Args().Get(0)

		// verify sandbox exists:
		if err := katautils.VerifyContainerID(sandboxID); err != nil {
			return err
		}

		// the shim does the pause/save/snapshot/resume work; we just send the
		// destination dir as the request body and print back the path it used.
		url := containerdshim.SnapshotUrl

		if err := shimclient.DoPut(sandboxID, defaultTimeout, url, "application/octet-stream", []byte(destDir)); err != nil {
			return fmt.Errorf("Error observed when making snapshot request: %s", err)
		}

		out := destDir
		if out == "" {
			out = fmt.Sprintf("/run/vc/vm/snapshots/%s", sandboxID)
		}
		fmt.Fprintln(defaultOutputFile, out)

		return nil
	},
}
