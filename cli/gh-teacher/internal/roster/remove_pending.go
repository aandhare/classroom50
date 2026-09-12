package roster

import (
	"fmt"
	"io"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/configwrite"
	"github.com/foundation50/gh-teacher/internal/githubapi"
)

// runRosterRemovePendingRow drops the pending row for an address once GitHub
// confirms nothing backs it: the web's Remove row for an "unlinked" row, plus
// the one check the CLI needs because its sync is manual (a team holding an
// accepted student must refuse, not drop). The row is the primary action and
// the only one that can fail the command; the invite team and GitHub's expired
// record are cleared afterwards with warnings, since the sync's GC and the
// People page are the backstops for both.
func runRosterRemovePendingRow(client githubapi.Client, out, errOut io.Writer, org, classroom, email string) error {
	email = configrepo.NormalizeInviteEmail(email)
	path := fmt.Sprintf("%s/%s/%s", org, configrepo.ConfigRepoName, configrepo.RosterFilePath(classroom))

	branch, err := configrepo.ResolveConfigRepoBranch(client, org)
	if err != nil {
		return err
	}
	rows, err := configrepo.LoadRosterLenient(client, org, classroom, branch)
	if err != nil {
		return err
	}
	holder, pending := rosterEmailClaim(rows, email)
	if !pending {
		if holder != "" {
			return fmt.Errorf("%s is the address on %s's row, which has an account, so nothing was changed. To remove that student, run `gh teacher roster remove %s %s %s`",
				email, holder, org, classroom, holder)
		}
		_, _ = fmt.Fprintf(out, "%s: no pending row for %s, nothing to do\n", path, email)
		return nil
	}

	live := &liveInvitations{client: client, org: org}
	state, err := classifyPendingRow(client, org, classroom, email, live)
	if err != nil {
		return err
	}
	switch state {
	case pendingLive:
		return fmt.Errorf("GitHub still lists a pending invitation for %s, so the row was kept: dropping it would leave an invitation whose acceptance nothing records. Run `gh teacher roster cancel-invite %s %s %s` to revoke the invitation and drop the row together",
			email, org, classroom, email)
	case pendingAccepted:
		return fmt.Errorf("%s accepted an earlier invitation to %s but isn't recorded on the roster yet, so the row was kept. Run %s to record their username and github_id, then remove them by username if they should not be on the roster",
			email, classroom, syncWriteCommand(org, classroom))
	}

	// Every pending row carrying the address goes (identical duplicates are
	// indistinguishable). A row that gained an identity under the rebase is no
	// longer a pending row, so RemovePendingEmailRow leaves it alone.
	removed := 0
	build := func(parentSHA string) (configwrite.CommitChange, error) {
		removed = 0
		current, err := configrepo.LoadRosterLenient(client, org, classroom, parentSHA)
		if err != nil {
			return configwrite.CommitChange{}, err
		}
		for {
			next, ok := configrepo.RemovePendingEmailRow(current, email)
			if !ok {
				break
			}
			current = next
			removed++
		}
		if removed == 0 {
			return configwrite.CommitChange{}, nil
		}
		return configrepo.RosterWriteChange(classroom, current)
	}
	message := contract.PrefixCommit(fmt.Sprintf("roster: remove the expired pending row for %s from %s (gh teacher roster remove)", email, classroom))
	if _, err := configwrite.CommitTreeChange(client, org, configrepo.ConfigRepoName, branch, message, build); err != nil {
		return err
	}
	if removed == 0 {
		_, _ = fmt.Fprintf(out, "%s: no pending row for %s, nothing to do\n", path, email)
		return nil
	}
	_, _ = fmt.Fprintf(out, "%s: removed the pending row for %s (no invitation was pending)\n", path, email)

	// The team was just proven record-less or member-less for this address, so
	// it maps nothing; only a record naming another classroom is not ours to
	// delete (the slug hashes (classroom, email), so that should never happen).
	slug := configrepo.InviteTeamName(classroom, email)
	team, found, err := configrepo.ReadInviteTeam(client, org, slug)
	switch {
	case err != nil:
		warnStrandedInviteTeam(errOut, "the row was removed, but reading", org, slug, err)
	case found && team.Record != nil && team.Record.Classroom != classroom:
		_, _ = fmt.Fprintf(errOut, "Warning: %s: left the metadata team %s alone: its record names classroom %s, not %s.\n", org, slug, team.Record.Classroom, classroom)
	case found:
		if err := configrepo.DeleteInviteTeam(client, org, slug); err != nil {
			warnStrandedInviteTeam(errOut, "the row was removed, but deleting", org, slug, err)
		} else {
			_, _ = fmt.Fprintf(out, "%s: deleted metadata team %s\n", org, slug)
		}
	}

	failed := &failedInviteRecords{client: client, org: org}
	failed.dismiss(out, errOut, email)
	return nil
}
