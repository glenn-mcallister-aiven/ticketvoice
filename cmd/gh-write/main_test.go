package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/justinstimatze/ticketvoice/internal/budgetgate"
)

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
	os.Exit(m.Run())
}

// noSiblings points cope/basanite at a path that can't exist, so gateBody fails open and every
// test not specifically exercising the gate isn't at the mercy of whatever's on this machine's
// PATH.
func noSiblings(t *testing.T) {
	t.Helper()
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	t.Setenv("TICKETVOICE_COPE_GATE", missing)
	t.Setenv("TICKETVOICE_BASANITE", missing)
}

// fakeSiblingBinary writes a stand-in for cope-gate or basanite that answers the way the real
// one does — see main_test.go's identical helper in the ticketvoice package; duplicated here
// because the two are separate `package main`s with no shared test-support package to hold it.
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

func TestRunRejectsUnsupportedObject(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"discussion", "create"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "unsupported object") {
		t.Fatalf("want exit 2 and an unsupported-object message, got code=%d stderr=%q", code, errb.String())
	}
}

func TestRunRejectsUnsupportedVerb(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"issue", "close"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "unsupported verb") {
		t.Fatalf("want exit 2 and an unsupported-verb message, got code=%d stderr=%q", code, errb.String())
	}
}

// An inline body is shell-quoted text the hook can't read back out of the command — see the
// package doc. --body, -b and --body= must all be refused.
func TestRunRejectsBodyFlags(t *testing.T) {
	for _, flag := range []string{"--body", "-b", "--body=hi"} {
		t.Run(flag, func(t *testing.T) {
			var out, errb bytes.Buffer
			code := run([]string{"issue", "create", "--title", "T", flag, "x"}, strings.NewReader(""), &out, &errb)
			if code != 2 || !strings.Contains(errb.String(), "not accepted") {
				t.Fatalf("want exit 2 and a rejection message for %s, got code=%d stderr=%q", flag, code, errb.String())
			}
		})
	}
}

func TestRunTooFewArgs(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"issue"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want exit 2 and a usage message, got code=%d stderr=%q", code, errb.String())
	}
}

// Builds a fake `gh` on PATH that just echoes its argv and stdin, so the real gh binary
// (or network) is never involved: this checks what gh-write invokes gh WITH, not what gh
// does with it.
func fakeGhOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'ARGS:%s\\n' \"$*\"\nprintf 'STDIN:'\ncat\n"
	path := filepath.Join(dir, "gh")
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script is POSIX-only")
	}
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestRunForwardsFlagsAndStdinToGh(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"issue", "create", "--title", "Bug: X", "--repo", "octocat/hello-world"},
		strings.NewReader("body text here"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ARGS:issue create --title Bug: X --repo octocat/hello-world --body-file -") {
		t.Fatalf("gh-write did not forward the expected args: %q", got)
	}
	if !strings.Contains(got, "STDIN:"+budgetgate.AgentTag+"body text here") {
		t.Fatalf("gh-write did not tag and forward stdin: %q", got)
	}
}

// gh-write takes --body-file itself: the file's bytes go to gh on stdin, the path does not.
func TestRunReadsBodyFile(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte("from a file"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"pr", "create", "--title", "T", "--body-file", path},
		{"pr", "create", "--body-file=" + path, "--title", "T"},
		{"pr", "create", "-F", path, "--title", "T"},
	} {
		var out, errb bytes.Buffer
		if code := run(args, strings.NewReader("ignored stdin"), &out, &errb); code != 0 {
			t.Fatalf("%v: want exit 0, got %d stderr=%q", args, code, errb.String())
		}
		got := out.String()
		if !strings.Contains(got, "ARGS:pr create --title T --body-file -\n") || strings.Contains(got, path) {
			t.Fatalf("%v: the path must not reach gh: %q", args, got)
		}
		if !strings.Contains(got, "STDIN:"+budgetgate.AgentTag+"from a file") {
			t.Fatalf("%v: want the file's bytes on gh's stdin: %q", args, got)
		}
	}
}

