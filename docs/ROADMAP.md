# Roadmap

[English](ROADMAP.md) | [简体中文](ROADMAP.zh-CN.md)

Where Tencent Cloud AIGate is heading. This is a living, high-level plan: it sets the
direction only — how each item is implemented is decided when work starts.
Day-to-day work is tracked in issues, and shipped work is recorded in
[CHANGELOG.md](../CHANGELOG.md).

## Recently shipped (v0.1.x)

The current foundation: a dual-format data plane (OpenAI + Anthropic), tiered
token quotas with usage accounting, a credential vault with BYOK, signed webhook
events, and idempotent import plus self-service keys. See the changelog for
details.

## Now — v0.2

Three directions for the next release, all about making model calls more
resilient and the gateway easier to extend:

- **Model fallback** — when a call cannot be served by the primary model, the
  gateway falls back to a backup automatically: transient upstream failures no
  longer break the request.
- **Auto-routing** — choose among a key's models by configurable policy instead
  of a fixed binding order: cost, latency or capability preferences become
  configuration.
- **Plugin support** — the plugin capability serves auto-routing: routing
  strategies are provided as plugins, so custom selection logic can be added
  without changing the gateway core.

## Next — candidates

- **Finer-grained quota management** — more flexible quota settings for users
  and groups.
- **Observability** — metrics for requests, errors and latency, ready for
  dashboards and alerting.

## Later — exploring

- **Quality-aware routing** — pick models by measured quality, not only cost or
  latency.
- **Multi-instance coordination** — consistent quotas and routing state across
  replicas.

## Not planned (for now)

- Web UI — AIGate is a pure API service by design.
- MCP proxying and session management — those belong to adjacent layers.

## How we plan

- Issues are the source of truth for ongoing work; this file is revised during
  release planning.
- API and configuration stay backward compatible within v0.x unless a change is
  explicitly flagged in the changelog.
- Feedback and design discussions are welcome via the project's issue tracker.
