// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"fmt"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
)

// restore_timing.go — LOCAL-ONLY phase-timing instrumentation for the net_fds annotation
// restore path. NEVER committed to the PR. Emits warn-level `phase-timing` lines + a
// machine-readable `<KIND>_PHASES` summary per timer group, so numbers diff directly against
// the branch-3 restore-phase-timing-TABLES.md output. Logged at WARN because the containerd
// shim runs at logrus.WarnLevel unless --debug.
//
// groups on the annotation net_fds restore path:
//   - RESTORE  (top-level: config, netnsAdopt, vmboot, netfdsRestorePut, assign, save)
//   - VMBOOT   (sub-phases of CLH RestoreVM: launchInit, prepFiles, memFill)
//   - NETWORK  (FinalizeRestoreNetwork: resume, identityReconcile, routeInstall, verify, activateFence)
//
// a phaseTimer is single-goroutine; the restore path is sequential so no locking is needed.

type phaseTimer struct {
	kind   string
	id     string
	start  time.Time
	mark   time.Time
	order  []string
	spent  map[string]float64
	logger *logrus.Entry
}

// newPhaseTimer starts a timing group. A nil logger falls back to the package virtLog.
func newPhaseTimer(kind, id string, logger *logrus.Entry) *phaseTimer {
	if logger == nil {
		logger = virtLog
	}
	now := time.Now()
	return &phaseTimer{
		kind:   kind,
		id:     id,
		start:  now,
		mark:   now,
		spent:  map[string]float64{},
		logger: logger,
	}
}

// phase records elapsed time since the previous mark under name, emits a warn-level
// `phase-timing` line, and advances the mark. Safe on a nil receiver.
func (t *phaseTimer) phase(name string) {
	if t == nil {
		return
	}
	now := time.Now()
	ms := float64(now.Sub(t.mark).Microseconds()) / 1000.0
	cum := float64(now.Sub(t.start).Microseconds()) / 1000.0
	t.mark = now
	if _, seen := t.spent[name]; !seen {
		t.order = append(t.order, name)
	}
	t.spent[name] += ms
	t.logger.WithFields(logrus.Fields{
		"perf":          "phase-timing",
		"kind":          t.kind,
		"restore-phase": name,
		"ms":            fmt.Sprintf("%.3f", ms),
		"cumulative-ms": fmt.Sprintf("%.3f", cum),
		"sandbox":       t.id,
	}).Warn("phase-timing")
}

// summary emits one warn-level line with every phase and the group total, e.g.
//   RESTORE_PHASES id=<id> total=132.571 config=1.246 netnsAdopt=44.701 ...
func (t *phaseTimer) summary() {
	if t == nil {
		return
	}
	total := float64(time.Since(t.start).Microseconds()) / 1000.0
	var b strings.Builder
	fmt.Fprintf(&b, "%s_PHASES id=%s total=%.3f", t.kind, t.id, total)
	for _, name := range t.order {
		fmt.Fprintf(&b, " %s=%.3f", name, t.spent[name])
	}
	t.logger.WithFields(logrus.Fields{
		"perf": "phase-summary",
	}).Warn(b.String())
}
