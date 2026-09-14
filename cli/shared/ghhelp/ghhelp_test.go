package ghhelp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func newTree() (*cobra.Command, *cobra.Command) {
	root := &cobra.Command{Use: "tool", Short: "Do tool things"}
	root.PersistentFlags().Bool("verbose", false, "Show operational details")
	leaf := &cobra.Command{
		Use:     "add <name>",
		Short:   "Add a thing",
		Long:    "Long description of add.",
		Example: "  tool add hello",
		Args:    cobra.ExactArgs(1),
		RunE:    func(*cobra.Command, []string) error { return nil },
	}
	leaf.Flags().String("mode", "individual", "Assignment mode: {individual|group|team}")
	root.AddCommand(leaf)
	Install(root)
	return root, leaf
}

func run(t *testing.T, root *cobra.Command, args ...string) string {
	t.Helper()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	_ = root.Execute()
	return buf.String()
}

func TestHelpTemplateDerivesFromCobra(t *testing.T) {
	tmpl := helpTemplate()
	if got := strings.Count(tmpl, "wrappedFlagUsages"); got != 2 {
		t.Fatalf("expected both flag sections swapped for wrappedFlagUsages, found %d; Cobra's default template may have changed", got)
	}
	if strings.Contains(tmpl, ".FlagUsages") {
		t.Fatal("an unwrapped FlagUsages call survived the replacement")
	}
}

func TestUsageErrorIsConcise(t *testing.T) {
	root, _ := newTree()
	out := run(t, root, "add")
	for _, want := range []string{"tool add <name> [flags]", "Run 'tool add --help' for details and examples."} {
		if !strings.Contains(out, want) {
			t.Errorf("usage error output missing %q:\n%s", want, out)
		}
	}
	for _, unwanted := range []string{"Flags:", "--mode", "Long description"} {
		if strings.Contains(out, unwanted) {
			t.Errorf("usage error output should not contain %q:\n%s", unwanted, out)
		}
	}
}

func TestHelpIsFull(t *testing.T) {
	root, _ := newTree()
	out := run(t, root, "add", "--help")
	for _, want := range []string{"Long description of add.", "Usage:", "Examples:", "tool add hello", "Flags:", "--mode string", "{individual|group|team}", "Global Flags:", "--verbose"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "Long description"), strings.Index(out, "Usage:"); i > j {
		t.Errorf("description should precede Usage (gh order):\n%s", out)
	}
}

func TestLintPassesCleanTree(t *testing.T) {
	root, _ := newTree()
	if got := Lint(root); len(got) != 0 {
		t.Fatalf("clean tree reported violations: %v", got)
	}
}

func TestLintFlagUsage(t *testing.T) {
	cases := map[string]struct {
		usage string
		want  string
	}{
		"clean":                {"Filter by state: {open|closed}", ""},
		"placeholder":          {"Repository home page `URL`", ""},
		"empty":                {"", "empty"},
		"lowercase":            {"filter by state", "capital"},
		"period":               {"Filter by state.", "period"},
		"decorative backticks": {"Requires `gh teacher init` first", "not a type placeholder"},
		"three backticks":      {"Use `a` or `b", "one pair"},
		"too long":             {strings.Repeat("Word ", 25), "max"},
		"em dash":              {"Filter \u2014 by state", "em dash"},
		"please":               {"Please pass a value", `"please"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := lintFlagUsage(tc.usage)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("expected clean, got %v", got)
				}
				return
			}
			if !strings.Contains(strings.Join(got, "\n"), tc.want) {
				t.Fatalf("expected a violation mentioning %q, got %v", tc.want, got)
			}
		})
	}
}

func TestLintShort(t *testing.T) {
	c := &cobra.Command{Use: "x", Short: "lowercase start."}
	got := strings.Join(lintShort(c), "\n")
	if !strings.Contains(got, "capital") {
		t.Fatalf("expected capital-letter violation, got %q", got)
	}
}
