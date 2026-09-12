package roster

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/foundation50/classroom50-cli-shared/contract"
	"github.com/foundation50/gh-teacher/internal/cliutil"
	"github.com/foundation50/gh-teacher/internal/configrepo"
	"github.com/foundation50/gh-teacher/internal/githubtest"
)

const (
	inviteTestOrg       = "o"
	inviteTestClassroom = "cs-principles"
	inviteTestEmail     = "ada@uni.edu"
	inviteTestActor     = "prof"
	// inviteTestClassroomTeamID is the team id classroom.json records; the
	// invitation must carry it alongside the invite team's.
	inviteTestClassroomTeamID = 5
	inviteTestInviteTeamID    = 7
)

// inviteMock is the roster-write mock plus every endpoint `roster invite`
// touches: /user (the acting teacher), the invite team
// create/patch/membership/members/delete, the org pending-invitation list, and
// the org invitation POST. classroom.json is served from rosterWriteMock.files.
type inviteMock struct {
	*rosterWriteMock
	// createStatus is the invite-team create status; 422 drives the adopt path
	// (a pre-existing team a failed run must NOT delete).
	createStatus int
	// invitationStatus is the org-invitation POST status (0 → 201). A 422 body
	// says what GitHub rejected: invitation422Message, or the "already" text
	// when empty.
	invitationStatus     int
	invitation422Message string
	// invitationRateLimited makes the POST fail as a secondary rate limit, which
	// the web deliberately treats differently from a hard failure.
	invitationRateLimited bool
	// pending is served as GET /orgs/o/invitations (nil → empty list), the
	// liveness signal for a pending roster row.
	pending []map[string]any
	// pendingStatus is that GET's status (0 → 200); pendingRateLimited makes it
	// fail as a 403-shaped secondary rate limit instead.
	pendingStatus      int
	pendingRateLimited bool
	// inviteTeamMembers is served as the invite team's members list; a member
	// means someone accepted an earlier invitation.
	inviteTeamMembers []map[string]any
	// inviteTeamStatus is the status of the invite team's GET and members read
	// (0 → 200); 404 is a team the sync already garbage-collected, and lasts
	// until this run recreates it. inviteTeamMembersStatus overrides the members
	// read alone, and inviteTeamRateLimited fails the team GET as a 403-shaped
	// secondary rate limit.
	inviteTeamStatus        int
	inviteTeamMembersStatus int
	inviteTeamRateLimited   bool
	// inviteTeamDescription is what the team's GET reports before this run
	// PATCHes it (empty → a plain record-less description).
	inviteTeamDescription string
	// failed is served as GET /orgs/o/failed_invitations (nil → empty list),
	// the expired records a re-invite dismisses; failedStatus overrides the
	// read (403 is the owner-only refusal, tolerated silently) and
	// failedRateLimited fails it as a 403-shaped secondary rate limit.
	failed            []map[string]any
	failedStatus      int
	failedRateLimited bool
	// dismissStatus is the status of DELETE /orgs/o/invitations/{id} (0 → 204).
	dismissStatus int
	// inviteTeamDeleteStatus is the status of the invite team's DELETE (0 → 204).
	inviteTeamDeleteStatus int
	// commitFails fails the tree POST, simulating a roster write failure after a
	// successful send.
	commitFails bool
	// afterCommit runs once the roster tree POST has been served, so a test can
	// change what GitHub reports in the window between a commit and the
	// teardown that follows it.
	afterCommit func()

	calls           []inviteCall
	invitationBody  map[string]any
	inviteTeamSlug  string
	deletedTeamSlug string
	teamCreated     bool
	dismissed       []string
}

