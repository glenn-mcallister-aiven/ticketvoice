// ticketvoice is a Claude Code PreToolUse hook that gates ticket prose — Linear, Jira and GitHub —
// through cope, basanite, and a word budget before it posts.
//
// A memory saying "write like a pragmatic staff engineer" held, and tickets still ran long anyway —
// a memory is a taste, and a taste can be talked past mid-generation without ever registering as a
// violation. cope and basanite replace the taste with an actual read of the prose; the word budget
// below catches what neither of them scores: sheer length.
//
// Silent when the body clears all three. Otherwise it returns permissionDecision "deny" — the reason
// goes to Claude, not a human, so Claude retries on its own instead of paging anyone.
// Every budget is overridable ahead of time, never decided per ticket: TICKETVOICE_MAX_ISSUE_WORDS,
// TICKETVOICE_MAX_COMMENT_WORDS and TICKETVOICE_MAX_SUMMARY_WORDS per class, TICKETVOICE_MAX_WORDS
// as the shared fallback. A denial states the count and the cap; shape advice is separate and comes
// from a template keyed on tracker and class, since the hook cannot know a body's genre — see
// internal/budgetgate/template.go.
//
// Two sibling tools watch the same Linear writes — basanite (vocabulary tics) and cope (voicing and
// structure) — and both, independently, chose never to block on their own: additionalContext only.
// This hook closes that by calling both directly, forwarding to cope-gate -pretool and
// basanite writecheck -no-dedup and gating on their verdicts too, not just the budget — see
// judgeCope and judgeBasanite. A Linear or Bash call is forwarded verbatim; a Jira call is in no
// shape either sibling can read, so siblingStdin bridges it. See CHANGELOG.md.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"strings"

	"github.com/justinstimatze/ticketvoice/internal/budgetgate"
)

var version = "dev"

// buildVersion resolves the version from ldflags, then the module's own build info, then vcs
// metadata, then "dev". See the go-cli-versioning note: the git tag is the source of truth and a
// hand-maintained const drifts from it.
func buildVersion() string {
	if version != "dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return version
	}
	if len(rev) > 12 {
		rev = rev[:12]
	}
	if dirty {
		return rev + "-dirty"
	}
	return rev
}

type hookInput struct {
	ToolName  string          `json:"tool_name"`
	ToolInput json.RawMessage `json:"tool_input"`
	Cwd       string          `json:"cwd"`
}

type linearInput struct {
	Description string `json:"description"`
	Body        string `json:"body"`
	Patch       []struct {
		NewString string `json:"new_string"`
		Text      string `json:"text"`
	} `json:"patch"`
}

// jiraInput is the Atlassian MCP's shape across the four write tools this hook watches. Every
// prose field is json.RawMessage rather than string deliberately: `description` and both
// `commentBody` fields accept either a Markdown string or an ADF document object, chosen by the
// call's contentFormat, so typing them as string would make one ADF body fail the unmarshal for
// the whole call and lose the summary check with it. jsonString yields "" for anything that isn't
// a string, which drops an ADF field through as "nothing to check" while the plain-string fields
// on the same call are still read. Walking an ADF tree for its text leaves is left undone, the
// same call the repo already makes on a Linear patch op list.
type jiraInput struct {
	Summary     json.RawMessage            `json:"summary"`
	Description json.RawMessage            `json:"description"`
	CommentBody json.RawMessage            `json:"commentBody"`
	Fields      map[string]json.RawMessage `json:"fields"`
}

type bashInput struct {
	Command string `json:"command"`
}

// proseField is one checkable field out of a single tool call. A Jira create or edit carries two
// independent ones — a title under its own budget and a body under another — which is why
// extraction returns a slice rather than the one field a Linear write ever has.
type proseField struct {
	Text   string
	Kind   string
	Budget int
}

