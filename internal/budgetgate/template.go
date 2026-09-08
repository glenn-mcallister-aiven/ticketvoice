// Shape advice, separated from the budget verdict that used to carry it.
//
// The hook printed one four-slot defect-report template on every denial, whatever was being written.
// That is an assertion about genre it has no way to justify: a progress update, a triage record and
// a verification log are all legitimate comment bodies, and told to fill four defect slots, Claude
// restructures them into something nobody asked for. Measured on 2026-08-31: a 1,246-word SRE
// progress update denied and instructed to become a defect report.
//
// So the verdict states the count and the cap, which is never wrong, and shape advice prints only
// where a template is configured for that vendor and kind. The built-in table deliberately leaves
// the comment classes empty — an over-long comment is told it is over long, and nothing else.
// Nothing here infers genre. The only handle on genre is TICKETVOICE_TEMPLATE, which is the
// operator naming a template for a session, not the hook guessing.
package budgetgate

import (
	"fmt"
	"os"
	"strings"
)

// maxTemplateFile caps TICKETVOICE_TEMPLATE_FILE. Whatever it holds reaches Claude's context on
// every denial, so an unbounded file would spend the model's window on shape advice. 4 KiB holds
// roughly seven blocks the size of the built-in issue template.
const maxTemplateFile = 4 << 10

// builtinAdvice is the compiled-in table, so a fresh install gates with advice rather than none.
// Every key carries a class, and there is no class-less fallback on purpose: a `[jira]` catch-all
// would hand the issue template to a Jira comment, which is precisely the failure this file exists
// to remove. The comment and summary classes are present and empty — found, so the lookup stops
// there, rather than absent and falling through to the issue advice. A summary denial carries its
// own one-line reason instead (EvaluateSummary).
//
// No vendor differs from the default yet. Vendor keys are what an operator file supplies.
//
// The edit entry is empty for the same reason the comment one is. An edit revises a document that
// already has a shape, so the writer is not composing a defect report and telling them to is wrong
// for every edit rather than only for an Epic's — and an edit is the least-informed call this hook
// sees, since editJiraIssue has no issueTypeName in its schema and labels ride along only when the
// caller means to write them.
var builtinAdvice = map[string]string{
	"default.issue":         slots,
	"default.comment":       "",
	"default.summary":       "",
	"default.op:edit.issue": "",
}

const slots = `Four slots, in this order:
  1. The mechanism, one paragraph — what is broken, and why nothing catches it.
  2. Evidence it is real — a SHA, a log line, a failing assertion. One sentence.
  3. What is still exposed — file:line, not a description of the file.
  4. The fix, as a code block, plus one line on how to prove it can go red.
SHAs and file:line carry the detail; do not narrate what the reader can open.`

// Method strips the MCP server segment off a hook tool name, so "mcp__atlassian__editJiraIssue"
// and "mcp__claude_ai_Atlassian_Rovo__editJiraIssue" both read as "editJiraIssue". The server
// segment is whatever the user named it in `claude mcp add`, or whatever the claude.ai connector
// is called this month — measured on 2026-09-08, the Atlassian connector surfaced as
// `claude_ai_Atlassian_Rovo`, and a hook keyed on `mcp__atlassian__` let every Jira write through
// unchecked. The method name is the one part the MCP server itself controls, so it is the only
// stable thing to key on. A name with no MCP prefix (Bash, github) comes back as is.
func Method(tool string) string {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return tool
	}
	if _, m, ok := strings.Cut(rest, "__"); ok {
		return m
	}
	return tool
}

// The methods each tracker's MCP exposes that carry prose. Membership here, not the server
// segment, is what makes a call a Linear or a Jira write.
var (
	linearMethods = map[string]bool{
		"save_issue": true, "save_comment": true, "save_diff_comment": true, "submit_diff_review": true,
	}
	jiraMethods = map[string]bool{
		"createJiraIssue": true, "editJiraIssue": true,
		"addCommentToJiraIssue": true, "addWorklogToJiraIssue": true,
	}
)

// Vendor maps a calling tool to the tracker whose conventions apply. A Bash call reaching this
// package is a gh-write invocation, which is the GitHub surface.
func Vendor(tool string) string {
	m := Method(tool)
	switch {
	case linearMethods[m]:
		return "linear"
	case jiraMethods[m]:
		return "jira"
	case tool == "Bash", tool == "github":
		return "github"
	}
	return ""
}

// Operation is what a call does to something that already exists, where the tool name says so.
// Derived from the tool rather than passed in, because the tool name is where the fact lives.
//
// Measured on 2026-09-01: a description-only editJiraIssue against an Epic resolved straight to the
// four-slot defect template, having neither of the two signals the keying was built around. Linear's
// patch-based save_issue is the same shape of operation but its tool name does not distinguish one
// from a full-content write, so it is not keyed here.
func Operation(tool string) string {
	if Method(tool) == "editJiraIssue" {
		return "edit"
	}
	return ""
}