func (m *inviteMock) handler(t *testing.T) http.Handler {
	t.Helper()
	base := m.rosterWriteMock.handler(t).(*http.ServeMux)
	m.inviteTeamSlug = configrepo.InviteTeamName(inviteTestClassroom, inviteTestEmail)
	teamPath := "/orgs/" + inviteTestOrg + "/teams/" + m.inviteTeamSlug

	base.HandleFunc("/user", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"login": inviteTestActor, "id": 1})
	})

	base.HandleFunc("/orgs/o/teams", func(w http.ResponseWriter, r *http.Request) {
		if m.createStatus != 0 && m.createStatus != http.StatusCreated {
			w.WriteHeader(m.createStatus)
			_, _ = w.Write([]byte(`{"message":"Name must be unique for this org"}`))
			return
		}
		m.teamCreated = true
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": inviteTestInviteTeamID, "slug": m.inviteTeamSlug, "privacy": "secret",
		})
	})

	base.HandleFunc(teamPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			if status := m.inviteTeamDeleteStatus; status != 0 && status != http.StatusNoContent {
				w.WriteHeader(status)
				return
			}
			m.deletedTeamSlug = m.inviteTeamSlug
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method == http.MethodGet {
			if m.inviteTeamRateLimited {
				writeSecondaryRateLimit403(w)
				return
			}
			if status := m.teamStatus(); status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": inviteTestInviteTeamID, "slug": m.inviteTeamSlug,
				"privacy": "secret", "description": m.inviteTeamDescription,
			})
			return
		}
		var body struct {
			Description string `json:"description"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Description != "" {
			m.inviteTeamDescription = body.Description
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": inviteTestInviteTeamID, "slug": m.inviteTeamSlug,
			"privacy": "secret", "description": body.Description,
		})
	})
	base.HandleFunc(teamPath+"/memberships/"+inviteTestActor, func(w http.ResponseWriter, r *http.Request) {
		// The drop removes the acting teacher and nobody else, so a team that
		// holds someone else stays not-empty.
		if r.Method == http.MethodDelete {
			m.inviteTeamMembers = slices.DeleteFunc(m.inviteTeamMembers, func(member map[string]any) bool {
				return member["login"] == inviteTestActor
			})
		}
		w.WriteHeader(http.StatusNoContent)
	})
	base.HandleFunc(teamPath+"/members", func(w http.ResponseWriter, r *http.Request) {
		status := m.teamStatus()
		if m.inviteTeamMembersStatus != 0 {
			status = m.inviteTeamMembersStatus
		}
		if status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		members := m.inviteTeamMembers
		if members == nil {
			members = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(members)
	})

	base.HandleFunc("/orgs/o/invitations", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			if m.pendingRateLimited {
				writeSecondaryRateLimit403(w)
				return
			}
			if status := m.pendingStatus; status != 0 && status != http.StatusOK {
				w.WriteHeader(status)
				return
			}
			pending := m.pending
			if pending == nil {
				pending = []map[string]any{}
			}
			_ = json.NewEncoder(w).Encode(pending)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&m.invitationBody)
		if m.invitationRateLimited {
			w.Header().Set("Retry-After", "60")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
			return
		}
		status := m.invitationStatus
		if status == 0 {
			status = http.StatusCreated
		}
		if status == http.StatusUnprocessableEntity {
			message := m.invitation422Message
			if message == "" {
				message = "Invitee is already a part of this org"
			}
			// go-gh only reads the message off a JSON content type.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": message})
			return
		}
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})

	base.HandleFunc("/orgs/o/failed_invitations", func(w http.ResponseWriter, r *http.Request) {
		if m.failedRateLimited {
			writeSecondaryRateLimit403(w)
			return
		}
		if status := m.failedStatus; status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		failed := m.failed
		if failed == nil {
			failed = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(failed)
	})
	base.HandleFunc("/orgs/o/invitations/", func(w http.ResponseWriter, r *http.Request) {
		if status := m.dismissStatus; status != 0 && status != http.StatusNoContent {
			w.WriteHeader(status)
			return
		}
		m.dismissed = append(m.dismissed, strings.TrimPrefix(r.URL.Path, "/orgs/o/invitations/"))
		w.WriteHeader(http.StatusNoContent)
	})

	// commitFails intercepts before the mux so the tree POST never reaches it;
	// afterCommit runs once the mux has served it.
	failing := http.Handler(base)
	if m.commitFails || m.afterCommit != nil {
		failing = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			isTreePost := r.Method == http.MethodPost && r.URL.Path == "/repos/o/classroom50/git/trees"
			if isTreePost && m.commitFails {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			base.ServeHTTP(w, r)
			if isTreePost && m.afterCommit != nil {
				m.afterCommit()
			}
		})
	}
	return recordCalls(&m.calls, failing)
}

// teamStatus is the invite team's current status: a 404 lasts only until this
// run recreates the team.
func (m *inviteMock) teamStatus() int {
	if m.inviteTeamStatus == 0 || (m.inviteTeamStatus == http.StatusNotFound && m.teamCreated) {
		return http.StatusOK
	}
	return m.inviteTeamStatus
}

// writeSecondaryRateLimit403 answers as GitHub does when a secondary limit
// trips: a 403 that only Retry-After and the body distinguish from a denial.
func writeSecondaryRateLimit403(w http.ResponseWriter) {
	w.Header().Set("Retry-After", "60")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"message":"You have exceeded a secondary rate limit"}`))
}

// inviteTestRecord is the valid v1 record an earlier run wrote onto the invite
// team for inviteTestEmail.
func inviteTestRecord(t *testing.T) string {
	t.Helper()
	record, err := configrepo.MarshalInviteDescription(inviteTestClassroom, inviteTestEmail)
	if err != nil {
		t.Fatalf("marshal invite record: %v", err)
	}
	return record
}

// inviteTestClassroomJSON records the classroom team the invitation must carry.
func inviteTestClassroomJSON(t *testing.T) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"name": inviteTestClassroom,
		"team": map[string]any{"id": inviteTestClassroomTeamID, "slug": "classroom50-" + inviteTestClassroom},
	})
	if err != nil {
		t.Fatalf("marshal classroom.json: %v", err)
	}
	return string(b)
}

