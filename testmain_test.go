package main

import (
	"os"
	"testing"
)

// TestMain points attempt state at a throwaway directory for the whole package. Tests that skip
// freshState used to write into the operator's real ~/.local/state/ticketvoice, where the Stop
// hook would read their records as live denied writes.
//
// It also clears the budget and template variables. These are meant to be set in the hook's
// environment — a Claude Code `env` block reaches every tool subprocess, this test binary included
// — so a suite that reads them ambiently asserts against whatever the machine happens to be
// configured for. Found live: an operator's 300/160 in settings.json turned four assertions about
// the compiled defaults red on a clean checkout.
func TestMain(m *testing.M) {
	for _, v := range []string{
		"TICKETVOICE_MAX_WORDS", "TICKETVOICE_MAX_ISSUE_WORDS", "TICKETVOICE_MAX_COMMENT_WORDS",
		"TICKETVOICE_MAX_SUMMARY_WORDS", "TICKETVOICE_TEMPLATE", "TICKETVOICE_TEMPLATE_FILE",
		"TICKETVOICE_NO_AGENT_TAG",
	} {
		_ = os.Unsetenv(v)
	}
	dir, err := os.MkdirTemp("", "ticketvoice-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("TICKETVOICE_STATE_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
