package ghcmd

import (
	"reflect"
	"strings"
	"testing"
)

func TestRawWriteMatches(t *testing.T) {
	cases := []struct {
		name, command string
		want          Write
	}{
		{"body-file path", "gh pr comment 1568 --body-file /abs/path/comment.md",
			Write{Object: "pr", Verb: "comment", Target: "1568", BodyFile: "/abs/path/comment.md"}},
		{"body from a command substitution", `gh pr comment 1568 --body "$(cat /abs/path/comment.md)"`,
			Write{Object: "pr", Verb: "comment", Target: "1568"}},
		{"short -F", "gh issue comment 3 -F notes.md --repo o/r",
			Write{Object: "issue", Verb: "comment", Target: "3", BodyFile: "notes.md", Passthrough: []string{"--repo", "o/r"}}},
		{"body-file stdin with a redirect", "gh pr comment 1 --body-file - < x.md",
			Write{Object: "pr", Verb: "comment", Target: "1", BodyFile: "-", Redirect: "x.md"}},
		{"-F stdin with a heredoc", "gh pr comment 1 -F - <<'EOF'\nhello there\nEOF",
			Write{Object: "pr", Verb: "comment", Target: "1", BodyFile: "-"}},
		{"review", `gh pr review 5 --approve -b "looks good to me"`,
			Write{Object: "pr", Verb: "review", Target: "5", Passthrough: []string{"--approve"}}},
		{"after cd", "cd x && gh issue comment 3 --body y",
			Write{Object: "issue", Verb: "comment", Target: "3"}},
		{"env assignment", "GH_REPO=o/r gh pr comment 1 -b z",
			Write{Object: "pr", Verb: "comment", Target: "1"}},
		{"create with a quoted title", `gh issue create --title "Bug: X" --body="two words"`,
			Write{Object: "issue", Verb: "create", Passthrough: []string{"--title", `"Bug:`, `X"`}}},
		{"api comment edit", "gh api -X PATCH repos/o/r/issues/comments/9 -f body=trimmed",
			Write{Object: "api", Verb: "comment", Target: "9"}},
		{"api new comment", "gh api repos/o/r/issues/12/comments -F 'body=hello'",
			Write{Object: "api", Verb: "comment"}},
		{"full path to gh", "/usr/bin/gh pr edit 4 --body x",
			Write{Object: "pr", Verb: "edit", Target: "4"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := RawWrite(c.command)
			if !ok {
				t.Fatalf("RawWrite(%q) = no match, want a match", c.command)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("RawWrite(%q) = %+v, want %+v", c.command, got, c.want)
			}
		})
	}
}

func TestRawWriteIgnores(t *testing.T) {
	for _, command := range []string{
		"gh-write pr comment 1568 < /abs/path/comment.md",
		"gh-write issue create --title T <<'EOF'\nbody\nEOF",
		"gh pr view 1",
		"gh pr create --fill",
		"gh issue edit 5 --add-label bug",
		`git commit -m "gh pr comment 1 --body x"`,
		`git commit -m "fix; gh pr comment 1 --body x"`,
		"gh-write pr comment 2 <<'EOF'\nuse gh pr comment 1 --body x instead\nEOF",
		"gh api repos/o/r/pulls",
		"gh api repos/o/r/pulls -F state=open",
		"echo gh pr comment 1 --body x",
	} {
		if w, ok := RawWrite(command); ok {
			t.Errorf("RawWrite(%q) matched %+v, want no match", command, w)
		}
	}
}

func TestIsBodyFlag(t *testing.T) {
	for arg, want := range map[string]bool{
		"--body": true, "-b": true, "--body-file": true, "-F": true, "--body=x": true, "--body-file=x": true,
		"--title": false, "-B": false, "--bodyx": false,
	} {
		if got := IsBodyFlag(arg); got != want {
			t.Errorf("IsBodyFlag(%q) = %v, want %v", arg, got, want)
		}
	}
}

func TestInCommandPosition(t *testing.T) {
	cases := []struct {
		command, word string
		want          bool
	}{
		{"gh-write pr comment 1 < x.md", "gh-write", true},
		{"cd repo && gh-write pr comment 1 < x.md", "gh-write", true},
		{"FOO=1 command gh-write pr comment 1", "gh-write", true},
		{`echo "use gh-write pr comment 1 instead"`, "gh-write", false},
		{"python3 - <<'PY'\ns = 'gh-write issue create --title T'\nPY", "gh-write", false},
		{"git log --grep gh-write", "gh-write", false},
	}
	for _, c := range cases {
		idx := strings.Index(c.command, c.word)
		if got := InCommandPosition(c.command, idx); got != c.want {
			t.Errorf("InCommandPosition(%q, %q) = %v, want %v", c.command, c.word, got, c.want)
		}
	}
}