// runInvite drives runRosterInvite against a scripted server.
func runInvite(t *testing.T, mock *inviteMock) (string, string, error) {
	t.Helper()
	server := httptest.NewServer(mock.handler(t))
	t.Cleanup(server.Close)
	client := githubtest.NewTestClient(t, server)

	var out, errOut bytes.Buffer
	err := runRosterInvite(client, &out, &errOut,
		inviteTestOrg, inviteTestClassroom, inviteTestEmail, "Ada", "Lovelace", "section-1")
	return out.String(), errOut.String(), err
}

// newInviteMock is a mock with a resolvable classroom team and an empty roster.
func newInviteMock(t *testing.T, rosterCSV string) *inviteMock {
	t.Helper()
	return &inviteMock{
		rosterWriteMock: &rosterWriteMock{files: map[string]string{
			inviteTestClassroom + "/roster.csv":     rosterCSV,
			inviteTestClassroom + "/classroom.json": inviteTestClassroomJSON(t),
		}},
	}
}

// TestRunRosterInvite_HappyPath is the AE2 artifact leg: the invite team is
// created secret carrying only the provisional description, the acting teacher
// is dropped, the record lands last, the invitation carries BOTH team ids, and
// only then does the pending roster row get committed.
func TestRunRosterInvite_HappyPath(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	out, _, err := runInvite(t, mock)
	if err != nil {
		t.Fatalf("runRosterInvite: %v", err)
	}

	slug := mock.inviteTeamSlug
	teamPath := "/orgs/o/teams/" + slug
	createIdx := indexOfCall(mock.calls, http.MethodPost, "/orgs/o/teams")
	dropIdx := indexOfCall(mock.calls, http.MethodDelete, teamPath+"/memberships/"+inviteTestActor)
	membersIdx := indexOfCall(mock.calls, http.MethodGet, teamPath+"/members")
	recordIdx := indexOfCall(mock.calls, http.MethodPatch, teamPath)
	inviteIdx := indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations")
	treeIdx := indexOfCall(mock.calls, http.MethodPost, "/repos/o/classroom50/git/trees")
	sequence := []struct {
		name string
		idx  int
	}{
		{"team create", createIdx}, {"actor drop", dropIdx}, {"members read-back", membersIdx},
		{"record PATCH", recordIdx}, {"invitation POST", inviteIdx}, {"roster tree POST", treeIdx},
	}
	for i, step := range sequence {
		if step.idx < 0 {
			t.Fatalf("%s never happened; calls = %#v", step.name, mock.calls)
		}
		if i > 0 && step.idx < sequence[i-1].idx {
			t.Errorf("%s (call %d) came before %s (call %d); the send order is load-bearing",
				step.name, step.idx, sequence[i-1].name, sequence[i-1].idx)
		}
	}

	if got := mock.calls[createIdx].Description; strings.Contains(got, inviteTestEmail) {
		t.Errorf("create carried the invited email (%q); an interrupted run would strand it", got)
	}
	wantRecord, err := configrepo.MarshalInviteDescription(inviteTestClassroom, inviteTestEmail)
	if err != nil {
		t.Fatalf("MarshalInviteDescription: %v", err)
	}
	if got := mock.calls[recordIdx].Description; got != wantRecord {
		t.Errorf("record PATCH description = %q, want %q", got, wantRecord)
	}

	if got := mock.invitationBody["email"]; got != inviteTestEmail {
		t.Errorf("invitation email = %v, want %s", got, inviteTestEmail)
	}
	if got := mock.invitationBody["role"]; got != "direct_member" {
		t.Errorf("invitation role = %v, want direct_member", got)
	}
	teamIDs, _ := mock.invitationBody["team_ids"].([]any)
	if len(teamIDs) != 2 || teamIDs[0] != float64(inviteTestClassroomTeamID) || teamIDs[1] != float64(inviteTestInviteTeamID) {
		t.Errorf("invitation team_ids = %v, want [%d %d]", teamIDs, inviteTestClassroomTeamID, inviteTestInviteTeamID)
	}

	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted team %q on the happy path", mock.deletedTeamSlug)
	}
	if len(mock.blobs) != 1 {
		t.Fatalf("got %d blobs POSTed, want 1: %#v", len(mock.blobs), mock.blobs)
	}
	rows, err := configrepo.ParseRoster([]byte(mock.blobs[0]))
	if err != nil {
		t.Fatalf("parse committed roster: %v\n%s", err, mock.blobs[0])
	}
	if len(rows) != 1 {
		t.Fatalf("committed %d rows, want 1:\n%s", len(rows), mock.blobs[0])
	}
	row := rows[0]
	if row.Username != "" || row.GitHubID != 0 {
		t.Errorf("pending row must carry no identity, got username %q id %d", row.Username, row.GitHubID)
	}
	if row.Email != inviteTestEmail || row.FirstName != "Ada" || row.LastName != "Lovelace" || row.Section != "section-1" {
		t.Errorf("pending row lost its email/metadata: %#v", row)
	}
	if row.Role != "student" {
		t.Errorf("pending row role = %q, want student (byte parity with the web's row)", row.Role)
	}
	if !strings.Contains(out, inviteTestEmail) {
		t.Errorf("stdout should name the invited address:\n%s", out)
	}
}

