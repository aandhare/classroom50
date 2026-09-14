package main

import (
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
