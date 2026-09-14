package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/foundation50/classroom50-cli-shared/ghhelp"
)

// Flag descriptions and Short lines follow the gh CLI's shape (one capitalized
// fragment, no period, no paragraph). Detail belongs in Long or the wiki.
func TestHelpCopyConventions(t *testing.T) {
	if v := ghhelp.Lint(newRootCmd()); len(v) != 0 {
		t.Fatalf("%d help copy violation(s):\n  %s", len(v), strings.Join(v, "\n  "))
	}
}

func runRoot(t *testing.T, args ...string) string {
	t.Helper()
	root := newRootCmd()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	_ = root.Execute()
	return buf.String()
}

// Guards that ghhelp.Install is wired into the real root: the package's own
// tests only cover a synthetic tree.
func TestUsageErrorIsConciseOnRealTree(t *testing.T) {
	out := runRoot(t, "assignment", "add")
	if !strings.Contains(out, "Run 'gh-teacher assignment add --help'") {
		t.Fatalf("usage error should point at --help:\n%s", out)
	}
	for _, unwanted := range []string{"Flags:", "--repo-visibility"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("usage error should not dump flags (%q):\n%s", unwanted, out)
		}
	}
}

func TestHelpRendersWrappedFlagsOnRealTree(t *testing.T) {
	out := runRoot(t, "assignment", "add", "--help")
	for _, want := range []string{"Usage:", "Examples:", "Flags:", "--repo-visibility string", "Global Flags:", "--verbose",
		// Safety caveats that moved from flag descriptions into Long must still render.
		"public repos are not autograded", "needs a paid GitHub plan", "--student-permission admin lets students change repo settings"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help missing %q", want)
		}
	}
	for _, l := range strings.Split(out, "\n") {
		if len([]rune(l)) > 100 {
			t.Errorf("help line longer than 100 columns: %q", l)
		}
	}
}
