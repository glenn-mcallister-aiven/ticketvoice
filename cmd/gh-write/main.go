// Command gh-write wraps `gh issue`/`gh pr` writes, forcing body text through stdin (a
// quoted heredoc, e.g. `gh-write issue create --title T <<'EOF' ... EOF`) or a --body-file
// gh-write reads itself, instead of a --body flag. `pr review <id>` posts a review, and `comment edit <id>` edits an
// existing conversation comment through the REST API. Those are the two raw gh writes
// ticketvoice's hook denies that had no gh-write form before.
//
// The reason is ticketvoice, not gh-write itself. ticketvoice's PreToolUse hook gates
// prose against a word budget and forwards it to cope/basanite, but for a Bash call it
// only ever sees the raw command string, not a shell's parsed argv — an inline
// --body "..." is shell-quoted and not reliably extractable from that string (nested
// quotes, $() expansion, multi-paragraph text). A heredoc's content is literal text
// between two markers in that same string, which a plain string search finds without a
// shell tokenizer. gh-write is what makes that convention the only way to write a body,
// rather than a discipline someone has to remember on every call.
//
// --body-file (and -F) is taken by gh-write, never passed to gh: gh-write reads the file and gates
// the bytes itself, and ticketvoice's hook reads the same path out of the command string the way
// it reads a `< file` redirect, so both checks still see the body. It exists because Claude Code's
// sandbox exclusion (`gh-write *` in sandbox.excludedCommands) stops matching once the command
// carries any `<<` or `<` redirect, so a stdin-only body made every call from inside Claude Code
// run sandboxed, where the proxy denies api.github.com. A file flag keeps the command bare.
//
// Everything gh-write doesn't recognize is passed straight through to gh, so `--repo`,
// `--title`, `--label`, `--base`, `--draft`, and the rest work exactly as they do on gh
// itself.
//
// gh-write also runs the word-budget/cope/basanite gate itself (gateBody, below), on the real
// body bytes it just read off its own stdin — not a reimplementation of ticketvoice's hook, the
// same check via internal/budgetgate. ticketvoice's hook can only see a heredoc or a `< file`
// redirect in the Bash command string it's watching; a pipe-sourced body (`... | gh-write ...`)
// defeats that entirely. gh-write doesn't have that limit, since by the time it runs it already
// holds the actual bytes regardless of how they arrived — so this is the backstop that closes
// the gap the hook structurally can't, at the cost of catching it after the Bash call already
// started instead of stopping it before it runs.
//
// Every body gh-write sends is also prefixed with an agent tag (🤖) by default, since it's
// posted under the operator's own GitHub account — set TICKETVOICE_NO_AGENT_TAG to turn that off.
package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/justinstimatze/ticketvoice/internal/budgetgate"
	"github.com/justinstimatze/ticketvoice/internal/ghcmd"
)

// validate checks the object/verb/flags shape and returns the gh args to run, or a usage error to
// print instead. Split out of run so the exec/exit-code plumbing there isn't tangled up with
// argument checking.
func validate(args []string) (ghArgs []string, bodyPath, usageErr string) {
	args, bodyPath, usageErr = takeBodyFile(args)
	if usageErr != "" {
		return nil, "", usageErr
	}
	ghArgs, usageErr = validateArgs(args)
	return ghArgs, bodyPath, usageErr
}

// takeBodyFile removes `--body-file FILE`, `--body-file=FILE` and `-F FILE` from args, so gh never
// reads the file unchecked. `-` means stdin, as it does on gh, and comes back as "".
func takeBodyFile(args []string) (rest []string, bodyPath, usageErr string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--body-file" || a == "-F":
			if i+1 >= len(args) {
				return nil, "", "gh-write: " + a + " needs a file"
			}
			bodyPath = args[i+1]
			i++
		case strings.HasPrefix(a, "--body-file="):
			bodyPath = strings.TrimPrefix(a, "--body-file=")
		default:
			rest = append(rest, a)
		}
	}
	if bodyPath == "-" {
		bodyPath = ""
	}
	return rest, bodyPath, ""
}

func validateArgs(args []string) (ghArgs []string, usageErr string) {
	if len(args) < 2 {
		return nil, "usage: gh-write <issue|pr> <create|comment|edit> [id] [gh flags...]\n       gh-write pr review <id> [--approve|--comment|--request-changes] [gh flags...]\n       gh-write comment edit <comment-id> [--repo owner/repo]\n       the body comes from stdin, or from --body-file FILE"
	}
	object, verb := args[0], args[1]
	switch object {
	case "issue", "pr":
		if verb != "create" && verb != "comment" && verb != "edit" && !(object == "pr" && verb == "review") {
			return nil, fmt.Sprintf("gh-write: unsupported verb %q for %s (want create, comment, or edit; pr also takes review)", verb, object)
		}
	case "comment":
		if verb != "edit" {
			return nil, fmt.Sprintf("gh-write: unsupported verb %q for comment (want edit)", verb)
		}
	default:
		return nil, fmt.Sprintf("gh-write: unsupported object %q (want issue, pr, or comment)", object)
	}
	for _, a := range args[2:] {
		if ghcmd.IsBodyFlag(a) {
			return nil, fmt.Sprintf("gh-write: %s is not accepted — pass --body-file FILE or the body on stdin instead, so it lands in the Bash command text ticketvoice reads", a)
		}
	}
	switch {
	case object == "comment":
		return commentEditArgs(args[2:])
	case verb == "review":
		return reviewArgs(args), ""
	}
	return append(append([]string{}, args...), "--body-file", "-"), ""
}

