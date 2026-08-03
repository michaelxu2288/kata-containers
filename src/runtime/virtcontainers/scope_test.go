// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestProcessInSystemdScope(t *testing.T) {
	assert := assert.New(t)

	// this process's own scope, whatever it is, must never look like a kata
	// sandbox scope built from a made-up id
	assert.False(processInSystemdScope("kubepods.slice:cri-containerd:definitelynotourscope"),
		"an unrelated scope must not match")

	// a non-systemd (cgroupfs) path has no unit to stop
	assert.False(processInSystemdScope("/kubepods/besteffort/podXYZ"),
		"a cgroupfs path is not a systemd scope")

	// malformed paths must fail closed rather than panic or match
	for _, bad := range []string{"", "only:two", "a:b:c:d", "slice::name", "slice:prefix:"} {
		assert.False(processInSystemdScope(bad), "malformed path %q must not match", bad)
	}
}

func TestProcessInSystemdScopeMatchesOwnCgroup(t *testing.T) {
	// derive a scope name from this process's real cgroup and check we detect
	// membership in it. Skips on hosts without cgroup v2 scope naming.
	data, err := os.ReadFile("/proc/self/cgroup")
	require.NoError(t, err)

	const marker = ".scope"
	line := string(data)
	end := -1
	if i := indexOf(line, marker); i >= 0 {
		end = i
	}
	if end < 0 {
		t.Skip("this process is not in a systemd scope")
	}
	start := end
	for start > 0 && line[start-1] != '/' {
		start--
	}
	unit := line[start : end+len(marker)] // e.g. "session-3.scope"

	// unit is "<prefix>-<name>.scope"; rebuild the kata "slice:prefix:name" form
	dash := -1
	for i := 0; i < len(unit)-len(marker); i++ {
		if unit[i] == '-' {
			dash = i
			break
		}
	}
	if dash < 0 {
		t.Skip("scope name has no prefix-name form")
	}
	prefix := unit[:dash]
	name := unit[dash+1 : len(unit)-len(marker)]

	assert.True(t, processInSystemdScope("some.slice:"+prefix+":"+name),
		"must detect membership in this process's own scope %q", unit)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