// An unresolvable classroom team must abort BEFORE anything is created or sent:
// a team-less email invite lands the student in the org attached to nothing.
func TestRunRosterInvite_ClassroomTeamMissing(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	delete(mock.files, inviteTestClassroom+"/classroom.json") // → ResolveClassroomTeam ok=false

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want a hard failure naming the missing classroom team")
	}
	if !strings.Contains(err.Error(), "classroom add") {
		t.Errorf("error should point at `classroom add`: %v", err)
	}
	if n := countCalls(mock.calls, http.MethodPost, "/orgs/o/teams"); n != 0 {
		t.Errorf("created %d invite team(s) with no classroom team to attach", n)
	}
	if n := countCalls(mock.calls, http.MethodPost, "/orgs/o/invitations"); n != 0 {
		t.Errorf("sent %d invitation(s) with no classroom team", n)
	}
	if len(mock.blobs) != 0 {
		t.Errorf("committed %d blob(s) on a blocked send", len(mock.blobs))
	}
}

// GitHub's 422 means the address is already a member or already invited —
// nothing to send, so it's a skip (exit 0), and the team this run created is
// removed rather than left for the GC.
func TestRunRosterInvite_InvitationAlreadyMemberSkips(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	mock.invitationStatus = http.StatusUnprocessableEntity

	out, _, err := runInvite(t, mock)
	if err != nil {
		t.Fatalf("a 422 must be a skip, not an error: %v", err)
	}
	if !strings.Contains(out, "skipped") {
		t.Errorf("stdout should report the skip:\n%s", out)
	}
	if mock.deletedTeamSlug != mock.inviteTeamSlug {
		t.Errorf("deleted team = %q, want the team this run created (%q)", mock.deletedTeamSlug, mock.inviteTeamSlug)
	}
	if len(mock.blobs) != 0 {
		t.Errorf("a skipped send must write no roster row, got %d blob(s)", len(mock.blobs))
	}
}

// A hard invitation failure deletes only what this run created: a fresh team
// holds nothing anyone can recover, so leaving it would only feed the GC.
func TestRunRosterInvite_InvitationFailureDeletesCreatedTeam(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	mock.invitationStatus = http.StatusInternalServerError

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want the invitation failure to propagate")
	}
	if mock.deletedTeamSlug != mock.inviteTeamSlug {
		t.Errorf("deleted team = %q, want the team this run created", mock.deletedTeamSlug)
	}
	if len(mock.blobs) != 0 {
		t.Errorf("a failed send must write no roster row, got %d blob(s)", len(mock.blobs))
	}
}

// An ADOPTED team may hold an earlier invite's still-unrecovered record, so a
// failure must leave it standing — deleting it would destroy the only
// email→account mapping that invite has.
func TestRunRosterInvite_InvitationFailureKeepsAdoptedTeam(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	mock.createStatus = http.StatusUnprocessableEntity // name taken → adopt
	mock.invitationStatus = http.StatusInternalServerError

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want the invitation failure to propagate")
	}
	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted the adopted team %q; it may hold an earlier unrecovered record", mock.deletedTeamSlug)
	}
}

// A rate-limited send keeps the metadata team so a retry adopts it, matching the
// web's bulkInviteByEmail (`if (!rateLimited && inviteTeam.created)`). Deleting
// it would make every retry re-create the team, feeding the same limit.
func TestRunRosterInvite_RateLimitedSendKeepsCreatedTeam(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	mock.invitationRateLimited = true

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want the rate limit to propagate")
	}
	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted team %q on a rate limit; a retry must be able to adopt it", mock.deletedTeamSlug)
	}
	if len(mock.blobs) != 0 {
		t.Errorf("a rate-limited send must write no roster row, got %d blob(s)", len(mock.blobs))
	}
}

// An address an account row already carries still gets its invitation (the web
// sends too), but NOT a second row: appendEmailInviteRows skips any claimed
// address, so appending one here would leave a duplicate for the reconcile.
func TestRunRosterInvite_AddressOnAnAccountRowWarnsAndSends(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+"sibling,Sib,Ling,"+inviteTestEmail+",,101,student\n")

	out, errOut, err := runInvite(t, mock)
	if err != nil {
		t.Fatalf("a shared address must not block the send: %v", err)
	}
	if !strings.Contains(errOut, "sibling") {
		t.Errorf("stderr should name the account already holding the address:\n%s", errOut)
	}
	if indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations") < 0 {
		t.Fatalf("the invitation was never sent; calls = %#v", mock.calls)
	}
	if len(mock.blobs) != 0 {
		t.Fatalf("appended a second row for an already-claimed address: %#v", mock.blobs)
	}
	if !strings.Contains(out, "roster unchanged") {
		t.Errorf("stdout should report the row was skipped:\n%s", out)
	}
}

