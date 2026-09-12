package roster

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubtest"
)

// runRemovePending drives the email form of `roster remove` against inviteMock,
// which serves every endpoint the drop consults: the roster, the pending list,
// the invite team and its members, the failed list, and both DELETEs.
func runRemovePending(t *testing.T, mock *inviteMock, email string) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	var out, errOut bytes.Buffer
	err := runRosterRemovePendingRow(client, &out, &errOut, inviteTestOrg, inviteTestClassroom, email)
	return out.String(), errOut.String(), err
}

const removeTestPendingRoster = storedRosterHeader + ",Ada,Lovelace," + inviteTestEmail + ",section-1,,student\n" +
	"bea,Bea,Byte,bea@uni.edu,section-1,202,student\n"

// The #970 leftover: the invitation expired, the sync reaped the team, and only
// the row remains. Dropping it is the one thing no command could do; the row
// goes, nothing else on the roster moves, and GitHub's expired record is
// dismissed so the People page stops showing it.
func TestRunRosterRemovePendingRow_DeadRowDropped(t *testing.T) {
	mock := newInviteMock(t, removeTestPendingRoster)
	mock.pending = nil
	mock.inviteTeamStatus = http.StatusNotFound
	mock.failed = []map[string]any{
		{"id": 70, "email": inviteTestEmail},
		{"id": 71, "email": "someone-else@uni.edu"},
	}

	out, _, err := runRemovePending(t, mock, inviteTestEmail)
	if err != nil {
		t.Fatalf("a dead pending row must be removable: %v", err)
	}
	if len(mock.blobs) != 1 {
		t.Fatalf("want one roster commit, got %d blobs", len(mock.blobs))
	}
	rows, err := configrepo.ParseRoster([]byte(mock.blobs[0]))
	if err != nil {
		t.Fatalf("parse committed roster: %v", err)
	}
	if len(rows) != 1 || rows[0].Username != "bea" {
		t.Errorf("committed rows = %#v, want only bea's", rows)
	}
	if !strings.Contains(out, "removed the pending row for "+inviteTestEmail) {
		t.Errorf("stdout should report the removed row:\n%s", out)
	}
	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted a team GitHub no longer had: %q", mock.deletedTeamSlug)
	}
	if len(mock.dismissed) != 1 || mock.dismissed[0] != "70" {
		t.Errorf("dismissed = %v, want exactly record 70", mock.dismissed)
	}
}

// A team the sync hasn't reaped yet (recorded, empty) and a provisional team
// stranded with the acting teacher both map nothing once the row goes, so both
// are deleted with it.
func TestRunRosterRemovePendingRow_LeftoverTeamDeleted(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*inviteMock)
	}{
		{"recorded empty team", func(m *inviteMock) { m.inviteTeamDescription = inviteTestRecord(t) }},
		{"provisional team with the teacher on it", func(m *inviteMock) {
			m.inviteTeamDescription = contract.InviteProvisionalDescription
			m.inviteTeamMembers = []map[string]any{{"login": inviteTestActor, "id": 1}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newInviteMock(t, removeTestPendingRoster)
			mock.pending = nil
			tc.apply(mock)

			out, _, err := runRemovePending(t, mock, inviteTestEmail)
			if err != nil {
				t.Fatalf("runRosterRemovePendingRow: %v", err)
			}
			if len(mock.blobs) != 1 {
				t.Errorf("want one roster commit, got %d", len(mock.blobs))
			}
			if mock.deletedTeamSlug != mock.inviteTeamSlug {
				t.Errorf("deleted team = %q, want %q", mock.deletedTeamSlug, mock.inviteTeamSlug)
			}
			if !strings.Contains(out, "deleted metadata team") {
				t.Errorf("stdout should report the deleted team:\n%s", out)
			}
		})
	}
}

// Only a row nothing backs may go. A live invitation is cancel-invite's job, an
// accepted student is the sync's, and a hand-edited team is a human's; each
// refusal names its remedy and changes nothing.
func TestRunRosterRemovePendingRow_BackedRowRefused(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*inviteMock)
		want  []string
	}{
		{"live invitation", func(m *inviteMock) {
			m.pending = []map[string]any{{"id": 42, "email": inviteTestEmail, "role": "direct_member"}}
		}, []string{"still lists a pending invitation", "cancel-invite"}},
		{"accepted but unsynced", func(m *inviteMock) {
			m.pending = nil
			m.inviteTeamDescription = inviteTestRecord(t)
			m.inviteTeamMembers = []map[string]any{{"login": "ada", "id": 99}}
		}, []string{"accepted", "roster sync", "--write"}},
		{"hand-edited team with a member", func(m *inviteMock) {
			m.pending = nil
			m.inviteTeamDescription = "not an invite record"
			m.inviteTeamMembers = []map[string]any{{"login": "ada", "id": 99}}
		}, []string{"readable invite record"}},
		{"pending-list read failed", func(m *inviteMock) {
			m.pendingStatus = http.StatusInternalServerError
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newInviteMock(t, removeTestPendingRoster)
			tc.apply(mock)

			_, _, err := runRemovePending(t, mock, inviteTestEmail)
			if err == nil {
				t.Fatal("err = nil, want a refusal")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q: %v", want, err)
				}
			}
			if writes := writeCalls(mock.calls); len(writes) != 0 {
				t.Errorf("refusal issued %d write(s): %#v", len(writes), writes)
			}
		})
	}
}

// An address with no pending row is a no-op, like removing an absent username,
// and reads neither GitHub list. An address that sits on an account row is a
// different request (remove that student), so it is refused by name.
func TestRunRosterRemovePendingRow_NoPendingRow(t *testing.T) {
	absent := newInviteMock(t, removeTestPendingRoster)
	absent.pendingStatus = http.StatusInternalServerError // would fail if read
	out, _, err := runRemovePending(t, absent, "nobody@uni.edu")
	if err != nil {
		t.Fatalf("an absent row is a clean no-op: %v", err)
	}
	if !strings.Contains(out, "nothing to do") {
		t.Errorf("stdout should say there was nothing to do:\n%s", out)
	}
	if n := len(absent.calls); countCalls(absent.calls, http.MethodGet, "/orgs/o/invitations") != 0 || writeCalls(absent.calls) != nil {
		t.Errorf("an absent row touched GitHub's lists or wrote (%d calls): %#v", n, absent.calls)
	}

	onAccount := newInviteMock(t, removeTestPendingRoster)
	_, _, err = runRemovePending(t, onAccount, "bea@uni.edu")
	if err == nil || !strings.Contains(err.Error(), "roster remove o cs-principles bea") {
		t.Errorf("err = %v, want a refusal naming the username form", err)
	}
	if writes := writeCalls(onAccount.calls); len(writes) != 0 {
		t.Errorf("refusal issued %d write(s): %#v", len(writes), writes)
	}
}
