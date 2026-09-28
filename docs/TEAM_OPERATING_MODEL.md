# Team Operating Model

## Who's actually on this project

One person — you, as Tech Lead — working with Claude Code agents
playing the other roles a small team would have. That's it. No other
GitHub accounts, no other people, unless and until a real person
actually joins (see "If a real person joins later" below).

This is worth being explicit about because the two things this model
is optimizing for pull in the same direction: **it's more honest, and
it's more useful for learning**, than pretending there are four people.
The value of "working like a team" isn't the appearance of headcount —
it's the *process*: design gets reviewed before code, code gets tested
by someone other than its author, decisions leave a paper trail. All
of that is real and reproducible with one account. Fabricated
teammates would add none of the process value and would actively hurt
the CV story if anyone ever looked closely (mismatched account
histories, "reviews" with no independent judgment behind them).

## Roles

### Tech Lead — you

Not delegated to an agent. You:
- Decide what gets built and in what order.
- Approve (or send back) design docs before implementation starts.
- Make the final call on every PR merge — the `tech-lead-review` agent
  below can surface findings, but it doesn't get to decide "good
  enough to ship," you do.
- Own the repo-level decisions that are expensive to reverse
  (orchestration platform, which store owns which data, breaking
  schema changes).

### Solution Architect — `.claude/agents/solution-architect.md`

Invoked before any non-trivial feature. Output is always a document
under `docs/design/<feature-name>.md`, never application code:
- The bounded context(s) involved and how they relate to existing ones
  (see this repo's own `docs/ARCHITECTURE.md`).
- Ubiquitous language: the terms this feature introduces or touches,
  defined precisely.
- Aggregates and their invariants — what must never be violated, and
  what enforces it.
- At least one diagram: a Mermaid sequence diagram for any flow that
  crosses a service or store boundary, or a user-flow diagram for a
  user-facing feature.
- Open questions flagged for the Tech Lead, explicitly — the architect
  agent should surface uncertainty, not paper over it with a plausible
  guess.

You (Tech Lead) review this doc like a real architecture review: push
back on it, ask for alternatives, or approve it. Only once it's
approved does...

### Dev — `.claude/agents/dev.md`

...implement it. Scope: write the code, write the tests, keep to the
approved design's boundaries. If asked to build something non-trivial
with no approved design doc, it should stop and say so rather than
inventing architecture on the fly — that's a process violation, not a
shortcut.

### Tester / QA — `.claude/agents/tester.md`

Runs *after* a dev PR exists, as an independent pass — not the same
context/session that wrote the code, so it isn't just re-reading its
own assumptions. Job:
- Actually run the test suite and confirm it's meaningful, not just
  present (are the assertions checking the right thing?).
- Hunt for edge cases the design doc's invariants imply but the tests
  don't cover (e.g. "what happens if two withdrawal requests for the
  same user land concurrently").
- Report findings — doesn't fix them itself; that goes back to `dev`
  (or to you, if it's a design gap rather than a bug).

### Tech-lead review (advisory) — `.claude/agents/tech-lead-review.md`

A tool *for* you, not a stand-in for you. Checks a PR against the
approved design doc specifically (did the implementation actually
match the architecture, not just "is this code fine in isolation") and
summarizes it so your own review is faster. It never approves or
merges anything.

## Workflow per feature

1. **You** define the feature ask (what, why, any hard constraints).
2. **Solution Architect** produces `docs/design/<feature>.md`. You
   review and approve it (or send it back).
3. **Dev** implements against the approved doc, opens a PR that links
   to it.
4. **Tester** runs an independent pass on that PR, posts findings.
5. **Tech-lead review** summarizes design-conformance for you.
6. **You** merge, or send it back with what's missing.

Trivial changes (bug fix, config tweak, a field addition with no new
invariant) skip straight to `dev` — don't force step 1-2 onto
work that doesn't need it.

## Identity & git conventions

- One GitHub account (yours), one SSH key (yours). See `CLAUDE.md`'s
  identity policy — it's the same rule, stated there because it's
  load-bearing for every agent working in this repo.
- Every commit an agent makes carries a `Role: <architect|dev|tester>`
  trailer alongside the required Claude attribution trailer, so the
  history says which hat produced which change without inventing a
  second identity to wear it.

## Communication — GitHub + Claude Code CLI, nothing else for now

No Slack, no Discord, no third-party agent (Hermes or otherwise). For
one person working through Claude Code, GitHub already *is* the
durable, searchable log this process needs:

- **Decision log** = `docs/design/*.md` + the PR/commit history that
  references them. "Why did we choose X" has an answer because it's a
  committed file, not because it's sitting in a chat channel's history.
- **Status** = GitHub's own PR/CI state. Nothing to wire up, nothing
  else to check.
- **Discussion** = the Claude Code CLI session itself, live, with you.

A chat platform is worth adding the day there's an actual problem it
solves — e.g. you want a push notification on your phone when CI breaks
overnight, or a second real person joins and needs async coordination
across timezones. Until then it's a service to run and a thing to check
that isn't earning its keep. Revisit this section if that day comes;
don't provision for it now.

## If a real person joins later

Swap the relevant agent role for that person: they get their own real
GitHub account and SSH key (completely normal at that point — it's a
real account for a real person), and the workflow above is unchanged —
they just do the "Dev" or "Tester" step themselves instead of an agent
doing it. Nothing here has to be restructured to make room for that.