// The gate runs on a --body-file body the same as on stdin.
func TestRunGatesBodyFile(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	path := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(path, []byte(words(budgetgate.IssueBudget+1)), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := run([]string{"issue", "create", "--title", "T", "--body-file", path}, strings.NewReader(""), &out, &errb); code != 1 || strings.Contains(out.String(), "ARGS:") {
		t.Fatalf("want exit 1 with gh never called, got code=%d out=%q", code, out.String())
	}
}

// `--body-file -` is stdin, as on gh.
func TestRunBodyFileDashReadsStdin(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	if code := run([]string{"pr", "comment", "3", "--body-file", "-"}, strings.NewReader("on stdin"), &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if got := out.String(); !strings.Contains(got, "ARGS:pr comment 3 --body-file -\n") || !strings.Contains(got, "STDIN:"+budgetgate.AgentTag+"on stdin") {
		t.Fatalf("want stdin forwarded once: %q", got)
	}
}

func TestRunBodyFileErrors(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"issue", "create", "--body-file"}, strings.NewReader(""), &out, &errb); code != 2 {
		t.Errorf("--body-file with no file: want exit 2, got %d", code)
	}
	if code := run([]string{"issue", "create", "--body-file", filepath.Join(t.TempDir(), "nope")}, strings.NewReader(""), &out, &errb); code != 1 {
		t.Errorf("missing file: want exit 1, got %d", code)
	}
}

func TestRunForwardsPositionalIDForCommentAndEdit(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"pr", "comment", "42"}, strings.NewReader("lgtm"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ARGS:pr comment 42 --body-file -") {
		t.Fatalf("gh-write did not forward the comment id positionally: %q", out.String())
	}
}

// Derived, not a literal: `words(200)` was this fixture until 2026-09-14 and stopped being
// over budget the moment IssueBudget reached 200.
func words(n int) string { return strings.TrimSpace(strings.Repeat("word ", n)) }

// The whole point of the backstop (see the package doc) is that it works no matter how the body
// reached gh-write's stdin — the caller here doesn't matter, only the bytes that arrived.
func TestRunRefusesOverBudgetBody(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"issue", "create", "--title", "T"}, strings.NewReader(words(budgetgate.IssueBudget+50)), &out, &errb)
	if code == 0 {
		t.Fatalf("over-budget body must not exit 0")
	}
	if strings.Contains(out.String(), "ARGS:") {
		t.Fatalf("gh must never be invoked for a refused body, got stdout %q", out.String())
	}
	if !strings.Contains(errb.String(), fmt.Sprintf("%d words", budgetgate.IssueBudget+50)) {
		t.Fatalf("stderr must name the overage: %q", errb.String())
	}
}

func TestRunRefusesWhenCopeFlagsAnUnderBudgetBody(t *testing.T) {
	t.Setenv("TICKETVOICE_COPE_GATE", fakeSiblingBinary(t, "cope-gate", "clause_symmetry: 1 violation(s)"))
	t.Setenv("TICKETVOICE_BASANITE", filepath.Join(t.TempDir(), "does-not-exist"))
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"issue", "create", "--title", "T"}, strings.NewReader(words(20)), &out, &errb)
	if code == 0 {
		t.Fatalf("cope-flagged body must not exit 0")
	}
	if strings.Contains(out.String(), "ARGS:") {
		t.Fatalf("gh must never be invoked for a refused body, got stdout %q", out.String())
	}
	if !strings.Contains(errb.String(), "clause_symmetry") {
		t.Fatalf("stderr must carry cope's finding: %q", errb.String())
	}
}

// The tag is on by default and off only when the operator explicitly says so.
func TestRunOmitsAgentTagWhenDisabled(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	t.Setenv("TICKETVOICE_NO_AGENT_TAG", "1")
	var out, errb bytes.Buffer
	code := run([]string{"issue", "create", "--title", "T"}, strings.NewReader("body text here"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "STDIN:body text here") {
		t.Fatalf("TICKETVOICE_NO_AGENT_TAG must suppress the tag: %q", out.String())
	}
}

func TestRunReviewDefaultsToComment(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"pr", "review", "5"}, strings.NewReader("one nit"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ARGS:pr review 5 --comment --body-file -") {
		t.Fatalf("a review with no verdict must go out as --comment: %q", out.String())
	}
}