type hookOutput struct {
	HookSpecificOutput struct {
		HookEventName            string          `json:"hookEventName"`
		PermissionDecision       string          `json:"permissionDecision"`
		PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
		UpdatedInput             json.RawMessage `json:"updatedInput,omitempty"`
	} `json:"hookSpecificOutput"`
}

// prose pulls the human-readable field for the tool being called, and the budget that applies to it.
// A patch is counted on its inserted text only: the rest of the body is not being rewritten.
func prose(tool string, raw json.RawMessage) (text string, budget int, kind string) {
	var in linearInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return "", 0, ""
	}
	switch tool {
	case "mcp__linear__save_issue":
		if in.Description != "" {
			return in.Description, budgetgate.IssueBudget, "issue description"
		}
		var b strings.Builder
		for _, p := range in.Patch {
			b.WriteString(p.NewString + " " + p.Text + " ")
		}
		if s := strings.TrimSpace(b.String()); s != "" {
			return s, budgetgate.IssueBudget, "issue description patch"
		}
	case "mcp__linear__save_comment":
		if in.Body != "" {
			return in.Body, budgetgate.CommentBudget, "comment"
		}
	case "mcp__linear__save_diff_comment":
		if in.Body != "" {
			return in.Body, budgetgate.CommentBudget, "diff comment"
		}
	case "mcp__linear__submit_diff_review":
		if in.Body != "" {
			return in.Body, budgetgate.CommentBudget, "diff review"
		}
	}
	return "", 0, ""
}

// jsonString reads a prose field that the Atlassian schema types as "string or ADF object".
// Anything that is not a JSON string — an ADF document, a null, an absent key — yields "".
func jsonString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// jiraProse pulls every prose field a Jira write carries, each under the budget for its own kind.
// A create and an edit both carry a title and a body, checked in one pass so that a call over on
// both is denied once with both counts, rather than making Claude discover the second on a retry.
//
// editJiraIssue nests everything under `fields`, keyed by Jira field name, which is why that one
// reads a map instead of named struct fields — an edit is also the call most likely to touch no
// prose at all (a label, an assignee, a resolution), and one that touches none yields nothing and
// leaves the hook silent.
func jiraProse(tool string, in jiraInput) []proseField {
	var out []proseField
	add := func(text, kind string, budget int) {
		if text != "" {
			out = append(out, proseField{Text: text, Kind: kind, Budget: budget})
		}
	}
	switch tool {
	case "mcp__atlassian__createJiraIssue":
		add(jsonString(in.Summary), "summary", budgetgate.SummaryBudget)
		add(jsonString(in.Description), "issue description", budgetgate.IssueBudget)
	case "mcp__atlassian__editJiraIssue":
		add(jsonString(in.Fields["summary"]), "summary", budgetgate.SummaryBudget)
		add(jsonString(in.Fields["description"]), "issue description", budgetgate.IssueBudget)
	case "mcp__atlassian__addCommentToJiraIssue":
		add(jsonString(in.CommentBody), "comment", budgetgate.CommentBudget)
	case "mcp__atlassian__addWorklogToJiraIssue":
		add(jsonString(in.CommentBody), "worklog comment", budgetgate.CommentBudget)
	}
	return out
}

var (
	ghWriteInvoke = regexp.MustCompile(`\bgh-write\s+(issue|pr)\s+(create|comment|edit)\b`)
	heredocOpener = regexp.MustCompile(`<<-?\s*(['"]?)(\w+)['"]?[ \t]*\r?\n`)
)

// maxRedirectBytes caps how much of a `< file` redirect target ghWriteProse will read. A ticket
// body is words, not megabytes; anything bigger isn't a body this gate needs to look at.
const maxRedirectBytes = 2 << 20

// redirectPathByte is the literal-path alphabet ghWriteProse trusts for a `< file` redirect: no
// $VAR, no backtick, no glob, no process substitution — the same "text, not shell semantics"
// limit that keeps the heredoc match honest without a real tokenizer.
func redirectPathByte(b byte) bool {
	switch {
	case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z', b >= '0' && b <= '9':
		return true
	}
	switch b {
	case '/', '.', '_', '-':
		return true
	}
	return false
}

