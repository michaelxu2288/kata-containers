// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	kataclient "github.com/kata-containers/kata-containers/src/runtime/virtcontainers/pkg/agent/protocols/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCheckAgentEndpoint(t *testing.T) {
	assert := assert.New(t)
	dir := t.TempDir()
	sock := filepath.Join(dir, "clh.sock")
	require.NoError(t, os.WriteFile(sock, nil, 0o600))

	live := fmt.Sprintf("%s://%s:1024", kataclient.HybridVSockScheme, sock)
	assert.NoError(checkAgentEndpoint(live), "an existing socket must be dialable")

	gone := fmt.Sprintf("%s://%s:1024", kataclient.HybridVSockScheme, filepath.Join(dir, "missing.sock"))
	assert.Error(checkAgentEndpoint(gone), "a missing socket must be reported, not dialed")

	// the VMM directory disappearing is the common case after a crash
	require.NoError(t, os.Remove(sock))
	assert.Error(checkAgentEndpoint(live), "a socket removed under us must be reported")

	// non-hybrid-vsock transports are none of this check's business
	assert.NoError(checkAgentEndpoint("unix:///run/vc/vm/x/agent.sock"))
	assert.NoError(checkAgentEndpoint("mock://whatever"))
	assert.NoError(checkAgentEndpoint(""))
}
