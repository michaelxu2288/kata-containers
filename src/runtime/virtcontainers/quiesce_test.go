// Copyright (c) 2026 Microsoft Corporation
//
// SPDX-License-Identifier: Apache-2.0
//

package virtcontainers

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func TestGuestQuiesceAccounting(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{}

	assert.False(s.guestQuiesced(), "a fresh sandbox is not quiesced")

	s.beginGuestQuiesce()
	assert.True(s.guestQuiesced(), "pausing marks the guest quiesced")

	// nested pauses must both be lifted before liveness checks resume
	s.beginGuestQuiesce()
	s.endGuestQuiesce()
	assert.True(s.guestQuiesced(), "an inner resume must not clear an outer pause")

	s.endGuestQuiesce()
	assert.False(s.guestQuiesced(), "the last resume clears the quiesce")

	// an unbalanced resume must not drive the depth negative
	s.endGuestQuiesce()
	s.beginGuestQuiesce()
	assert.True(s.guestQuiesced(), "depth must not go negative")
	s.endGuestQuiesce()
	assert.False(s.guestQuiesced())
}

func TestGuestQuiesceExpires(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{}

	s.beginGuestQuiesce()
	assert.True(s.guestQuiesced())

	// a resume that never lands must not mute the monitor forever
	s.quiesceMu.Lock()
	s.quiesceStart = time.Now().Add(-2 * maxGuestQuiesce)
	s.quiesceMu.Unlock()
	assert.False(s.guestQuiesced(), "a stuck pause stops suppressing liveness checks")
}