// A pending row is only a reason to refuse while GitHub still lists its
// invitation: then a second send would be a no-op on GitHub and a duplicate on
// the roster, so refuse before any API write and name both remedies.
func TestRunRosterInvite_PendingRowWithLiveInvitationRefused(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = []map[string]any{{"id": 42, "email": inviteTestEmail, "role": "direct_member"}}

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want a refusal naming the existing pending invitation")
	}
	for _, want := range []string{"pending invitation", "roster sync", "cancel-invite"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if writes := writeCalls(mock.calls); len(writes) != 0 {
		t.Errorf("wrote %d request(s) before the refusal: %#v", len(writes), writes)
	}
}

// The issue #970 deadlock: an org invitation expires after 7 days, GitHub drops
// it from the pending list, the sync reaps the empty invite team, and the
// pending row is left with nothing backing it. The web offers Re-invite for
// exactly this row, so `roster invite` sends again against the same row: a
// fresh team, a fresh invitation, NO second row, and GitHub's expired record
// dismissed once the new invitation is confirmed (the web's
// dismissFailedInvitation ordering).
func TestRunRosterInvite_ExpiredInvitationIsReinvitedOnTheSameRow(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",Ada,Lovelace,"+inviteTestEmail+",section-1,,student\n")
	mock.pending = nil
	mock.inviteTeamStatus = http.StatusNotFound // the sync already GC'd the team
	mock.failed = []map[string]any{
		{"id": 70, "email": inviteTestEmail},
		{"id": 71, "email": "someone-else@uni.edu"},
	}

	out, errOut, err := runInvite(t, mock)
	if err != nil {
		t.Fatalf("an expired invitation must be re-sent, not refused: %v", err)
	}
	inviteIdx := indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations")
	if inviteIdx < 0 {
		t.Fatalf("no invitation was sent; calls = %#v", mock.calls)
	}
	if got := mock.invitationBody["email"]; got != inviteTestEmail {
		t.Errorf("invitation email = %v, want %s", got, inviteTestEmail)
	}
	if len(mock.blobs) != 0 {
		t.Errorf("wrote a second row for an address the pending row already carries: %#v", mock.blobs)
	}
	if !strings.Contains(errOut, "expire") {
		t.Errorf("stderr should explain that the earlier invitation expired:\n%s", errOut)
	}
	if !strings.Contains(out, "roster unchanged") {
		t.Errorf("stdout should report the existing row was kept:\n%s", out)
	}
	// Only this address's record goes, and only after the send is confirmed.
	if len(mock.dismissed) != 1 || mock.dismissed[0] != "70" {
		t.Errorf("dismissed = %v, want exactly record 70 (not someone else's 71)", mock.dismissed)
	}
	if dismissIdx := indexOfCall(mock.calls, http.MethodDelete, "/orgs/o/invitations/70"); dismissIdx < inviteIdx {
		t.Errorf("record dismissed (call %d) before the new invitation was sent (call %d)", dismissIdx, inviteIdx)
	}
	if !strings.Contains(out, "dismissed 1 expired invitation record") {
		t.Errorf("stdout should report the dismissed record:\n%s", out)
	}
}

// The failed list is owner-only and the record is bookkeeping, so an unreadable
// list or a failed DELETE can never fail a send that already went out: a plain
// 403 or a 404 is silent, anything else (including a rate limit, which GitHub
// also sends as a 403) warns, a record already gone counts as dismissed, and the
// exit code stays 0.
func TestRunRosterInvite_FailedRecordProblemsNeverFailTheSend(t *testing.T) {
	cases := []struct {
		name          string
		apply         func(*inviteMock)
		wantWarn      bool
		wantDismissed bool
	}{
		{"failed list 403 is silent", func(m *inviteMock) { m.failedStatus = http.StatusForbidden }, false, false},
		{"failed list 404 is silent", func(m *inviteMock) { m.failedStatus = http.StatusNotFound }, false, false},
		{"failed list rate limit warns", func(m *inviteMock) { m.failedRateLimited = true }, true, false},
		{"failed list 500 warns", func(m *inviteMock) { m.failedStatus = http.StatusInternalServerError }, true, false},
		{"dismiss 500 warns", func(m *inviteMock) {
			m.failed = []map[string]any{{"id": 70, "email": inviteTestEmail}}
			m.dismissStatus = http.StatusInternalServerError
		}, true, false},
		{"dismiss 404 counts as dismissed", func(m *inviteMock) {
			m.failed = []map[string]any{{"id": 70, "email": inviteTestEmail}}
			m.dismissStatus = http.StatusNotFound
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
			mock.pending = nil
			tc.apply(mock)

			out, errOut, err := runInvite(t, mock)
			if err != nil {
				t.Fatalf("bookkeeping must not fail the send: %v", err)
			}
			if indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations") < 0 {
				t.Fatal("the invitation itself was never sent")
			}
			// Silence must come from tolerating the read, not from skipping it.
			if n := countCalls(mock.calls, http.MethodGet, "/orgs/o/failed_invitations"); n != 1 {
				t.Errorf("failed-list reads = %d, want 1", n)
			}
			if got := strings.Contains(errOut, "failed_invitations"); got != tc.wantWarn {
				t.Errorf("warning pointing at GitHub's failed-invitations page = %v, want %v:\n%s", got, tc.wantWarn, errOut)
			}
			if got := strings.Contains(out, "dismissed 1 expired invitation record"); got != tc.wantDismissed {
				t.Errorf("dismissed report = %v, want %v:\n%s", got, tc.wantDismissed, out)
			}
		})
	}
}

