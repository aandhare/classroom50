import { describe, expect, it, vi } from "vitest"

import type { GitHubClient } from "@/github-core/client"

import { buildClassroomUpdate, editClassroom } from "./edit"

// classroom.json is a strict cross-binary contract (the Go gh-teacher CLI
// round-trips it with DisallowUnknownFields), so the edit merge must (a) only
// write fields the caller actually changed and (b) preserve everything else —
// including unknown/future fields a sibling binary wrote. Mirrors the
// present/absent discipline of util/yaml.test.ts.
describe("buildClassroomUpdate", () => {
  const base = {
    schema: "classroom50/classroom/v1",
    name: "Intro CS",
    short_name: "intro-cs",
    term: "Fall 2026",
    org: "acme",
  }

  it("writes a field only when provided; omits it otherwise", () => {
    expect(buildClassroomUpdate(base, { name: "Renamed" })).toEqual({
      ...base,
      name: "Renamed",
    })
    // A name-only edit leaves term untouched.
    const out = buildClassroomUpdate(base, { name: "Renamed" })
    expect(out.term).toBe("Fall 2026")
  })

  it("archive writes active:false; unarchive writes active:true (not delete)", () => {
    const archived = buildClassroomUpdate(base, { active: false })
    expect(archived.active).toBe(false)

    // Unarchiving an already-archived record overwrites false with true.
    const unarchived = buildClassroomUpdate(
      { ...base, active: false },
      { active: true },
    )
    expect(unarchived.active).toBe(true)
  })

  it("a pure archive toggle preserves the persisted name/term", () => {
    const out = buildClassroomUpdate(base, { active: false })
    expect(out.name).toBe("Intro CS")
    expect(out.term).toBe("Fall 2026")
    expect(out.active).toBe(false)
  })

  it("a name/term edit does NOT introduce an active key on a legacy record", () => {
    // Legacy classroom.json never wrote `active`; editing name/term must not
    // add it (absent = active).
    const out = buildClassroomUpdate(base, { name: "X", term: "Y" })
    expect("active" in out).toBe(false)
  })

  it("preserves unknown/future fields written by a sibling binary", () => {
    const withUnknown = {
      ...base,
      future_field: "from-newer-cli",
      nested: { a: 1 },
    }
    const out = buildClassroomUpdate(withUnknown, { active: false })
    expect(out.future_field).toBe("from-newer-cli")
    expect(out.nested).toEqual({ a: 1 })
  })

  it("omits every optional field when none are provided (identity merge)", () => {
    expect(buildClassroomUpdate(base, {})).toEqual(base)
  })

  it('pages_base_url: non-empty sets, undefined preserves, "" deletes', () => {
    const url = "https://pages.example.edu/classroom50"

    const set = buildClassroomUpdate(base, { pages_base_url: url })
    expect(set.pages_base_url).toBe(url)

    // An edit that omits the field (e.g. a pure archive toggle) must not
    // touch a persisted custom domain.
    const preserved = buildClassroomUpdate(
      { ...base, pages_base_url: url },
      { active: false },
    )
    expect(preserved.pages_base_url).toBe(url)

    // Clearing deletes the key outright — never writes an empty string an
    // old reader would trip on.
    const cleared = buildClassroomUpdate(
      { ...base, pages_base_url: url },
      { pages_base_url: "" },
    )
    expect("pages_base_url" in cleared).toBe(false)

    // Clearing an already-absent field stays a no-op (no key introduced).
    const noop = buildClassroomUpdate(base, { pages_base_url: "" })
    expect("pages_base_url" in noop).toBe(false)
  })
})

