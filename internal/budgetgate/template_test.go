package budgetgate

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
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

func writeTemplateFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "templates.txt")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKindClass(t *testing.T) {
	for kind, want := range map[string]string{
		"summary":                 "summary",
		"comment":                 "comment",
		"diff comment":            "comment",
		"issue comment":           "comment",
		"pr comment":              "comment",
		"worklog comment":         "comment",
		"diff review":             "comment",
		"issue description":       "issue",
		"issue description patch": "issue",
		"PR description":          "issue",
	} {
		if got := KindClass(kind); got != want {
			t.Errorf("KindClass(%q) = %q, want %q", kind, got, want)
		}
	}
}

func TestVendor(t *testing.T) {
	for tool, want := range map[string]string{
		"mcp__linear__save_issue":               "linear",
		"mcp__atlassian__addCommentToJiraIssue": "jira",
		"Bash":                                  "github",
		"github":                                "github",
		"Write":                                 "",
	} {
		if got := Vendor(tool); got != want {
			t.Errorf("Vendor(%q) = %q, want %q", tool, got, want)
		}
	}
}

// The regression this file exists for. A comment denial once carried the four-slot defect template,
// which told a progress update to restructure itself as a defect report. No lookup — for any vendor,
// with or without an operator file — may reach the issue advice from a comment kind.
func TestCommentClassNeverGetsIssueAdvice(t *testing.T) {
	for _, tool := range []string{
		"mcp__atlassian__addCommentToJiraIssue",
		"mcp__atlassian__addWorklogToJiraIssue",
		"mcp__linear__save_comment",
		"mcp__linear__submit_diff_review",
		"Bash",
		"", // no vendor: still resolves through default.comment
	} {
		for _, kind := range []string{"comment", "worklog comment", "diff review", "pr comment"} {
			advice, _ := Advice(tool, "", kind)
			if advice != "" {
				t.Fatalf("Advice(%q, %q) = %q, want none", tool, kind, advice)
			}
		}
	}
}

func TestIssueClassCarriesTheBuiltinTemplate(t *testing.T) {
	advice, note := Advice("mcp__atlassian__createJiraIssue", "", "issue description")
	if !strings.Contains(advice, "Four slots") {
		t.Fatalf("an issue denial must carry the built-in template, got %q", advice)
	}
	if note != "" {
		t.Fatalf("no template file was named, so there is nothing to note: %q", note)
	}
}

func TestSummaryClassCarriesNoAdvice(t *testing.T) {
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "", "summary"); advice != "" {
		t.Fatalf("a summary carries its own one-line reason, not advice: %q", advice)
	}
}

// An operator file overrides one key and leaves the rest of the table standing — the reason
// resolution consults the file and the built-in map at each key rather than choosing one source.
func TestOperatorFileOverridesOneKeyOnly(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[jira.comment]
One finding per comment. Lead with what changed.
`))
	if advice, _ := Advice("mcp__atlassian__addCommentToJiraIssue", "", "comment"); advice != "One finding per comment. Lead with what changed." {
		t.Fatalf("the file's jira.comment must win, got %q", advice)
	}
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "", "issue description"); !strings.Contains(advice, "Four slots") {
		t.Fatalf("jira.issue was not overridden, so the built-in stands, got %q", advice)
	}
	if advice, _ := Advice("mcp__linear__save_comment", "", "comment"); advice != "" {
		t.Fatalf("a jira key must not apply to linear, got %q", advice)
	}
}

// A section declared empty says "this vendor and kind get the count alone", which is different from
// omitting it and inheriting the built-in.
func TestEmptySectionSuppressesTheBuiltin(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, "[jira.issue]\n"))
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "", "issue description"); advice != "" {
		t.Fatalf("an empty jira.issue section must suppress the built-in, got %q", advice)
	}
}

// The only handle on genre, and it is the operator's, not an inference.
func TestTicketvoiceTemplateSelectsByName(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[progress-update]
State what moved, what is blocked, and what you need.
`))
	t.Setenv("TICKETVOICE_TEMPLATE", "progress-update")
	advice, _ := Advice("mcp__atlassian__createJiraIssue", "", "issue description")
	if advice != "State what moved, what is blocked, and what you need." {
		t.Fatalf("the named template must beat vendor.class, got %q", advice)
	}
	// A name with no section falls back rather than silently emptying the advice.
	t.Setenv("TICKETVOICE_TEMPLATE", "no-such-template")
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "", "issue description"); !strings.Contains(advice, "Four slots") {
		t.Fatalf("an unknown name must fall through to vendor.class, got %q", advice)
	}
}

