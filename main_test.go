package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/justinstimatze/ticketvoice/internal/budgetgate"
)

func words(n int) string { return strings.TrimSpace(strings.Repeat("word ", n)) }

// TestMain clears the budget and template variables for the whole package. These are meant to be
// set in the hook's environment — a Claude Code `env` block reaches every tool subprocess, this
// test binary included — so a suite that reads them ambiently asserts against whatever the machine
// happens to be configured for. Found live: an operator's 300/160 in settings.json turned four
// assertions about the compiled defaults red on a clean checkout.
func TestMain(m *testing.M) {
	for _, v := range []string{
		"TICKETVOICE_MAX_WORDS", "TICKETVOICE_MAX_ISSUE_WORDS", "TICKETVOICE_MAX_COMMENT_WORDS",
		"TICKETVOICE_MAX_SUMMARY_WORDS", "TICKETVOICE_TEMPLATE", "TICKETVOICE_TEMPLATE_FILE",
		"TICKETVOICE_NO_AGENT_TAG",
	} {
		_ = os.Unsetenv(v)
	}
	// A flagged Linear write in any test reaches auto-rewrite, which spends real money whenever a
	// key resolves. Unsetting ANTHROPIC_API_KEY is not enough on its own: tokensrc.Resolve also
	// reads a .env up the tree and ~/.config/ticketvoice/.env, which on a developer machine holds a
	// real key. So the endpoint is pointed at a closed local port as well, and whatever key resolves,
	// the call fails fast and the hook falls back to deny. fakeAutorewriteServer overrides this with
	// t.Setenv.
	_ = os.Unsetenv("ANTHROPIC_API_KEY")
	_ = os.Setenv("TICKETVOICE_ANTHROPIC_ENDPOINT", "http://127.0.0.1:1")
	os.Exit(m.Run())
}

// Comfortably past the budget, derived rather than written down. A literal here is how three
// over-budget fixtures silently became under-budget ones when IssueBudget moved 150 -> 200.
var overBudgetWords = defaultIssueBudget + 50

// cleanRewrittenText is words(n) plus an impact line, for a fake autorewrite server's response —
// unlike cleanIssueBody (below), which embeds a literal backslash-n pair meant to be spliced raw
// into hand-built JSON text, this uses a real newline byte, because it goes through json.Marshal
// in fakeAutorewriteServer: marshaling a real newline correctly round-trips as JSON's own \n
// escape, but marshaling cleanIssueBody's already-literal "\n" text would double-escape it into
// four bytes that never decode back to a real newline.
func cleanRewrittenText(n int) string {
	return words(n) + "\n\nImpact: none - test fixture."
}

// cleanIssueBody is words(n) plus an impact line — the impactline check only applies to an issue
// description, so any fixture meant to read as fully clean needs this, not just an under-budget,
// unflagged word count.
func cleanIssueBody(n int) string {
	return words(n) + "\\n\\nImpact: none - test fixture."
}

func TestProseWordsExcludesFencedCode(t *testing.T) {
	body := words(10) + "\n\n```ts\n" + words(500) + "\n```\n\n" + words(5)
	if got := proseWords(body); got != 15 {
		t.Fatalf("fenced code counted: want 15, got %d", got)
	}
}

func TestProseWordsIgnoresBareSymbols(t *testing.T) {
	if got := proseWords("- > | 42 alpha beta"); got != 2 {
		t.Fatalf("want 2 wordish tokens, got %d", got)
	}
}

// The gate must fire for the failure it names — a long issue body — and stay silent otherwise.
func TestProseSelectsFieldAndBudgetPerTool(t *testing.T) {
	for _, tc := range []struct {
		name, tool, raw, want string
		budget                int
	}{
		{"issue description", "mcp__linear__save_issue", `{"description":"hello there"}`, "hello there", defaultIssueBudget},
		{"comment body", "mcp__linear__save_comment", `{"body":"hello there"}`, "hello there", defaultCommentBudget},
		{"patch counts inserted text only", "mcp__linear__save_issue", `{"id":"ABC-1","patch":[{"op":"append","text":"added"}]}`, "added", defaultIssueBudget},
		{"unrelated tool", "mcp__linear__list_issues", `{"description":"hello"}`, "", 0},
		{"issue with no prose", "mcp__linear__save_issue", `{"state":"Done"}`, "", 0},
		{"diff comment body", "mcp__linear__save_diff_comment", `{"body":"hello there"}`, "hello there", defaultCommentBudget},
		{"diff review body", "mcp__linear__submit_diff_review", `{"body":"hello there"}`, "hello there", defaultCommentBudget},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, budget, _ := prose(tc.tool, json.RawMessage(tc.raw))
			if strings.TrimSpace(got) != tc.want || budget != tc.budget {
				t.Fatalf("want (%q, %d), got (%q, %d)", tc.want, tc.budget, strings.TrimSpace(got), budget)
			}
		})
	}
}

// gh-write (cmd/gh-write) is the only thing allowed to put a body in front of this hook as a
// Bash command, and it always does so as a heredoc — see ghWriteProse's doc comment for why.
func TestGhWriteProseExtractsHeredocBody(t *testing.T) {
	for _, tc := range []struct {
		name, command, wantText, wantKind string
		wantBudget                        int
		wantOK                            bool
	}{
		{
			name:       "issue create",
			command:    "gh-write issue create --title 'Bug: X' <<'EOF'\nhello there\nEOF\n",
			wantText:   "hello there",
			wantKind:   "issue description",
			wantBudget: defaultIssueBudget,
			wantOK:     true,
		},
		{
			name:       "pr create",
			command:    "gh-write pr create --title T --base main <<'EOF'\nhello there\nEOF\n",
			wantText:   "hello there",
			wantKind:   "PR description",
			wantBudget: defaultIssueBudget,
			wantOK:     true,
		},
		{
			name:       "issue comment",
			command:    "gh-write issue comment 42 <<'EOF'\nlgtm\nEOF\n",
			wantText:   "lgtm",
			wantKind:   "issue comment",
			wantBudget: defaultCommentBudget,
			wantOK:     true,
		},
		{
			name:       "multi-line body preserved",
			command:    "gh-write pr edit 7 <<'EOF'\nline one\nline two\nEOF\n",
			wantText:   "line one\nline two",
			wantKind:   "PR description",
			wantBudget: defaultIssueBudget,
			wantOK:     true,
		},
		{
			name:    "unrelated bash command",
			command: "gh pr create --title T --body 'inline, unreadable'",
			wantOK:  false,
		},
		{
			name:    "gh-write with no heredoc",
			command: "gh-write issue create --title T --body-file notes.md",
			wantOK:  false,
		},
		{
			name:       "comment edit",
			command:    "gh-write comment edit 9 --repo o/r <<'EOF'\ntrimmed\nEOF\n",
			wantText:   "trimmed",
			wantKind:   "comment",
			wantBudget: defaultCommentBudget,
			wantOK:     true,
		},
		{
			name:       "pr review",
			command:    "gh-write pr review 5 --approve <<'EOF'\nship it\nEOF\n",
			wantText:   "ship it",
			wantKind:   "PR review",
			wantBudget: defaultCommentBudget,
			wantOK:     true,
		},
		{
			// Hit live while writing this change: a script whose own heredoc quoted a gh-write
			// usage line had its test file scored as an issue description.
			name:    "gh-write named inside another command's heredoc",
			command: "python3 - <<'PY'\ndoc = \"gh-write issue create --title T <<'EOF' ... EOF\"\nPY\ncat > t.go <<'EOF'\nlots of test code\nEOF\n",
			wantOK:  false,
		},
		{
			name:    "gh-write named in a quoted argument",
			command: "echo \"run gh-write pr comment 1 <<'EOF'\"\ncat <<'EOF'\nnot a body\nEOF\n",
			wantOK:  false,
		},
		{
			name:       "chained with && before it, on the same line",
			command:    "cd /some/repo && gh-write issue create --title T <<'EOF'\nhello there\nEOF\n",
			wantText:   "hello there",
			wantKind:   "issue description",
			wantBudget: defaultIssueBudget,
			wantOK:     true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text, kind, budget, ok := ghWriteProse(tc.command, "")
			if ok != tc.wantOK {
				t.Fatalf("ok: want %v, got %v (text=%q kind=%q budget=%d)", tc.wantOK, ok, text, kind, budget)
			}
			if !ok {
				return
			}
			if text != tc.wantText || kind != tc.wantKind || budget != tc.wantBudget {
				t.Fatalf("want (%q, %q, %d), got (%q, %q, %d)", tc.wantText, tc.wantKind, tc.wantBudget, text, kind, budget)
			}
		})
	}
}

