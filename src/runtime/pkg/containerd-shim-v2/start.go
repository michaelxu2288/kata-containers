// Copyright (c) 2018 HyperHQ Inc.
//
// SPDX-License-Identifier: Apache-2.0
//

package containerdshim

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/containerd/containerd/api/types/task"
	"github.com/kata-containers/kata-containers/src/runtime/pkg/katautils"
)

func startContainer(ctx context.Context, s *service, c *container) (retErr error) {
	shimLog.WithField("container", c.id).Debug("start container")
	defer func() {
		if retErr != nil {
			// notify the wait goroutine to continue
			c.exitCh <- exitCode255
		}
	}()
	// start a container
	if c.cType == "" {
		err := fmt.Errorf("Bug, the container %s type is empty", c.id)
		return err
	}

	if s.sandbox == nil {
		err := fmt.Errorf("Bug, the sandbox hasn't been created for this container %s", c.id)
		return err
	}

	if c.cType.IsSandbox() {
		// a restored sandbox is brought up PAUSED by RestoreSandbox (netfds rework): the guest
		// resume is deferred to the workload StartContainer path below, not done here. re-running
		// sandbox.Start() would fail the running->running transition, so skip it. For a restored
		// sandbox the VM is still PAUSED at this point, so the monitor/watchSandbox/watchOOMEvents
		// (each dials kata-agent) are NOT armed here -- they are armed after the workload Start path
		// resumes and verifies the VM.
		if !s.restoredSandbox {
			if err := s.sandbox.Start(ctx); err != nil {
				return err
			}
			var err error
			// Start monitor after starting sandbox
			s.monitor, err = s.sandbox.Monitor(ctx)
			if err != nil {
				return err
			}
			go watchSandbox(ctx, s)

			// We use s.ctx(`ctx` derived from `s.ctx`) to check for cancellation of the
			// shim context and the context passed to startContainer for tracing.
			go watchOOMEvents(ctx, s)
		} else {
			// restored sandbox pause-container start: the VM is still paused, so nothing agent-backed
			// is armed here. Report RUNNING and return; the pause task's IO/wait is armed later, once
			// the workload StartContainer resumes the VM (see armDeferredRestoredPauseTask). Mark it
			// so that arming happens exactly once.
			c.status = task.Status_RUNNING
			c.restorePauseIOArmPending = true
			return nil
		}
	} else {
		if !s.restoredSandbox {
			_, err := s.sandbox.StartContainer(ctx, c.id)
			if err != nil {
				return err
			}
		} else {
			// NETFDS restored workload start (§7.4): the app is already live in the guest from the
			// snapshot, so no guest StartContainer is sent. Instead, resume the paused VM and
			// reconcile its NIC to the exact target CNI identity + activate forwarding. A networking
			// failure is fatal (fail-only contract).
			if err := s.sandbox.FinalizeRestoreNetwork(ctx); err != nil {
				return err
			}
			// the sandbox monitor/OOM watchers were deferred while the VM was paused; arm them now
			// that the VM is resumed and the agent is reachable.
			var err error
			s.monitor, err = s.sandbox.Monitor(ctx)
			if err != nil {
				return err
			}
			go watchSandbox(ctx, s)
			go watchOOMEvents(ctx, s)

			// M4: the restored PAUSE task's IO/wait/reaping was deferred at its own startContainer
			// (the VM was paused then). Arm it exactly once now that the VM is resumed, otherwise
			// containerd's Wait on the pause task blocks forever and normal sandbox teardown is
			// bypassed (this was the observed Stop/Delete hang). Best-effort per container.
			armDeferredRestoredPauseTask(ctx, s)
		}
	}

	err := katautils.EnterNetNS(s.sandbox.GetNetNs(), func() error {
		return katautils.PostStartHooks(ctx, *c.spec, s.sandbox.ID(), c.bundle)
	})
	if err != nil {
		// log warning and continue, as defined in oci runtime spec
		// https://github.com/opencontainers/runtime-spec/blob/master/runtime.md#lifecycle
		shimLog.WithError(err).Warn("Failed to run post-start hooks")
	}

	c.status = task.Status_RUNNING

	stdin, stdout, stderr, err := s.sandbox.IOStream(c.id, c.id)
	if err != nil {
		return err
	}

	c.stdinPipe = stdin

	if c.stdin != "" || c.stdout != "" || c.stderr != "" {
		tty, err := newTtyIO(ctx, s.namespace, c.id, c.stdin, c.stdout, c.stderr, c.terminal)
		if err != nil {
			return err
		}
		c.ttyio = tty

		go ioCopy(shimLog.WithField("container", c.id), c.exitIOch, c.stdinCloser, tty, stdin, stdout, stderr)
	} else {
		// close the io exit channel, since there is no io for this container,
		// otherwise the following wait goroutine will hang on this channel.
		close(c.exitIOch)
		// close the stdin closer channel to notify that it's safe to close process's
		// io.
		close(c.stdinCloser)
	}

	go wait(ctx, s, c, "")

	return nil
}

