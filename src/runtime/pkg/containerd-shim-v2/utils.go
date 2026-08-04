// Copyright (c) 2017 Intel Corporation
// Copyright (c) 2018 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package containerdshim

import (
	"context"
	"fmt"
	"github.com/containerd/containerd/api/types/task"
	"os"
	"path/filepath"
	"time"

	"github.com/containerd/containerd/mount"
	cdshim "github.com/containerd/containerd/runtime/v2/shim"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/oci"
	vc "github.com/kata-containers/kata-containers/src/runtime/virtcontainers"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/compatoci"
)

func cReap(s *service, status int, id, execid string, exitat time.Time) {
	s.ec <- exit{
		timestamp: exitat,
		pid:       s.hpid,
		status:    status,
		id:        id,
		execid:    execid,
	}
}

// abortRestoredSandbox terminates a restored sandbox whose activation failed, and
// makes the failure visible to containerd.
//
// The runtime-side abort reaps the VM, but on its own that is not enough to let the
// pod go away. A restored sandbox arms its pause task's waiter only once activation
// succeeds, so a failure leaves no goroutine to notice the sandbox died. Nothing ever
// publishes a task exit, StopPodSandbox blocks until its deadline, and the kubelet
// retries forever -- the pod stays Terminating with a live shim behind it.
//
// Mark the pause task stopped and reap it here, which is what wait() would have done
// had it been running.
func abortRestoredSandbox(ctx context.Context, s *service, cause error) {
	s.sandbox.AbortRestore(ctx, cause)

	c := s.containers[s.id]
	if c == nil || c.status == task.Status_STOPPED {
		return
	}
	exitedAt := time.Now()
	c.status = task.Status_STOPPED
	c.exit = exitCode255
	c.exitTime = exitedAt
	shimLog.WithField("sandbox", s.id).Info("aborted restore: reaping the sandbox task")

	// Wait() blocks on this channel and containerd's StopPodSandbox blocks on
	// Wait(), so without a value here the stop never returns and the pod cannot
	// be removed. The channel is buffered, and the select keeps this safe if a
	// value is somehow already pending.
	select {
	case c.exitCh <- exitCode255:
	default:
	}
	go cReap(s, int(exitCode255), c.id, "", exitedAt)
}

func cleanupContainer(ctx context.Context, sandboxID, cid, bundlePath string) error {
	shimLog.WithField("service", "cleanup").WithField("container", cid).Info("Cleanup container")

	err := vci.CleanupContainer(ctx, sandboxID, cid, true)
	if err != nil {
		shimLog.WithError(err).WithField("container", cid).Warn("failed to cleanup container")
	}

	// The rootfs mount belongs to the host and is independent of any sandbox
	// state, so unmount it whatever happened above. Returning early on a cleanup
	// error left the mount behind for good: nothing runs this path twice.
	rootfs := filepath.Join(bundlePath, "rootfs")
	if uerr := mount.UnmountAll(rootfs, 0); uerr != nil {
		shimLog.WithError(uerr).WithField("container", cid).Warn("failed to cleanup container rootfs")
		if err == nil {
			err = uerr
		}
	}

	return err
}

func validBundle(containerID, bundlePath string) (string, error) {
	// container ID MUST be provided.
	if containerID == "" {
		return "", fmt.Errorf("Missing container ID")
	}

	// bundle path MUST be provided.
	if bundlePath == "" {
		return "", fmt.Errorf("Missing bundle path")
	}

	// bundle path MUST be valid.
	fileInfo, err := os.Stat(bundlePath)
	if err != nil {
		return "", fmt.Errorf("Invalid bundle path '%s': %s", bundlePath, err)
	}
	if !fileInfo.IsDir() {
		return "", fmt.Errorf("Invalid bundle path '%s', it should be a directory", bundlePath)
	}

	resolved, err := katautils.ResolvePath(bundlePath)
	if err != nil {
		return "", err
	}

	return resolved, nil
}

func getAddress(ctx context.Context, bundlePath, address, id string) (string, error) {
	var err error

	// Checks the MUST and MUST NOT from OCI runtime specification
	if bundlePath, err = validBundle(id, bundlePath); err != nil {
		return "", err
	}

	ociSpec, err := compatoci.ParseConfigJSON(bundlePath)
	if err != nil {
		return "", err
	}

	containerType, err := oci.ContainerType(ociSpec)
	if err != nil {
		return "", err
	}

	if containerType == vc.PodContainer {
		sandboxID, err := oci.SandboxID(ociSpec)
		if err != nil {
			return "", err
		}
		address, err := cdshim.SocketAddress(ctx, address, sandboxID)
		if err != nil {
			return "", err
		}
		return address, nil
	}

	return "", nil
}

func noNeedForOutput(detach bool, tty bool) bool {
	return detach && tty
}