// gh-write's own validate() refuses --body-file, but not a `< file` redirect — that form is a
// plain file read, not the shell-quoted flag validate() blocks, so it's this function's job to
// find it.
func TestGhWriteProseReadsRedirectFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "body.txt")
	if err := os.WriteFile(path, []byte("hello there"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Run("absolute path", func(t *testing.T) {
		text, kind, budget, ok := ghWriteProse("gh-write issue create --title T < "+path, "")
		if !ok || text != "hello there" || kind != "issue description" || budget != defaultIssueBudget {
			t.Fatalf("want (%q, %q, %d, true), got (%q, %q, %d, %v)", "hello there", "issue description", defaultIssueBudget, text, kind, budget, ok)
		}
	})

	t.Run("relative path resolved against cwd", func(t *testing.T) {
		text, _, _, ok := ghWriteProse("gh-write pr comment 7 < body.txt", dir)
		if !ok || text != "hello there" {
			t.Fatalf("want (%q, true), got (%q, %v)", "hello there", text, ok)
		}
	})

	t.Run("missing file fails open", func(t *testing.T) {
		_, _, _, ok := ghWriteProse("gh-write issue create < nope.txt", dir)
		if ok {
			t.Fatal("a redirect to a nonexistent file must not be treated as a body")
		}
	})

	t.Run("shell-expanded path is not followed", func(t *testing.T) {
		_, _, _, ok := ghWriteProse("gh-write issue create < $HOME/body.txt", "")
		if ok {
			t.Fatal("a redirect path with shell expansion must not be read literally")
		}
	})

	t.Run("process substitution is not a redirect", func(t *testing.T) {
		_, _, _, ok := ghWriteProse("gh-write issue create < <(echo hi)", "")
		if ok {
			t.Fatal("process substitution must not be misread as a file path")
		}
	})
}

// The --body-file rejection is gh-write's job (cmd/gh-write); this only has to confirm that a
// Bash command carrying one instead of a heredoc is *ignored*, not misread as an empty body that
// would pass every budget silently.
func TestExtractProseIgnoresNonGhWriteBash(t *testing.T) {
	if got := extractProse("Bash", json.RawMessage(`{"command":"ls -la"}`), ""); len(got) != 0 {
		t.Fatalf("want no prose for an unrelated Bash command, got %+v", got)
	}
}

// Injure the thing it guards: the real over-budget ticket body was 238 words and must trip the
// budget, while the 118-word rewrite must not. A gate nobody has watched fail is not a gate.
func TestBudgetBoundary(t *testing.T) {
	over, budget, _ := prose("mcp__linear__save_issue", json.RawMessage(`{"description":"`+words(overBudgetWords)+`"}`))
	if proseWords(over) <= budget {
		t.Fatalf("%d words did not exceed the %d-word budget", overBudgetWords, budget)
	}
	under, _, _ := prose("mcp__linear__save_issue", json.RawMessage(`{"description":"`+words(118)+`"}`))
	if proseWords(under) > budget {
		t.Fatalf("118 words tripped the %d-word budget", budget)
	}
}

// evaluate is the single code path both the hook and --check run through; this is the one place
// that would miss a divergence between them.
func TestEvaluateMatchesBudget(t *testing.T) {
	// Derived from the constant, not written as a literal. The literal 150 here passed unchanged
	// when the budget moved to 200 — it was asserting "150 words is under the budget", which stays
	// true for any larger budget and so stopped testing the boundary at all.
	atBudget, overBudget := defaultIssueBudget, defaultIssueBudget+1
	if over, reason := evaluate(words(atBudget), "issue description", defaultIssueBudget); over || reason != "" {
		t.Fatalf("%d words against a %d-word budget must not trip: over=%v reason=%q", atBudget, defaultIssueBudget, over, reason)
	}
	over, reason := evaluate(words(overBudget), "issue description", defaultIssueBudget)
	if !over || !strings.Contains(reason, fmt.Sprintf("%d words", overBudget)) || !strings.Contains(reason, "1 over") {
		t.Fatalf("%d words must trip with a reason naming the overage: over=%v reason=%q", overBudget, over, reason)
	}
}

// fakeSiblingBinary writes a stand-in for cope-gate or basanite that answers the way the real one
// does — the same hookSpecificOutput.additionalContext shape both use — so the subprocess boundary
// is real but the test doesn't need either sibling built. additionalContext == "" reproduces a
// clean run (both siblings print nothing at all on a clean score).
func fakeSiblingBinary(t *testing.T, name, additionalContext string) string {
	t.Helper()
	dir := t.TempDir()
	script := filepath.Join(dir, name)
	body := "#!/bin/sh\n"
	if additionalContext != "" {
		var resp struct {
			HookSpecificOutput struct {
				HookEventName     string `json:"hookEventName"`
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		resp.HookSpecificOutput.HookEventName = "PreToolUse"
		resp.HookSpecificOutput.AdditionalContext = additionalContext
		data, err := json.Marshal(resp)
		if err != nil {
			t.Fatal(err)
		}
		payload := filepath.Join(dir, "payload.json")
		if err := os.WriteFile(payload, data, 0o644); err != nil {
			t.Fatal(err)
		}
		body += "cat \"$(dirname \"$0\")/payload.json\"\n"
	}
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	return script
}

// clean stubs both siblings to a clean run, so a test exercising one flagged sibling isn't
// contaminated by whatever cope-gate/basanite happen to be installed (or not) on the machine
// running the test.
func clean(t *testing.T) {
	t.Helper()
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", ""))
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", ""))
}

// freshState points the retry-attempt state (internal/attemptstate) at a private temp dir, so a
// test exercising it never touches the operator's real ~/.local/state/ticketvoice.
func freshState(t *testing.T) {
	t.Helper()
	t.Setenv("TICKETVOICE_STATE_DIR", t.TempDir())
}

// copeFixture renders a cope-gate note in its real tally-line shape (pretool.go's
// writePreToolReport: "field: N violation(s) — id×count ..."), which is what
// budgetgate.ViolationIDs actually parses. The existing fixtures elsewhere in this file
// ("dangling_end: 1 violation(s)") are illustrative only and never exercise multi-attempt state.
func copeFixture(ids ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "description: %d violation(s) —", len(ids))
	for _, id := range ids {
		fmt.Fprintf(&b, " %s×1", id)
	}
	return b.String()
}

func TestJudgeCopeMissingBinaryFailsOpen(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", filepath.Join(t.TempDir(), "does-not-exist"))
	if j := judgeCope([]byte(`{}`)); j.Flagged {
		t.Fatalf("missing binary must fail open, got %+v", j)
	}
}

func TestJudgeCopeParsesFlaggedOutput(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "clause_symmetry: 1 violation(s)"))
	j := judgeCope([]byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"x"}}`))
	if !j.Flagged || j.Note != "clause_symmetry: 1 violation(s)" {
		t.Fatalf("want flagged with cope's note, got %+v", j)
	}
}

func TestJudgeCopeCleanOutputNotFlagged(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", ""))
	if j := judgeCope([]byte(`{}`)); j.Flagged {
		t.Fatalf("clean run must not flag, got %+v", j)
	}
}

func TestJudgeBasaniteMissingBinaryFailsOpen(t *testing.T) {
	t.Setenv("TICKETVOICE_BASANITE", filepath.Join(t.TempDir(), "does-not-exist"))
	if j := judgeBasanite([]byte(`{}`)); j.Flagged {
		t.Fatalf("missing binary must fail open, got %+v", j)
	}
}

func TestJudgeBasaniteParsesFlaggedOutput(t *testing.T) {
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", "load-bearing ×1 → supporting"))
	j := judgeBasanite([]byte(`{"session_id":"s","tool_input":{"description":"x"}}`))
	if !j.Flagged || j.Note != "load-bearing ×1 → supporting" {
		t.Fatalf("want flagged with basanite's note, got %+v", j)
	}
}

func TestJudgeBasaniteCleanOutputNotFlagged(t *testing.T) {
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", ""))
	if j := judgeBasanite([]byte(`{}`)); j.Flagged {
		t.Fatalf("clean run must not flag, got %+v", j)
	}
}

// The gap this closes: a within-budget ticket carrying a flagged tic used to ship with nobody
// forced to look. This is the test that would have caught it staying open. Denied, not asked —
// the reason goes to Claude, not a human; see the runHookWithInput doc comment.
func TestRunHookDeniesWhenCopeFlagsAnUnderBudgetTicket(t *testing.T) {
	clean(t)
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "dangling_end: 1 violation(s)"))
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil {
		t.Fatal("cope-flagged, under-budget ticket must still be denied")
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %q", out.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "dangling_end") {
		t.Fatalf("reason must carry cope's finding: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

func TestRunHookDeniesWhenBasaniteFlagsAnUnderBudgetTicket(t *testing.T) {
	clean(t)
	freshState(t)
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", "load-bearing ×1 → supporting"))
	raw := []byte(`{"session_id":"s","tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil {
		t.Fatal("basanite-flagged, under-budget ticket must still be denied")
	}
	if out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %q", out.HookSpecificOutput.PermissionDecision)
	}
	if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "load-bearing") {
		t.Fatalf("reason must carry basanite's finding: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

// A call this hook has no prose to check (wrong tool, no matching field) gets no hook output at
// all — not "allow", not a tag, nothing. That's the one case that must stay truly silent.
func TestRunHookSilentOnUnrelatedTool(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__linear__list_issues","tool_input":{"description":"` + words(20) + `"}}`)
	if out := runHookWithInput(raw); out != nil {
		t.Fatalf("a tool this hook doesn't check must stay silent, got %+v", out)
	}
}

// A clean, under-budget Linear write isn't silent anymore — it gets "allow" with the tag applied,
// so a reader always sees the ticket is agent-authored, not only when something got flagged.
func TestRunHookTagsCleanLinearWrite(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"id":"ABC-1","teamId":"eng","description":"` + cleanIssueBody(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil {
		t.Fatal("a clean write must still come back tagged, not nil")
	}
	if out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("want allow, got %q", out.HookSpecificOutput.PermissionDecision)
	}
	var updated map[string]any
	if err := json.Unmarshal(out.HookSpecificOutput.UpdatedInput, &updated); err != nil {
		t.Fatalf("updatedInput must be valid JSON: %v (%s)", err, out.HookSpecificOutput.UpdatedInput)
	}
	if !strings.HasPrefix(updated["description"].(string), "🤖 ") {
		t.Fatalf("description must carry the agent tag, got %+v", updated)
	}
	if updated["id"] != "ABC-1" || updated["teamId"] != "eng" {
		t.Fatalf("unrelated fields must survive the rewrite untouched, got %+v", updated)
	}
}