// editClassroom enforces "archived classrooms are read-only" on the write path
// — the authoritative guard, not just UI gating. The gate must (a) refuse a
// settings edit (name/term) on an archived classroom even when a crafted payload
// bundles `active: false` to re-assert the archived state, and (b) let a genuine
// unarchive (active: true) through. editClassroom does I/O via getBranchRef/
// getCommit/getClassroomJson/createGitTree/createGitCommit/updateRef, all on
// the GitHubClient, so we stub a path-routing fake client.
describe("editClassroom archived read-only guard", () => {
  // A fake client routing each git/contents path to a canned response. The
  // archived classroom.json is returned by the contents endpoint; if the guard
  // is bypassed, the tree POST (the first write) fires, which we assert against.
  const makeClient = (archivedRecord: Record<string, unknown>) => {
    const treePost = vi.fn()
    const requestRaw = vi.fn().mockImplementation((path: string) => {
      if (path.includes("/contents/")) {
        return Promise.resolve(JSON.stringify(archivedRecord))
      }
      return Promise.reject(new Error(`unexpected requestRaw: ${path}`))
    })
    const request = vi.fn().mockImplementation((path: string) => {
      if (/\/repos\/[^/]+\/classroom50$/.test(path)) {
        return Promise.resolve({ default_branch: "main" })
      }
      if (path.endsWith("/git/ref/heads/main")) {
        return Promise.resolve({ object: { sha: "base-sha" } })
      }
      if (path.includes("/git/commits/")) {
        return Promise.resolve({ tree: { sha: "base-tree-sha" } })
      }
      if (path.endsWith("/git/trees")) {
        treePost()
        return Promise.resolve({ sha: "tree-sha" })
      }
      if (path.endsWith("/git/commits")) {
        return Promise.resolve({ sha: "new-commit-sha" })
      }
      if (path.endsWith("/git/refs/heads/main")) {
        return Promise.resolve({})
      }
      return Promise.reject(new Error(`unexpected request: ${path}`))
    })
    const client = { request, requestRaw } as unknown as GitHubClient
    return { client, treePost }
  }

  const archived = {
    short_name: "cs101",
    name: "CS 101",
    term: "Fall",
    active: false,
  }

  it("refuses a name/term edit on an archived classroom (no active sent)", async () => {
    const { client, treePost } = makeClient(archived)
    await expect(
      editClassroom(client, { org: "acme", slug: "cs101", name: "Renamed" }),
    ).rejects.toThrow(/read-only/i)
    // Fail-closed BEFORE any write: no tree was created.
    expect(treePost).not.toHaveBeenCalled()
  })

  it("refuses a settings edit even when active:false is bundled in (bypass closed)", async () => {
    const { client, treePost } = makeClient(archived)
    await expect(
      editClassroom(client, {
        org: "acme",
        slug: "cs101",
        name: "Renamed",
        active: false,
      }),
    ).rejects.toThrow(/read-only/i)
    expect(treePost).not.toHaveBeenCalled()
  })

  it("allows a pure unarchive (active:true) on an archived classroom", async () => {
    const { client, treePost } = makeClient(archived)
    await expect(
      editClassroom(client, { org: "acme", slug: "cs101", active: true }),
    ).resolves.toMatchObject({ newCommitSha: "new-commit-sha" })
    // Unarchive proceeds to write.
    expect(treePost).toHaveBeenCalledTimes(1)
  })

  it("allows an unarchive bundled with a settings edit (active:true + name)", async () => {
    const { client, treePost } = makeClient(archived)
    await expect(
      editClassroom(client, {
        org: "acme",
        slug: "cs101",
        active: true,
        name: "Reopened",
      }),
    ).resolves.toMatchObject({ newCommitSha: "new-commit-sha" })
    expect(treePost).toHaveBeenCalledTimes(1)
  })

  it("allows a normal edit on an active classroom", async () => {
    const { client, treePost } = makeClient({
      short_name: "cs101",
      name: "CS 101",
      term: "Fall",
      active: true,
    })
    await expect(
      editClassroom(client, { org: "acme", slug: "cs101", name: "Renamed" }),
    ).resolves.toMatchObject({ newCommitSha: "new-commit-sha" })
    expect(treePost).toHaveBeenCalledTimes(1)
  })
})