// findRedirectPath looks for a plain `< path` input redirect in a gh-write invocation's tail —
// the case the heredoc marker doesn't cover. `<<` (heredoc) and `<(` (process substitution) are
// skipped, not matched. Anything after `<` that isn't a literal path falls through to ok=false.
func findRedirectPath(rest string) (path string, ok bool) {
	for i := 0; i < len(rest); i++ {
		if rest[i] != '<' {
			continue
		}
		if i+1 < len(rest) && (rest[i+1] == '<' || rest[i+1] == '(') {
			i++
			continue
		}
		j := i + 1
		for j < len(rest) && (rest[j] == ' ' || rest[j] == '\t') {
			j++
		}
		k := j
		for k < len(rest) && redirectPathByte(rest[k]) {
			k++
		}
		if k > j {
			return rest[j:k], true
		}
	}
	return "", false
}

// ghWriteProse finds a gh-write invocation (see cmd/gh-write) in a Bash command string and
// extracts the body that follows it — a heredoc, or a `< file` redirect — with the budget kind
// that applies. gh-write refuses --body/--body-file, so an inline body is never a quoted, escaped
// argument: it's either literal heredoc text this can string-search for, or a plain file this can
// read, both without a shell tokenizer. cwd resolves a relative redirect path; pass "" when it's
// unknown and only an absolute path will be followed. Returns ok=false for any Bash command that
// isn't a gh-write call, or whose body this can't find, and the caller must treat that as
// "nothing to check," not "block" — same as an unparseable Linear call. A pipe-sourced body
// (`... | gh-write ...`) is deliberately not handled here: seeing what a pipe's upstream stage
// would produce means running it, and a text matcher has no business doing that — see gh-write's
// own budgetgate call for the backstop that covers this case instead.
func ghWriteProse(command, cwd string) (text, kind string, budget int, ok bool) {
	inv := ghWriteInvoke.FindStringSubmatchIndex(command)
	if inv == nil {
		return "", "", 0, false
	}
	object := command[inv[2]:inv[3]]
	verb := command[inv[4]:inv[5]]
	kind, budget = budgetgate.Classify(object, verb)
	rest := command[inv[1]:]

	if open := heredocOpener.FindStringSubmatchIndex(rest); open != nil {
		delim := rest[open[4]:open[5]]
		bodyStart := open[1]
		closer := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(delim) + `[ \t]*$`)
		loc := closer.FindStringIndex(rest[bodyStart:])
		if loc == nil {
			return "", "", 0, false
		}
		text = strings.TrimSuffix(rest[bodyStart:bodyStart+loc[0]], "\n")
		return text, kind, budget, true
	}

	if path, found := findRedirectPath(rest); found {
		if !filepath.IsAbs(path) && cwd != "" {
			path = filepath.Join(cwd, path)
		}
		fi, err := os.Stat(path)
		if err != nil || fi.Size() > maxRedirectBytes {
			return "", "", 0, false
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", 0, false
		}
		return string(data), kind, budget, true
	}

	return "", "", 0, false
}

// extractProse dispatches on the calling tool, one tracker per branch: Linear's MCP tools carry a
// structured description/body field (prose, above), Jira's carry a title and a body that are
// budgeted apart (jiraProse, above), and a Bash call carries an opaque command string that only a
// gh-write invocation makes readable (ghWriteProse, above). Any other tool yields nothing to check.
func extractProse(tool string, raw json.RawMessage, cwd string) []proseField {
	switch {
	case tool == "Bash":
		var b bashInput
		if json.Unmarshal(raw, &b) != nil {
			return nil
		}
		t, k, bud, ok := ghWriteProse(b.Command, cwd)
		if !ok {
			return nil
		}
		return []proseField{{Text: t, Kind: k, Budget: bud}}
	case strings.HasPrefix(tool, "mcp__atlassian__"):
		var in jiraInput
		if json.Unmarshal(raw, &in) != nil {
			return nil
		}
		return jiraProse(tool, in)
	}
	text, budget, kind := prose(tool, raw)
	if text == "" {
		return nil
	}
	return []proseField{{Text: text, Kind: kind, Budget: budget}}
}

