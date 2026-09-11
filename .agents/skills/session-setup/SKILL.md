---
name: session-setup
description: First-run developer onboarding for the session journal system in a repository that already has the framework installed. Creates or opens the developer's personal journal branch, initializes .journal, pushes it, and explains how to use sessions. Invoke when the user asks for session setup, setup sessions, onboarding, or first-time journal setup.
disable-model-invocation: true
---

The user is asking to set up their developer journal for this workspace. Follow
the protocol defined in `.session.md` at the workspace root — do not restate it,
follow it.

Specifically:

1. **Verify prerequisites before changing anything:**
   - `command -v wt`
   - `gh auth status`
   - `gh api user --jq .login`
   - `git remote get-url origin`
   - `git remote show origin | sed -n 's/  HEAD branch: //p'`
   - `wt config show --full` must show `worktree-path = "{{ repo_path }}/.wt/{{ branch | sanitize }}"`
   If any prerequisite fails, stop and tell the developer what is missing. Do
   not install tools, run `gh auth login`, or edit Worktrunk config yourself.
2. **Resolve setup values:**
   - GitHub login from `gh api user --jq .login`
   - default branch from `git remote show origin`
   - journal branch `journal/<login>`
3. **Fetch and inspect:**
   - Run `git fetch origin --prune`.
   - Run `wt list --format=json`.
   - If an existing worktree for `journal/<login>` is present, use that as the journal root. Run the journal sync transaction from `.session.md`: inspect `git status --short`; if the only dirty paths are existing `.journal/<ID>/NOTES.md` files, commit those checkpoints first with `docs(journal): checkpoint session <ID>` for one session or `docs(journal): checkpoint active sessions` for several; if any other files are dirty, stop and surface them. Then run `git pull --rebase` and push any checkpoint commit.
4. **Open an existing journal branch if needed:**
   - If no worktree exists but `origin/journal/<login>` exists, create a local tracking branch if needed with `git branch --track journal/<login> origin/journal/<login>`.
   - Open it with `wt switch --no-cd --format=json journal/<login>`.
   - Run the same journal sync transaction in that worktree.
5. **Create the journal branch if needed:**
   - If neither a worktree nor `origin/journal/<login>` exists, create one with `wt switch --create --base origin/<default-branch> --no-cd --format=json journal/<login>`.
   - If the current checkout has an ignored local `.journal/`, copy its contents into the new journal worktree without overwriting existing files, for example with `rsync -a --ignore-existing .journal/ <journal-root>/.journal/`.
6. **Bootstrap and publish setup state:**
   - In the journal worktree, ensure `.journal/` exists.
   - Bootstrap any missing `.journal/INDEX.md`, `.journal/SKILLS.md`, and `.journal/TECH_NOTES.md` from the repo's scaffold files.
   - If `.journal/**` changed, run `git add -f .journal`, commit `docs(journal): initialize journal for <login>`, and push with upstream set to `origin/journal/<login>`.
   - If nothing changed, do not create an empty commit; confirm the existing journal branch is ready.
7. **Confirm and orient the developer.** End with a concise onboarding note in your own words that covers:
   - Sessions preserve agent context across days and teammates.
   - The default branch stays clean; their journal lives on `journal/<login>`.
   - Use sessions for substantial implementation, multi-step research, architecture work, or anything another agent may need to resume.
   - Do not use sessions for quick questions or one-off commands.
   - Start with `new session`, resume with `continue session <id>`, and close with `session close`.
   - Teammate journal branches are for context discovery, not implementation bases.
   - What setup just did, including the journal branch and journal worktree path.

Do not create a new session during setup unless the user explicitly asks for one
after setup is complete.