// Students never read classroom.json — they render the classroom50/team/v1
// record projected onto the student team's description (GET /user/teams). An
// edit must re-project that record, or existing students keep seeing the old
// name forever (the classroom-rename stale-title bug). The projection derives
// from the record the edit just committed, NOT a contents re-read (the
// Contents API is read-after-write eventual and could echo the pre-write body).
describe("editClassroom team-description re-projection", () => {
  const makeClient = (opts?: {
    teamPrivacy?: string
    patchFails?: boolean
  }) => {
    const patched: { body: unknown }[] = []
    const requestRaw = vi.fn().mockImplementation((path: string) => {
      if (path.includes("/contents/")) {
        return Promise.resolve(
          JSON.stringify({
            schema: "classroom50/classroom/v1",
            short_name: "cs101",
            name: "CS 101",
            term: "Fall",
            org: "acme",
            team: { id: 7, slug: "classroom50-cs101" },
          }),
        )
      }
      return Promise.reject(new Error(`unexpected requestRaw: ${path}`))
    })
    const request = vi
      .fn()
      .mockImplementation(
        (path: string, init?: { method?: string; body?: unknown }) => {
          const method = init?.method ?? "GET"
          if (/\/repos\/[^/]+\/classroom50$/.test(path)) {
            return Promise.resolve({ default_branch: "main" })
          }
          if (path.endsWith("/git/ref/heads/main")) {
            return Promise.resolve({ object: { sha: "base-sha" } })
          }
          if (path.includes("/git/commits/")) {
            return Promise.resolve({ tree: { sha: "base-tree-sha" } })
          }
          if (path.endsWith("/git/trees")) {
            return Promise.resolve({ sha: "tree-sha" })
          }
          if (path.endsWith("/git/commits")) {
            return Promise.resolve({ sha: "new-commit-sha" })
          }
          if (path.endsWith("/git/refs/heads/main")) {
            return Promise.resolve({})
          }
          if (method === "GET" && /\/orgs\/[^/]+\/teams\/[^/]+$/.test(path)) {
            return Promise.resolve({
              id: 7,
              slug: "classroom50-cs101",
              privacy: opts?.teamPrivacy ?? "secret",
              description: JSON.stringify({
                schema: "classroom50/team/v1",
                name: "CS 101",
                term: "Fall",
              }),
            })
          }
          if (method === "PATCH" && /\/orgs\/[^/]+\/teams\/[^/]+$/.test(path)) {
            if (opts?.patchFails) {
              return Promise.reject(new Error("PATCH forbidden"))
            }
            patched.push({ body: init?.body })
            return Promise.resolve({})
          }
          return Promise.reject(new Error(`unexpected request: ${path}`))
        },
      )
    const client = { request, requestRaw } as unknown as GitHubClient
    return { client, patched, requestRaw }
  }

  it("PATCHes the student team description with the just-committed record", async () => {
    const { client, patched, requestRaw } = makeClient()

    const result = await editClassroom(client, {
      org: "acme",
      slug: "cs101",
      name: "Renamed",
      term: "Spring",
    })

    expect(result.teamDescription).toEqual({
      changed: true,
      slug: "classroom50-cs101",
    })
    expect(patched).toHaveLength(1)
    const body = patched[0].body as { description: string }
    expect(JSON.parse(body.description)).toEqual({
      schema: "classroom50/team/v1",
      name: "Renamed",
      term: "Spring",
    })
    // Exactly one contents read (the pre-edit merge source): the projection
    // must come from the committed record, never a post-commit re-read that
    // could echo the pre-write body.
    expect(requestRaw).toHaveBeenCalledTimes(1)
  })

  it("is best-effort: a failed team PATCH doesn't fail the committed edit", async () => {
    const { client } = makeClient({ patchFails: true })

    const result = await editClassroom(client, {
      org: "acme",
      slug: "cs101",
      name: "Renamed",
    })

    expect(result.newCommitSha).toBe("new-commit-sha")
    expect(result.teamDescription).toEqual({ changed: false })
  })

  it("skips a non-secret team rather than leaking the record", async () => {
    const { client, patched } = makeClient({ teamPrivacy: "closed" })

    const result = await editClassroom(client, {
      org: "acme",
      slug: "cs101",
      name: "Renamed",
    })

    expect(result.teamDescription).toEqual({ changed: false })
    expect(patched).toHaveLength(0)
  })
})