// updatedInput replaces the whole input object (Claude Code's semantics, not a merge) — this is
// the test that would catch dropping a field the hook never parses, which would corrupt a real
// Linear write rather than just skip the tag.
func TestRunHookTagPreservesUnknownFields(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__linear__save_comment","tool_input":{"issueId":"ABC-9","priority":2,"body":"` + words(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil {
		t.Fatal("want a tagged allow, got nil")
	}
	var updated map[string]any
	if err := json.Unmarshal(out.HookSpecificOutput.UpdatedInput, &updated); err != nil {
		t.Fatalf("updatedInput must be valid JSON: %v", err)
	}
	if updated["issueId"] != "ABC-9" || updated["priority"] != float64(2) {
		t.Fatalf("fields this hook never reads must still round-trip, got %+v", updated)
	}
}

// TICKETVOICE_NO_AGENT_TAG must fully restore the old silent-on-clean behavior, not just skip the
// tag while still returning an output.
func TestRunHookNoTagWhenDisabled(t *testing.T) {
	clean(t)
	t.Setenv("TICKETVOICE_NO_AGENT_TAG", "1")
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(20) + `"}}`)
	if out := runHookWithInput(raw); out != nil {
		t.Fatalf("a clean write with the tag disabled must stay silent, got %+v", out)
	}
}

// A denied write never posts, so there's nothing to tag — updatedInput is for a call that's about
// to execute, and this one isn't. The tag only ever applies on the eventual allow, once Claude
// retries with something that actually clears the gate.
func TestRunHookDeniedLinearWriteCarriesNoUpdatedInput(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "dangling_end: 1 violation(s)"))
	t.Setenv("TICKETVOICE_BASANITE", filepath.Join(t.TempDir(), "does-not-exist"))
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", out)
	}
	if out.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("a denied call must carry no updatedInput, got %s", out.HookSpecificOutput.UpdatedInput)
	}
}

// A gh-write Bash call already tags itself (cmd/gh-write). The hook must never also try to rewrite
// a Bash command's tool_input — there's no "description"/"body" field on it to begin with.
func TestRunHookNeverTagsBashCalls(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"Bash","tool_input":{"command":"gh-write issue create --title T <<'EOF'\n` + words(20) + `\nEOF\n"}}`)
	out := runHookWithInput(raw)
	if out == nil {
		return
	}
	if out.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("a Bash call must never get updatedInput, got %s", out.HookSpecificOutput.UpdatedInput)
	}
}

// A patch is a diff against an existing description, not a fresh post — there's no single field a
// prefix belongs on, so this stays untagged even though it's still gated on word count.
func TestRunHookNeverTagsAPatch(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"id":"ABC-1","patch":[{"op":"append","text":"` + words(20) + `"}]}}`)
	out := runHookWithInput(raw)
	if out != nil && out.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("a patch call must never get updatedInput, got %s", out.HookSpecificOutput.UpdatedInput)
	}
}

