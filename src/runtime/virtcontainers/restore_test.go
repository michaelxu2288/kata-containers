// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	persistapi "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/persist/api"
	"github.com/kata-containers/kata-containers/src/runtime/virtcontainers/persist"
	"github.com/stretchr/testify/assert"
)

func TestRestoreValidateSandboxID(t *testing.T) {
	assert := assert.New(t)
	assert.NoError(validateSandboxID("clone-abc123"))
	assert.NoError(validateSandboxID("040cec64218ab62a"))
	assert.Error(validateSandboxID(""))
	assert.Error(validateSandboxID(".."))
	assert.Error(validateSandboxID("../etc"))
	assert.Error(validateSandboxID("a/b"))
	assert.Error(validateSandboxID("a..b"))
}

func TestRestoreGenCloneID(t *testing.T) {
	assert := assert.New(t)
	assert.Equal("my-id", genCloneID("my-id"))
	id := genCloneID("")
	assert.True(strings.HasPrefix(id, "clone-"), "got %q", id)
	assert.NotEqual(id, genCloneID(""))
}

func TestRestoreSplitCIDR(t *testing.T) {
	assert := assert.New(t)
	a, m := splitCIDR("192.168.240.1/24")
	assert.Equal("192.168.240.1", a)
	assert.Equal("24", m)
	a, m = splitCIDR("10.0.0.5")
	assert.Equal("10.0.0.5", a)
	assert.Equal("", m)
}

func TestRestoreReadSnapshotMAC(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	cfg := `{"net":[{"mac":"2a:73:8d:36:45:3a","tap":null}]}`
	assert.NoError(os.WriteFile(filepath.Join(dir, "config.json"), []byte(cfg), 0600))
	mac, err := readSnapshotMAC(dir)
	assert.NoError(err)
	assert.Equal("2a:73:8d:36:45:3a", mac)

	// missing mac -> error
	bad := t.TempDir()
	assert.NoError(os.WriteFile(filepath.Join(bad, "config.json"), []byte(`{"net":[]}`), 0600))
	_, err = readSnapshotMAC(bad)
	assert.Error(err)
}

// TestRestoreSeedPersist is the meaningful one: it verifies seedPersist rewrites the
// sandbox-identity field (SandboxContainer) to the new id and clears the source's
// cgroup paths, WITHOUT a running hypervisor. It uses the real persist driver against a
// temp store root.
func TestRestoreSeedPersist(t *testing.T) {
	assert := assert.New(t)

	// craft a snapshot persist.json carrying a SOURCE sandbox identity + cgroup paths.
	snapDir := t.TempDir()
	srcState := persistapi.SandboxState{
		State:              "running",
		SandboxContainer:   "source-sbid-0000",
		SandboxCgroupPath:  "/kubepods/source-sbid-0000",
		OverheadCgroupPath: "/kubepods/overhead/source-sbid-0000",
		CgroupPaths:        map[string]string{"memory": "/sys/fs/cgroup/memory/source-sbid-0000"},
	}
	raw, err := json.Marshal(srcState)
	assert.NoError(err)
	assert.NoError(os.WriteFile(filepath.Join(snapDir, "persist.json"), raw, 0600))

	newID := "clone-deadbeef"
	assert.NoError(seedPersist(snapDir, newID))

	// read it back through the persist driver under the NEW id.
	store, err := persist.GetDriver()
	if err != nil {
		t.Skipf("persist driver unavailable in this env: %v", err)
	}
	got, _, err := store.FromDisk(newID)
	assert.NoError(err)

	// identity rewritten to the new id.
	assert.Equal(newID, got.SandboxContainer)
	// state preserved (must be non-empty so createSandbox early-returns).
	assert.Equal("running", got.State)
	// source cgroup paths cleared (re-derived fresh for the new id later).
	assert.Empty(got.SandboxCgroupPath)
	assert.Empty(got.OverheadCgroupPath)
	assert.Empty(got.CgroupPaths)

	// cleanup the seeded dir.
	store.Destroy(newID)
}
