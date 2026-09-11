---
name: session-continue
description: Continue an existing journal session by ID. Invoke only when the user explicitly asks to continue a specific session. Requires prior session-setup, then takes one argument, the session ID (e.g. "1", "003"). The argument follows the skill name in the user's invocation.
argument-hint: <session-id>
disable-model-invocation: true
---

The user is **continuing an existing session** in this workspace. The session ID is passed as the argument to this skill — treat any numeric argument the user provided as the session ID, resolving it against the zero-padded folder names under the journal root's `.journal/` (e.g. `1` → `001`, `12` → `012`).

Follow the session protocol defined in `.session.md` at the workspace root — do not restate it, follow it.

Specifically:

1. **Verify session setup (mandatory, first):** Resolve the developer identity with `gh api user --jq .login`, then locate an existing Worktrunk worktree for `journal/<login>` with `wt list --format=json`. If no worktree exists, stop and tell the developer to run `session-setup` before continuing a session. Do not create the journal branch here.
2. **Prepare the journal root:** In the journal worktree, run the journal sync transaction from `.session.md`: inspect `git status --short`; if the only dirty paths are existing `.journal/<ID>/NOTES.md` files, commit those checkpoints first with `docs(journal): checkpoint session <ID>` for one session or `docs(journal): checkpoint active sessions` for several; if any other files are dirty, stop and surface them. Then run `git pull --rebase`, push any checkpoint commit, and verify `.journal/INDEX.md`, `.journal/SKILLS.md`, and `.journal/TECH_NOTES.md` exist. If the root journal files are missing, stop and tell the developer to rerun `session-setup`.
3. **Startup:** Read `<journal-root>/.journal/SKILLS.md` if present and load every required skill listed there. Read `<journal-root>/.journal/TECH_NOTES.md` if present. Then read the `SUMMARY.md` of the last three closed sessions in `<journal-root>/.journal/` (skip sessions without a `SUMMARY.md`; read fewer if fewer exist). Do **not** read their `NOTES.md` files at this step. This startup read is required in addition to the continuing-session reads below.
4. **Resume the target session:**
   - Resolve the user-provided session ID to the matching `<journal-root>/.journal/<ID>/` folder. If the folder does not exist, stop and ask the user to clarify before doing anything else.
   - Read `<journal-root>/.journal/<ID>/NOTES.md` **in full**, top to bottom. This is your primary context for the session.
   - Read `<journal-root>/.journal/<ID>/SUMMARY.md` if it exists (the session may have been closed and is being reopened).
5. **Log the resume:** Append a new `## <timestamp> — Resume` entry to `NOTES.md` stating your understanding of the current state and what you're about to do. If `<journal-root>/.journal/INDEX.md` has no `in-progress` row for this session, add or update the session row to `in-progress`; if the session was previously `complete` or `abandoned`, say in the resume entry that it was reopened.
6. **Record the journal mutation:** In the personal journal worktree, commit the resume entry and any `INDEX.md` update with `docs(journal): resume session <ID>` and push `journal/<login>`. If the push is rejected, fetch/rebase once and retry once; if conflicts remain, stop and surface them.
7. Confirm to the user that the session is resumed and which journal branch was updated, then wait for their actual request.

Do not proceed with any substantive work until the resume entry is logged.
