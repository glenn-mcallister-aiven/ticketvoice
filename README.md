# ticketvoice

[![ci](https://github.com/justinstimatze/ticketvoice/actions/workflows/ci.yml/badge.svg)](https://github.com/justinstimatze/ticketvoice/actions/workflows/ci.yml)

A Claude Code `PreToolUse` hook that gates ticket prose through
[cope](https://github.com/justinstimatze/cope) (voicing and structure) and
[basanite](https://github.com/justinstimatze/basanite) (vocabulary tics) before it posts. Behind
both sits a word budget — 200 words for an issue or PR description, 120 for a comment, 20 for a Jira
summary, fenced code excluded — as a narrower backstop: neither cope nor basanite is built to score
sheer length, independent of register or vocabulary. Any of the checks flagging a body returns
`permissionDecision: "deny"` — the reason goes to Claude, not a human, so it rewrites and retries on
its own instead of paging anyone — **or, on a Linear write, ticketvoice fixes it itself first: see
[Auto-rewrite](#auto-rewrite), on by default and the one check here that spends real money without
being asked.** No prompt when a body clears every check — on Linear and Jira it still tags the body
as agent-authored before letting it through; see [Agent tag](#agent-tag).

Two more checks, both first-party (built here, not delegated to a sibling binary): an issue
description must state its user-facing impact in plain language — see [Impact
line](#impact-line) — and any ticket-id, file:line, or commit SHA a ticket cites gets verified
against Linear, the local filesystem, and the local git repo, not trusted at face value — see
[Ground-truth citations](#ground-truth-citations).

Covers three surfaces: Linear, via its MCP tools' structured `description`/`body` fields — issues,
comments, and PR-review-thread ("diff") comments and reviews; Jira, via the Atlassian MCP's
`summary`, `description` and `commentBody` — issues, comments and worklog comments, with the title
budgeted apart from the body; and GitHub issues/PRs, via
[`gh-write`](#gh-write-github-issues-and-prs) — a thin wrapper this repo also builds, which is the
only way this hook can see a GitHub body at all (see that section for why a plain
`gh issue create --body "..."` can't be gated).

Deliberately not covered on the Linear side: project/initiative descriptions, status updates,
documents, milestones, release notes. Same reasoning as GitHub's release notes below — a different
genre of writing than a ticket, not a gap that was missed.

## Why

A memory saying "write like a pragmatic staff engineer" held, and ticket bodies still ran long
anyway — a recurring habit, not a one-off: a memory is a taste, and a taste can be talked past
mid-generation without ever registering as a violation. cope replaces the taste with an actual read
of the prose — the same voicing and structure check a human reviewer would run, just automatic.
basanite adds the vocabulary-tic layer a voicing check alone misses.

Neither one judges length on its own. That's the one habit they don't catch, and the budget below
is what holds the line on it instead.

## What the denial looks like

Claude sees this as the reason the write was blocked — nothing is shown to you unless Claude
surfaces it in chat on its own. Over budget:

```
This issue description is 238 words of prose against a 150-word budget — 88 over.

Four slots, in this order:
  1. The mechanism, one paragraph — what is broken, and why nothing catches it.
  2. Evidence it is real — a SHA, a log line, a failing assertion. One sentence.
  3. What is still exposed — file:line, not a description of the file.
  4. The fix, as a code block, plus one line on how to prove it can go red.
SHAs and file:line carry the detail; do not narrate what the reader can open.

Revise it and call again now — asking the operator to do the rewrite is the failure this reason
exists to prevent.
```

Inside budget but flagged by cope or basanite:

```
This comment is inside the 120-word budget, but a sibling scorer flagged it on the way out.

cope flagged this:

clause_symmetry: 1 violation(s)

Revise it and call again now — asking the operator to do the rewrite is the failure this reason
exists to prevent.
```

## When a deny repeats

A deny that comes back unchanged three times in a row is a loop, not a gate — the model rewrote
blind because nothing told it whether the rewrite helped. A flagged-but-in-budget write tracks that
in `internal/attemptstate`, one small state file per session, tool, and prose kind, since a fresh
hook process has no memory of its own between calls:

- **Attempt 1** denies as above.
- **Attempt 2** denies again, and names what changed since attempt 1 — cleared, still flagged, or
  newly flagged, by cope or basanite's own rule or word id.
- **Attempt 3**, if that set hasn't shrunk since attempt 2, lets the write through instead of
  denying a fourth time, with `additionalContext` naming what's still flagged rather than going
  quiet about it. A set that did shrink denies again, and the same check runs at attempt 4.

Being over budget is exempt from all of this: it always denies, at any attempt count, since it's
meant to be a hard limit, not a register a rewrite can talk its way past.

gh-write's own backstop (below) never gets this: it only ever sees the body on its stdin, never a
session id, so it has nothing to key a retry sequence on — every gh-write refusal stays attempt 1.

`TICKETVOICE_STATE_DIR` overrides where the attempt state lives; see
[Configuration](#configuration).

## Auto-rewrite

**On by default, and it spends real money the moment a Linear write is flagged** — this is the one
check in this whole tool that makes a paid API call automatically, without you asking for it, per
write. Read this section before assuming a flagged write is free.

The deny-and-retry loop above depends on whichever Claude session made the write choosing to
rewrite and resubmit on its own — reliable most of the time, not always: sometimes the calling
session asks the operator to fix the ticket by hand instead of retrying, which defeats the point of
routing the reason back to the model at all. For the categories a rewrite can actually fix — a
`cope`- or `basanite`-flagged voice/vocabulary issue, or over-budget length — ticketvoice fixes the
text itself, inside the hook, before ever denying:

1. One bounded call to `claude-sonnet-5` (`TICKETVOICE_REWRITE_MODEL` overrides), given the original
   text and the specific violation(s) it was flagged for.
2. The candidate is re-validated against **every** check the original text went through — not just
   the ones that triggered the rewrite, since a rewrite that trims a paragraph could just as easily
   mangle a citation or delete an impact line that was there.
3. Only if the candidate comes back fully clean does the write proceed — `allow`, with the
   rewritten, 🤖-tagged text substituted in via `updatedInput`. Nothing is shown to you, and nothing
   asks the calling Claude session to do anything.

If the rewrite call fails, times out, or the candidate still doesn't pass every check, the write
falls through to today's deny-and-retry behavior exactly as if auto-rewrite didn't exist — same
reason text, same attempt-state bookkeeping.

**Deliberately excluded, not an oversight:**
- **Ground-truth citation violations** ([below](#ground-truth-citations)) — a nonexistent ticket id,
  file, or SHA has no correct rewrite to guess. Still denies and retries as before.
- **A missing impact line** ([below](#impact-line)) — inventing a user-facing impact claim risks
  shipping a fabricated fact into a ticket a PM/exec and a release-notes-generating agent both read
  at face value. Still denies and retries as before.
- **`Bash`/`gh-write` writes** — that surface fails synchronously in the same turn the calling agent
  sees it (`gh-write`'s own exit-nonzero path), not a hook deny that can drift into asking a human;
  there's also no single structured field a rewrite could be substituted into on a shell command.
- **A patch** (editing an existing description) — same reason: no single field to substitute a
  rewrite into.
- **Jira writes** — not extended to Jira yet. The rewriter and its re-validation take one field,
  and a Jira create or edit carries two (summary and body). Still denies and retries as before.

Requires `ANTHROPIC_API_KEY` resolvable — see [Configuration](#configuration) for how. Unset, this
whole feature skips and every flagged Linear write falls straight back to deny-and-retry.
`TICKETVOICE_NO_AUTOREWRITE` turns it off explicitly, even with a key present.

## Impact line

An issue description (not a comment — a comment isn't the ticket) must say what a customer or
exec would notice, in plain language, or say plainly that nobody would:

```
Impact: users on the map page see load times drop from ~4s to under 1s.
```

```
Impact: none — internal maintenance, no user-facing change.
```

Missing entirely denies:

```
This issue description has no impact line. Add one, in plain language a PM or exec would understand:

Impact: users on the map page see load times drop from ~4s to under 1s.

or, for work nobody outside engineering would notice:

Impact: none — internal maintenance, no user-facing change.

Revise it and call again now — asking the operator to do the rewrite is the failure this reason
exists to prevent.
```

This never checks whether the stated impact is *true* — that's a narrative judgment call, out of
scope for a fast synchronous hook (see [Ground-truth citations](#ground-truth-citations) for the
line drawn the other way, on citations). It also deliberately doesn't ask a ticket to restate
which project or initiative it belongs to — that's already a native, structured Linear field
(`project`/`projectMilestone`), visible in Linear's own UI and readable directly by anything that
wants it, including whatever turns Linear search results into release notes. Impact has no such
field, which is the only reason it's worth requiring here at all.

`TICKETVOICE_NO_IMPACT_CHECK` turns this off.

## Ground-truth citations

A ticket citing another ticket id, a `file:line`, or a commit SHA is verified, not trusted at face
value. Each check fails open on anything it can't determine and flags only a citation it can
positively confirm is wrong:

- **Ticket ids** (`ABC-550`-shaped) are checked against Linear directly. Ordinary jargon that
  matches the same shape — `UTF-8`, `SHA-256`, `GPT-4`, `RFC-2119`, `COVID-19` — never reaches
  Linear at all: a citation only counts as a candidate if its letter prefix matches one of the
  workspace's real team keys, fetched and cached once a day. Up to `TICKETVOICE_LINEAR_CITE_CAP`
  (default 5) distinct ids are checked per call.
- **`` `path/to/file.go:123` `` or `` `path/to/file.go:100-200` ``** must exist, at that line
  count, relative to the call's `cwd`. A path with no extension (`Makefile`, `Dockerfile`) isn't
  matched — a known v1 gap, not a silent misread.
- **`` `<sha>` ``** (7-40 hex characters) must be a real commit in the repo at `cwd`. Requires
  `git` and a real repository; skipped entirely otherwise.

```
ticket reference(s) don't exist: ABC-99999999
nope.go doesn't exist
`deadbeefcafe` isn't a commit in this repo
```

This stops at existence, not truth. Verifying a *narrative* claim — "already fixed in ABC-777,"
"the file already exists" — needs real code-reading judgment, which means an LLM call, not a fast
deterministic check; that's out of scope for a synchronous `PreToolUse` hook. Checking that a
cited id, path, or SHA is real needs no judgment at all, which is what keeps it in scope.

Unlike the impact line and the escalation in [When a deny repeats](#when-a-deny-repeats), a
citation check is exempt from the 3-attempt escalation: a nonexistent SHA doesn't become real by
attempt 3, so this always denies regardless of attempt count, the same as being over budget.

Requires `TICKETVOICE_LINEAR_TOKEN` for the ticket-id check only — unset, that one check skips
(file:line and SHA checks don't need Linear at all). `TICKETVOICE_NO_CITATION_CHECK` turns off all
three.

## Install

```bash
git clone https://github.com/justinstimatze/ticketvoice
cd ticketvoice
make install   # builds ticketvoice and gh-write to $GOBIN, or $(go env GOPATH)/bin, version from git describe
```

`make install` follows `go install`: `GOBIN` when set — an environment variable, `go env -w
GOBIN=...`, or `make install GOBIN=...` — and `GOPATH/bin` otherwise. Pick a directory that is on
`PATH`. `gh-write` has to be: it is invoked as a bare command in the Bash call the hook is watching,
so a `gh-write` the shell cannot resolve is a body that reaches `gh` ungated. `ticketvoice` itself
does not care, since the hook runs it by absolute path.

Then wire `ticketvoice` into `~/.claude/settings.json` as a `PreToolUse` hook on the four Linear
write tools, the four Jira ones, and on `Bash` (for `gh-write` calls — see below). The path has to
be absolute — hooks run in whatever environment Claude Code was launched from, which may not have
your Go bin directory on `PATH`:

```json
{
  "hooks": {
    "PreToolUse": [
      {
        "matcher": "mcp__.*__(save_issue|save_comment|save_diff_comment|submit_diff_review|linear_createIssue|linear_updateIssue|linear_createComment|linear_updateComment|createJiraIssue|editJiraIssue|addCommentToJiraIssue|addWorklogToJiraIssue)|Bash",
        "hooks": [
          { "type": "command", "command": "/home/you/go/bin/ticketvoice" }
        ]
      }
    ]
  }
}
```

Drop whichever tracker you don't use from the matcher. The Jira tools left out are the ones that
carry no prose — `transitionJiraIssue`, `createIssueLink` and the rest — where the hook would have
nothing to check.

The `mcp__.*__` prefix matches any server name, which is whatever you called the server in
`claude mcp add` — or whatever the claude.ai connector calls itself, which for Atlassian is
currently `claude_ai_Atlassian_Rovo`. The binary keys on the method after the last `__` the same
way, so any server name reaches the same checks.

There's no installer subcommand — this is a plain hook binary, wired by hand once. Matching on
`Bash` runs ticketvoice on every Bash call, but it's a fast string check that returns immediately
for anything that isn't a `gh-write` call or a raw `gh` write — see [Development](#development)
for the cost.

## gh-write: GitHub issues and PRs

`gh-write` is a companion binary this repo also builds — `gh issue`/`gh pr`, but the body always
comes from stdin instead of a `--body`/`--body-file` flag:

```bash
gh-write issue create --title "Bug: X" --repo you/repo <<'EOF'
Whatever the body is. No shell escaping to think about — it's a heredoc, not a quoted argument.
EOF

gh-write pr comment 42 <<'EOF'
lgtm
EOF

gh-write pr review 42 --approve <<'EOF'
One nit in the retry loop, fine to land.
EOF

gh-write comment edit 2918375521 --repo you/repo <<'EOF'
The trimmed comment.
EOF
```

`pr review` with no `--approve`, `--comment` or `--request-changes` goes out as `--comment`. `comment
edit` rewrites one existing issue or PR conversation comment by its id, through `gh api`; to edit
your own last comment, `gh-write issue comment 42 --edit-last` also works.

Everything gh-write doesn't recognize (`--repo`, `--label`, `--base`, `--draft`, ...) passes
straight through to `gh`, unchanged. `--body`, `-b`, `--body-file`, `-F`, and their `=value` forms
are refused outright, so a body can only arrive on stdin — as a heredoc, a `< file` redirect, or a
pipe.

Every body gh-write sends is also prefixed with an agent tag — see [Agent tag](#agent-tag).

**A raw `gh` write is denied.** `gh issue|pr create|comment|edit` or `gh pr review` with `--body`,
`-b`, `--body-file` or `-F`, and `gh api` with a `body=` field, are refused at the hook with the
`gh-write` command that carries the same body — `gh-write pr comment 1568 < comment.md` for a
`--body-file`. The flag's presence decides it; the body is never parsed. Only a `gh` in command
position counts, so a commit message or heredoc that mentions one is left alone. If `gh-write`
isn't on `PATH` the hook lets the call through, since there'd be nothing to point at. Not covered:
`gh release --notes`, `gh gist`, and a `gh` call hidden behind `bash -c`, `eval` or a script file.
PR review line comments sent through `gh api` are denied with no `gh-write` form to point at yet.

**Why this exists**, and why it isn't as simple as pointing ticketvoice's matcher at `gh` itself:
ticketvoice reads a Bash `PreToolUse` call's `tool_input.command` — the same opaque shell string
Claude submitted, not a parsed argv. Linear's MCP tools hand it a clean JSON `description`/`body`
field; a raw `gh issue create --title "..." --body "..."` hands it one shell-quoted line, and
`--body`, `--body-file`, `--notes`/`--notes-file` differ across `issue`/`pr`/`release` and
`create`/`comment`/`edit`, each with its own escaping and heredoc/file-path variants. Reliably
pulling prose out of that would need a real shell tokenizer, and a tokenizer that gets it wrong
doesn't fail open the way an unparseable Linear call does — it can match the wrong span (a
`--title` instead of a `--body`) and report a plausible, wrong verdict, which is worse than no
gate at all.

gh-write turns that into a much narrower problem: it owns a single, fixed CLI grammar, so the
only thing ticketvoice has to find is a `gh-write` call in command position followed by a
heredoc or a `< file` redirect — both literal text, no shell escaping to resolve, extractable
without a tokenizer (`ghWriteProse` in `main.go`). Covers issue and PR create/comment/edit, PR
reviews and comment edits,
matching the Linear surface this hook already covers (issues and comments) — not release notes,
which are a different genre (a changelog, not a ticket) that this gate isn't shaped for.

A pipe-sourced body (`cat notes.txt | gh-write issue create ...`) defeats even that: seeing what a
pipe's upstream stage would produce means running it, and a `PreToolUse` hook has no business doing
that. So the hook doesn't try — instead, gh-write runs the same cope/basanite/word-budget check
itself, on the real bytes it just read off its own stdin, before it ever calls `gh`. That check
doesn't care how the body arrived, which means it's the actual backstop for all three forms, not
just the two the hook can see ahead of time. What differs is only when each one catches it: the
hook's `deny` stops the Bash call before it ever runs, while gh-write's own refusal happens after —
gh-write is the thing that call invoked, so by the time it can check, the call has already started.
Either way Claude gets the same reason text and no result but "retry shorter," with nobody paged.

## Configuration

`TICKETVOICE_MAX_ISSUE_WORDS`, `TICKETVOICE_MAX_COMMENT_WORDS` and `TICKETVOICE_MAX_SUMMARY_WORDS`
override the length backstop per class. `TICKETVOICE_MAX_WORDS` still applies to every class as the
fallback, and a class variable beats it. None of them touch cope or basanite's own verdicts.

One knob for every class cannot express the problem. Measured across 291 issues and 278 comments on
one tracker, the two fields drifted by different multiples once agents began filing: description
medians went 129 to 508 words, comment medians 44 to 302. A `TICKETVOICE_MAX_WORDS` raised far enough
to stop denying descriptions retires the comment check entirely.

`TICKETVOICE_TEMPLATE_FILE` and `TICKETVOICE_TEMPLATE` select shape advice — see
[Shape advice](#shape-advice).

`TICKETVOICE_COPE_GATE` and `TICKETVOICE_BASANITE` point at those binaries if they aren't on `PATH`.
Missing or unreachable is not an error for either — the call just isn't scored against that sibling's
rules that time.

`TICKETVOICE_STATE_DIR` overrides where the retry-attempt state from
[When a deny repeats](#when-a-deny-repeats) lives. Unset, it follows the same
`$XDG_STATE_HOME`/`~/.local/state/ticketvoice` convention cope and basanite already use for their
own state.

`TICKETVOICE_NO_AGENT_TAG` turns off the agent tag below. Unset (the default) means tagged.

`TICKETVOICE_LINEAR_TOKEN` is a Linear personal API key (`lin_api_...`), sent raw with no `Bearer`
prefix — the same kind of token an operator already has for Linear's own MCP server, just read
from its own env var so the two never collide. Missing everywhere, the ticket-id half of
[Ground-truth citations](#ground-truth-citations) skips. Resolved the same way hindcast resolves
`ANTHROPIC_API_KEY`: the env var first, then a `.env` file found by walking up from the call's
`cwd`, then a global fallback at `~/.config/ticketvoice/.env` (one `TICKETVOICE_LINEAR_TOKEN=...`
line, `#`-comments allowed, quotes optional) — the global file is what lets one hook wired into
every project's `settings.json` resolve a token regardless of which project's `cwd` it's currently
handling a call for, worktrees included. `TICKETVOICE_LINEAR_ENDPOINT` overrides the GraphQL
endpoint (mainly for tests). `TICKETVOICE_LINEAR_CITE_CAP` (default `5`) caps how many distinct
ticket ids get checked per call.

`TICKETVOICE_NO_IMPACT_CHECK` and `TICKETVOICE_NO_CITATION_CHECK` disable
[Impact line](#impact-line) and [Ground-truth citations](#ground-truth-citations) independently of
each other and of the Linear token.

`ANTHROPIC_API_KEY` (send-real-money — see [Auto-rewrite](#auto-rewrite) before setting this)
resolves the same way `TICKETVOICE_LINEAR_TOKEN` does — env var, then a `.env` walked up from `cwd`,
then the same global `~/.config/ticketvoice/.env` fallback (a second line in that file, alongside
the Linear token). `TICKETVOICE_REWRITE_MODEL` (default `claude-sonnet-5`) and
`TICKETVOICE_ANTHROPIC_ENDPOINT` (mainly for tests) override the model and API endpoint.
`TICKETVOICE_NO_AUTOREWRITE` disables auto-rewrite outright.

## What it counts

Prose only — ticketvoice strips fenced code before counting, since code is the part of a ticket
that's supposed to be long. A patch-based edit is counted on its inserted text alone; it leaves the
rest of the body alone. Below 200 words, a body with section headers gets an extra line in the
reason: headers cost two lines each and imply more document than there is.

A Jira summary is counted against its own 20-word budget, which is measured rather than chosen:
across 100 consecutively filed issues, summary length ran p50 11 words, p90 16, max 28. A cap at the
p90 would have denied six of that hundred, four of which read as correctly sized; 20 denies the two
that put a whole finding in the title. Its denial reason carries the count and the cap and nothing
else — the four-slot body template tells a writer to structure a body, and told that about a title,
Claude puts markdown headers in a Jira summary.

A Jira create and a Jira edit each carry a title and a body under separate budgets, and both are
checked before anything is reported, so a call over on both is denied once with both counts rather
than making Claude discover the second on a retry. A body sent as ADF (`contentFormat: "adf"`) is a
JSON document object rather than a string, and isn't counted: pulling text leaves out of a node tree
isn't worth it while the MCP's default is Markdown. The summary on the same call is still checked.

## Shape advice

A denial states the count and the cap. Shape advice — what the body should look like — is separate,
and prints only where a template covers that tracker and class.

It used to print unconditionally, one four-slot defect-report template on every denial whatever was
being written. That is an assertion about genre the hook has no way to justify: a progress update, a
triage record and a verification log are all legitimate comment bodies. Measured on 2026-08-31, a
1,246-word progress update was denied and instructed to become a defect report.

So the built-in table gives the issue class the four-slot template and gives the comment and summary
classes nothing. An over-long comment is told it is over long, and that is all. Nothing infers
genre; the one handle on genre is an operator's.

`TICKETVOICE_TEMPLATE_FILE` points at a file of headed sections, at most 4 KiB, since whatever it
holds reaches Claude's context on every denial:

```
[jira.comment]
One finding per comment. Lead with what changed since the last one.

[progress-update]
State what moved, what is blocked, and what you need. No mechanism recap.
```

Keys resolve most specific first, and `default.<class>` catches what nothing else does:

```
[<tracker>.label:<label>.<class>]   a label on the call, tried in the order the call carries them
[<tracker>.<subtype>.<class>]       Jira's issueTypeName; only a create carries one
[<tracker>.op:edit.<class>]         an editJiraIssue, whatever it is editing
[<tracker>.<class>]
[default.<class>]
```

An edit gets no advice at all by default. It revises a document that already has a shape, so the
writer is not composing a defect report, and `editJiraIssue` is also the least-informed call this
hook sees: its schema carries no `issueTypeName`, and labels ride along only when the caller means
to write them. A description-only edit of an Epic was denied on 2026-09-01 and handed the four-slot
template on exactly that basis. What the document is still wins over what is being done to it, so a
label beats the operation; the operation beats the bare class.

Tracker is `jira`, `linear` or `github`; class is `issue`, `comment` or `summary`. Labels match
verbatim apart from case, separators included, so a Jira label of `wayfinder:map` is the section
`[jira.label:wayfinder:map.issue]` — keys are constructed and looked up whole, never parsed, so a
label carrying a separator is unambiguous rather than merely tolerated. The `label:` prefix keeps the
two namespaces apart: a label named `bug` cannot be mistaken for the Bug issue type.

A label sits ahead of the subtype because it is the finer signal — a `wayfinder:map` Epic and a plain
Epic are different documents and the type alone cannot say which. Neither is the hook inferring a
genre: both are fields the caller filled in before the hook ran, which is exactly what makes them
usable. An Epic holding a project map was denied on 2026-09-01 and told to fill four defect slots,
which a class-only key cannot tell from a bug report. Every key carries a class deliberately: a bare `[jira]`
catch-all would hand the issue template to a Jira comment, which is the failure above. A section
declared with no body suppresses the built-in, which is how you ask for the count alone.

`TICKETVOICE_TEMPLATE=<name>` names a section outright, overriding every key above it. An unknown
name falls through rather than emptying the advice. It is read from the hook's environment, so it
applies to a whole session and is not a per-write handle — reaching a genre the call does not state
means stating it, with a label.

A file that is missing or over the cap leaves the built-in table standing and adds one line to the
next denial saying so — the only abnormal path this tool does not pass over in silence, because an
operator who set the variable expects it to work.

## Agent tag

Every issue and comment this hook or gh-write lets through is prefixed with 🤖 by default, since
it's posted under the operator's own Linear or GitHub account and a reader shouldn't have to
already know to ask whether an agent wrote it. `TICKETVOICE_NO_AGENT_TAG` turns it off.

The two surfaces apply it differently, because they have different amounts of control over the
write. gh-write owns the actual bytes it sends to `gh`, so it just prepends the tag to its own
stdin before exec'ing. The hook doesn't own the write at all — a Linear MCP call goes straight from
Claude to `mcp.linear.app`, and a `PreToolUse` hook can only allow, deny, ask, or, via Claude Code's
`updatedInput`, replace the tool's entire input before it runs. So a clean Linear write returns
`permissionDecision: "allow"` with `updatedInput` set to the original input, verbatim, except the
one field carrying prose gets the tag prepended — every other field (`id`, `teamId`, whatever else
the real schema carries that this hook never parses) round-trips untouched, since `updatedInput`
replaces the whole object rather than merging into it. A patch (`save_issue` editing an existing
description) isn't tagged: it's a diff against prose already tagged once, not a fresh post.

Jira works the same way, with two differences. The tag goes on the body and never on the `summary`:
four characters out of a 20-word budget is real, and a board or list view showing a truncated title
is the worst place to spend them, while the body is where a reader who has actually opened the ticket
sees the provenance. And an `editJiraIssue` nests every field it sets under `fields`, so the tag goes
on `fields.description` — a full description replacement is a fresh post into a field, not a patch,
and the already-tagged check keeps a second edit from stacking a second marker. A field holding ADF
rather than a string isn't tagged: there is no correct place to put a prefix in a node tree.

## Where this sits next to cope and basanite

[cope](https://github.com/justinstimatze/cope) and
[basanite](https://github.com/justinstimatze/basanite) are the actual read on the prose — see
[Why](#why) — but neither blocks on its own: they answer with `additionalContext`, after the call
already went out, and only on Linear's own `PreToolUse` matchers. Ticketvoice is what turns their
verdict into a gate: it forwards its own stdin to `cope-gate -pretool` and
`basanite writecheck -no-dedup` directly and denies on either flag, whether or not the body is over
budget — so a within-budget ticket carrying a flagged tic gets sent back to Claude the same as an
over-length one. See [CHANGELOG.md](CHANGELOG.md) for how basanite's dedup state made this need a
new flag on its side.

What each sibling contributes to a denial is bounded: long lines lose their tails, identical lines
collapse to one carrying a count, and the whole note is capped at 1 KiB with a marker. All three
passes exist because the reason reaches Claude's context on every denial. A 936-word Epic drew a
4,342-byte reason of which 4,256 were cope's, since cope restates a rule's rationale once per
violation and that Epic tripped one rule nine times — and capping without the dedup pass kept the
nine copies and dropped the two findings that had only fired once.

Both siblings also name Linear in a note about a Jira or GitHub write, because the bridge hands them
a Linear-shaped payload: cope echoes the tool it was given, basanite derives the label from the
absent `file_path`. Naming the real tool in the payload is not an option — `cope-gate -pretool`
returns no verdict at all for a tool name it does not know — so the note is corrected on the way out
instead.

Neither sibling's matcher reaches Jira either, and adding it wouldn't help: basanite reads a flat
`file_path`/`content`/`new_string`/`body`/`description` input, which has no case for `commentBody`
and can't reach a level into `fields.description`. So ticketvoice's forward is the *only* path by
which Jira prose reaches either binary, and rather than the raw stdin it sends the body through
`budgetgate.LinearPayload` — the same bridge gh-write uses — in a shape both siblings already read.
The summary isn't forwarded, matching Linear, whose title never is: cope scores paragraph structure,
which a one-line title standing in for a paragraph would skew. A call carrying only a summary reaches
neither sibling, and its budget is the whole check.

Neither sibling's own matcher reaches `Bash`, so a GitHub write is never scored through their
independently-registered hooks the way a Linear call is. It's scored twice over by two other
callers instead: ticketvoice's hook forwards its own stdin the moment it can find a heredoc or
`< file` body ahead of the `gh` call, and gh-write forwards the real body bytes itself right
before sending, regardless of how they arrived — see the gh-write section above. Same two
binaries, same verdict shape, just two different callers covering what the other can't see.

[Impact line](#impact-line) and [Ground-truth citations](#ground-truth-citations) are a different
shape from cope and basanite: first-party, not delegated to a sibling binary, and neither reads on
register or vocabulary — one checks a plain-language line is present, the other checks a citation
against ground truth. The impact check never touches the network at all — see [Impact
line](#impact-line) for why it never asks a ticket to restate data (the project/initiative link)
Linear already tracks natively. The citation check's ticket-id half does read from Linear, but only
to confirm an id resolves to something, the same read-only existence check it runs against a local
file or a local git commit.

## Development

`git config core.hooksPath hooks` once, after cloning, activates the tracked pre-commit hook —
gofmt, vet, test, `make check-readme`, plus a non-blocking CodeScene delta check when `cs` is on
`PATH`.

`make check-readme` runs the tool's own gate against the Why section above, as if it were a Linear
issue description — the one passage in this file written in ticket-body register, so it's the
only fair target. Same convention as cope's `make check-readme` (`cope-gate --check README.md`) and
[effigy](https://github.com/justinstimatze/effigy)'s `generate_readme.py`, narrowed to what this
tool does: it doesn't generate prose, so there's nothing to write through it, only something to
check.

## License

MIT. See `LICENSE`.
