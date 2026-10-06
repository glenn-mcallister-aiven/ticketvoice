package main

import (
	"os"
	"testing"
)

// TestMain points attempt state at a throwaway directory for the whole package. Tests that skip
// freshState used to write into the operator's real ~/.local/state/ticketvoice, where the Stop
// hook would read their records as live denied writes.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "ticketvoice-state-")
	if err != nil {
		panic(err)
	}
	os.Setenv("TICKETVOICE_STATE_DIR", dir)
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