// The budget check, sibling forwarding, and their word-counting live in internal/budgetgate now
// — gh-write's own backstop needs the exact same gate, not a second copy of it. These names stay
// as thin aliases so the hook logic below and the tests that exercise it don't have to spell the
// package out at every call site.
const (
	defaultIssueBudget   = budgetgate.IssueBudget
	defaultCommentBudget = budgetgate.CommentBudget
	defaultSummaryBudget = budgetgate.SummaryBudget
)

func proseWords(s string) int { return budgetgate.ProseWords(s) }
func evaluate(text, kind string, budget int) (bool, string) {
	return budgetgate.Evaluate(text, kind, budget)
}
func judgeCope(rawStdin []byte) budgetgate.Judgment     { return budgetgate.JudgeCope(rawStdin) }
func judgeBasanite(rawStdin []byte) budgetgate.Judgment { return budgetgate.JudgeBasanite(rawStdin) }

// evaluateField resolves a field's budget and checks it. Resolution can't be one call for every
// kind: a summary carries its own override, deliberately not riding on TICKETVOICE_MAX_WORDS, and
// its denial reason is its own too — see budgetgate.EvaluateSummary.
func evaluateField(f proseField) (bool, string) {
	budget := budgetgate.BudgetForKind(f.Kind, f.Budget)
	if f.Kind == "summary" {
		return budgetgate.EvaluateSummary(f.Text, budget)
	}
	return evaluate(f.Text, f.Kind, budget)
}

// bodyField is the field a message about the call as a whole should name. Extraction appends title
// before body, so on the two-field calls (a Jira create or edit) the body is last, and on every
// other call it is the only field there is.
func bodyField(fields []proseField) proseField { return fields[len(fields)-1] }

// siblingStdin is what the sibling scorers get. A Linear or Bash call is forwarded verbatim: the
// exact bytes this hook received, in the shape both siblings already read. A Jira call is in no
// shape either of them can read — `commentBody` is a field name neither has a case for, and
// `fields.description` is a level deeper than basanite's flat input struct reaches — so its body is
// bridged through the same LinearPayload gh-write uses to reach these two binaries.
//
// The summary is not forwarded. Linear's own title never is, and cope scores paragraph structure —
// length coefficient of variation, dangling ends — which a one-line title standing in for a
// paragraph would skew. Its 20-word budget is the control on it. A call carrying only a summary
// therefore has nothing to forward and returns nil, which skips both subprocesses rather than
// handing them an empty payload to guess at.
func siblingStdin(tool string, raw []byte, fields []proseField) []byte {
	if !strings.HasPrefix(tool, "mcp__atlassian__") {
		return raw
	}
	for _, f := range fields {
		if f.Kind != "summary" {
			return budgetgate.LinearPayload(f.Kind, f.Text)
		}
	}
	return nil
}

// tagPath names the tool_input field taggedInput should rewrite, as a path from the input object's
// root — one element for a top-level field, two for editJiraIssue, which nests every field it sets
// under `fields`. nil means there is nothing to tag: a Bash call (GitHub's tag is gh-write's own
// job, and "description"/"body" mean nothing on a command string), a Linear patch (a diff against
// an existing description, not a fresh post, so no single field owns the prefix), or a Jira summary.
//
// The summary never carries the tag. Four characters out of a 20-word budget is real, and a board
// or list view showing a truncated title is the worst place to spend them; the body is where a
// reader who has actually opened the ticket sees the provenance. A Jira edit that replaces a whole
// description does get tagged — that is a fresh post in a field, not a patch — and the
// already-tagged guard in taggedInput keeps a second edit from stacking a second marker.
func tagPath(tool, kind string) []string {
	switch {
	case strings.HasPrefix(tool, "mcp__linear__"):
		switch kind {
		case "issue description":
			return []string{"description"}
		case "comment", "diff comment", "diff review":
			return []string{"body"}
		}
	case tool == "mcp__atlassian__createJiraIssue":
		if kind == "issue description" {
			return []string{"description"}
		}
	case tool == "mcp__atlassian__editJiraIssue":
		if kind == "issue description" {
			return []string{"fields", "description"}
		}
	case tool == "mcp__atlassian__addCommentToJiraIssue", tool == "mcp__atlassian__addWorklogToJiraIssue":
		return []string{"commentBody"}
	}
	return nil
}