// armDeferredRestoredPauseTask arms the IO/wait lifecycle of a restored sandbox's PAUSE task
// exactly once, after the workload StartContainer has resumed the VM. The pause task's
// startContainer returned early while the VM was paused (agent unreachable), leaving its wait
// goroutine unstarted; without arming it here containerd's Wait on the pause task never returns and
// normal Stop/Delete teardown is bypassed (M4). Best-effort and idempotent: it acts only on a pause
// container still flagged pending, and clears the flag.
func armDeferredRestoredPauseTask(ctx context.Context, s *service) {
	for _, c := range s.containers {
		if c == nil || !c.cType.IsSandbox() || !c.restorePauseIOArmPending {
			continue
		}
		c.restorePauseIOArmPending = false

		stdin, stdout, stderr, err := s.sandbox.IOStream(c.id, c.id)
		if err != nil {
			shimLog.WithError(err).WithField("container", c.id).Warn("restore: could not open pause task IO stream")
			// close the IO channels so the teardown drain (<-c.exitIOch) is not permanently blocked,
			// then still start the wait so the exit path completes.
			close(c.exitIOch)
			close(c.stdinCloser)
			go wait(ctx, s, c, "")
			continue
		}
		c.stdinPipe = stdin
		if c.stdin != "" || c.stdout != "" || c.stderr != "" {
			tty, terr := newTtyIO(ctx, s.namespace, c.id, c.stdin, c.stdout, c.stderr, c.terminal)
			if terr != nil {
				shimLog.WithError(terr).WithField("container", c.id).Warn("restore: could not create pause task tty")
			} else {
				c.ttyio = tty
				go ioCopy(shimLog.WithField("container", c.id), c.exitIOch, c.stdinCloser, tty, stdin, stdout, stderr)
			}
		} else {
			close(c.exitIOch)
			close(c.stdinCloser)
		}
		go wait(ctx, s, c, "")
	}
}

func startExec(ctx context.Context, s *service, containerID, execID string) (e *exec, retErr error) {
	shimLog.WithFields(logrus.Fields{
		"container": containerID,
		"exec":      execID,
	}).Debug("start container execution")
	// start an exec
	c, err := s.getContainer(containerID)
	if err != nil {
		return nil, err
	}

	execs, err := c.getExec(execID)
	if err != nil {
		return nil, err
	}

	defer func() {
		if retErr != nil {
			// notify the wait goroutine to continue
			execs.exitCh <- exitCode255
		}
	}()

	_, proc, err := s.sandbox.EnterContainer(ctx, containerID, *execs.cmds)
	if err != nil {
		err := fmt.Errorf("cannot enter container %s, with err %s", containerID, err)
		return nil, err
	}
	execs.id = proc.Token

	execs.status = task.Status_RUNNING
	if execs.tty.height != 0 && execs.tty.width != 0 {
		err = s.sandbox.WinsizeProcess(ctx, c.id, execs.id, execs.tty.height, execs.tty.width)
		if err != nil {
			return nil, err
		}
	}

	stdin, stdout, stderr, err := s.sandbox.IOStream(c.id, execs.id)
	if err != nil {
		return nil, err
	}

	execs.stdinPipe = stdin

	tty, err := newTtyIO(ctx, s.namespace, execs.id, execs.tty.stdin, execs.tty.stdout, execs.tty.stderr, execs.tty.terminal)
	if err != nil {
		return nil, err
	}
	execs.ttyio = tty

	go ioCopy(shimLog.WithFields(logrus.Fields{
		"container": c.id,
		"exec":      execID,
	}), execs.exitIOch, execs.stdinCloser, tty, stdin, stdout, stderr)

	go wait(ctx, s, c, execID)

	return execs, nil
}