// An operator who set the variable expects it to work, so a file that is too big to send is
// reported in the reason rather than dropped in silence — the one place this package is not silent
// on an abnormal path.
func TestOversizedTemplateFileLoadsNothingAndSaysSo(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t,
		"[jira.issue]\n"+strings.Repeat("x ", maxTemplateFile)))
	advice, note := Advice("mcp__atlassian__createJiraIssue", "", "issue description")
	if !strings.Contains(advice, "Four slots") {
		t.Fatalf("an unusable file leaves the built-in table standing, got %q", advice)
	}
	if !strings.Contains(note, "over the") {
		t.Fatalf("the note must say the file was skipped for size, got %q", note)
	}
}

func TestMissingTemplateFileIsReportedNotFatal(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	advice, note := Advice("mcp__atlassian__createJiraIssue", "", "issue description")
	if !strings.Contains(advice, "Four slots") || note == "" {
		t.Fatalf("want the built-in plus a note, got advice=%q note=%q", advice, note)
	}
}

// One knob for every class cannot express two fields that drifted by different multiples, which is
// the whole reason the per-class variables exist.
func TestBudgetForKindPrecedence(t *testing.T) {
	if got := BudgetForKind("issue description", IssueBudget); got != IssueBudget {
		t.Fatalf("unset: want the compiled default %d, got %d", IssueBudget, got)
	}

	t.Setenv("TICKETVOICE_MAX_WORDS", "500")
	if got := BudgetForKind("issue description", IssueBudget); got != 500 {
		t.Fatalf("the shared variable must still apply as a fallback, got %d", got)
	}
	if got := BudgetForKind("comment", CommentBudget); got != 500 {
		t.Fatalf("the shared variable applies to every class, got %d", got)
	}

	t.Setenv("TICKETVOICE_MAX_ISSUE_WORDS", "300")
	t.Setenv("TICKETVOICE_MAX_COMMENT_WORDS", "160")
	if got := BudgetForKind("issue description", IssueBudget); got != 300 {
		t.Fatalf("the class variable must beat the shared one, got %d", got)
	}
	if got := BudgetForKind("comment", CommentBudget); got != 160 {
		t.Fatalf("each class resolves independently, got %d", got)
	}
	// The summary rides on neither: raising a body budget must not buy a longer title.
	if got := BudgetForKind("summary", SummaryBudget); got != SummaryBudget {
		t.Fatalf("the summary must ignore the body overrides, got %d", got)
	}
}

// Telling a 160-word comment to carry fewer section headers is the same misfire as handing it the
// defect template, so the nag is scoped to the issue class alongside the advice.
func TestHeaderNagIsIssueClassOnly(t *testing.T) {
	body := "## One\n\n## Two\n\n" + strings.TrimSpace(strings.Repeat("word ", 130))
	_, issueReason := Evaluate(body, "issue description", 100)
	if !strings.Contains(issueReason, "section header") {
		t.Fatalf("an over-budget issue keeps the header nag: %q", issueReason)
	}
	_, commentReason := Evaluate(body, "comment", 100)
	if strings.Contains(commentReason, "section header") {
		t.Fatalf("a comment must not be told to drop its sections: %q", commentReason)
	}
}