// reviewArgs defaults a review with no verdict to --comment: gh pr review refuses a body without
// one of the three modes, and a body with no verdict is a comment.
func reviewArgs(args []string) []string {
	out := append([]string{}, args...)
	hasMode := false
	for _, a := range args[2:] {
		switch a {
		case "--approve", "-a", "--comment", "-c", "--request-changes", "-r":
			hasMode = true
		}
	}
	if !hasMode {
		out = append(out, "--comment")
	}
	return append(out, "--body-file", "-")
}

// commentEditArgs edits one existing issue or PR conversation comment by id. gh has no command for
// that except `--edit-last`, so this goes through the REST API, with the body read from stdin.
func commentEditArgs(rest []string) (ghArgs []string, usageErr string) {
	repo := "{owner}/{repo}"
	id := ""
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		switch {
		case (a == "--repo" || a == "-R") && i+1 < len(rest):
			repo = rest[i+1]
			i++
		case strings.HasPrefix(a, "--repo="):
			repo = strings.TrimPrefix(a, "--repo=")
		case id == "" && isDigits(a):
			id = a
		default:
			return nil, fmt.Sprintf("gh-write: comment edit takes a comment id and --repo only, not %q", a)
		}
	}
	if id == "" {
		return nil, "usage: gh-write comment edit <comment-id> [--repo owner/repo]"
	}
	return []string{"api", "-X", "PATCH", "repos/" + repo + "/issues/comments/" + id, "-F", "body=@-"}, ""
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// gateBody runs the same word-budget and sibling-scorer check ticketvoice's own PreToolUse hook
// runs, against the real body bytes gh-write just read off its own stdin — the one thing a hook
// watching the Bash command string can never see regardless of whether that body arrived as a
// heredoc, a `< file` redirect, or a pipe. Empty reason means clean.
func gateBody(object, verb, text string) (blocked bool, reason string) {
	kind, budget := budgetgate.Classify(object, verb)
	budget = budgetgate.BudgetForKind(kind, budget)
	over, budgetReason := budgetgate.Evaluate(text, kind, budget)

	// The bridge into the siblings keeps the Linear shape (cope scores nothing for a tool name it
	// does not know), so both notes name Linear and both have to be corrected on the way out.
	payload := budgetgate.LinearPayload(kind, text)
	cope := budgetgate.JudgeCope(payload)
	basanite := budgetgate.JudgeBasanite(payload)
	cope.Note = budgetgate.Relabel(cope.Note, "github", kind)
	basanite.Note = budgetgate.Relabel(basanite.Note, "github", kind)
	if !over && !cope.Flagged && !basanite.Flagged {
		return false, ""
	}

	if over {
		reason = budgetReason
	} else {
		reason = fmt.Sprintf("gh-write: this %s is inside the %d-word budget, but a sibling scorer flagged it.", kind, budget)
	}
	if cope.Flagged {
		reason += fmt.Sprintf("\n\ncope flagged this:\n\n%s", cope.Note)
	}
	if basanite.Flagged {
		reason += fmt.Sprintf("\n\nbasanite flagged this:\n\n%s", basanite.Note)
	}
	// Shape advice, where a template covers this vendor and kind. A GitHub issue or PR description
	// gets the four-slot template; a comment gets the count and nothing else — see template.go.
	if advice, note := budgetgate.Advice("github", "", kind); advice != "" || note != "" {
		for _, extra := range []string{advice, note} {
			if extra != "" {
				reason += "\n\n" + extra
			}
		}
	}
	reason += "\n\nCut it and call gh-write again with less."
	return true, reason
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	ghArgs, bodyPath, usageErr := validate(args)
	if usageErr != "" {
		fmt.Fprintln(stderr, usageErr)
		return 2
	}

	// With --body-file, stdin is ignored rather than refused: a caller's stdin may be a terminal
	// or /dev/null, and neither carries a body.
	var body []byte
	var err error
	if bodyPath != "" {
		body, err = os.ReadFile(bodyPath)
	} else {
		body, err = io.ReadAll(stdin)
	}
	if err != nil {
		fmt.Fprintln(stderr, "gh-write: reading body:", err)
		return 1
	}
	text := string(body)

	if blocked, reason := gateBody(args[0], args[1], text); blocked {
		fmt.Fprintln(stderr, reason)
		return 1
	}

	if budgetgate.AgentTagEnabled() {
		text = budgetgate.AgentTag + text
	}

	cmd := exec.Command("gh", ghArgs...)
	cmd.Stdin = strings.NewReader(text)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			return ee.ExitCode()
		}
		fmt.Fprintln(stderr, "gh-write:", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
