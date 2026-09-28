# Repo operating rules

This repo is operated by one person (the Tech Lead) working with Claude
Code agents playing the other roles a real team would have. Full
rationale and workflow: `docs/TEAM_OPERATING_MODEL.md`. The short
version, because it's load-bearing:

## Identity policy — read this before creating any account, key, or commit

- **One human, one GitHub account, one SSH key.** Never create GitHub
  accounts, bot personas, or fabricated author identities to make the
  commit/PR history look like more than one person worked on this.
- Every AI-authored commit carries the attribution trailer this
  session's system prompt requires (`Co-Authored-By: Claude ...` /
  `Claude-Session: ...`), **plus** a `Role: <architect|dev|tester>`
  line so it's clear which hat was on — as metadata on the one real
  identity, not as a separate fake one.
- If a task genuinely needs a bot account (e.g. CI posting comments),
  it must be a real GitHub App or an account clearly named/labeled as a
  bot — never a name meant to pass as a human teammate.

## Roles

| Role | Who | Claude Code agent |
|---|---|---|
| Tech Lead | **You.** Sets priorities, approves designs, has final say on every merge. | — (that's the human seat, not delegated) |
| Solution Architect | AI, scoped to design only | `.claude/agents/solution-architect.md` |
| Dev | AI, implements approved designs | `.claude/agents/dev.md` |
| Tester / QA | AI, independent verification | `.claude/agents/tester.md` |
| Tech-lead review (advisory) | AI, *assists* your review — never approves/merges on its own | `.claude/agents/tech-lead-review.md` |

## The one gate that matters

**No non-trivial implementation without an approved design note first.**
"Non-trivial" = touches more than one service/store, or introduces a
new bounded context, aggregate, or cross-service event. A `dev` agent
handed a task like that should stop and ask for a design doc under
`docs/design/` (produced by the `solution-architect` agent) instead of
improvising architecture inline. Small, obviously-local changes (a bug
fix, a config tweak, adding a field) don't need this — don't force
process onto things that don't need it.

## Commit convention

```
<summary>

<body>

Role: <architect|dev|tester>
Co-Authored-By: Claude Sonnet 5 <noreply@anthropic.com>
Claude-Session: <session URL>
```
