# Vision (OSS Edition)

**Document type:** Vision Document
**Status:** Draft v1
**Last updated:** 2026-05-19
**Edition:** OSS (Aplexica open-source)
**Maintainer:** Aplexica project

---

## 1. Mission

**End AI agent lock-in.** Make it so that developers never have to commit to a single agent for fear of losing the context they've built up. Let them move freely between Claude Code, Codex, Hermes, OpenClaw, Kilo, and whatever comes next — and run as many of them in parallel as they like — with their memories, their skills, their tools, and their conversation history following them everywhere.

## 2. The problem: agent lock-in

Every modern AI agent accumulates a substantial amount of personal state on the developer's machine:

- **Memories** — facts the agent has learned about the user, the project, the team's preferences.
- **Skills** — reusable instructions, prompts, or plugins the developer has installed or built.
- **Tools added later** — MCP server configurations, custom subagents, hooks, slash commands, and plugins. Distinct from the built-in tools (Read, Write, Bash, Grep) that ship with the agent — these are extensions the user has wired up themselves, often with OAuth flows or credentials.
- **Conversation history** — the running record of what the developer asked, what the agent did, and what it produced.

This state is what makes the agent feel personal and useful. After a few weeks of serious use, a developer's investment in an agent is dominated by this state, not by the agent itself. The agent without the context is just a binary. The context is the value.

**And the context is trapped.** Each agent stores it in a proprietary format, in a proprietary location, with no path to move it elsewhere. The consequences cascade:

- **Switching agents means starting over.** A developer who's spent months teaching Claude Code about their codebase can't carry any of that to Codex. Onboarding the new agent is a full restart of that investment.
- **Running multiple agents means duplicating context by hand.** Developers who want to use the right tool for the job — Claude Code for refactors, Codex for tests, something else for review — pay an N× cost in maintenance for every additional agent.
- **Trying a new agent means risking what you've built.** When a promising new agent ships, evaluating it requires recreating context that exists nowhere else. Most developers don't, and the new agent doesn't get a fair shake.
- **The agent market is distorted.** Developers stay with whatever they started with because the switching cost is artificially inflated. Experimentation is discouraged. The best agent doesn't necessarily win.

This is **agent lock-in**, and it is the biggest hidden cost of using AI agents today. It's not enforced by any individual vendor — it's an emergent property of every agent storing its state in incompatible silos. But the effect is the same: the developer is stuck.

## 3. The vision: freedom from lock-in

Aplexica makes AI agent context **yours, not the agent's.** Once your memories, skills, and conversations live in a portable, agent-agnostic store that you control, every consequence of lock-in disappears:

- You can **use every agent like it has always known you** — Claude Code, Codex, Hermes, OpenClaw, and Kilo all reading from the same continuously-updated context.
- You can **switch agents without losing a thing** — a single command transcodes your state into another agent's native format and you're up and running there as if you'd been using it from day one.
- You can **try a new agent the day it ships** — connect Aplexica's adapter, sync your state in, evaluate it on equal footing. If you don't like it, you walk away having lost nothing.
- You can **run several agents side-by-side and let them share what they know** — what one learns, the others know within seconds. Pick the right tool for each job at the moment, not based on which one you happened to start with.
- You can **fork a conversation into a second agent** — try the same prompt two ways, see which path is better, continue with whichever you prefer, and keep both as a record.

This is the headline value proposition of Aplexica: **you are never locked in.** Every other capability the system delivers is in service of this freedom.

### 3.1 The four capabilities that deliver freedom

The freedom from lock-in is realized through four concrete capabilities. They are the *how*; the lock-in elimination is the *why*.

#### 3.1.1 Backup and restore (any agent → archive → same agent)

A one-command way to back up everything that makes an agent useful, and a one-command way to restore it. This is disaster recovery: lost laptop, corrupted home directory, accidental delete, "I want to wipe and reinstall." It is also the precondition for everything else — once state is in a portable archive, all the other capabilities become possible.