// The three-way case: over budget and both siblings flag it. Nothing gets dropped picking a
// reason to show.
func TestRunHookReasonCarriesAllThreeFindingsWhenOverBudgetAndBothFlag(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "forked_end: 1 violation(s)"))
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", "load-bearing ×1 → supporting"))
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(overBudgetWords) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil {
		t.Fatal("over-budget ticket must be denied")
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	if !strings.Contains(reason, fmt.Sprintf("%d words", overBudgetWords)) || !strings.Contains(reason, "forked_end") || !strings.Contains(reason, "load-bearing") {
		t.Fatalf("reason must carry the word count and both siblings' findings: %q", reason)
	}
}

// A denied write that never changes reads as a loop, not a gate. Attempt 1 denies plainly;
// attempt 2 denies and says nothing changed; attempt 3, still unshrunk, lets the write through
// instead of denying a fourth time.
func TestRunHookEscalatesAfterThreeStalledAttempts(t *testing.T) {
	clean(t)
	freshState(t)
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", copeFixture("dangling_end", "clause_symmetry")))
	raw := []byte(`{"session_id":"esc-stall","tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(20) + `"}}`)

	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("attempt 1: want deny, got %+v", out)
	}
	if strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "Since the last attempt") {
		t.Fatalf("attempt 1 has no prior attempt to diff against: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}

	out = runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("attempt 2: want deny, got %+v", out)
	}
	if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "still flagged: cope:clause_symmetry, cope:dangling_end") {
		t.Fatalf("attempt 2 must say the set didn't move: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}

	out = runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("attempt 3, unshrunk: want allow, got %+v", out)
	}
	if !strings.Contains(out.HookSpecificOutput.AdditionalContext, "dangling_end") {
		t.Fatalf("the stalled note must still name what's flagged: %q", out.HookSpecificOutput.AdditionalContext)
	}
}

// Real progress must not be cut off at attempt 3 just because a counter hit a number — only a
// violation set that stops shrinking earns the allow-through.
func TestRunHookKeepsDenyingWhileTheViolationSetShrinks(t *testing.T) {
	clean(t)
	freshState(t)
	call := func(ids ...string) *hookOutput {
		t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", copeFixture(ids...)))
		raw := []byte(`{"session_id":"esc-shrink","tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(20) + `"}}`)
		return runHookWithInput(raw)
	}

	if out := call("dangling_end", "clause_symmetry"); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("attempt 1: want deny, got %+v", out)
	}
	if out := call("dangling_end", "clause_symmetry"); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("attempt 2: want deny, got %+v", out)
	}
	// Attempt 3 drops one violation — genuine progress, so this must still deny, not allow.
	if out := call("dangling_end"); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("attempt 3, shrunk: want deny (progress, not a stall), got %+v", out)
	}
	// Attempt 4 repeats that smaller set, stalled relative to attempt 3 — now it allows.
	if out := call("dangling_end"); out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("attempt 4, unshrunk since attempt 3: want allow, got %+v", out)
	}
}

// Being over budget is a hard limit, not a register — no attempt count earns it an allow-through.
func TestRunHookNeverEscalatesPastBudget(t *testing.T) {
	clean(t)
	freshState(t)
	raw := []byte(`{"session_id":"esc-budget","tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(overBudgetWords) + `"}}`)
	for i := 1; i <= 4; i++ {
		out := runHookWithInput(raw)
		if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
			t.Fatalf("attempt %d, over budget: want deny, got %+v", i, out)
		}
	}
}

// A clean rewrite closes the sequence, so the next flagged write on the same session and tool
// starts a fresh attempt 1 — it must not inherit a stranger's stalled count.
func TestRunHookResetsAttemptsAfterACleanWrite(t *testing.T) {
	clean(t)
	freshState(t)
	raw := []byte(`{"session_id":"esc-reset","tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(20) + `"}}`)

	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", copeFixture("dangling_end")))
	if out := runHookWithInput(raw); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", out)
	}

	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", ""))
	if out := runHookWithInput(raw); out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("clean rewrite: want allow, got %+v", out)
	}

	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", copeFixture("dangling_end")))
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("fresh sequence attempt 1: want deny, got %+v", out)
	}
	if strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "Since the last attempt") {
		t.Fatalf("a reset sequence must not carry delta text from the old one: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

// Two different tickets that happen to share a session, tool, and kind must not share one retry
// counter — an issueId anchors the sequence to "this specific ticket," the same problem
// plancheck's PlanHash solves for a re-planned objective sharing a session.
func TestRunHookDoesNotConflateDifferentTicketsInOneSession(t *testing.T) {
	clean(t)
	freshState(t)
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", copeFixture("dangling_end")))

	ticketA := []byte(`{"session_id":"esc-anchor","tool_name":"mcp__linear__save_comment","tool_input":{"issueId":"ABC-1","body":"` + words(20) + `"}}`)
	ticketB := []byte(`{"session_id":"esc-anchor","tool_name":"mcp__linear__save_comment","tool_input":{"issueId":"ABC-2","body":"` + words(20) + `"}}`)

	if out := runHookWithInput(ticketA); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("ticket A attempt 1: want deny, got %+v", out)
	}
	if out := runHookWithInput(ticketA); out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("ticket A attempt 2: want deny, got %+v", out)
	}

	// A different ticket, same session/tool/kind, hitting the same rule on its own first try must
	// not read as ticket A's attempt 3 and get waved through.
	out := runHookWithInput(ticketB)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("ticket B's own attempt 1 must still deny, got %+v", out)
	}
	if strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "Since the last attempt") {
		t.Fatalf("ticket B must not inherit ticket A's delta text: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

// isolateAutorewriteEnv points ANTHROPIC_API_KEY at a fixed test value and HOME at an empty temp
// dir, so a test asserting autorewrite behavior isn't silently affected by whatever this
// developer's actual ~/.config/ticketvoice/.env happens to contain (it now holds a real key).
func isolateAutorewriteEnv(t *testing.T) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
}

// fakeAutorewriteServer answers every request with a forced tool_use response carrying rewritten
// as the "rewritten" field, and counts how many requests it received — so a test can assert
// autorewrite was never attempted at all, not just that its result didn't change the outcome.
func fakeAutorewriteServer(t *testing.T, rewritten string) (url string, calls *int32) {
	t.Helper()
	calls = new(int32)
	input, _ := json.Marshal(map[string]string{"rewritten": rewritten})
	body, _ := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "tool_use", "name": "rewrite", "input": json.RawMessage(input)},
		},
	})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(calls, 1)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("TICKETVOICE_ANTHROPIC_ENDPOINT", srv.URL)
	return srv.URL, calls
}

func TestRunHookAutoRewriteSucceedsAndAllows(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	_, calls := fakeAutorewriteServer(t, cleanRewrittenText(20))

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(200) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("a rewrite that re-validates clean must allow, got %+v", out)
	}
	if out.HookSpecificOutput.UpdatedInput == nil {
		t.Fatal("a successful auto-rewrite must carry the candidate as UpdatedInput")
	}
	var in struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal(out.HookSpecificOutput.UpdatedInput, &in); err != nil {
		t.Fatalf("UpdatedInput must round-trip as the original object shape: %v", err)
	}
	wantSuffix := budgetgate.AgentTag + cleanRewrittenText(20)
	if in.Description != wantSuffix {
		t.Fatalf("UpdatedInput must carry the tagged REWRITTEN text, not the original, got %q want %q", in.Description, wantSuffix)
	}
	if *calls != 1 {
		t.Fatalf("want exactly one rewrite call, got %d", *calls)
	}
	if ctx := out.HookSpecificOutput.AdditionalContext; !strings.Contains(ctx, "ticketvoice rewrote") ||
		!strings.Contains(ctx, cleanRewrittenText(20)) {
		t.Fatalf("a rewrite must be disclosed with the stored text, got context %q", ctx)
	}
}

