import type { GitHubClient } from "@/github-core/client"
import {
  getClassroomJson,
  getConfigRepoBranch,
} from "@/github-core/configRepoReads"
import {
  projectTeamDescriptionFromRecord,
  type TeamDescriptionReconcileResult,
  type TeamDescriptionSource,
} from "@/github-core/mutations"
import { isClassroomArchived } from "@/types/classroom"
import { classroomFilePath } from "@/util/configRepoPaths"
import { logger } from "@/lib/logger"

import {
  commitConfigRepoFiles,
  jsonFileEntry,
  readConfigRepoHeadAt,
} from "../configRepoWrite"

const log = logger.scope("mutations:classroomEdit")

export type EditClassroomInput = {
  org: string
  slug: string
  // name/term are written only when provided — a pure archive/unarchive toggle
  // omits them so editClassroom's `...current` spread preserves the persisted
  // values (no stale-cache overwrite, no lost-update of a concurrent rename).
  term?: string
  name?: string
  // Archive lifecycle: false = archive, true = unarchive. Omitted leaves the
  // current value (or its absence) intact. See isClassroomArchived.
  active?: boolean
  // Custom Pages base URL (normalized; see normalizePagesBaseUrl). Omitted
  // leaves the persisted value intact; "" clears it (the key is deleted, not
  // written empty, so old readers never see an empty string).
  pages_base_url?: string
}

export type EditClassroomResult = Awaited<ReturnType<typeof editClassroom>>

// Merge an edit onto the current classroom.json record. Pure (no I/O):
// - spreads `...current` first so unknown/future fields a sibling binary wrote
//   ride through verbatim (the strict CLI round-trips this file);
// - writes name/term/active ONLY when provided, so a pure archive toggle
//   preserves the persisted name/term. `active` is a meaningful boolean (false =
//   archived), so unarchive writes `true` rather than deleting the key.
// - pages_base_url set when non-empty; "" deletes the key (clearing the custom
//   domain must not leave an empty string an old reader would trip on).
export function buildClassroomUpdate(
  current: Record<string, unknown>,
  fields: {
    name?: string
    term?: string
    active?: boolean
    pages_base_url?: string
  },
): Record<string, unknown> {
  const { name, term, active, pages_base_url } = fields
  const next = {
    ...current,
    ...(name !== undefined ? { name } : {}),
    ...(term !== undefined ? { term } : {}),
    ...(active !== undefined ? { active } : {}),
    ...(pages_base_url ? { pages_base_url } : {}),
  }
  if (pages_base_url === "") {
    delete next.pages_base_url
  }
  return next
}

// Deliberately not behind assertClassroomNotArchived: an unarchive must get
// through, so this writer runs its own settings-only gate below.
export async function editClassroom(
  client: GitHubClient,
  input: EditClassroomInput,
) {
  const { org, slug, term, name, active, pages_base_url } = input

  // Org policy can seed the config repo on a non-`main` branch, so both the ref
  // read and the write must target the real branch.
  const configBranch = await getConfigRepoBranch(client, org)
  const head = await readConfigRepoHeadAt(client, org, configBranch)

  const current = await getClassroomJson(client, {
    org,
    classroom: slug,
    ref: head.headSha,
  })

  if (current.short_name !== slug) {
    throw new Error(
      `classroom.json slug mismatch: expected ${current.short_name}, got ${slug}`,
    )
  }

  // Archived classrooms are read-only — refuse a settings edit (name / term),
  // but let a lifecycle toggle through since unarchiving re-enables editing.
  // Gate on whether a settings field is actually present rather than on
  // `active === undefined`, so a payload bundling a settings change with
  // `active: false` (a stale tab, direct API call, or CLI/agent) can't slip an
  // edit past the guard by re-asserting the archived state.
  const editsSettings =
    name !== undefined || term !== undefined || pages_base_url !== undefined
  if (editsSettings && active !== true && isClassroomArchived(current)) {
    throw new Error(
      `Classroom "${slug}" is archived — settings are read-only. Unarchive it first to make changes.`,
    )
  }

  const next = buildClassroomUpdate(current, {
    name,
    term,
    active,
    pages_base_url,
  })

  const written = await commitConfigRepoFiles(
    client,
    org,
    head,
    [jsonFileEntry(classroomFilePath(slug), next)],
    `Update classroom ${slug}`,
  )

  // Students never read classroom.json — they render the classroom50/team/v1
  // record projected onto the student team's description (GET /user/teams). So
  // a rename/re-term/archive must re-project here, or existing students keep
  // seeing the old name forever (mirrors the CLI edit's reconcile). Derived
  // from the record just committed, not a re-read. Best-effort: the edit is
  // already committed, and a failure (e.g. a non-owner teacher's 403 on the
  // team PATCH) is healed by a later classroom-entry reconcile.
  let teamDescription: TeamDescriptionReconcileResult = { changed: false }
  try {
    teamDescription = await projectTeamDescriptionFromRecord(
      client,
      org,
      slug,
      next as TeamDescriptionSource,
    )
  } catch (err) {
    log.warn("edit classroom: team-description re-projection failed", {
      org,
      classroom: slug,
      err,
    })
  }

  return {
    previousCommitSha: head.headSha,
    baseTreeSha: head.baseTreeSha,
    ...written,
    classroom: next,
    teamDescription,
  }
}