// GitHub's "already invited or a member" 422 on a re-invite means something live
// now covers the address, so the expired record is noise here too.
func TestRunRosterInvite_ReinviteAlreadyCoveredStillDismisses(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = nil
	mock.invitationStatus = http.StatusUnprocessableEntity
	mock.failed = []map[string]any{{"id": 70, "email": inviteTestEmail}}

	if _, _, err := runInvite(t, mock); err != nil {
		t.Fatalf("a 422 is a skip: %v", err)
	}
	if len(mock.dismissed) != 1 || mock.dismissed[0] != "70" {
		t.Errorf("dismissed = %v, want record 70", mock.dismissed)
	}
}

// GitHub uses the same 422 for its invitation cap. Nothing covers the address
// after a capped send, so the expired record must stay, the fresh team must
// stay for a retry to adopt, and the outcome is a deferral, not a skip.
func TestRunRosterInvite_InvitationCapKeepsRecordAndTeam(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = nil
	mock.invitationStatus = http.StatusUnprocessableEntity
	mock.invitation422Message = "Over invitation rate limit. Try again later."
	mock.failed = []map[string]any{{"id": 70, "email": inviteTestEmail}}

	_, _, err := runInvite(t, mock)
	if err == nil || !strings.Contains(err.Error(), "invitation limit") {
		t.Fatalf("err = %v, want the invitation limit reported", err)
	}
	if len(mock.dismissed) != 0 {
		t.Errorf("dismissed %v although nothing covers the address", mock.dismissed)
	}
	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted the fresh team %q; a retry should adopt it", mock.deletedTeamSlug)
	}
	if len(mock.blobs) != 0 {
		t.Errorf("wrote a roster row for an address that was not invited: %#v", mock.blobs)
	}
}

// A GC'd team is the common expired shape, but a team the sync hasn't reaped yet
// (younger than the GC age) is the same case: no invitation, no member. The send
// must adopt that team rather than trip over the name collision.
func TestRunRosterInvite_ExpiredInvitationAdoptsSurvivingTeam(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = nil
	mock.inviteTeamDescription = inviteTestRecord(t)
	mock.createStatus = http.StatusUnprocessableEntity // name taken → adopt

	_, _, err := runInvite(t, mock)
	if err != nil {
		t.Fatalf("a surviving empty team must be adopted: %v", err)
	}
	if indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations") < 0 {
		t.Fatalf("no invitation was sent; calls = %#v", mock.calls)
	}
	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted the adopted team %q", mock.deletedTeamSlug)
	}
}

// A re-invite interrupted after the team create (a rate limit, a 5xx) strands a
// provisional team with the acting teacher on it: GitHub adds the creator, and
// the drop is a later request. That member is not an invitee (the team holds no
// record, so no invitation ever carried it), and the sync skips a provisional
// team rather than repairing it. Reading it as "accepted" would refuse this
// row forever; it must be re-sent, with EnsureInviteTeam adopting and healing
// the team.
func TestRunRosterInvite_StrandedProvisionalTeamIsReinvited(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = nil
	mock.inviteTeamDescription = contract.InviteProvisionalDescription
	mock.inviteTeamMembers = []map[string]any{{"login": inviteTestActor, "id": 1}}
	mock.createStatus = http.StatusUnprocessableEntity // the stranded team's name

	_, _, err := runInvite(t, mock)
	if err != nil {
		t.Fatalf("a stranded provisional team must be adopted, not read as accepted: %v", err)
	}
	if indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations") < 0 {
		t.Fatalf("no invitation was sent; calls = %#v", mock.calls)
	}
	if indexOfCall(mock.calls, http.MethodDelete, "/orgs/o/teams/"+mock.inviteTeamSlug+"/memberships/"+inviteTestActor) < 0 {
		t.Errorf("the stranded teacher was not dropped from the adopted team; calls = %#v", mock.calls)
	}
}