// A rewrite that introduces a citation the author never wrote is a different ticket: it must fall
// through to the ordinary deny, never be stored. 22 Sep 2026: a stored rewrite of CUR-1689 came
// back with an invented go-red criterion and bare ticket ids.
func TestRunHookAutoRewriteRejectsACandidateThatInventsEvidence(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	fakeAutorewriteServer(t, cleanRewrittenText(20)+" See CUR-9999.")

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(200) + `"}}`)
	out := runHookWithInput(raw)
	if out != nil && out.HookSpecificOutput.PermissionDecision == "allow" && out.HookSpecificOutput.UpdatedInput != nil {
		t.Fatalf("a rewrite that adds CUR-9999 must not be stored, got %+v", out)
	}
}

func TestSameEvidenceNamesTheContract(t *testing.T) {
	orig := "Broke in 0fe9cfbe9c, see `a.ts:12`, #1258 and https://x.y/z (CUR-12)."
	cases := []struct {
		name string
		cand string
		want bool
	}{
		{"shorter prose, every citation kept", "Broke in 0fe9cfbe9c: `a.ts:12`, #1258, https://x.y/z, CUR-12.", true},
		{"a dropped SHA is lost evidence", "Broke: `a.ts:12`, #1258, https://x.y/z, CUR-12.", false},
		{"a dropped URL is lost evidence", "Broke in 0fe9cfbe9c: `a.ts:12`, #1258, CUR-12.", false},
		{"an added ticket id is invented evidence", "Broke in 0fe9cfbe9c: `a.ts:12`, #1258, https://x.y/z, CUR-12, CUR-13.", false},
	}
	for _, c := range cases {
		if got := sameEvidence(orig, c.cand); got != c.want {
			t.Errorf("%s: sameEvidence = %v, want %v", c.name, got, c.want)
		}
	}
}

// The rewrite endpoint answers, but cope still flags the candidate on re-validation (cope-gate is
// stubbed to always flag here) — this must fall through to today's exact deny behavior, byte-
// identical to a TICKETVOICE_NO_AUTOREWRITE=1 run of the same input.
func TestRunHookAutoRewriteFallsThroughToDenyWhenCandidateStillFlagged(t *testing.T) {
	body := func() []byte {
		return []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(20) + `"}}`)
	}
	stubCope := func(t *testing.T) {
		t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "dangling_end: 1 violation(s)"))
		t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", ""))
	}

	t.Run("with autorewrite", func(t *testing.T) {
		freshState(t)
		isolateAutorewriteEnv(t)
		stubCope(t)
		fakeAutorewriteServer(t, "still bad text")
		out := runHookWithInput(body())
		if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
			t.Fatalf("a candidate that still fails re-validation must fall through to deny, got %+v", out)
		}
		if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "dangling_end") {
			t.Fatalf("the deny reason must name the ORIGINAL text's violation, got %q", out.HookSpecificOutput.PermissionDecisionReason)
		}
	})

	t.Run("with TICKETVOICE_NO_AUTOREWRITE baseline", func(t *testing.T) {
		freshState(t)
		t.Setenv("TICKETVOICE_NO_AUTOREWRITE", "1")
		stubCope(t)
		out := runHookWithInput(body())
		if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
			t.Fatalf("baseline must deny too, got %+v", out)
		}
		if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "dangling_end") {
			t.Fatalf("baseline reason must name the same violation, got %q", out.HookSpecificOutput.PermissionDecisionReason)
		}
	})
}

func TestRunHookAutoRewriteNeverAttemptedWhenCitationsFlagged(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	_, calls := fakeAutorewriteServer(t, cleanRewrittenText(20))
	t.Setenv("TICKETVOICE_LINEAR_TOKEN", "") // no Linear client → citations can't be the trigger here,
	// so force it via a SHA citation against a real repo instead.

	desc := words(overBudgetWords) + " `deadbeef1234`" // a backtick-fenced bogus SHA, confirmed-flagged since cwd is a real repo below
	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + desc + `"},"cwd":"` + mustGitRepo(t) + `"}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("a citations-flagged write must still deny, got %+v", out)
	}
	if *calls != 0 {
		t.Fatalf("citations-flagged writes must never attempt a rewrite, got %d calls", *calls)
	}
}

func TestRunHookAutoRewriteNeverAttemptedWhenImpactFlagged(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	_, calls := fakeAutorewriteServer(t, cleanRewrittenText(20))

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + words(overBudgetWords) + `"}}`) // over budget, no Impact: line
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("a missing-impact-line write must still deny, got %+v", out)
	}
	if *calls != 0 {
		t.Fatalf("impact-flagged writes must never attempt a rewrite, got %d calls", *calls)
	}
}

func TestRunHookAutoRewriteNeverAttemptedForBashCalls(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	_, calls := fakeAutorewriteServer(t, "irrelevant")

	raw := []byte(`{"tool_name":"Bash","tool_input":{"command":"gh-write issue create --title T <<'EOF'\n` + words(overBudgetWords) + `\nEOF\n"}}`)
	runHookWithInput(raw)
	if *calls != 0 {
		t.Fatalf("a Bash/gh-write call must never attempt a rewrite (no field to apply it to), got %d calls", *calls)
	}
}

func TestRunHookAutoRewriteNeverAttemptedForAPatch(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	_, calls := fakeAutorewriteServer(t, "irrelevant")

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"id":"ABC-1","patch":[{"op":"append","text":"` + words(overBudgetWords) + `"}]}}`)
	runHookWithInput(raw)
	if *calls != 0 {
		t.Fatalf("a patch call must never attempt a rewrite, got %d calls", *calls)
	}
}

func TestRunHookAutoRewriteRespectsNoAgentTag(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	t.Setenv("TICKETVOICE_NO_AGENT_TAG", "1")
	fakeAutorewriteServer(t, cleanRewrittenText(20))

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(200) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("want a successful rewrite to still allow with the tag disabled, got %+v", out)
	}
	var in struct {
		Description string `json:"description"`
	}
	json.Unmarshal(out.HookSpecificOutput.UpdatedInput, &in)
	if strings.HasPrefix(in.Description, budgetgate.AgentTag) {
		t.Fatalf("the tag must not be applied when disabled, got %q", in.Description)
	}
	if in.Description != cleanRewrittenText(20) {
		t.Fatalf("the candidate must still reach UpdatedInput untagged, not vanish, got %q", in.Description)
	}
}

func TestRunHookAutoRewriteSkippedWithNoKeyResolvable(t *testing.T) {
	clean(t)
	freshState(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	_, calls := fakeAutorewriteServer(t, "irrelevant")
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "dangling_end: 1 violation(s)"))

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("with no key resolvable, must fall through to deny cleanly, got %+v", out)
	}
	if *calls != 0 {
		t.Fatalf("with no key resolvable, no network call may be attempted, got %d calls", *calls)
	}
}

func TestRunHookAutoRewriteDisabledByEnvVar(t *testing.T) {
	clean(t)
	freshState(t)
	isolateAutorewriteEnv(t)
	t.Setenv("TICKETVOICE_NO_AUTOREWRITE", "1")
	_, calls := fakeAutorewriteServer(t, cleanRewrittenText(20))
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "dangling_end: 1 violation(s)"))

	raw := []byte(`{"tool_name":"mcp__linear__save_issue","tool_input":{"description":"` + cleanIssueBody(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("with autorewrite disabled, must deny as if it didn't exist, got %+v", out)
	}
	if *calls != 0 {
		t.Fatalf("TICKETVOICE_NO_AUTOREWRITE must skip resolving a client at all, got %d calls", *calls)
	}
}

