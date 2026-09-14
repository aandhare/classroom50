// Package ghhelp shapes the help output of the gh-teacher and gh-student CLIs
// the way clig.dev and git do: a usage error prints only the usage line and a
// pointer to --help, while --help prints the full text with flag descriptions
// wrapped to the terminal.
//
// It also lints the command tree against the flag-description conventions
// gh itself follows (one capitalized fragment, no period), so a paragraph
// pasted into a flag description fails a test instead of shipping.
package ghhelp

import (
	"fmt"
	"os"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"
)

// maxWrapWidth caps the Flags column on a very wide terminal; past it the
// descriptions get hard to scan.
const maxWrapWidth = 100

// usageTemplate prints on a usage error (wrong arg count, unknown flag).
// Cobra's default also dumps every flag here, which is what buried the usage
// line in issue #948.
const usageTemplate = `Usage:{{if and .Runnable (not .HasAvailableSubCommands)}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}

Run '{{.CommandPath}} --help' for details and examples.
`

// Install applies the templates to root and the unknown-subcommand guard to
// every group under it. Call it after the last AddCommand: templates are
// inherited at render time, but the guard is attached to the tree as it
// exists now.
func Install(root *cobra.Command) {
	cobra.AddTemplateFunc("wrappedFlagUsages", wrappedFlagUsages)
	root.SetUsageTemplate(usageTemplate)
	root.SetHelpTemplate(helpTemplate())
	for _, c := range root.Commands() {
		rejectUnknownSubcommands(c)
	}
}

// rejectUnknownSubcommands makes a command group fail on a mistyped
// subcommand. Cobra only does that for the root; a bare group is not
// runnable, so `tool group bogus` prints the group's full help and exits 0
// without ever validating args.
func rejectUnknownSubcommands(c *cobra.Command) {
	if c.HasSubCommands() && !c.Runnable() {
		// Cobra applies this default only on its own root-command path.
		if c.SuggestionsMinimumDistance <= 0 {
			c.SuggestionsMinimumDistance = 2
		}
		c.RunE = func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			var hint string
			if s := cmd.SuggestionsFor(args[0]); len(s) > 0 {
				hint = "\n\nDid you mean this?\n\t" + strings.Join(s, "\n\t")
			}
			return fmt.Errorf("unknown command %q for %q%s", args[0], cmd.CommandPath(), hint)
		}
	}
	for _, sub := range c.Commands() {
		rejectUnknownSubcommands(sub)
	}
}

// helpTemplate is Cobra's stock help output with the two flag sections
// swapped for the wrapping variant. Both halves are read off a bare Command
// so upstream template changes (groups, help topics) flow through instead of
// being frozen in a copy.
func helpTemplate() string {
	bare := &cobra.Command{}
	usage := strings.NewReplacer(
		"{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}", "{{wrappedFlagUsages .LocalFlags | trimTrailingWhitespaces}}",
		"{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}", "{{wrappedFlagUsages .InheritedFlags | trimTrailingWhitespaces}}",
	).Replace(bare.UsageTemplate())
	return strings.Replace(bare.HelpTemplate(), "{{.UsageString}}", usage, 1)
}

// wrappedFlagUsages renders a flag set wrapped to the terminal width (capped
// at maxWrapWidth). pflag's FlagUsages never wraps, so a long description
// prints as one line that runs off the screen.
func wrappedFlagUsages(fs *pflag.FlagSet) string {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 || width > maxWrapWidth {
		width = maxWrapWidth
	}
	return fs.FlagUsagesWrapped(width)
}

// Copy limits, chosen to match the official gh CLI: a flag description is a
// single fragment that fits beside the flag column, and Short is a one-line
// summary for the command list.
const (
	maxFlagDescription = 100
	maxShort           = 70
)

// Lint walks the command tree and returns one line per copy violation, empty
// when clean. Wire it into a test in each CLI's main package.
func Lint(root *cobra.Command) []string {
	var out []string
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		// Cobra's own help/completion commands and help/version flags are
		// not our copy; they exist once the tree has been executed.
		if c.Name() != "help" && c.Name() != "completion" {
			for _, v := range lintShort(c.Short) {
				out = append(out, c.CommandPath()+": "+v)
			}
			c.LocalFlags().VisitAll(func(f *pflag.Flag) {
				if f.Name == "help" || f.Name == "version" {
					return
				}
				for _, v := range lintFlagUsage(f) {
					out = append(out, fmt.Sprintf("%s --%s: %s", c.CommandPath(), f.Name, v))
				}
			})
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
	return out
}

func lintShort(s string) []string {
	if s == "" {
		return []string{"Short is empty"}
	}
	var out []string
	if !startsCapitalized(s) {
		out = append(out, "Short must start with a capital letter")
	}
	if strings.HasSuffix(s, ".") {
		out = append(out, "Short must not end with a period")
	}
	if len(s) > maxShort {
		out = append(out, fmt.Sprintf("Short is %d chars, max %d", len(s), maxShort))
	}
	return out
}

// lintFlagUsage checks the raw usage string a flag was registered with.
// pflag treats the first backtick pair as the flag's type placeholder
// (`--team name`), not as inline code, so a backtick-quoted command like
// `gh teacher init` hijacks the type column and misaligns every other flag.
func lintFlagUsage(f *pflag.Flag) []string {
	if f.Usage == "" {
		return []string{"description is empty"}
	}
	var out []string
	placeholder, desc := pflag.UnquoteUsage(f)
	if n := strings.Count(f.Usage, "`"); n != 0 && n != 2 {
		out = append(out, "backticks must come as one pair (a pflag type placeholder) or not at all")
	} else if n == 2 && (strings.ContainsAny(placeholder, " \t") || len(placeholder) > 24) {
		out = append(out, fmt.Sprintf("backtick pair %q is not a type placeholder; pflag prints it in the type column", placeholder))
	}
	if !startsCapitalized(desc) {
		out = append(out, "description must start with a capital letter")
	}
	if strings.HasSuffix(strings.TrimSpace(desc), ".") {
		out = append(out, "description must not end with a period")
	}
	if len(desc) > maxFlagDescription {
		out = append(out, fmt.Sprintf("description is %d chars, max %d (move detail into Long)", len(desc), maxFlagDescription))
	}
	if strings.ContainsRune(desc, '\u2014') {
		out = append(out, "no em dashes")
	}
	for _, banned := range []string{"please", "sorry", "successfully"} {
		if strings.Contains(strings.ToLower(desc), banned) {
			out = append(out, fmt.Sprintf("avoid %q", banned))
		}
	}
	return out
}

// startsCapitalized accepts a leading digit too ("3 retries ...").
func startsCapitalized(s string) bool {
	r, _ := utf8.DecodeRuneInString(s)
	return unicode.IsUpper(r) || unicode.IsDigit(r)
}
