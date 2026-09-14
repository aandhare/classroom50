package ghhelp

import (
	"bytes"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const longFlagDescription = "Ordered gitignore-style pattern deciding which files count as the submission, last match wins"

func newTree() *cobra.Command {
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
	leaf.Flags().StringArray("allowed-files", nil, longFlagDescription)
	root.AddCommand(leaf)
	group := &cobra.Command{Use: "group", Short: "Manage things", Long: "Long description of group."}
	group.AddCommand(&cobra.Command{Use: "list", Short: "List things", RunE: func(*cobra.Command, []string) error { return nil }})
	root.AddCommand(group)
	Install(root)
	return root
}

func run(t *testing.T, root *cobra.Command, args ...string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	root.SetOut(&buf)
	root.SetErr(&buf)
	root.SetArgs(args)
	err := root.Execute()
	return buf.String(), err
}

func TestHelpTemplateDerivesFromCobra(t *testing.T) {
	tmpl := helpTemplate()
	if got := strings.Count(tmpl, "wrappedFlagUsages"); got != 2 {
		t.Fatalf("expected both flag sections swapped for wrappedFlagUsages, found %d; Cobra's default template may have changed", got)
	}
	for _, leftover := range []string{".FlagUsages", "{{.UsageString}}"} {
		if strings.Contains(tmpl, leftover) {
			t.Errorf("%s survived the replacement; Cobra's default template may have changed", leftover)
		}
	}
}

func TestUsageErrorIsConcise(t *testing.T) {
	out, _ := run(t, newTree(), "add")
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
	out, _ := run(t, newTree(), "add", "--help")
	for _, want := range []string{"Long description of add.", "Usage:", "Examples:", "tool add hello", "Flags:", "--mode string", "{individual|group|team}", "Global Flags:", "--verbose"} {
		if !strings.Contains(out, want) {
			t.Errorf("--help output missing %q:\n%s", want, out)
		}
	}
	if i, j := strings.Index(out, "Long description"), strings.Index(out, "Usage:"); i > j {
		t.Errorf("description should precede Usage (gh order):\n%s", out)
	}
	// The flag column is wrapped: the long description never renders on one
	// line and nothing exceeds the width cap.
	if strings.Contains(out, longFlagDescription) {
		t.Errorf("long flag description was not wrapped:\n%s", out)
	}
	if !strings.Contains(out, "last match wins") {
		t.Errorf("wrapping dropped the end of the description:\n%s", out)
	}
	for _, l := range strings.Split(out, "\n") {
		if n := len([]rune(l)); n > maxWrapWidth {
			t.Errorf("line exceeds %d columns (%d): %q", maxWrapWidth, n, l)
		}
	}
}

func TestGroupRejectsUnknownSubcommand(t *testing.T) {
	out, err := run(t, newTree(), "group", "lst")
	if err == nil {
		t.Fatalf("a mistyped subcommand must fail, got exit 0 with:\n%s", out)
	}
	for _, want := range []string{`unknown command "lst" for "tool group"`, "Did you mean this?", "list", "Available Commands:"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Long description of group.") {
		t.Errorf("a typo should print the concise usage, not the group's full help:\n%s", out)
	}
}

func TestBareGroupStillPrintsHelp(t *testing.T) {
	out, err := run(t, newTree(), "group")
	if err != nil {
		t.Fatalf("bare group should succeed, got %v", err)
	}
	for _, want := range []string{"Long description of group.", "Available Commands:", "list"} {
		if !strings.Contains(out, want) {
			t.Errorf("bare group help missing %q:\n%s", want, out)
		}
	}
}

func TestLintPassesCleanTree(t *testing.T) {
	if got := Lint(newTree()); len(got) != 0 {
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
		"digit start":          {"3 retries before giving up", ""},
		"empty":                {"", "empty"},
		"lowercase":            {"filter by state", "capital"},
		"period":               {"Filter by state.", "period"},
		"decorative backticks": {"Requires `gh teacher init` first", "not a type placeholder"},
		"long placeholder":     {"Mode `" + strings.Repeat("x", 25) + "`", "not a type placeholder"},
		"three backticks":      {"Use `a` or `b", "one pair"},
		"too long":             {strings.Repeat("Word ", 25), "max"},
		"em dash":              {"Filter \u2014 by state", "em dash"},
		"please":               {"Please pass a value", `"please"`},
		"successfully":         {"Report successfully sent", `"successfully"`},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			fs := pflag.NewFlagSet("t", pflag.ContinueOnError)
			fs.String("x", "", tc.usage)
			got := lintFlagUsage(fs.Lookup("x"))
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
	cases := map[string]struct {
		short string
		want  []string
	}{
		"clean":     {"Add a thing", nil},
		"empty":     {"", []string{"empty"}},
		"lowercase": {"add a thing", []string{"capital"}},
		"period":    {"Add a thing.", []string{"period"}},
		"too long":  {strings.Repeat("A", maxShort+1), []string{"max"}},
		// Every violation is reported at once, not just the first.
		"lowercase and period": {"add a thing.", []string{"capital", "period"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			got := lintShort(tc.short)
			if len(got) != len(tc.want) {
				t.Fatalf("expected %d violation(s) %v, got %v", len(tc.want), tc.want, got)
			}
			for i, w := range tc.want {
				if !strings.Contains(got[i], w) {
					t.Errorf("violation %d: expected %q in %q", i, w, got[i])
				}
			}
		})
	}
}
