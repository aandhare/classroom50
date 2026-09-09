package submitcmd

// These tests exist to prove a regression fix, not just exercise the happy
// path: before this fix, resolveRepoDefaultBranch, fetchRepoPath,
// commitWorkTreeOnRemoteBranch, and pushSubmitTag could all hang
// indefinitely against a connection that accepts but never responds (a
// stalled network, dead VPN, silent firewall drop). Each test below sets up
// exactly that — a server that never answers — and asserts the call still
// returns, with an error, close to its configured timeout rather than
// blocking the test (and, in production, the user's terminal) forever.
//
// slowTestTimeout is deliberately generous headroom above each shrunk
// timeout: it's the ceiling that would fail the test if the fix regressed
// back to "hangs forever", not a tight bound on exact timing.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"
	"time"

	"github.com/foundation50/gh-student/internal/githubtest"
	identitypkg "github.com/foundation50/gh-student/internal/identity"
)

const slowTestTimeout = 2 * time.Second

// neverRespondHandler accepts the request but writes nothing back until the
// test itself unblocks it (on cleanup) or a generous safety net fires. It
// simulates a stalled connection, not a slow-but-completing one.
func neverRespondHandler(t *testing.T) http.HandlerFunc {
	t.Helper()
	unblock := make(chan struct{})
	t.Cleanup(func() { close(unblock) })
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-time.After(slowTestTimeout):
		}
	}
}

func TestResolveRepoDefaultBranch_FailsFastOnStalledConnection(t *testing.T) {
	restore := defaultBranchTimeout
	defaultBranchTimeout = 50 * time.Millisecond
	t.Cleanup(func() { defaultBranchTimeout = restore })

	server := httptest.NewServer(neverRespondHandler(t))
	defer server.Close()

	start := time.Now()
	_, err := resolveRepoDefaultBranch(context.Background(), githubtest.NewTestClient(t, server), "o", "repo")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error against a stalled connection, got nil")
	}
	if elapsed > slowTestTimeout {
		t.Fatalf("resolveRepoDefaultBranch took %v to fail (timeout was 50ms); "+
			"it should fail near the timeout, not hang", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected error to wrap context.DeadlineExceeded, got: %v", err)
	}
}

func TestFetchRepoPath_FailsFastOnStalledConnection(t *testing.T) {
	restore := teacherFileRefreshTimeout
	teacherFileRefreshTimeout = 50 * time.Millisecond
	t.Cleanup(func() { teacherFileRefreshTimeout = restore })

	server := httptest.NewServer(neverRespondHandler(t))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), teacherFileRefreshTimeout)
	defer cancel()

	start := time.Now()
	err := fetchRepoPath(ctx, githubtest.NewTestClient(t, server), t.TempDir(), "o", "repo", "main", ".github")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error against a stalled connection, got nil")
	}
	if elapsed > slowTestTimeout {
		t.Fatalf("fetchRepoPath took %v to fail (timeout was 50ms); "+
			"it should fail near the timeout, not hang", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected error to wrap context.DeadlineExceeded, got: %v", err)
	}
}

func TestCommitWorkTreeOnRemoteBranch_FailsFastOnStalledClone(t *testing.T) {
	restoreDelay := cmdWaitDelay
	cmdWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() { cmdWaitDelay = restoreDelay })

	// A TCP listener that never calls Accept() still completes the TCP
	// handshake (the kernel does that via the listen backlog), but no
	// application data ever flows — exactly what a stalled connection looks
	// like to the git subprocess trying to clone it. The git process itself
	// gets killed at the context deadline, but a grandchild transport
	// helper (git-remote-http) can outlive it, still blocked on that same
	// stalled socket — which is exactly what cmdWaitDelay bounds.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	remoteURL := fmt.Sprintf("http://%s/o/repo.git", ln.Addr().String())

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err = commitWorkTreeOnRemoteBranch(
		ctx,
		t.TempDir(),
		t.TempDir(),
		remoteURL,
		"main",
		"Submit test",
		identitypkg.GitIdentity{Name: "Test Student", Email: "test@example.com"},
		io.Discard,
		io.Discard,
	)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected clone to fail against a stalled server, got nil")
	}
	if elapsed > slowTestTimeout {
		t.Fatalf("commitWorkTreeOnRemoteBranch took %v to fail (timeout was 200ms); "+
			"it should fail near the timeout, not hang", elapsed)
	}
}

func TestPushSubmitTag_FailsFastOnStalledRemote(t *testing.T) {
	restoreDelay := cmdWaitDelay
	cmdWaitDelay = 50 * time.Millisecond
	t.Cleanup(func() { cmdWaitDelay = restoreDelay })

	restoreTimeout := submitTagTimeout
	submitTagTimeout = 200 * time.Millisecond
	t.Cleanup(func() { submitTagTimeout = restoreTimeout })

	local, _, sha := _tagTestRepos(t)

	// Same stalled-listener mechanism as
	// TestCommitWorkTreeOnRemoteBranch_FailsFastOnStalledClone: point origin
	// at a socket that completes the TCP handshake but never answers, then
	// repoint the fixture's already-working origin at it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	stalledURL := fmt.Sprintf("http://%s/o/repo.git", ln.Addr().String())
	if out, err := exec.Command("git", "--git-dir", local, "remote", "set-url", "origin", stalledURL).CombinedOutput(); err != nil {
		t.Fatalf("remote set-url: %v\n%s", err, out)
	}

	start := time.Now()
	_, err = pushSubmitTag(context.Background(), local, sha)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected pushSubmitTag to fail against a stalled remote, got nil")
	}
	if elapsed > slowTestTimeout {
		t.Fatalf("pushSubmitTag took %v to fail (timeout was 200ms); "+
			"it should fail near the timeout, not hang", elapsed)
	}
}