// Jira states the issue type in the call, so an Epic and a Bug can be told different things without
// anything inferring genre. Measured on 2026-09-01: an Epic holding a project map was denied and
// instructed to fill four defect-report slots.
func TestSubtypeBeatsClassButFallsBack(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[jira.epic.issue]
Name the destination, what is settled, and what is not. No mechanism recap.
`))
	epic, _ := Advice("mcp__atlassian__createJiraIssue", "Epic", "issue description")
	if epic != "Name the destination, what is settled, and what is not. No mechanism recap." {
		t.Fatalf("jira.epic.issue must win for an Epic, got %q", epic)
	}
	// Case-insensitive, since the caller writes the type as Jira displays it.
	if lower, _ := Advice("mcp__atlassian__createJiraIssue", "epic", "issue description"); lower != epic {
		t.Fatalf("subtype matching must not be case-sensitive, got %q", lower)
	}
	// A type with no section of its own falls back to the class, not to nothing.
	if bug, _ := Advice("mcp__atlassian__createJiraIssue", "Bug", "issue description"); !strings.Contains(bug, "Four slots") {
		t.Fatalf("an unkeyed type falls back to jira.issue, got %q", bug)
	}
	// A subtype must not resurrect issue advice for a comment.
	if c, _ := Advice("mcp__atlassian__addCommentToJiraIssue", "Epic", "comment"); c != "" {
		t.Fatalf("a comment stays advice-free whatever the subtype, got %q", c)
	}
}

func TestTrimNoteDropsLineTailsThenBoundsTheWhole(t *testing.T) {
	long := "[flip] " + strings.Repeat("rationale ", 40)
	if got := trimNote(long); len(got) > maxSiblingLine+len("…") {
		t.Fatalf("a long line must lose its tail, got %d bytes", len(got))
	}
	if got := trimNote(long); !strings.HasPrefix(got, "[flip] ") {
		t.Fatalf("the leading identifier is the part worth keeping, got %q", got[:20])
	}

	// Distinct findings, enough of them to exceed the cap: the note is bounded and says so. They
	// have to differ, since identical lines collapse before the cap is ever reached.
	var distinct strings.Builder
	for i := 0; i < 40; i++ {
		fmt.Fprintf(&distinct, "[rule%02d] %s\n", i, strings.Repeat("rationale ", 40))
	}
	got := trimNote(distinct.String())
	if len(got) > maxSiblingNote+128 {
		t.Fatalf("the note must be bounded, got %d bytes", len(got))
	}
	if !strings.Contains(got, "further findings not shown") {
		t.Fatalf("a truncated note must be distinguishable from a short one: %q", got)
	}
}

// Capping without deduping inverts the priority: a rule that fired nine times states its rationale
// nine times, fills the budget, and the cap then drops the findings the writer has not already been
// told about. Measured on 2026-09-01 against a real Epic — paragraph_uniformity and short_close were
// the two that went.
func TestTrimNoteCollapsesRepeatsSoDistinctFindingsSurvive(t *testing.T) {
	rule := "  [flip] " + strings.Repeat("rationale ", 40)
	note := "description: 11 violation(s)\n" +
		strings.Repeat(rule+"\n", 9) +
		"  [paragraph_uniformity] lengths too even\n" +
		"  [short_close] the close is one or two sentences\n"

	got := trimNote(note)
	for _, want := range []string{"paragraph_uniformity", "short_close"} {
		if !strings.Contains(got, want) {
			t.Fatalf("%s must survive nine repeats of another rule:\n%s", want, got)
		}
	}
	if n := strings.Count(got, "[flip]"); n != 1 {
		t.Fatalf("the repeated rule must appear once, got %d times", n)
	}
	if !strings.Contains(got, "×9") {
		t.Fatalf("the collapsed line must carry its count: %q", got)
	}
	if strings.Contains(got, "further findings not shown") {
		t.Fatalf("deduping should have brought this under the cap without truncating:\n%s", got)
	}
}

// A byte-offset slice through an em-dash produces mojibake in the text Claude is asked to act on.
func TestTrimNoteNeverSplitsARune(t *testing.T) {
	for _, s := range []string{
		strings.Repeat("—", 400),
		strings.Repeat("a—b ", 200),
		"[flip] " + strings.Repeat("…verdict— ", 60),
	} {
		got := trimNote(s)
		if !utf8.ValidString(got) {
			t.Fatalf("trimNote produced invalid UTF-8 from %d bytes of input", len(s))
		}
	}
}

// Both siblings read the synthetic Linear payload and echo it back. Naming the real tool in the
// payload is not an option — cope returns no verdict for a tool name it does not know — so the note
// is corrected afterwards.
func TestRelabelCorrectsTheDestination(t *testing.T) {
	copeNote := "cope scored the prose this mcp__linear__save_issue call is about to post, in the external lane"
	got := Relabel(copeNote, "mcp__atlassian__createJiraIssue", "issue description")
	if strings.Contains(got, "mcp__linear__save_issue") {
		t.Fatalf("the synthetic tool name must not survive: %q", got)
	}
	if !strings.Contains(got, "mcp__atlassian__createJiraIssue") {
		t.Fatalf("the real tool name must appear: %q", got)
	}

	basNote := "basanite — words you lean on, in the text about to be written to Linear (awareness, not prohibition)"
	if got := Relabel(basNote, "mcp__atlassian__addCommentToJiraIssue", "comment"); !strings.Contains(got, "written to Jira") {
		t.Fatalf("basanite's hardcoded label must be corrected: %q", got)
	}
	if got := Relabel(basNote, "github", "issue comment"); !strings.Contains(got, "written to GitHub") {
		t.Fatalf("gh-write bridges the same way: %q", got)
	}

	// A real Linear write is already accurate and must be left exactly as it came.
	if got := Relabel(copeNote, "mcp__linear__save_issue", "issue description"); got != copeNote {
		t.Fatalf("a Linear note must pass through untouched: %q", got)
	}
	if got := Relabel("", "mcp__atlassian__createJiraIssue", "comment"); got != "" {
		t.Fatalf("an empty note stays empty, got %q", got)
	}
}

// A label is finer than a type — a wayfinder:map Epic and a plain Epic are different documents —
// so it is tried first. Both are fields the caller filled in before the hook ran, which is what
// makes either usable; TICKETVOICE_TEMPLATE is the only mechanism that could reach an unstated
// genre, and being session-wide it reaches nothing in practice.
func TestLabelKeyBeatsSubtype(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[jira.label:wayfinder:map.issue]
Destination, settled, not yet specified, out of scope.

[jira.epic.issue]
A map, not a plan.
`))
	tool, kind := "mcp__atlassian__createJiraIssue", "issue description"

	if got, _ := Advice(tool, "Epic", kind, "wayfinder:map"); got != "Destination, settled, not yet specified, out of scope." {
		t.Fatalf("the label must beat the subtype, got %q", got)
	}
	// Verbatim apart from case: the section is the label as Jira carries it, separator and all.
	if got, _ := Advice(tool, "Epic", kind, "Wayfinder:Map"); !strings.HasPrefix(got, "Destination,") {
		t.Fatalf("label matching must not be case-sensitive, got %q", got)
	}
	// An unkeyed label costs one lookup and falls to the subtype.
	if got, _ := Advice(tool, "Epic", kind, "enhancement"); got != "A map, not a plan." {
		t.Fatalf("an unkeyed label falls through to the subtype, got %q", got)
	}
	// Several labels are tried in the order the call carries them.
	if got, _ := Advice(tool, "Epic", kind, "enhancement", "wayfinder:map"); !strings.HasPrefix(got, "Destination,") {
		t.Fatalf("every label is tried, got %q", got)
	}
	// A label cannot resurrect issue advice for a comment.
	if got, _ := Advice("mcp__atlassian__addCommentToJiraIssue", "", "comment", "wayfinder:map"); got != "" {
		t.Fatalf("a comment stays advice-free whatever the labels, got %q", got)
	}
	// No labels at all is the pre-existing path, unchanged.
	if got, _ := Advice(tool, "Epic", kind); got != "A map, not a plan." {
		t.Fatalf("a call with no labels resolves by subtype, got %q", got)
	}
}