func TestRunReviewKeepsItsVerdict(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"pr", "review", "5", "--approve"}, strings.NewReader("ship it"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ARGS:pr review 5 --approve --body-file -") {
		t.Fatalf("gh-write must forward --approve and add no second mode: %q", out.String())
	}
}

func TestRunReviewIsHeldToTheCommentBudget(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"pr", "review", "5"}, strings.NewReader(words(budgetgate.CommentBudget+10)), &out, &errb)
	if code == 0 || strings.Contains(out.String(), "ARGS:") {
		t.Fatalf("an over-budget review must be refused before gh runs, got code=%d stdout=%q", code, out.String())
	}
}

func TestRunIssueReviewIsUnsupported(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"issue", "review", "5"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "unsupported verb") {
		t.Fatalf("want exit 2 for issue review, got code=%d stderr=%q", code, errb.String())
	}
}

func TestRunCommentEditPatchesThroughTheAPI(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"comment", "edit", "9", "--repo", "o/r"}, strings.NewReader("trimmed"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ARGS:api -X PATCH repos/o/r/issues/comments/9 -F body=@-") {
		t.Fatalf("comment edit must PATCH the comment with the body from stdin: %q", got)
	}
	if !strings.Contains(got, "STDIN:"+budgetgate.AgentTag+"trimmed") {
		t.Fatalf("comment edit must tag and forward stdin: %q", got)
	}
}

func TestRunCommentEditDefaultsToTheCurrentRepo(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	if code := run([]string{"comment", "edit", "9"}, strings.NewReader("x"), &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "repos/{owner}/{repo}/issues/comments/9") {
		t.Fatalf("with no --repo, gh api's own placeholders must name the repo: %q", out.String())
	}
}

func TestRunCommentEditNeedsAnID(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"comment", "edit", "--repo", "o/r"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want exit 2 and a usage message, got code=%d stderr=%q", code, errb.String())
	}
}

func TestRunForwardsEditLast(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	if code := run([]string{"issue", "comment", "3", "--edit-last"}, strings.NewReader("x"), &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	if !strings.Contains(out.String(), "ARGS:issue comment 3 --edit-last --body-file -") {
		t.Fatalf("--edit-last must pass straight through: %q", out.String())
	}
}

// A title-only edit reads an empty stdin. Passing `--body-file -` then would blank the description.
func TestRunEditWithNoBodyLeavesTheBodyAlone(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	if code := run([]string{"pr", "edit", "7", "--title", "New"}, strings.NewReader(""), &out, &errb); code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ARGS:pr edit 7 --title New\n") || strings.Contains(got, "--body-file") {
		t.Fatalf("a bodyless edit must not pass --body-file: %q", got)
	}
	if strings.Contains(got, budgetgate.AgentTag) {
		t.Fatalf("a bodyless edit must not send the agent tag as a body: %q", got)
	}
}

func TestRunPrReplyPostsToTheThread(t *testing.T) {
	noSiblings(t)
	fakeGhOnPath(t)
	var out, errb bytes.Buffer
	code := run([]string{"pr", "reply", "42", "2918375521", "--repo", "o/r"}, strings.NewReader("fixed"), &out, &errb)
	if code != 0 {
		t.Fatalf("want exit 0, got %d stderr=%q", code, errb.String())
	}
	got := out.String()
	if !strings.Contains(got, "ARGS:api -X POST repos/o/r/pulls/42/comments/2918375521/replies -F body=@-") {
		t.Fatalf("pr reply must POST to the comment's replies endpoint: %q", got)
	}
	if !strings.Contains(got, "STDIN:"+budgetgate.AgentTag+"fixed") {
		t.Fatalf("pr reply must tag and forward stdin: %q", got)
	}
}

func TestRunPrReplyNeedsBothIDs(t *testing.T) {
	var out, errb bytes.Buffer
	code := run([]string{"pr", "reply", "42"}, strings.NewReader(""), &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "usage:") {
		t.Fatalf("want exit 2 and a usage message, got code=%d stderr=%q", code, errb.String())
	}
}