// mustGitRepo returns a real, empty git repo's path — enough for citecheck's isGitRepo check to
// pass so a bogus SHA citation gets confirmed-flagged rather than skipped.
func mustGitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return dir
}

// runCheck is the dogfooding path — README.md's own Why section, gated as if it were a Linear
// issue description, the way `make check-readme` runs it. It must hold, not just compile.
func TestRunCheckAcceptsReadmeWhySection(t *testing.T) {
	data, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	const start, end = "## Why\n", "\n## "
	i := strings.Index(string(data), start)
	if i < 0 {
		t.Fatal("README.md has no ## Why section")
	}
	body := string(data)[i+len(start):]
	if j := strings.Index(body, end); j >= 0 {
		body = body[:j]
	}
	if over, reason := evaluate(body, "issue description", defaultIssueBudget); over {
		t.Fatalf("README's Why section no longer fits its own budget: %s", reason)
	}
}

// --- Jira (Atlassian MCP) ---

// The four write tools, three field shapes. editJiraIssue is the one that nests, and the one that
// most often carries no prose at all.
func TestJiraProseSelectsFieldsAndBudgetsPerTool(t *testing.T) {
	type want struct {
		kind   string
		text   string
		budget int
	}
	for _, tc := range []struct {
		name, tool, raw string
		want            []want
	}{
		{"create carries title and body", "mcp__atlassian__createJiraIssue",
			`{"cloudId":"c","projectKey":"RE","summary":"disk fills","description":"the body"}`,
			[]want{{"summary", "disk fills", defaultSummaryBudget}, {"issue description", "the body", defaultIssueBudget}}},
		{"create with no description", "mcp__atlassian__createJiraIssue",
			`{"cloudId":"c","projectKey":"RE","summary":"disk fills"}`,
			[]want{{"summary", "disk fills", defaultSummaryBudget}}},
		{"edit reads the nested fields map", "mcp__atlassian__editJiraIssue",
			`{"issueIdOrKey":"RE-1","fields":{"summary":"new title","description":"new body"}}`,
			[]want{{"summary", "new title", defaultSummaryBudget}, {"issue description", "new body", defaultIssueBudget}}},
		{"edit touching no prose", "mcp__atlassian__editJiraIssue",
			`{"issueIdOrKey":"RE-1","fields":{"labels":["bug"],"resolution":null}}`, nil},
		{"comment", "mcp__atlassian__addCommentToJiraIssue",
			`{"issueIdOrKey":"RE-1","commentBody":"one point"}`,
			[]want{{"comment", "one point", defaultCommentBudget}}},
		{"worklog comment", "mcp__atlassian__addWorklogToJiraIssue",
			`{"issueIdOrKey":"RE-1","timeSpent":"2h","commentBody":"one point"}`,
			[]want{{"worklog comment", "one point", defaultCommentBudget}}},
		{"unwatched atlassian tool", "mcp__atlassian__getJiraIssue",
			`{"issueIdOrKey":"RE-1"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := extractProse(tc.tool, json.RawMessage(tc.raw), "")
			if len(got) != len(tc.want) {
				t.Fatalf("want %d field(s), got %d: %+v", len(tc.want), len(got), got)
			}
			for i, w := range tc.want {
				if got[i].Kind != w.kind || got[i].Text != w.text || got[i].Budget != w.budget {
					t.Fatalf("field %d: want (%q, %q, %d), got (%q, %q, %d)",
						i, w.kind, w.text, w.budget, got[i].Kind, got[i].Text, got[i].Budget)
				}
			}
		})
	}
}

// An ADF body is a JSON object where the schema also allows a string. Typed as a string it would
// fail the unmarshal for the whole call and take the summary check down with it; the point of the
// json.RawMessage fields is that it doesn't.
func TestJiraAdfDescriptionFallsThroughButSummaryStillChecked(t *testing.T) {
	raw := `{"cloudId":"c","projectKey":"RE","summary":"disk fills","contentFormat":"adf",` +
		`"description":{"type":"doc","version":1,"content":[{"type":"paragraph"}]}}`
	got := extractProse("mcp__atlassian__createJiraIssue", json.RawMessage(raw), "")
	if len(got) != 1 || got[0].Kind != "summary" {
		t.Fatalf("want the summary alone, got %+v", got)
	}
}

// 20 words is the measured cap (see budgetgate.SummaryBudget). SRE-12460's real 28-word summary is
// the shape it exists to deny: a whole finding in the title.
func TestJiraSummaryBudgetBoundary(t *testing.T) {
	if over, _ := evaluateField(proseField{Text: words(20), Kind: "summary", Budget: defaultSummaryBudget}); over {
		t.Fatal("20 words must be within the summary budget")
	}
	if over, _ := evaluateField(proseField{Text: words(21), Kind: "summary", Budget: defaultSummaryBudget}); !over {
		t.Fatal("21 words must be over the summary budget")
	}
	real28 := "OpenSearchRestoreProgressAge.md Case 10 authorises force-deleting the last old-generation node " +
		"without an explicit pass condition, a replica check or a recovery check, which loses data on a " +
		"zero-replica cluster"
	over, reason := evaluateField(proseField{Text: real28, Kind: "summary", Budget: defaultSummaryBudget})
	if !over {
		t.Fatalf("a 28-word summary must be denied, got %d words within budget", proseWords(real28))
	}
	// The four-slot template tells a writer to structure a body. Told that about a title, Claude
	// puts markdown headers in a Jira summary.
	if strings.Contains(reason, "Four slots") {
		t.Fatalf("a summary denial must not carry the body template: %q", reason)
	}
}

// TICKETVOICE_MAX_WORDS loosening a body must not silently buy a 40-word title.
func TestJiraSummaryOverrideIsSeparateFromBodyOverride(t *testing.T) {
	t.Setenv("TICKETVOICE_MAX_WORDS", "400")
	if over, _ := evaluateField(proseField{Text: words(25), Kind: "summary", Budget: defaultSummaryBudget}); !over {
		t.Fatal("the body override must not raise the summary budget")
	}
	t.Setenv("TICKETVOICE_MAX_SUMMARY_WORDS", "30")
	if over, _ := evaluateField(proseField{Text: words(25), Kind: "summary", Budget: defaultSummaryBudget}); over {
		t.Fatal("TICKETVOICE_MAX_SUMMARY_WORDS must raise the summary budget")
	}
}

// Both fields are checked before anything is reported, so one denial carries both counts and Claude
// doesn't discover the second on a retry.
func TestRunHookDeniesJiraCreateOnBothFieldsAtOnce(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c","projectKey":"RE",` +
		`"summary":"` + words(30) + `","description":"` + words(overBudgetWords) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", out)
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	if !strings.Contains(reason, "This summary is 30 words") || !strings.Contains(reason, fmt.Sprintf("%d words", overBudgetWords)) {
		t.Fatalf("reason must carry both counts: %q", reason)
	}
}

// The server segment of an MCP tool name is the user's (or the claude.ai connector's) to choose.
// Measured on 2026-09-08: the connector surfaced as `claude_ai_Atlassian_Rovo`, and a hook keyed on
// `mcp__atlassian__` let a 200-word Jira comment through silently. Every dispatch keys on the method.
func TestRunHookMatchesJiraByMethodNotServerName(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__claude_ai_Atlassian_Rovo__addCommentToJiraIssue","tool_input":{"cloudId":"c",` +
		`"issueIdOrKey":"RE-1","commentBody":"` + words(200) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", out)
	}
	if got := tagPath("mcp__claude_ai_Atlassian_Rovo__editJiraIssue", "issue description"); len(got) != 2 {
		t.Fatalf("tagPath under a connector server name = %v, want fields.description", got)
	}
}

// The tag goes on the body, never the title: four characters out of a 20-word budget, spent in
// every board view. Every field this hook never parses has to round-trip, since updatedInput
// replaces the whole object rather than merging into it.
func TestRunHookTagsJiraDescriptionNotSummary(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c","projectKey":"RE",` +
		`"issueTypeName":"Bug","summary":"disk fills","description":"the body\n\nImpact: none - test fixture."}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("want a tagged allow, got %+v", out)
	}
	var got map[string]any
	if err := json.Unmarshal(out.HookSpecificOutput.UpdatedInput, &got); err != nil {
		t.Fatal(err)
	}
	if got["description"] != "🤖 "+"the body\n\nImpact: none - test fixture." {
		t.Fatalf("description must carry the tag, got %q", got["description"])
	}
	if got["summary"] != "disk fills" {
		t.Fatalf("summary must be untouched, got %q", got["summary"])
	}
	for _, k := range []string{"cloudId", "projectKey", "issueTypeName"} {
		if _, ok := got[k]; !ok {
			t.Fatalf("%s must round-trip through updatedInput", k)
		}
	}
}

func TestCanonicalToolMapsTheOfficialServerAlias(t *testing.T) {
	cases := map[string]string{
		"mcp__linear-official__save_issue":       "mcp__linear__save_issue",
		"mcp__linear-official__save_comment":     "mcp__linear__save_comment",
		"mcp__linear__save_issue":                "mcp__linear__save_issue",
		"Bash":                                   "Bash",
		"mcp__linear-full__linear_createIssue":   "mcp__linear__save_issue",
		"mcp__linear__linear_updateIssue":        "mcp__linear__save_issue",
		"mcp__linear-full__linear_createComment": "mcp__linear__save_comment",
		"mcp__linear__linear_updateComment":      "mcp__linear__save_comment",
		"mcp__linear-full__linear_getIssueById":  "mcp__linear-full__linear_getIssueById",
		"mcp__renamed-anything__save_issue":      "mcp__linear__save_issue",
		"mcp__linear-official__list_issues":      "mcp__linear-official__list_issues",
	}
	for in, want := range cases {
		if got := canonicalTool(in); got != want {
			t.Errorf("canonicalTool(%q) = %q, want %q", in, got, want)
		}
	}
}

// An edit replacing a whole description is a fresh post in a field, not a patch, so it is tagged —
// and the sibling keys inside `fields` are values this hook does not understand and must not drop.
func TestRunHookTagsNestedJiraEditDescription(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__atlassian__editJiraIssue","tool_input":{"cloudId":"c","issueIdOrKey":"RE-1",` +
		`"fields":{"description":"the body\n\nImpact: none - test fixture.","labels":["bug"]}}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("want a tagged allow, got %+v", out)
	}
	var got struct {
		IssueIDOrKey string `json:"issueIdOrKey"`
		Fields       struct {
			Description string   `json:"description"`
			Labels      []string `json:"labels"`
		} `json:"fields"`
	}
	if err := json.Unmarshal(out.HookSpecificOutput.UpdatedInput, &got); err != nil {
		t.Fatal(err)
	}
	if got.Fields.Description != "🤖 "+"the body\n\nImpact: none - test fixture." {
		t.Fatalf("fields.description must carry the tag, got %q", got.Fields.Description)
	}
	if len(got.Fields.Labels) != 1 || got.IssueIDOrKey != "RE-1" {
		t.Fatalf("sibling keys must round-trip, got %+v", got)
	}
}

// A Jira comment is in no shape either sibling can read on its own — this is the test that would
// catch the bridge being dropped, which would silently leave the Jira path budget-only.
func TestRunHookBridgesJiraCommentToSiblings(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", ""))
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", "load-bearing ×1 → supporting"))
	raw := []byte(`{"tool_name":"mcp__atlassian__addCommentToJiraIssue","tool_input":{"issueIdOrKey":"RE-1",` +
		`"commentBody":"` + words(20) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("a basanite-flagged Jira comment must be denied, got %+v", out)
	}
	if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "load-bearing") {
		t.Fatalf("reason must carry basanite's finding: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

// The summary is never forwarded (siblingStdin): cope scores paragraph structure and a title is not
// a paragraph. A create carrying only a summary has nothing to forward, so a sibling that would
// flag anything it were handed must not be reached at all.
func TestRunHookDoesNotForwardASummaryToSiblings(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "dangling_end: 1 violation(s)"))
	t.Setenv("TICKETVOICE_BASANITE", fakeSiblingBinary(t, "basanite", "load-bearing ×1 → supporting"))
	raw := []byte(`{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c","projectKey":"RE",` +
		`"summary":"` + words(10) + `"}}`)
	if out := runHookWithInput(raw); out != nil {
		t.Fatalf("a summary-only create must reach no sibling and produce no output, got %+v", out)
	}
}

// The failure this shipped to fix, at the hook boundary rather than in the package: a denied comment
// carries the count and stops, while a denied issue description still gets the four-slot template.
func TestRunHookDeniedCommentCarriesNoShapeTemplate(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__atlassian__addCommentToJiraIssue","tool_input":{"cloudId":"c",` +
		`"issueIdOrKey":"SRE-1","commentBody":"` + words(300) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", out)
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	if strings.Contains(reason, "Four slots") {
		t.Fatalf("a comment must not be told to become a defect report: %q", reason)
	}
	if !strings.Contains(reason, "300 words") {
		t.Fatalf("the count is the part that is never wrong: %q", reason)
	}
}

func TestRunHookDeniedIssueStillCarriesTheTemplate(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c",` +
		`"projectKey":"SRE","summary":"short enough title","description":"` + words(300) + `"}}`)
	out := runHookWithInput(raw)
	if out == nil || !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "Four slots") {
		t.Fatalf("an issue description keeps the template: %+v", out)
	}
}

