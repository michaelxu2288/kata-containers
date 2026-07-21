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

// restore_timing.go — lightweight phase-timing instrumentation for the restore path.
//
// this is the annotation-branch extension of the branch-3 phaseTimer: it emits the same
// warn-level `phase-timing` lines + a summary line per timer group, so numbers here diff
// directly against the branch-3 RESTORE_PHASES/VMBOOT_PHASES output. logged at WARN because
// the containerd shim runs at logrus.WarnLevel unless --debug (INFO lines are dropped).
//
// groups used on the annotation restore path:
//   - RESTORE  (top-level: config, netnsAdopt, vmboot, assign, resume, endpointHotplug,
//               network, save)
//   - VMBOOT   (sub-phases of the CLH RestoreVM: launchInit, prepFiles, memFill)
//   - NETWORK  (sub-phases of applyRestoreNetwork: guestReIP, routeInstall, neutralizeNIC)
//
// a phaseTimer is single-goroutine; the restore path is sequential so no locking is needed.

type phaseTimer struct {
	kind   string        // group label, e.g. "RESTORE" / "VMBOOT" / "NETWORK"
	id     string        // sandbox/vm id for correlation
	start  time.Time     // group start (for cumulative-ms)
	mark   time.Time     // last mark (for per-phase ms)
	order  []string      // phase names in emission order (for the summary line)
	spent  map[string]float64
	logger *logrus.Entry
}

// newPhaseTimer starts a timing group. logger should be the caller's *logrus.Entry (virtLog
// or clh.Logger()); a nil logger falls back to the package virtLog.
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

// phase records the elapsed time since the previous mark under name, emits a warn-level
// `phase-timing` line (ms = this phase, cumulative-ms = since group start), and advances the
// mark. safe on a nil receiver (instrumentation is best-effort and must never break restore).
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
//   RESTORE_PHASES id=<id> total=132.571 config=1.246 netnsAdopt=35.1 vmboot=73.3 ...
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
