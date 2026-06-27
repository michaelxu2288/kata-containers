// Copyright (c) 2018 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package main

import (
	"fmt"
	"os"

	containerdtypes "github.com/containerd/containerd/api/types"
	shimapi "github.com/containerd/containerd/runtime/v2/shim"
	"google.golang.org/protobuf/proto"

	shim "github.com/kata-containers/kata-containers/src/runtime/pkg/containerd-shim-v2"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/types"
)

func shimConfig(config *shimapi.Config) {
	config.NoReaper = true
	config.NoSubreaper = true
}

func handleInfoFlag() {
	info := &containerdtypes.RuntimeInfo{
		Name: types.DefaultKataRuntimeName,
		Version: &containerdtypes.RuntimeVersion{
			Version:  katautils.VERSION,
			Revision: katautils.COMMIT,
		},
	}

	data, err := proto.Marshal(info)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to marshal RuntimeInfo: %v\n", err)
		os.Exit(1)
	}

	os.Stdout.Write(data)
	os.Exit(0)
}

func main() {

	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Printf("%s containerd shim (Golang): id: %q, version: %s, commit: %v\n", katautils.PROJECT, types.DefaultKataRuntimeName, katautils.VERSION, katautils.COMMIT)
		os.Exit(0)
	}

	if len(os.Args) == 2 && os.Args[1] == "-info" {
		handleInfoFlag()
	}

	// restore mode: if launched with a snapshot dir, skip the normal containerd-driven
	// shim loop and instead restore a managed sandbox, serve its management API, and run
	// long-lived. triggered by the KATA_RESTORE_FROM env var or a --restore-from flag.
	if restoreFrom := restoreFromArg(); restoreFrom != "" {
		if err := shim.RunRestore(restoreFrom); err != nil {
			fmt.Fprintf(os.Stderr, "restore failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	shimapi.Run(types.DefaultKataRuntimeName, shim.New, shimConfig)
}

// restoreFromArg returns the snapshot dir to restore from, or "" for normal shim mode.
// Accepts either the KATA_RESTORE_FROM env var or a `--restore-from <dir>` flag.
func restoreFromArg() string {
	if v := os.Getenv("KATA_RESTORE_FROM"); v != "" {
		return v
	}
	args := os.Args[1:]
	for i, a := range args {
		if a == "--restore-from" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