#### 3.1.2 Migration (any agent → any other agent)

Take your state from Agent A and load it into Agent B as if you'd been using Agent B all along. Memories carry over. Skills carry over (when the target agent has an equivalent capability) or convert to documentation when it doesn't. Conversations carry over and can be continued in the new agent. This is the operation that lets you walk out of any agent and into any other.

#### 3.1.3 Live sync (multiple agents stay in step)

When you run more than one agent, Aplexica keeps them aligned automatically. When Claude Code learns a new fact about a project, Codex knows it within seconds. When you install a new skill, every agent that supports it has it. Live sync turns multi-agent from a maintenance burden into the default.

#### 3.1.4 Forking (continue in a different direction without losing the original)

Take a conversation from one agent, fork it into another agent, and continue in a different direction without affecting the original. This is the workflow that turns multiple agents from a problem into a feature — ask the same question two ways, see which agent's path is better, keep both.

## 4. Who this is for

| Segment | Why they care |
|---|---|
| **Solo developers using one agent** | Backup and disaster recovery, peace of mind. |
| **Solo developers trying a new agent** | One-shot migration without losing context. |
| **Solo developers using multiple agents** | Multi-agent productivity without manual context duplication. Forking workflows. |

## 5. Non-goals

Aplexica is not:

- **A new AI agent.** It does not generate responses or call models. It manages state for agents that exist.
- **A general file-sync product** (Dropbox/iCloud/Syncthing replacement). It only handles AI agent state and is intimately aware of each agent's format.
- **A model router or proxy.** It does not sit in the request path between an agent and its LLM provider.
- **A prompt or skill catalog.** Community sharing may be explored later but is not a V1 goal.
- **An agent harness or IDE.** It does not provide a UI for chatting with an agent. The user interacts with each agent through that agent's own UI; Aplexica works in the background.
- **An agent chat application.** The local dashboard and tray surfaces manage Aplexica; users continue to interact with each agent through that agent's own interface.

## 6. Guiding principles

1. **Your data, your machine.** The OSS core runs locally with no account or telemetry. Network access occurs only for an explicit user action, such as checking for an update, or through a remote plugin the user deliberately configures.
2. **Lossless where possible, transparent where not.** When converting between agents, preserve fidelity. When fidelity loss is unavoidable, surface it explicitly rather than silently dropping data.
3. **The user owns the routing.** Defaults work for the simple case; users can choose precisely which memories, skills, and conversations sync to which agents.
4. **Git semantics for state.** Conversations are append-only event logs with branches, fork, and merge. Developers already understand git; the model should feel familiar.
5. **Adapter-based extensibility.** Every supported agent is implemented as a plugin against a documented adapter API. Adding the sixth agent doesn't require core changes.
6. **Boring infrastructure.** Prefer proven, inspectable primitives and versioned formats over exotic systems.
7. **Open-by-default OSS.** The AGPL-3.0 license keeps the program and distributed modifications open.
8. **🪙 The golden rule: no hardcoded values.** Every tunable in the system — cadences, thresholds, timeouts, retries, buffer sizes, paths, sizes, limits, ports, intervals — lives in configuration files, not in source code. The shipped binary embeds a `defaults.toml` that captures every default; users override at the user, system, or project level. This makes Aplexica tweakable for power users, debuggable for operators, auditable for security reviewers, and serviceable for support — all without code changes or rebuilds. Concrete enforcement and the configuration layering model live in [10-non-functional-requirements.md §11](10-non-functional-requirements.md).

## 7. Capabilities at a glance

| Capability | OSS |
|---|---|
| Local backup / restore | ✅ |
| Format conversion between agents | ✅ |
| Local real-time sync (same machine) | ✅ |
| Fork / merge / branches | ✅ |
| Selective sync rules | ✅ |
| Number of agents synced | unlimited (local) |

## 8. Architecture in one diagram

