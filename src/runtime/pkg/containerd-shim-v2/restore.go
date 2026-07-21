// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package containerdshim

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ResolveRestoreSource maps a restore source (snapshot name or path) to a snapshot dir.
// a bare name (no separator) must resolve to a direct child of SnapshotBaseDir; a path is
// taken as given. in both cases the final path is symlink-resolved and re-checked so a
// symlink planted inside the base dir cannot escape it (TOCTOU), then verified to be a real
// snapshot dir (has config.json). lives here, not in the CLI, so the shim restore dispatch
// can reuse it.
func ResolveRestoreSource(from string, confineToBase bool) (string, error) {
	if from == "" {
		return "", fmt.Errorf("restore source is required")
	}
	dir := from
	if !strings.Contains(from, "/") {
		clean := filepath.Clean(filepath.Join(SnapshotBaseDir, from))
		if filepath.Dir(clean) != filepath.Clean(SnapshotBaseDir) {
			return "", fmt.Errorf("invalid snapshot name %q: must not contain path separators or ..", from)
		}
		dir = clean
	}
	// resolve symlinks then re-validate, so a symlinked entry cannot point the restore at an
	// arbitrary host dir after the name check passed.
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return "", fmt.Errorf("cannot resolve snapshot source %q: %w", dir, err)
	}
	// for a bare name, the resolved path must still be inside SnapshotBaseDir -- a symlink
	// planted under the base could otherwise resolve to any host dir. (a path input is an
	// explicit operator choice; annotation-sourced path values are confined separately.)
	if !strings.Contains(from, "/") {
		base := filepath.Clean(SnapshotBaseDir)
		if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
			return "", fmt.Errorf("snapshot %q resolves outside %s", from, base)
		}
	}
	if _, err := os.Stat(filepath.Join(resolved, "config.json")); err != nil {
		return "", fmt.Errorf("not a snapshot dir (no config.json): %s", resolved)
	}
	// when the source is untrusted-ish (annotation-driven restore), confine ANY input -- path
	// or name -- to SnapshotBaseDir; the CLI passes false since a root operator may name an
	// explicit path.
	if confineToBase {
		base := filepath.Clean(SnapshotBaseDir)
		if resolved != base && !strings.HasPrefix(resolved, base+string(os.PathSeparator)) {
			return "", fmt.Errorf("restore source %q resolves outside %s", from, base)
		}
	}
	return resolved, nil
}