// A summary over its own cap while the body is fine: the denial names the summary and nothing about
// body structure, since advice comes from the field that is actually over.
func TestRunHookOverSummaryUnderBodyCarriesNoBodyAdvice(t *testing.T) {
	clean(t)
	raw := []byte(`{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c",` +
		`"projectKey":"SRE","summary":"` + words(30) + `","description":"a short body"}}`)
	out := runHookWithInput(raw)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("want deny, got %+v", out)
	}
	reason := out.HookSpecificOutput.PermissionDecisionReason
	if !strings.Contains(reason, "This summary is 30 words") || strings.Contains(reason, "Four slots") {
		t.Fatalf("want the summary count alone, got %q", reason)
	}
}

// The per-class overrides reaching the hook: 300/160 pass a 300-word description and still deny a
// 200-word comment, which one shared TICKETVOICE_MAX_WORDS cannot express.
func TestRunHookPerClassBudgetOverrides(t *testing.T) {
	clean(t)
	t.Setenv("TICKETVOICE_MAX_ISSUE_WORDS", "300")
	t.Setenv("TICKETVOICE_MAX_COMMENT_WORDS", "160")

	desc := []byte(`{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c",` +
		`"projectKey":"SRE","summary":"short title","description":"` + cleanIssueBody(296) + `"}}`) // 300 words once the impact line is counted
	if out := runHookWithInput(desc); out == nil || out.HookSpecificOutput.PermissionDecision != "allow" {
		t.Fatalf("300 words against a 300-word issue budget must pass, got %+v", out)
	}

	cmt := []byte(`{"tool_name":"mcp__atlassian__addCommentToJiraIssue","tool_input":{"cloudId":"c",` +
		`"issueIdOrKey":"SRE-1","commentBody":"` + words(200) + `"}}`)
	out := runHookWithInput(cmt)
	if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
		t.Fatalf("200 words against a 160-word comment budget must deny, got %+v", out)
	}
	if !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "160-word budget") {
		t.Fatalf("the reason must name the class budget in force: %q", out.HookSpecificOutput.PermissionDecisionReason)
	}
}