// KindClass collapses the per-tool kind labels ("worklog comment", "PR description", "diff review"
// and the rest) into the three classes that carry a budget and a template. The HasSuffix test on
// "comment" is the same one LinearPayload uses to route a payload, kept in step deliberately.
func KindClass(kind string) string {
	switch {
	case kind == "summary":
		return "summary"
	case strings.HasSuffix(kind, "comment"), kind == "diff review":
		return "comment"
	}
	return "issue"
}

// operatorAdvice parses TICKETVOICE_TEMPLATE_FILE. Read on each call rather than memoized: a
// denial is rare and the file is capped at 4 KiB, so caching buys nothing measurable and costs the
// ability to test two configurations in one process.
//
// note is non-empty when a file was named but not used, which is the one condition worth
// surfacing — an operator who set the variable expects it to work, and a hook has no channel to
// say otherwise except the reason text it was already about to print.
func operatorAdvice() (map[string]string, string) {
	path := os.Getenv("TICKETVOICE_TEMPLATE_FILE")
	if path == "" {
		return nil, ""
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Sprintf("Template file %s could not be read, so no shape advice was loaded.", path)
	}
	if fi.Size() > maxTemplateFile {
		return nil, fmt.Sprintf("Template file %s is %d bytes, over the %d-byte cap, so no shape advice was loaded.",
			path, fi.Size(), maxTemplateFile)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Sprintf("Template file %s could not be read, so no shape advice was loaded.", path)
	}
	return parseTemplates(string(data)), ""
}

// parseTemplates reads the headed-section format: a `[name]` line opens a section and its body runs
// to the next such line. Chosen over JSON because the whole value of an operator file is editing
// prose by hand, and multi-line prose in JSON is a wall of escaped newlines. A section may be
// declared empty, which is a way to say "this vendor and kind get the count alone" — distinct from
// omitting it, which falls through to the built-in.
func parseTemplates(s string) map[string]string {
	out := map[string]string{}
	name := ""
	var body []string
	flush := func() {
		if name != "" {
			out[name] = strings.TrimSpace(strings.Join(body, "\n"))
		}
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") && len(t) > 2 {
			flush()
			name, body = strings.ToLower(strings.TrimSpace(t[1:len(t)-1])), nil
			continue
		}
		if name != "" {
			body = append(body, line)
		}
	}
	flush()
	return out
}

// Advice returns the shape advice for one write, and a note about a template file that was named but
// not loaded. Resolution runs most specific to least: TICKETVOICE_TEMPLATE names a section outright,
// then "<vendor>.<subtype>.<class>", then "<vendor>.<class>", then "default.<class>" — each looked up
// in the operator's file before the built-in table, so a file can override one key without restating
// the rest. Every key carries the class, so no lookup can end up handing issue advice to a comment.
//
// subtype is the tracker's own name for what is being written, where the call states one: Jira's
// `issueTypeName`, so an Epic and a Bug can be told different things. labels are the call's labels,
// keyed "label:<label>" ahead of the subtype because a label is the finer signal — a `wayfinder:map`
// Epic and a plain Epic are different documents, and the type alone cannot say so.
//
// Neither is the hook guessing at genre. Both are fields the caller filled in before the hook ran,
// which is the whole reason they are usable: the one mechanism that could reach a genre the call
// does not state is TICKETVOICE_TEMPLATE, and that is session-wide, so in practice it reaches
// nothing. Measured on 2026-09-01, an Epic holding a project map was denied and instructed to fill
// four defect-report slots, which is what a class-only key cannot distinguish.
//
// Labels are matched verbatim apart from case, so a Jira label of `wayfinder:map` is the section
// `[jira.label:wayfinder:map.issue]`. Keys are constructed and looked up whole, never parsed, so a
// label carrying the separator is unambiguous rather than merely tolerated.
//
// "op:<operation>" sits below both, above the bare class: what the call does to an existing thing
// matters less than what that thing is, but more than the class alone. See Operation.
//
// An empty result is an outcome, not a failure: the denial states the count and stops there.
func Advice(tool, subtype, kind string, labels ...string) (advice, note string) {
	ops, note := operatorAdvice()
	keys := []string{}
	if name := strings.ToLower(strings.TrimSpace(os.Getenv("TICKETVOICE_TEMPLATE"))); name != "" {
		keys = append(keys, name)
	}
	class, op := KindClass(kind), Operation(tool)
	if vendor := Vendor(tool); vendor != "" {
		for _, l := range labels {
			if l = strings.ToLower(strings.TrimSpace(l)); l != "" {
				keys = append(keys, vendor+".label:"+l+"."+class)
			}
		}
		if sub := strings.ToLower(strings.TrimSpace(subtype)); sub != "" {
			keys = append(keys, vendor+"."+sub+"."+class)
		}
		if op != "" {
			keys = append(keys, vendor+".op:"+op+"."+class)
		}
		keys = append(keys, vendor+"."+class)
	}
	if op != "" {
		keys = append(keys, "default.op:"+op+"."+class)
	}
	keys = append(keys, "default."+class)

	for _, k := range keys {
		if v, ok := ops[k]; ok {
			return v, note
		}
		if v, ok := builtinAdvice[k]; ok {
			return v, note
		}
	}
	return "", note
}
