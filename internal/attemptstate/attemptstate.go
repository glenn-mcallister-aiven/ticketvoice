// Package attemptstate tracks how many times in a row the same session has had the same write
// denied, and what was flagged the last time, so ticketvoice can tell a rewrite that's converging
// from one that's stalled.
//
// A PreToolUse hook is a fresh process per call, so this can't live in memory: it's one small
// file per retry sequence, the same convention as cope's internal/state and basanite's
// internal/report.StateDir — $XDG_STATE_HOME/ticketvoice, or ~/.local/state/ticketvoice.
package attemptstate

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// validSessionID accepts the shapes Claude Code emits and rejects anything that could escape the
// state directory as a path component — same pattern basanite's own validSessionID uses.
var validSessionID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`).MatchString

var filenameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// Dir resolves the state directory and creates it. TICKETVOICE_STATE_DIR overrides it outright —
// mainly so tests never touch the operator's real state directory; the default otherwise follows
// XDG_STATE_HOME, or ~/.local/state, matching basanite's report.StateDir.
func Dir() (string, error) {
	if v := os.Getenv("TICKETVOICE_STATE_DIR"); v != "" {
		return v, os.MkdirAll(v, 0o755)
	}
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "state")
	}
	dir := filepath.Join(base, "ticketvoice")
	return dir, os.MkdirAll(dir, 0o755)
}

// Key names one retry sequence: the same session denying the same kind of write. Kind is carried
// separately from Tool because a Bash call's tool name is always "Bash" regardless of whether
// gh-write is posting an issue, a PR, or a comment — without it, unrelated gh-write invocations in
// the same session would share one counter.
//
// Anchor is whatever pre-existing id the write already carries — a Linear issue id, an issueId a
// comment attaches to, a gh-write target number — so two different tickets that happen to share a
// session, tool, and kind don't share a counter either. A fresh create has no id yet and Anchor is
// "", which puts every fresh create in one session+tool+kind bucket: unprotected, the same boundary
// case plancheck's own PlanHash-based reset has for a plan with nothing yet to hash.
type Key struct {
	SessionID string
	Tool      string
	Kind      string
	Anchor    string
}

func (k Key) valid() bool { return validSessionID(k.SessionID) && k.Tool != "" }

func slug(s string) string {
	if s = filenameUnsafe.ReplaceAllString(s, "_"); s != "" {
		return s
	}
	return "_"
}

func (k Key) fileName() string {
	return prefix + k.SessionID + "-" + slug(k.Tool) + "-" + slug(k.Kind) + "-" + slug(k.Anchor) + ".json"
}

// Record is one retry sequence's state: how many times in a row this key has just been denied,
// and which rule or word ids were flagged the most recent time.
type Record struct {
	Attempts int      `json:"attempts"`
	Prior    []string `json:"prior,omitempty"`
	// Label and Reason say what was denied and why, so the Stop hook can name a write that was
	// refused and never rewritten. Stops counts the turn ends this record has already blocked.
	Label  string `json:"label,omitempty"`
	Reason string `json:"reason,omitempty"`
	Stops  int    `json:"stops,omitempty"`
}

// Load returns the stored record, or a zero Record when there is none, or when the file is
// missing, corrupt, or unreadable. This is advisory state: losing it costs one attempt of
// escalation, never the gate itself — the same fail-open posture cope's own session state takes.
func Load(k Key) Record {
	if !k.valid() {
		return Record{}
	}
	dir, err := Dir()
	if err != nil {
		return Record{}
	}
	raw, err := os.ReadFile(filepath.Join(dir, k.fileName()))
	if err != nil {
		return Record{}
	}
	var r Record
	if json.Unmarshal(raw, &r) != nil {
		return Record{}
	}
	return r
}

// Save writes the record. A failure to write is silent — the next call on this key just starts
// over at attempt 1, the same outcome a corrupt or missing file produces on Load.
func Save(k Key, r Record) {
	if !k.valid() {
		return
	}
	dir, err := Dir()
	if err != nil {
		return
	}
	raw, err := json.Marshal(r)
	if err != nil {
		return
	}
	_ = os.WriteFile(filepath.Join(dir, k.fileName()), raw, 0o644)
}

// Clear removes the record — called once a key stops being denied, whether the write finally
// cleared every check or the gate let it through after a stalled sequence. Either way, the next
// call on this key is a fresh attempt 1, not a continuation of the last one.
func Clear(k Key) {
	if !k.valid() {
		return
	}
	dir, err := Dir()
	if err != nil {
		return
	}
	_ = os.Remove(filepath.Join(dir, k.fileName()))
}

// Pending is one denied write that hasn't been rewritten yet: its record, and the short id that
// `ticketvoice drop` takes.
type Pending struct {
	ID     string
	Record Record
	file   string
}

const prefix = "attempts-"

// shortID is a stable 8-hex id for a state file, short enough to type into `ticketvoice drop`.
func shortID(file string) string {
	sum := sha256.Sum256([]byte(file))
	return hex.EncodeToString(sum[:4])
}

// PendingFor lists every write this session had denied and has not since landed. A record exists
// exactly while its last attempt was denied: Clear runs on every allowed write of the same key.
func PendingFor(sessionID string) []Pending {
	if !validSessionID(sessionID) {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return nil
	}
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+sessionID+"-*.json"))
	sort.Strings(matches)
	var out []Pending
	for _, m := range matches {
		raw, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil {
			continue
		}
		name := filepath.Base(m)
		out = append(out, Pending{ID: shortID(name), Record: r, file: name})
	}
	return out
}

// Update rewrites a pending record in place, for the Stop hook's block counter.
func (p Pending) Update(r Record) {
	dir, err := Dir()
	if err != nil {
		return
	}
	if raw, err := json.Marshal(r); err == nil {
		_ = os.WriteFile(filepath.Join(dir, p.file), raw, 0o644)
	}
}

// Remove deletes a pending record.
func (p Pending) Remove() {
	if dir, err := Dir(); err == nil {
		_ = os.Remove(filepath.Join(dir, p.file))
	}
}

// FindByID looks a pending record up by its short id across every session, since the Bash call
// that runs `ticketvoice drop` doesn't know its own session id.
func FindByID(id string) (Pending, bool) {
	dir, err := Dir()
	if err != nil {
		return Pending{}, false
	}
	matches, _ := filepath.Glob(filepath.Join(dir, prefix+"*.json"))
	for _, m := range matches {
		name := filepath.Base(m)
		if shortID(name) != id {
			continue
		}
		raw, err := os.ReadFile(m)
		if err != nil {
			return Pending{}, false
		}
		var r Record
		if json.Unmarshal(raw, &r) != nil {
			return Pending{}, false
		}
		return Pending{ID: id, Record: r, file: name}, true
	}
	return Pending{}, false
}

// LogDropped appends one line to dropped.jsonl, the record of every denied write that ended up
// never posting, whether the author dropped it or the Stop hook gave up on it.
func LogDropped(sessionID string, p Pending, why string) {
	dir, err := Dir()
	if err != nil {
		return
	}
	line, err := json.Marshal(map[string]any{
		"ts": time.Now().UTC().Format(time.RFC3339), "session": sessionID, "id": p.ID,
		"label": p.Record.Label, "why": why,
	})
	if err != nil {
		return
	}
	f, err := os.OpenFile(filepath.Join(dir, "dropped.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(line, '\n'))
}