// A record-less team that is NOT provisional was edited by hand (the accepted
// invitee owns their own team's description), so its member may well be the
// student. The sync reports that shape as an anomaly and leaves it alone; the
// re-invite must refuse the same way rather than adopt the team and tell the
// teacher to remove the member.
func TestRunRosterInvite_HandEditedTeamWithMemberRefused(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = nil
	mock.inviteTeamDescription = "not an invite record"
	mock.inviteTeamMembers = []map[string]any{{"login": "ada", "id": 99}}

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want a refusal naming the unreadable record")
	}
	for _, want := range []string{"readable invite record", mock.inviteTeamSlug} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "remove them") {
		t.Errorf("must not tell the teacher to strip a member who may be the student: %v", err)
	}
	if writes := writeCalls(mock.calls); len(writes) != 0 {
		t.Errorf("wrote %d request(s) for a team a human must check: %#v", len(writes), writes)
	}
}

// A degraded read of the invite team (its GET or its members) refuses like a
// degraded pending-list read: neither "dead" nor "accepted" can be proven, and
// a rate limit must keep its shape so a bulk run defers instead of failing.
func TestRunRosterInvite_DegradedTeamReadRefuses(t *testing.T) {
	cases := []struct {
		name          string
		apply         func(*inviteMock)
		wantRateLimit bool
	}{
		{"team GET 500", func(m *inviteMock) { m.inviteTeamStatus = http.StatusInternalServerError }, false},
		{"team GET rate limited", func(m *inviteMock) { m.inviteTeamRateLimited = true }, true},
		{"members GET 500 on a recorded team", func(m *inviteMock) {
			m.inviteTeamDescription = inviteTestRecord(t)
			m.inviteTeamMembersStatus = http.StatusInternalServerError
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
			mock.pending = nil
			tc.apply(mock)

			_, _, err := runInvite(t, mock)
			if err == nil {
				t.Fatal("err = nil, want the degraded read to refuse")
			}
			if got := cliutil.IsRateLimited(err); got != tc.wantRateLimit {
				t.Errorf("IsRateLimited = %v, want %v: %v", got, tc.wantRateLimit, err)
			}
			if writes := writeCalls(mock.calls); len(writes) != 0 {
				t.Errorf("wrote %d request(s) after a degraded read: %#v", len(writes), writes)
			}
		})
	}
}

// No pending invitation plus a member on a recorded invite team means the
// student ACCEPTED and only the sync is missing (common in the CLI, where the
// sync is manual). Sending again would only trip EnsureInviteTeam's not-empty
// check with a message telling the teacher to remove the invitee from the team,
// which would destroy the only email→account mapping. Refuse, and point at the
// sync with --write, since a dry run records nothing.
func TestRunRosterInvite_AcceptedButUnsyncedRefused(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pending = nil
	mock.inviteTeamDescription = inviteTestRecord(t)
	mock.inviteTeamMembers = []map[string]any{{"login": "ada", "id": 99}}

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want a refusal pointing at `roster sync`")
	}
	for _, want := range []string{"accepted", "roster sync", "--write"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}
	if writes := writeCalls(mock.calls); len(writes) != 0 {
		t.Errorf("wrote %d request(s) for an accepted invitation: %#v", len(writes), writes)
	}
}

// Liveness is only knowable from GitHub, so a failed pending-list read refuses
// rather than guessing either way: re-sending could duplicate a live invitation,
// and refusing forever is the deadlock this path exists to break. A rate limit
// on that read is the one failure that must keep its shape: GitHub sends
// secondary limits as a 403, and the classifier's admin-access message for a
// 403 would send the teacher to `gh teacher login` for a throttle.
func TestRunRosterInvite_PendingListReadFailureRefuses(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	mock.pendingStatus = http.StatusInternalServerError

	_, _, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want the pending-list read failure to propagate")
	}
	if writes := writeCalls(mock.calls); len(writes) != 0 {
		t.Errorf("wrote %d request(s) after a degraded read: %#v", len(writes), writes)
	}

	throttled := newInviteMock(t, storedRosterHeader+",,,"+inviteTestEmail+",,,student\n")
	throttled.pendingRateLimited = true
	_, _, err = runInvite(t, throttled)
	if err == nil {
		t.Fatal("err = nil, want the rate limit to propagate")
	}
	if !cliutil.IsRateLimited(err) {
		t.Errorf("a throttled pending-list read must still read as a rate limit, got: %v", err)
	}
	if strings.Contains(err.Error(), "admin") {
		t.Errorf("a throttle must not be reported as an admin-access problem: %v", err)
	}
}

// A fresh address never needs either invitation list, so it must read neither:
// both are owner-scoped and paginated, and a fresh invite must not gain a new way
// to fail.
func TestRunRosterInvite_FreshAddressSkipsTheInvitationListReads(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	mock.pendingStatus = http.StatusInternalServerError // would fail if read
	mock.failedStatus = http.StatusInternalServerError  // would warn if read

	if _, errOut, err := runInvite(t, mock); err != nil {
		t.Fatalf("a fresh address must not depend on the invitation lists: %v", err)
	} else if strings.Contains(errOut, "Warning") {
		t.Errorf("a fresh address read the failed list:\n%s", errOut)
	}
	for _, path := range []string{"/orgs/o/invitations", "/orgs/o/failed_invitations"} {
		if n := countCalls(mock.calls, http.MethodGet, path); n != 0 {
			t.Errorf("read %s %d time(s) for a fresh address", path, n)
		}
	}
}

