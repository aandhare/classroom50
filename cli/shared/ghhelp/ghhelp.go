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
const usageTemplate = `Usage:{{if .Runnable}}
  {{.UseLine}}{{end}}{{if .HasAvailableSubCommands}}
  {{.CommandPath}} [command]

Available Commands:{{range .Commands}}{{if (or .IsAvailableCommand (eq .Name "help"))}}
  {{rpad .Name .NamePadding }} {{.Short}}{{end}}{{end}}{{end}}

Run '{{.CommandPath}} --help' for details and examples.
`

// Install applies the templates to root. Cobra resolves templates through the
// parent chain at render time, so subcommands inherit them regardless of when
// they are added.
func Install(root *cobra.Command) {
	cobra.AddTemplateFunc("wrappedFlagUsages", wrappedFlagUsages)
	root.SetUsageTemplate(usageTemplate)
	root.SetHelpTemplate(helpTemplate())
}

// helpTemplate is Cobra's stock help output (description, usage, examples,
// commands, flags) with the two flag sections swapped for the wrapping
// variant. Deriving it from Cobra's own template keeps future upstream
// changes (groups, help topics) instead of freezing a copy.
func helpTemplate() string {
	full := strings.NewReplacer(
		"{{.LocalFlags.FlagUsages | trimTrailingWhitespaces}}", "{{wrappedFlagUsages .LocalFlags | trimTrailingWhitespaces}}",
		"{{.InheritedFlags.FlagUsages | trimTrailingWhitespaces}}", "{{wrappedFlagUsages .InheritedFlags | trimTrailingWhitespaces}}",
	).Replace(cobraDefaultUsageTemplate())
	return `{{with (or .Long .Short)}}{{. | trimTrailingWhitespaces}}

{{end}}{{if or .Runnable .HasSubCommands}}` + full + `{{end}}`
}

// cobraDefaultUsageTemplate reads Cobra's unexported default off a bare
// command, which is the only public way to get it.
func cobraDefaultUsageTemplate() string {
	return (&cobra.Command{}).UsageTemplate()
}

// wrappedFlagUsages renders a flag set wrapped to the terminal width (capped
// at maxWrapWidth). pflag's FlagUsages never wraps, so a long description
// prints as one line that runs off the screen.
func wrappedFlagUsages(fs *pflag.FlagSet) string {
	width, _, err := term.GetSize(int(os.Stdout.Fd()))
	if err != nil || width <= 0 || width > maxWrapWidth {
		width = maxWrapWidth
	}
	return wrapFlagUsages(fs, width)
}

func wrapFlagUsages(fs *pflag.FlagSet, width int) string {
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
		if c.Name() != "help" && c.Name() != "completion" {
			out = append(out, lintShort(c)...)
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

func lintShort(c *cobra.Command) []string {
	s := c.Short
	if s == "" {
		return []string{c.CommandPath() + ": Short is empty"}
	}
	var out []string
	if !startsUpper(s) {
		out = append(out, c.CommandPath()+": Short must start with a capital letter")
	}
	if strings.HasSuffix(s, ".") {
		out = append(out, c.CommandPath()+": Short must not end with a period")
	}
	if len(s) > maxShort {
		out = append(out, fmt.Sprintf("%s: Short is %d chars, max %d", c.CommandPath(), len(s), maxShort))
	}
	return out
}

// lintFlagUsage checks the raw usage string a flag was registered with.
// pflag treats the first backtick pair as the flag's type placeholder
// (`--team name`), not as inline code, so a backtick-quoted command like
// `gh teacher init` hijacks the type column and misaligns every other flag.
func lintFlagUsage(f *pflag.Flag) []string {
	usage := f.Usage
	if usage == "" {
		return []string{"description is empty"}
	}
	var out []string
	if n := strings.Count(usage, "`"); n != 0 && n != 2 {
		out = append(out, "backticks must come as one pair (a pflag type placeholder) or not at all")
	} else if n == 2 {
		start := strings.Index(usage, "`")
		end := strings.LastIndex(usage, "`")
		if ph := usage[start+1 : end]; strings.ContainsAny(ph, " \t") || len(ph) > 24 {
			out = append(out, fmt.Sprintf("backtick pair %q is not a type placeholder; pflag prints it in the type column", ph))
		}
	}
	_, desc := pflag.UnquoteUsage(f)
	if !startsUpper(desc) {
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

func startsUpper(s string) bool {
	for _, r := range s {
		return unicode.IsUpper(r) || unicode.IsDigit(r)
	}
	return false
}
