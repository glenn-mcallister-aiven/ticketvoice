// Package ghcmd reads gh command lines out of a Bash command string without a shell tokenizer.
// The hook finds raw `gh` writes with it. gh-write shares IsBodyFlag so the two agree on what a
// body flag is.
package ghcmd

import (
	"regexp"
	"strings"
)

// HeredocOpener matches `<<EOF`, `<<-'EOF'`, `<< "EOF"` and the newline that ends the opener line.
// Group 2 is the delimiter.
var HeredocOpener = regexp.MustCompile(`<<-?\s*(['"]?)(\w+)['"]?[ \t]*\r?\n`)

var bodyFlags = map[string]bool{
	"--body": true, "-b": true, "--body-file": true, "-F": true,
}

// IsBodyFlag reports whether arg carries a body for `gh issue`/`gh pr`. For `gh api`, `-F` means
// --field, so this must never be used on an api call's arguments.
func IsBodyFlag(arg string) bool {
	return bodyFlags[arg] || strings.HasPrefix(arg, "--body=") || strings.HasPrefix(arg, "--body-file=")
}

// Write is one raw gh invocation that carries prose.
type Write struct {
	Object, Verb string   // "pr" "comment", "issue" "edit", "pr" "review", "api" "comment"
	Target       string   // the issue, PR or comment id, if the command names one
	BodyFile     string   // a --body-file path; "-" when the body comes from stdin
	Redirect     string   // the command's own `< path` redirect, if it has one
	Passthrough  []string // the remaining flags and their values, body flags removed
}

// BlankHeredocs replaces every heredoc body in command with spaces, so text inside one (a comment
// that mentions `gh pr comment --body`) is never read as a command. Newlines and length are kept,
// so an offset into the result is an offset into command. An unterminated heredoc blanks to the
// end.
func BlankHeredocs(command string) string {
	b := []byte(command)
	pos := 0
	for {
		open := HeredocOpener.FindStringSubmatchIndex(command[pos:])
		if open == nil {
			return string(b)
		}
		bodyStart := pos + open[1]
		delim := command[pos+open[4] : pos+open[5]]
		closer := regexp.MustCompile(`(?m)^[ \t]*` + regexp.QuoteMeta(delim) + `[ \t]*$`)
		end := len(command)
		next := len(command)
		if loc := closer.FindStringIndex(command[bodyStart:]); loc != nil {
			end = bodyStart + loc[0]
			next = bodyStart + loc[1]
		}
		for i := bodyStart; i < end; i++ {
			if b[i] != '\n' {
				b[i] = ' '
			}
		}
		pos = next
	}
}

// prefixWord reports a word that can stand in front of a command without being it.
func prefixWord(w string) bool {
	return assignment.MatchString(w) || w == "command" || w == "exec" || w == "env" || w == "sudo"
}

// InCommandPosition reports whether idx in command starts the word a simple command runs, rather
// than an argument to something else or text inside a quote or a heredoc body.
func InCommandPosition(command string, idx int) bool {
	blanked := BlankHeredocs(command)
	for _, seg := range segments(blanked) {
		if idx < seg[0] || idx >= seg[1] {
			continue
		}
		i := seg[0]
		for {
			for i < seg[1] && (blanked[i] == ' ' || blanked[i] == '\t') {
				i++
			}
			j := i
			for j < seg[1] && blanked[j] != ' ' && blanked[j] != '\t' {
				j++
			}
			if i >= j || !prefixWord(blanked[i:j]) {
				return i == idx
			}
			i = j
		}
	}
	return false
}

var assignment = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)

var apiCommentID = regexp.MustCompile(`issues/comments/(\d+)`)

// RawWrite finds the first raw gh invocation in command that carries a body gh-write should be
// carrying instead. ok=false for every gh-write call and every gh call with no body.
func RawWrite(command string) (w Write, ok bool) {
	blanked := BlankHeredocs(command)
	for _, seg := range segments(blanked) {
		if w, ok := rawWriteSegment(strings.Fields(blanked[seg[0]:seg[1]])); ok {
			return w, true
		}
	}
	return Write{}, false
}