```
      Native agent storage on disk
   ┌────────────┬────────────┬────────────┬────────────┬────────────┐
   │ Claude     │ Codex      │ Hermes     │ OpenClaw   │ Kilo       │
   │  Code      │            │            │            │            │
   └─────┬──────┴─────┬──────┴─────┬──────┴─────┬──────┴─────┬──────┘
         │            │            │            │            │
         ▼            ▼            ▼            ▼            ▼
   ┌──────────────────────────────────────────────────────────────┐
   │            Per-agent adapters (5 plugins in V1)              │
   │   Each adapter knows: where to read, how to translate,       │
   │   where to write. Inbound = native → canonical events.       │
   │   Outbound = canonical events → native files.                │
   └──────────────────────────────┬───────────────────────────────┘
                                  │
                                  ▼
   ┌──────────────────────────────────────────────────────────────┐
   │   Aplexica Canonical Store (~/.aplexica/store)             │
   │   - append-only event log per artifact (memory/skill/convo)  │
   │   - branches: main + user-created forks                      │
   │   - tags on artifacts: drive selective-sync routing          │
   └──────────────────────────────┬───────────────────────────────┘
                                  │
                                  ▼
                       ┌──────────────────────┐
                       │   Sync daemon        │
                       │  (aplexica daemon)  │
                       │   - FS watchers      │
                       │   - debounce         │
                       │   - fan-out by rules │
                       │                      │
                       └──────────────────────┘
```

Remote transport (cross-device sync, BYO storage) is provided by an optional `remote` plugin registered with the daemon. The OSS daemon has no built-in knowledge of any specific remote backend; any implementation against the documented plugin interface works identically.

## 9. What "done" looks like for V1

V1 is feature-complete when:

1. All five V1 agents (Claude Code, Codex, Hermes, OpenClaw, Kilo) have adapters that round-trip memories, skills, and conversations with documented fidelity.
2. A developer with two agents installed on the same Mac, Linux, or Windows machine can install Aplexica OSS, run `aplexica setup --yes --install`, and within five minutes have live sync between those agents working.
3. A developer can fork a conversation from Agent A into Agent B, continue it independently, and the original conversation in Agent A is unaffected.
4. A developer can choose, per artifact or per tag, which agents receive that artifact.
5. Release artifacts are published for supported platforms through the channels listed in the [installation guide](install/_index.md); channel availability may vary.

Current installation and availability details live in the [installation guide](install/_index.md).

## 10. Long-term direction (beyond V1)

These are explicit non-goals for V1 but are the directions Aplexica is most likely to grow in:

- **More agents.** Aider, Cursor agents, GitHub Copilot Workspaces, Cline, and others as the market evolves. Adapter API is the extension point.
- **Community skill sharing.** Portable skills can support user-controlled sharing without changing the local-first core.
- **Selective LLM context export.** Take the relevant slice of agent state and feed it to a one-off model call (e.g., generate a brief from accumulated project memories). Adjacent to the core, but the data is in the right shape.
- **Agent-agnostic policy.** Once memories are in a canonical store, organization-wide policies (e.g., "no secrets in memories," "redact customer names") become enforceable across whichever agent the developer is using.

## 11. Glossary teaser

| Term | One-line definition |
|---|---|
| **Artifact** | A memory, a skill, a tool, or a conversation. The unit of state Aplexica manages. |
| **Agent** | One of Claude Code, Codex, Hermes, OpenClaw, Kilo (V1) — the AI coding tool whose state Aplexica syncs. |
| **Adapter** | A plugin that translates between one agent's native format and Aplexica's canonical format. |
| **Canonical Store** | The Aplexica-owned, agent-agnostic store of artifacts on the local disk. |
| **Canonical Format (ACF)** | The schema artifacts use inside the canonical store. |
| **Branch / Fork / Merge** | Git-style operations on conversation history. |
| **Sync rule** | A user-defined or default policy that decides which artifacts go to which agents. |

Full glossary in [appendix-a-glossary.md](appendix-a-glossary.md).
