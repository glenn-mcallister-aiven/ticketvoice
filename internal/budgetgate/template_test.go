package budgetgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			advice, _ := Advice(tool, kind)
			if advice != "" {
				t.Fatalf("Advice(%q, %q) = %q, want none", tool, kind, advice)
			}
		}
	}
}

func TestIssueClassCarriesTheBuiltinTemplate(t *testing.T) {
	advice, note := Advice("mcp__atlassian__createJiraIssue", "issue description")
	if !strings.Contains(advice, "Four slots") {
		t.Fatalf("an issue denial must carry the built-in template, got %q", advice)
	}
	if note != "" {
		t.Fatalf("no template file was named, so there is nothing to note: %q", note)
	}
}

func TestSummaryClassCarriesNoAdvice(t *testing.T) {
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "summary"); advice != "" {
		t.Fatalf("a summary carries its own one-line reason, not advice: %q", advice)
	}
}

// An operator file overrides one key and leaves the rest of the table standing — the reason
// resolution consults the file and the built-in map at each key rather than choosing one source.
func TestOperatorFileOverridesOneKeyOnly(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[jira.comment]
One finding per comment. Lead with what changed.
`))
	if advice, _ := Advice("mcp__atlassian__addCommentToJiraIssue", "comment"); advice != "One finding per comment. Lead with what changed." {
		t.Fatalf("the file's jira.comment must win, got %q", advice)
	}
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "issue description"); !strings.Contains(advice, "Four slots") {
		t.Fatalf("jira.issue was not overridden, so the built-in stands, got %q", advice)
	}
	if advice, _ := Advice("mcp__linear__save_comment", "comment"); advice != "" {
		t.Fatalf("a jira key must not apply to linear, got %q", advice)
	}
}

// A section declared empty says "this vendor and kind get the count alone", which is different from
// omitting it and inheriting the built-in.
func TestEmptySectionSuppressesTheBuiltin(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, "[jira.issue]\n"))
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "issue description"); advice != "" {
		t.Fatalf("an empty jira.issue section must suppress the built-in, got %q", advice)
	}
}

// The only handle on genre, and it is the operator's, not an inference.
func TestTicketvoiceTemplateSelectsByName(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t, `[progress-update]
State what moved, what is blocked, and what you need.
`))
	t.Setenv("TICKETVOICE_TEMPLATE", "progress-update")
	advice, _ := Advice("mcp__atlassian__createJiraIssue", "issue description")
	if advice != "State what moved, what is blocked, and what you need." {
		t.Fatalf("the named template must beat vendor.class, got %q", advice)
	}
	// A name with no section falls back rather than silently emptying the advice.
	t.Setenv("TICKETVOICE_TEMPLATE", "no-such-template")
	if advice, _ := Advice("mcp__atlassian__createJiraIssue", "issue description"); !strings.Contains(advice, "Four slots") {
		t.Fatalf("an unknown name must fall through to vendor.class, got %q", advice)
	}
}

// An operator who set the variable expects it to work, so a file that is too big to send is
// reported in the reason rather than dropped in silence — the one place this package is not silent
// on an abnormal path.
func TestOversizedTemplateFileLoadsNothingAndSaysSo(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", writeTemplateFile(t,
		"[jira.issue]\n"+strings.Repeat("x ", maxTemplateFile)))
	advice, note := Advice("mcp__atlassian__createJiraIssue", "issue description")
	if !strings.Contains(advice, "Four slots") {
		t.Fatalf("an unusable file leaves the built-in table standing, got %q", advice)
	}
	if !strings.Contains(note, "over the") {
		t.Fatalf("the note must say the file was skipped for size, got %q", note)
	}
}

func TestMissingTemplateFileIsReportedNotFatal(t *testing.T) {
	t.Setenv("TICKETVOICE_TEMPLATE_FILE", filepath.Join(t.TempDir(), "does-not-exist"))
	advice, note := Advice("mcp__atlassian__createJiraIssue", "issue description")
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