func rawWriteSegment(words []string) (Write, bool) {
	for len(words) > 0 && prefixWord(words[0]) {
		words = words[1:]
	}
	if len(words) < 2 || (words[0] != "gh" && !strings.HasSuffix(words[0], "/gh")) {
		return Write{}, false
	}
	if words[1] == "api" {
		return rawAPI(words[2:])
	}
	if len(words) < 3 {
		return Write{}, false
	}
	object, verb := words[1], words[2]
	if object != "issue" && object != "pr" {
		return Write{}, false
	}
	if !(verb == "create" || verb == "comment" || verb == "edit" || (object == "pr" && verb == "review")) {
		return Write{}, false
	}
	w := Write{Object: object, Verb: verb}
	found := false
	args := words[3:]
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--body-file" || a == "-F":
			found = true
			if i+1 < len(args) {
				w.BodyFile = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--body-file="):
			found = true
			w.BodyFile = strings.TrimPrefix(a, "--body-file=")
		case a == "--body" || a == "-b":
			found = true
			i = skipValue(args, i)
		case strings.HasPrefix(a, "--body="):
			found = true
			i = skipQuoted(args, i, strings.TrimPrefix(a, "--body="))
		case strings.HasPrefix(a, "<"):
			w.Redirect = redirectTarget(args, i)
			if a == "<" {
				i++
			}
		case w.Target == "" && isDigits(a) && len(w.Passthrough) == 0:
			w.Target = a
		default:
			w.Passthrough = append(w.Passthrough, a)
		}
	}
	return w, found
}

// rawAPI matches a `gh api` call that sends a body field. A field value is a single word here;
// anything quoted with spaces still starts with `body=`, which is all this needs.
func rawAPI(args []string) (Write, bool) {
	for i, a := range args {
		field := ""
		switch {
		case a == "-f" || a == "-F" || a == "--field" || a == "--raw-field":
			if i+1 < len(args) {
				field = args[i+1]
			}
		case strings.HasPrefix(a, "--field="), strings.HasPrefix(a, "--raw-field="):
			field = a[strings.Index(a, "=")+1:]
		case strings.HasPrefix(a, "-f") || strings.HasPrefix(a, "-F"):
			field = a[2:]
		}
		if strings.HasPrefix(strings.Trim(field, `'"`), "body=") {
			w := Write{Object: "api", Verb: "comment"}
			if m := apiCommentID.FindStringSubmatch(strings.Join(args, " ")); m != nil {
				w.Target = m[1]
			}
			return w, true
		}
	}
	return Write{}, false
}

// skipValue steps over a flag's value, including a quoted value that spans several words.
func skipValue(args []string, i int) int {
	if i+1 >= len(args) {
		return i
	}
	i++
	q := args[i][0]
	if q != '"' && q != '\'' {
		return i
	}
	if len(args[i]) > 1 && args[i][len(args[i])-1] == q {
		return i
	}
	for i++; i < len(args); i++ {
		if strings.HasSuffix(args[i], string(q)) {
			return i
		}
	}
	return len(args) - 1
}

// skipQuoted steps over the rest of a `--flag="several words"` value that started in args[i].
func skipQuoted(args []string, i int, val string) int {
	if val == "" || (val[0] != '"' && val[0] != '\'') || (len(val) > 1 && val[len(val)-1] == val[0]) {
		return i
	}
	for i++; i < len(args); i++ {
		if strings.HasSuffix(args[i], val[:1]) {
			return i
		}
	}
	return len(args) - 1
}

// segments splits a command string into simple commands on ;, &&, ||, |, & and newlines, and on
// ( and $( — but never inside quotes, so `git commit -m "x; gh pr comment 1 --body y"` stays one
// segment whose first word is git. Each segment is a [start, end) byte range into command.
func segments(command string) [][2]int {
	var out [][2]int
	start := 0
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			if c == '\\' && quote == '"' {
				i++
			} else if c == quote {
				quote = 0
			}
			continue
		}
		switch c {
		case '\\':
			i++
		case '"', '\'':
			quote = c
		case ';', '|', '&', '\n', '(':
			out = append(out, [2]int{start, i})
			start = i + 1
		}
	}
	return append(out, [2]int{start, len(command)})
}

func redirectTarget(args []string, i int) string {
	if args[i] == "<" {
		if i+1 < len(args) {
			return args[i+1]
		}
		return ""
	}
	if strings.HasPrefix(args[i], "<<") || strings.HasPrefix(args[i], "<(") {
		return ""
	}
	return strings.TrimPrefix(args[i], "<")
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