// The invitation is the source of truth once sent, so a failed roster write is
// never rolled back: warn, name `roster sync` as the repair, and still exit
// non-zero so a script sees the partial state.
func TestRunRosterInvite_RosterWriteFailureWarnsAndFails(t *testing.T) {
	mock := newInviteMock(t, storedRosterHeader)
	mock.commitFails = true

	_, errOut, err := runInvite(t, mock)
	if err == nil {
		t.Fatal("err = nil, want a non-zero exit for the partial state")
	}
	if !strings.Contains(errOut, "roster sync") {
		t.Errorf("stderr must name `roster sync` as the repair:\n%s", errOut)
	}
	if indexOfCall(mock.calls, http.MethodPost, "/orgs/o/invitations") < 0 {
		t.Fatal("the invitation should have been sent before the roster write")
	}
	if mock.deletedTeamSlug != "" {
		t.Errorf("deleted team %q after a SENT invitation; the record must outlive the row", mock.deletedTeamSlug)
	}
}

// Arg/flag validation runs inside RunE before any auth or network, so these
// need no server.
func TestRosterInviteCmd(t *testing.T) {
	run := func(t *testing.T, args ...string) error {
		t.Helper()
		return runRosterSubcommand(t, rosterInviteCmd(), args...)
	}

	t.Run("blank email is rejected before any auth/network", func(t *testing.T) {
		err := run(t, "o", "cs-principles", "   ")
		if err == nil || !strings.Contains(err.Error(), "non-empty") {
			t.Fatalf("err = %v, want a non-empty email error", err)
		}
	})

	t.Run("invalid email is rejected before any auth/network", func(t *testing.T) {
		err := run(t, "o", "cs-principles", "Ada <ada@uni.edu>")
		if err == nil || !strings.Contains(err.Error(), "invalid email") {
			t.Fatalf("err = %v, want 'invalid email'", err)
		}
	})

	t.Run("carries roster add's metadata flags and no --role", func(t *testing.T) {
		flags := rosterInviteCmd().Flags()
		for _, name := range []string{"first-name", "last-name", "section"} {
			if flags.Lookup(name) == nil {
				t.Errorf("missing --%s", name)
			}
		}
		// CLI email invites are student-role only: the web gates an owner grant
		// behind a confirmation the CLI has no equivalent for.
		if flags.Lookup("role") != nil {
			t.Error("--role must not exist on `roster invite`")
		}
	})

	t.Run("has a --file flag for bulk invites", func(t *testing.T) {
		if rosterInviteCmd().Flags().Lookup("file") == nil {
			t.Error("missing --file")
		}
	})

	// --file with a positional email is rejected before any network call.
	t.Run("both a positional email and --file is an arg error", func(t *testing.T) {
		err := run(t, "o", "cs-principles", "ada@uni.edu", "--file", "list.txt")
		if err == nil || !strings.Contains(err.Error(), "--file") {
			t.Fatalf("err = %v, want an arg error naming --file", err)
		}
	})

	t.Run("--file at a nonexistent path errors before network", func(t *testing.T) {
		err := run(t, "o", "cs-principles", "--file", "/no/such/list-file.txt")
		if err == nil || !strings.Contains(err.Error(), "read") {
			t.Fatalf("err = %v, want a read error", err)
		}
	})

	// A per-student flag can't apply to a list; silently dropping it would lose
	// metadata the teacher believes they set.
	t.Run("metadata flags are rejected with --file", func(t *testing.T) {
		for _, flag := range []string{"--first-name", "--last-name", "--section"} {
			err := run(t, "o", "cs-principles", "--file", "list.txt", flag, "x")
			if err == nil || !strings.Contains(err.Error(), flag) {
				t.Errorf("%s with --file: err = %v, want it rejected by name", flag, err)
			}
		}
	})
}

// The three email-lifecycle subcommands must be reachable and discoverable: a
// registration miss is invisible without this, since the package compiles fine.
func TestRosterCmdRegistersInviteLifecycleSubcommands(t *testing.T) {
	for _, name := range []string{"invite", "cancel-invite", "sync"} {
		t.Run(name, func(t *testing.T) {
			var found bool
			for _, sub := range NewCmd().Commands() {
				if sub.Name() == name {
					found = true
				}
			}
			if !found {
				t.Errorf("`roster %s` is not registered on the roster command", name)
			}
			if !strings.Contains(NewCmd().Long, name) {
				t.Errorf("the roster subcommand summary should list %s", name)
			}
		})
	}
}
