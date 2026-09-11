---
name: session-new
description: Start a new journal session in this workspace. Invoke only when the user explicitly asks to start a new session (e.g. "new session", "start a session"). Requires prior session-setup, then primes the next .journal/<ID>/ folder in the developer's personal journal worktree per .session.md. Does not accept arguments.
disable-model-invocation: true
---

The user is starting a **new session** in this workspace. Follow the session protocol defined in `.session.md` at the workspace root — do not restate it, follow it.

Specifically:

1. **Verify session setup (mandatory, first):** Resolve the developer identity with `gh api user --jq .login`, then locate an existing Worktrunk worktree for `journal/<login>` with `wt list --format=json`. If no worktree exists, stop and tell the developer to run `session-setup` before starting a session. Do not create the journal branch here.
2. **Prepare the journal root:** In the journal worktree, run the journal sync transaction from `.session.md`: inspect `git status --short`; if the only dirty paths are existing `.journal/<ID>/NOTES.md` files, commit those checkpoints first with `docs(journal): checkpoint session <ID>` for one session or `docs(journal): checkpoint active sessions` for several; if any other files are dirty, stop and surface them. Then run `git pull --rebase`, push any checkpoint commit, and verify `.journal/INDEX.md`, `.journal/SKILLS.md`, and `.journal/TECH_NOTES.md` exist. If the root journal files are missing, stop and tell the developer to rerun `session-setup`.
3. **Startup:** Read `<journal-root>/.journal/SKILLS.md` if present and load every required skill listed there. Read `<journal-root>/.journal/TECH_NOTES.md` if present. Then read the `SUMMARY.md` of the last three closed sessions in `<journal-root>/.journal/` (skip sessions without a `SUMMARY.md`; read fewer if fewer exist). Do **not** read their `NOTES.md` files.
4. **Prime the new session:** Read `references/notes-template.md`, then:
   - Find the highest existing session ID under `<journal-root>/.journal/` and increment by 1 (zero-padded, 3 digits). If no session folders exist, start at `001`.
   - Create `<journal-root>/.journal/<ID>/`.
   - Create `<journal-root>/.journal/<ID>/NOTES.md` from the template, then append an initial `## <timestamp> — Kickoff` entry capturing the user's stated goal and the current state of the world.
   - Do **not** create `SUMMARY.md` — that's written at session close.
   - Add an `in-progress` row for the new session to `.journal/INDEX.md`. Derive a short title from the user's stated goal, use today's date, keep rows ordered oldest to newest, and keep the summary cell to one sentence.
5. **Record the journal mutation:** In the personal journal worktree, commit the new session files and `INDEX.md` row with `docs(journal): start session <ID>` and push `journal/<login>`. If the push is rejected, fetch/rebase once and retry once; if conflicts remain, stop and surface them.
6. Confirm to the user which session ID was created and which journal branch was updated, then wait for their actual request.

Do not proceed with any substantive work until priming is complete.
