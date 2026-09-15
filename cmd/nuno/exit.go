// SPDX-License-Identifier: AGPL-3.0-or-later

package main

// Exit codes are a contract, not an implementation detail. FR-72.
const (
	ExitClean      = 0 // nothing to do, or everything applied
	ExitError      = 1 // something failed
	ExitIncomplete = 2 // applied but incomplete: guarded, throttled, or a non-empty dry run
	ExitConfig     = 3 // configuration or startup failure
)