// The plumbing most likely to be wrong: labels reach the advice lookup from where the Atlassian MCP
// actually puts them — `additional_fields` on a create, `fields` on an edit.
func TestRunHookLabelsReachAdviceFromBothPlaces(t *testing.T) {
	clean(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "templates.txt")
	if err := os.WriteFile(path, []byte("[jira.label:wayfinder:map.issue]\nDestination, settled, out of scope.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", path)

	for name, raw := range map[string]string{
		"create carries them in additional_fields": `{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{` +
			`"cloudId":"c","projectKey":"SRE","issueTypeName":"Epic","summary":"short title",` +
			`"additional_fields":{"labels":["wayfinder:map"]},"description":"` + words(300) + `"}}`,
		"edit carries them in fields": `{"tool_name":"mcp__atlassian__editJiraIssue","tool_input":{` +
			`"cloudId":"c","issueIdOrKey":"SRE-1","fields":{"labels":["wayfinder:map"],` +
			`"description":"` + words(300) + `"}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			out := runHookWithInput([]byte(raw))
			if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("want deny, got %+v", out)
			}
			reason := out.HookSpecificOutput.PermissionDecisionReason
			if !strings.Contains(reason, "Destination, settled, out of scope.") {
				t.Fatalf("the label's section must be selected: %q", reason)
			}
			if strings.Contains(reason, "Four slots") {
				t.Fatalf("the built-in must not also appear: %q", reason)
			}
		})
	}

	// A label array that is not an array of strings costs a lookup and nothing else.
	raw := `{"tool_name":"mcp__atlassian__createJiraIssue","tool_input":{"cloudId":"c","projectKey":"SRE",` +
		`"issueTypeName":"Epic","summary":"short title","additional_fields":{"labels":"wayfinder:map"},` +
		`"description":"` + words(300) + `"}}`
	out := runHookWithInput([]byte(raw))
	if out == nil || !strings.Contains(out.HookSpecificOutput.PermissionDecisionReason, "Four slots") {
		t.Fatalf("a malformed labels value must fall through to the built-in: %+v", out)
	}
}

func TestAnOfficialServerWriteIsScoredNotIgnored(t *testing.T) {
	raw := []byte(`{"session_id":"s","tool_name":"mcp__linear-official__save_comment","tool_input":{"body":"` + words(overBudgetWords) + `"}}`)
	if runHookWithInput(raw) == nil {
		t.Fatal("an over-budget comment through mcp__linear-official__save_comment produced no verdict; the alias is not reaching the checks")
	}
}

func TestATacticlaunchCommentIsScored(t *testing.T) {
	raw := []byte(`{"session_id":"s","tool_name":"mcp__linear-full__linear_createComment","tool_input":{"issueId":"CUR-1","body":"` + words(overBudgetWords) + `"}}`)
	if runHookWithInput(raw) == nil {
		t.Fatal("an over-budget comment through linear-full's linear_createComment produced no verdict")
	}
}

// stubGhWrite puts an executable named gh-write first on PATH, so the raw-gh deny doesn't depend on
// whether this machine has the real one installed. It is never run.
func stubGhWrite(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh-write"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func bashHookPayload(t *testing.T, command string) []byte {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"tool_name": "Bash", "tool_input": map[string]string{"command": command}})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// FEEDBACK 2026-09-28: a 318-word `gh pr comment --body-file` went out with no check at all, while
// the same text as a Linear comment was denied.
func TestRunHookDeniesRawGhBodyWrites(t *testing.T) {
	clean(t)
	stubGhWrite(t)
	for _, tc := range []struct{ command, want string }{
		{"gh pr comment 1568 --body-file /abs/path/comment.md", "gh-write pr comment 1568 < /abs/path/comment.md"},
		{`gh pr comment 1568 --body "$(cat /abs/path/comment.md)"`, "gh-write pr comment 1568 <<'EOF'"},
		{"gh issue comment 3 -F notes.md --repo o/r", "gh-write issue comment 3 --repo o/r < notes.md"},
		{"gh pr comment 1 --body-file - < x.md", "gh-write pr comment 1 < x.md"},
		{`gh pr review 5 --approve -b "looks good"`, "gh-write pr review 5 --approve <<'EOF'"},
		{"gh api -X PATCH repos/o/r/issues/comments/9 -f body=trimmed", "gh-write comment edit 9"},
	} {
		t.Run(tc.command, func(t *testing.T) {
			out := runHookWithInput(bashHookPayload(t, tc.command))
			if out == nil || out.HookSpecificOutput.PermissionDecision != "deny" {
				t.Fatalf("want a deny, got %+v", out)
			}
			if reason := out.HookSpecificOutput.PermissionDecisionReason; !strings.Contains(reason, tc.want) {
				t.Fatalf("reason must name %q, got:\n%s", tc.want, reason)
			}
		})
	}
}

func TestRunHookLeavesOtherGhCallsAlone(t *testing.T) {
	clean(t)
	stubGhWrite(t)
	for _, command := range []string{
		"gh pr view 1568",
		"gh pr create --fill",
		"gh issue edit 5 --add-label bug",
		`git commit -m "deny a raw gh pr comment --body"`,
	} {
		if out := runHookWithInput(bashHookPayload(t, command)); out != nil {
			t.Errorf("%q: want no hook output, got %+v", command, out.HookSpecificOutput)
		}
	}
}

// With no gh-write to point at, the deny would leave no compliant path, so it fails open the same
// way a missing cope-gate does.
func TestRunHookRawGhFailsOpenWithoutGhWrite(t *testing.T) {
	clean(t)
	t.Setenv("PATH", t.TempDir())
	if out := runHookWithInput(bashHookPayload(t, "gh pr comment 1568 --body-file /abs/path/comment.md")); out != nil {
		t.Fatalf("want no hook output without gh-write on PATH, got %+v", out.HookSpecificOutput)
	}
}

func TestGhWriteIdentityCoversNewVerbs(t *testing.T) {
	for command, want := range map[string]string{
		"gh-write pr review 5 --approve <<'EOF'\nx\nEOF":     "5",
		"gh-write comment edit 9 --repo o/r <<'EOF'\nx\nEOF": "9",
		"gh-write issue create --title T <<'EOF'\nx\nEOF":    "",
	} {
		if got := ghWriteIdentity(command); got != want {
			t.Errorf("ghWriteIdentity(%q) = %q, want %q", command, got, want)
		}
	}
}
