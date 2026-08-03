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

// A resume that fails leaves the quiesce depth raised. The next pause must still
// suppress liveness checks: keying the start stamp off a 0 -> 1 transition made
// every later pause inherit the stale timestamp and count as already expired,
// silently disabling the protection for the rest of the sandbox's life.
func TestGuestQuiesceSurvivesLeakedDepth(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{}

	// pause, then a resume that never lands
	s.beginGuestQuiesce()
	assert.True(s.guestQuiesced())

	// age the window past its budget, as a stuck pause would
	s.quiesceMu.Lock()
	s.quiesceStart = time.Now().Add(-2 * maxGuestQuiesce)
	s.quiesceMu.Unlock()
	assert.False(s.guestQuiesced(), "an expired pause stops suppressing checks")

	// a later, healthy snapshot must be protected again
	s.beginGuestQuiesce()
	assert.True(s.guestQuiesced(), "a new pause must restamp and suppress checks")
	s.endGuestQuiesce()
	assert.False(s.guestQuiesced())
}

// Reading an expired window clears the accounting so a leaked depth cannot
// accumulate across many failed resumes.
func TestGuestQuiesceExpiryClearsDepth(t *testing.T) {
	assert := assert.New(t)
	s := &Sandbox{}

	s.beginGuestQuiesce()
	s.beginGuestQuiesce()
	s.quiesceMu.Lock()
	s.quiesceStart = time.Now().Add(-2 * maxGuestQuiesce)
	s.quiesceMu.Unlock()

	assert.False(s.guestQuiesced())
	s.quiesceMu.Lock()
	depth := s.quiesceDepth
	s.quiesceMu.Unlock()
	assert.Equal(0, depth, "expiry must reset the depth, not leave it raised")
}
