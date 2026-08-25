# Registry v2 to v3 migration

Registry v3 is an explicit two-phase security migration. The normal daemon and
registry APIs reject v1/v2 files; they cannot silently authorize or persist a
legacy registry. Back up the state directory, then run the migration with the
newly installed `aplexica` binary while the daemon is stopped.

The plan binds the exact v2 SHA-256, every resolved physical path and file
identity, missing/inactive paths, all collision decisions, revision-1 output,
and generation-2 removal tombstones. Apply accepts only the independently
reviewed plan digest and remeasures all of those inputs before its atomic
fsync-backed commit. It writes an exact no-clobber v2 backup and canonical
collision report beside `projects.json`. Neither command reads or logs project
file bodies.

## Migrate a device with no path collisions

This is the common case: every registry entry resolves to a distinct physical
directory. Missing paths become explicit inactive recovery records rather than
being dropped.

Always measure the input digest yourself after stopping the daemon. A digest
recorded during an earlier dry run is evidence of that run, not permission to
skip the mandatory measurement — the registry can change while the daemon is
running.

```sh
APLEXICA_BIN=/absolute/path/to/new/aplexica
STATE="$HOME/.aplexica/state"

"$APLEXICA_BIN" daemon stop
INPUT_SHA256=$(shasum -a 256 "$STATE/projects.json" | awk '{print $1}')
# Compare INPUT_SHA256 with the independently reviewed value before continuing.
"$APLEXICA_BIN" project --state-dir "$STATE" migrate-v3 plan \
  --expected-input-sha256 "$INPUT_SHA256"
```

Review the emitted immutable plan from a separate terminal. Recompute its hash
instead of copying a value from an untrusted request:

```sh
PLAN=/absolute/path/printed/by/plan
APPROVED_PLAN_SHA256=$(shasum -a 256 "$PLAN" | awk '{print $1}')
jq . "$PLAN"
"$APLEXICA_BIN" project --state-dir "$STATE" migrate-v3 apply \
  --approve-plan-sha256 "$APPROVED_PLAN_SHA256"
```

## Migrate a device that has path collisions

If two or more legacy project IDs resolve to the same physical directory — for
example because a home directory was renamed, or a device was re-paired under a
new username — the plan refuses to guess. You must state which ID survives and
which is removed.

Running `migrate-v3 plan` without the flags does **not** list the colliding
IDs: it stops at the first undecided collision with `collision resolution is
partial; every colliding ID requires an explicit retain/remove decision`, and
that message names neither the path nor any ID. Find them yourself by grouping
the registry's entries by resolved physical path:

```sh
jq -r '.projects[] | "\(.id)\t\(.path)"' "$HOME/.aplexica/state/projects.json" \
  | while IFS=$'\t' read -r id path; do
      printf '%s\t%s\n' "$(cd "$path" 2>/dev/null && pwd -P || echo "$path")" "$id"
    done | sort | awk -F'\t' '{c[$1]=c[$1]" "$2} END {for (p in c) if (split(c[p],a," ")>1) print p":"c[p]}'
```

Each line of output is one collision: the physical path, then the IDs that
share it. Keep the ID whose history you want and remove the rest.

```sh
APLEXICA_BIN=/absolute/path/to/new/aplexica
STATE="$HOME/.aplexica/state"

"$APLEXICA_BIN" daemon stop
INPUT_SHA256=$(shasum -a 256 "$STATE/projects.json" | awk '{print $1}')
# Compare INPUT_SHA256 with the independently reviewed value before continuing.
"$APLEXICA_BIN" project --state-dir "$STATE" migrate-v3 plan \
  --expected-input-sha256 "$INPUT_SHA256" \
  --retain-id local:<retained-id> \
  --remove-id local:<removed-id>

PLAN=/absolute/path/printed/by/plan
APPROVED_PLAN_SHA256=$(shasum -a 256 "$PLAN" | awk '{print $1}')
jq . "$PLAN"
"$APLEXICA_BIN" project --state-dir "$STATE" migrate-v3 apply \
  --approve-plan-sha256 "$APPROVED_PLAN_SHA256"
```

Each removed ID becomes a tombstone at authorization generation 2 rather than
disappearing, so the removal stays auditable.

## Rehearse against a copy first

`plan` never mutates anything, but rehearsing against a copy of your registry
lets you review the collision report and the resulting project counts before
you stop your daemon. Never pass your live state directory to a rehearsal, and
never run `apply` against a copy you intend to discard:

```sh
ALIAS=$(mktemp -d /tmp/aplexica-registry-local.XXXXXX)
COPY=$(cd "$ALIAS" && pwd -P) # migration rejects /tmp's macOS alias
chmod 700 "$COPY"
cp "$HOME/.aplexica/state/projects.json" "$COPY/projects.json"
chmod 600 "$COPY/projects.json"
INPUT=$(shasum -a 256 "$COPY/projects.json" | awk '{print $1}')
"$APLEXICA_BIN" project --state-dir "$COPY" \
  migrate-v3 plan --expected-input-sha256 "$INPUT"
```

The plan reports the input digest, the plan digest, and the counts of total,
active, and inactive projects plus collisions and removals. Compare those
counts against what you expect for that device before running the real
migration.

A plan digest includes its UTC planning time, so it deliberately does **not**
repeat between the rehearsal and the real run. Approve the digest printed by
the run you are actually applying. Delete the temporary copy when you are done.

## Verification and recovery constraints

Before restarting, verify `version == "3"`, `revision == 1`, nonzero generation
on every project, a file identity on every active entry, no identity on every
inactive entry, and exact hashes for the backup and report printed by `apply`.
The plan's input SHA must equal the backup, its digest and collisions must equal
the collision report, and its canonical output bytes must equal the live
registry. Every reported removal must have the exact generation-2 tombstone at
the plan time.

Do not overwrite or delete the v2 backup, collision report, or approved plan.
If apply fails before the atomic commit, `projects.json` remains v2 and the
exact no-clobber evidence can be safely reused. If post-commit verification
fails, keep the daemon stopped and investigate; do not copy the v2 bytes over
the v3 file while the new daemon is running.