// A label named for an issue type must not be mistaken for one. The "label:" prefix is what keeps
// the two namespaces apart.
func TestLabelNamespaceDoesNotCollideWithSubtype(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[jira.bug.issue]
Reached by issue type.

[jira.label:bug.issue]
Reached by label.
`))
	tool, kind := "mcp__atlassian__createJiraIssue", "issue description"
	if got, _ := Advice(tool, "Task", kind, "bug"); got != "Reached by label." {
		t.Fatalf("a `bug` label must reach the label section, got %q", got)
	}
	if got, _ := Advice(tool, "Bug", kind); got != "Reached by issue type." {
		t.Fatalf("a Bug type must reach the type section, got %q", got)
	}
}

// The failure this keying exists for: a description-only editJiraIssue carries no issueTypeName —
// the schema has none — and no labels, since labels ride along only when the caller means to write
// them. Measured on 2026-09-01, such an edit against an Epic got the four-slot defect template.
func TestEditGetsNoDefectTemplateByDefault(t *testing.T) {
	edit, create := "mcp__atlassian__editJiraIssue", "mcp__atlassian__createJiraIssue"

	if got, _ := Advice(edit, "", "issue description"); got != "" {
		t.Fatalf("an unkeyed edit must carry the count alone, got %q", got)
	}
	// A create with nothing stated is still composing something, so it keeps the template.
	if got, _ := Advice(create, "", "issue description"); !strings.Contains(got, "Four slots") {
		t.Fatalf("a create keeps the built-in template, got %q", got)
	}
	if Operation(edit) != "edit" || Operation(create) != "" {
		t.Fatalf("Operation: edit=%q create=%q", Operation(edit), Operation(create))
	}
}

// An edit's own section, and the precedence around it: what the thing is beats what is being done
// to it, and both beat the bare class.
func TestEditKeyPrecedence(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[jira.op:edit.issue]
You are revising a document that already has a shape. Cut, do not restructure.

[jira.label:wayfinder:map.issue]
A map, and only a map.

[jira.issue]
Generic issue advice.
`))
	edit, kind := "mcp__atlassian__editJiraIssue", "issue description"

	if got, _ := Advice(edit, "", kind); !strings.HasPrefix(got, "You are revising") {
		t.Fatalf("an edit must reach its own section, got %q", got)
	}
	// A label says what the document is, which is the more useful thing to be told.
	if got, _ := Advice(edit, "", kind, "wayfinder:map"); got != "A map, and only a map." {
		t.Fatalf("a label must beat the operation, got %q", got)
	}
	// The operation still beats the bare class.
	if got, _ := Advice(edit, "", kind, "unkeyed-label"); !strings.HasPrefix(got, "You are revising") {
		t.Fatalf("the operation must beat jira.issue, got %q", got)
	}
	// A create is unaffected by the edit section.
	if got, _ := Advice("mcp__atlassian__createJiraIssue", "", kind); got != "Generic issue advice." {
		t.Fatalf("a create must not pick up the edit section, got %q", got)
	}
	// A comment stays advice-free whatever the operation.
	if got, _ := Advice(edit, "", "comment"); got != "" {
		t.Fatalf("the edit key must not resurrect advice for a comment, got %q", got)
	}
}
