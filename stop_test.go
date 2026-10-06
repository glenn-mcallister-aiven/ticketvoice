package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func commentCall(session, issue, body string) []byte {
	return []byte(`{"session_id":"` + session + `","tool_name":"mcp__linear__save_comment","tool_input":{"issueId":"` + issue + `","body":"` + body + `"}}`)
}

// stop runs the Stop hook for a session and returns its block reason, or "" when it allows.
func stop(t *testing.T, session string) string {
	t.Helper()
	var out bytes.Buffer
	if code := runStop(strings.NewReader(`{"session_id":"`+session+`","stop_hook_active":false}`), &out); code != 0 {
		t.Fatalf("runStop exit %d", code)
	}
	if out.Len() == 0 {
		return ""
	}
	var v struct{ Decision, Reason string }
	if err := json.Unmarshal(out.Bytes(), &v); err != nil || v.Decision != "block" {
		t.Fatalf("runStop wrote %q, want a block decision", out.String())
	}
	return v.Reason
}

var pendingID = regexp.MustCompile(`\[([0-9a-f]{8})\]`)

func TestStopBlocksTheTurnWhileADeniedWriteHasNotBeenRewritten(t *testing.T) {
	clean(t)
	freshState(t)
	if out := runHookWithInput(commentCall("s1", "CUR-1", words(overBudgetWords*3))); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("setup: an over-budget comment must be denied, got %+v", out)
	}
	reason := stop(t, "s1")
	if !strings.Contains(reason, "comment on CUR-1") || !strings.Contains(reason, "words") || !strings.Contains(reason, "ticketvoice drop") {
		t.Fatalf("the block must name the write, carry its deny reason, and offer the drop escape: %q", reason)
	}
	if got := stop(t, "other-session"); got != "" {
		t.Fatalf("another session's turn must not be held by this session's write: %q", got)
	}

	runHookWithInput(commentCall("s1", "CUR-1", words(10)))
	if got := stop(t, "s1"); got != "" {
		t.Fatalf("once the rewrite lands the turn may end: %q", got)
	}
}

func TestStopGivesUpAfterThreeBlocksAndLogsTheWriteAsDropped(t *testing.T) {
	clean(t)
	freshState(t)
	runHookWithInput(commentCall("s2", "CUR-2", words(overBudgetWords*3)))
	for i := 1; i <= maxStops; i++ {
		if stop(t, "s2") == "" {
			t.Fatalf("block %d of %d: want a block", i, maxStops)
		}
	}
	if got := stop(t, "s2"); got != "" {
		t.Fatalf("after %d blocks the hook must let the turn end: %q", maxStops, got)
	}
	log, err := os.ReadFile(filepath.Join(os.Getenv("TICKETVOICE_STATE_DIR"), "dropped.jsonl"))
	if err != nil || !strings.Contains(string(log), "comment on CUR-2") || !strings.Contains(string(log), "gave up") {
		t.Fatalf("a write the hook gave up on must be logged as dropped: %q %v", log, err)
	}
}

func TestDropNeedsAReasonThenClearsThePendingWrite(t *testing.T) {
	clean(t)
	freshState(t)
	runHookWithInput(commentCall("s3", "CUR-3", words(overBudgetWords*3)))
	m := pendingID.FindStringSubmatch(stop(t, "s3"))
	if m == nil {
		t.Fatal("the block must print an id for ticketvoice drop")
	}
	var out bytes.Buffer
	if code := runDrop([]string{m[1]}, &out); code != 2 {
		t.Fatalf("a drop with no reason must be refused, got exit %d", code)
	}
	if stop(t, "s3") == "" {
		t.Fatal("a refused drop must leave the write pending")
	}
	if code := runDrop([]string{m[1], "--reason", "posted on CUR-4 instead"}, &out); code != 0 {
		t.Fatalf("drop with a reason: exit %d, %s", code, out.String())
	}
	if got := stop(t, "s3"); got != "" {
		t.Fatalf("a dropped write must stop holding the turn: %q", got)
	}
	log, _ := os.ReadFile(filepath.Join(os.Getenv("TICKETVOICE_STATE_DIR"), "dropped.jsonl"))
	if !strings.Contains(string(log), "posted on CUR-4 instead") {
		t.Fatalf("the author's reason must be logged: %q", log)
	}
}

func TestStopAllowsOnUnreadableInput(t *testing.T) {
	var out bytes.Buffer
	if code := runStop(strings.NewReader("not json"), &out); code != 0 || out.Len() != 0 {
		t.Fatalf("a Stop hook that can't parse its input must allow: exit %d, %q", code, out.String())
	}
}

func TestStopHoldsADeniedStrictSectionWrite(t *testing.T) {
	strictEnv(t)
	clean(t)
	freshState(t)
	out := strictCall(t, "mcp__linear-strict__set_state", map[string]any{
		"issue": "ENG-9", "patch": []any{map[string]any{"section": "Observed", "mode": "append", "body": observedLine(60)}},
	})
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("setup: want deny, got %+v", out)
	}
	if reason := stop(t, "s1"); !strings.Contains(reason, "Observed section on ENG-9 (set_state)") {
		t.Fatalf("a denied strict section must hold the turn and name itself: %q", reason)
	}
}