// tagField prepends the agent tag to one string field of obj, in place. Reports false and leaves
// obj alone when the key is absent, already tagged, or holds anything but a JSON string — an ADF
// document among them, since there is no correct place to put a prefix in a node tree.
func tagField(obj map[string]json.RawMessage, field string) bool {
	rawVal, ok := obj[field]
	if !ok {
		return false
	}
	var val string
	if json.Unmarshal(rawVal, &val) != nil || strings.HasPrefix(val, budgetgate.AgentTag) {
		return false
	}
	tagged, err := json.Marshal(budgetgate.AgentTag + val)
	if err != nil {
		return false
	}
	obj[field] = tagged
	return true
}

// taggedInput returns the original tool_input with the agent tag prepended to the one field that
// carries prose — as a full replacement object, since Claude Code's updatedInput replaces the whole
// input rather than merging, so every other field (id, teamId, cloudId, issueTypeName, whatever
// else the real schema carries that this hook never parses) has to round-trip untouched. Returns
// nil when there's nothing to tag (see tagPath), the tag is disabled, or the input can't be read
// back as a plain object.
//
// A two-element path is editJiraIssue's nested `fields` map, which round-trips the same way the
// outer object does: the sibling keys of the field being tagged are values this hook does not
// understand and must not drop.
func taggedInput(tool string, raw json.RawMessage, kind string) json.RawMessage {
	if !budgetgate.AgentTagEnabled() {
		return nil
	}
	path := tagPath(tool, kind)
	if len(path) == 0 {
		return nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(raw, &obj) != nil {
		return nil
	}

	switch len(path) {
	case 1:
		if !tagField(obj, path[0]) {
			return nil
		}
	case 2:
		var nested map[string]json.RawMessage
		if json.Unmarshal(obj[path[0]], &nested) != nil || !tagField(nested, path[1]) {
			return nil
		}
		reNested, err := json.Marshal(nested)
		if err != nil {
			return nil
		}
		obj[path[0]] = reNested
	default:
		return nil
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return out
}

// runHookWithInput is the hook's decision logic, taking the raw bytes so the same bytes can be
// forwarded to the siblings unmodified and so this runs without a subprocess in tests. Returns nil
// when the call should proceed with no hook involvement at all — untagged, since there was nothing
// to tag (a Bash call, a patch, or the tag disabled) as well as unflagged.
func runHookWithInput(raw []byte) *hookOutput {
	var in hookInput
	// A hook that cannot parse its input must not block the call it was watching.
	if json.Unmarshal(raw, &in) != nil {
		return nil
	}
	fields := extractProse(in.ToolName, in.ToolInput, in.Cwd)
	if len(fields) == 0 {
		return nil
	}

	// Every field is checked before anything is reported, so a Jira create over on both its title
	// and its body is denied once carrying both counts. Reporting the first and stopping would make
	// Claude find the second on the retry, at the cost of a whole extra round trip.
	// Shape advice comes from the field that is actually over, not from the call: a create whose
	// title is too long and whose body is fine has nothing to say about body structure. Advice for
	// the comment and summary classes is empty by design (template.go), so a denied comment carries
	// the count and stops there.
	var overReasons []string
	var advice, adviceNote string
	for _, f := range fields {
		over, reason := evaluateField(f)
		if !over {
			continue
		}
		overReasons = append(overReasons, reason)
		if advice == "" {
			advice, adviceNote = budgetgate.Advice(in.ToolName, f.Kind)
		}
	}
	over := len(overReasons) > 0

	var cope, basanite budgetgate.Judgment
	if sib := siblingStdin(in.ToolName, raw, fields); sib != nil {
		cope = judgeCope(sib)
		basanite = judgeBasanite(sib)
	}

	// Only one kind per call maps to a taggable field (tagPath returns nil for a summary), so the
	// first non-nil result is the only one there is.
	var updatedInput json.RawMessage
	for _, f := range fields {
		if u := taggedInput(in.ToolName, in.ToolInput, f.Kind); u != nil {
			updatedInput = u
			break
		}
	}

	if !over && !cope.Flagged && !basanite.Flagged {
		if updatedInput == nil {
			return nil
		}
		var out hookOutput
		out.HookSpecificOutput.HookEventName = "PreToolUse"
		out.HookSpecificOutput.PermissionDecision = "allow"
		out.HookSpecificOutput.UpdatedInput = updatedInput
		return &out
	}

	var reason string
	if over {
		reason = strings.Join(overReasons, "\n\n")
	} else {
		// The number quoted here has to be the one actually in force for this class, or a
		// within-budget denial names a cap the write was never measured against.
		bf := bodyField(fields)
		reason = fmt.Sprintf("This %s is inside the %d-word budget, but a sibling scorer flagged it on the way out.",
			bf.Kind, budgetgate.BudgetForKind(bf.Kind, bf.Budget))
	}
	if cope.Flagged {
		reason += fmt.Sprintf("\n\ncope flagged this:\n\n%s", cope.Note)
	}
	if basanite.Flagged {
		reason += fmt.Sprintf("\n\nbasanite flagged this:\n\n%s", basanite.Note)
	}
	for _, extra := range []string{advice, adviceNote} {
		if extra != "" {
			reason += "\n\n" + extra
		}
	}
	reason += "\n\nCut it and call again."

	var out hookOutput
	out.HookSpecificOutput.HookEventName = "PreToolUse"
	out.HookSpecificOutput.PermissionDecision = "deny"
	out.HookSpecificOutput.PermissionDecisionReason = reason
	return &out
}

func runHook() {
	raw, err := io.ReadAll(os.Stdin)
	if err != nil {
		return
	}
	out := runHookWithInput(raw)
	if out == nil {
		return
	}
	_ = json.NewEncoder(os.Stdout).Encode(out)
}

// runCheck runs prose through the same gate as the hook, outside a tool call — for gating this
// project's own docs the way cope gates README.md through cope-gate and effigy writes its README
// through its own Layer 2. ticketvoice doesn't generate prose, so the analogue is narrower: check a
// slice of prose against the same budget a Linear write would face, as if it were a ticket entry.
func runCheck(args []string) int {
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	field := fs.String("field", "issue", `budget to check against: "issue" or "comment"`)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: ticketvoice --check <file|-> [-field issue|comment]")
		return 2
	}

	kind, budget := "issue description", defaultIssueBudget
	if *field == "comment" {
		kind, budget = "comment", defaultCommentBudget
	}
	budget = budgetgate.BudgetForKind(kind, budget)

	path := fs.Arg(0)
	var data []byte
	var err error
	if path == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(path)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}

	over, reason := evaluate(string(data), kind, budget)
	if !over {
		fmt.Printf("%s: %d words against a %d-word %s budget — within budget.\n", path, proseWords(string(data)), budget, kind)
		return 0
	}
	if advice, _ := budgetgate.Advice("", kind); advice != "" {
		reason += "\n\n" + advice
	}
	fmt.Println(reason + "\n\nCut it and check again.")
	return 1
}

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-version":
			fmt.Println(buildVersion())
			return
		case "--check":
			os.Exit(runCheck(os.Args[2:]))
		}
	}
	runHook()
}
