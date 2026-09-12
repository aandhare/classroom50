package roster

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"

	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubapi"
	"github.com/foundation50/gh-teacher/internal/membership"
)

// failedInviteRecords is the org's failed-invitation list indexed by address,
// read on first use. A fresh or accepted invitation makes the address's expired
// record noise on the org's People page, so re-invite and sync dismiss it, as the
// web's dismissFailedInvitation does.
//
// Bookkeeping, never state: a 403 (the list is owner-only) or 404 reads as
// nothing to dismiss, any other failed read or DELETE only warns, and none of it
// can fail the send or the roster commit that came before.
type failedInviteRecords struct {
	client githubapi.Client
	org    string
	loaded bool
	// byEmail is nil after an unreadable list; every lookup then finds nothing.
	byEmail map[string][]int64
}

func (f *failedInviteRecords) load(errOut io.Writer) {
	if f.loaded {
		return
	}
	f.loaded = true
	list, err := membership.ListFailedOrgInvitations(f.client, f.org)
	if err != nil {
		// A secondary rate limit is also a 403, and that one is worth a warning.
		silent := (cliutil.IsHTTPStatus(err, http.StatusForbidden) && !cliutil.IsRateLimited(err)) ||
			cliutil.IsHTTPStatus(err, http.StatusNotFound)
		if !silent {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: reading the failed invitations failed (%v); any expired record is left for you to dismiss from https://github.com/orgs/%s/people/failed_invitations.\n", f.org, err, f.org)
		}
		return
	}
	f.byEmail = map[string][]int64{}
	for _, inv := range list {
		if inv.ID == 0 || !inv.IsEmailKeyed() {
			continue
		}
		key := configrepo.NormalizeInviteEmail(inv.Email)
		f.byEmail[key] = append(f.byEmail[key], inv.ID)
	}
}

// idsFor is the records an address still has, in id order so output is stable.
func (f *failedInviteRecords) idsFor(errOut io.Writer, email string) []int64 {
	f.load(errOut)
	ids := f.byEmail[configrepo.NormalizeInviteEmail(email)]
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids
}

// dismiss deletes every failed record for email and reports how many went. A
// record already gone counts as dismissed, since the outcome is the same.
func (f *failedInviteRecords) dismiss(out, errOut io.Writer, email string) {
	ids := f.idsFor(errOut, email)
	if len(ids) == 0 {
		return
	}
	dismissed := 0
	for _, id := range ids {
		if err := membership.CancelOrgInvitation(f.client, f.org, id); err != nil && !errors.Is(err, membership.ErrInvitationAlreadyGone) {
			_, _ = fmt.Fprintf(errOut, "Warning: %s: dismissing the expired invitation record %d for %s failed (%v); dismiss it from https://github.com/orgs/%s/people/failed_invitations.\n", f.org, id, email, err, f.org)
			continue
		}
		dismissed++
	}
	delete(f.byEmail, configrepo.NormalizeInviteEmail(email))
	if dismissed > 0 {
		_, _ = fmt.Fprintf(out, "%s: dismissed %d expired invitation record(s) for %s\n", f.org, dismissed, email)
	}
}
