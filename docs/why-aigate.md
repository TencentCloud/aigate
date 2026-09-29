# Why we built Tencent Cloud AIGate

[English](why-aigate.md) | [简体中文](why-aigate.zh-CN.md)

## Incubated by ClawPro

Tencent Cloud AIGate is the model-access layer of
**[ClawPro](https://cloud.tencent.com/document/product/213/129156)** — Tencent
Cloud's one-stop enterprise AI agent management platform (企业 AI 智能体管控台).
ClawPro rolls secure, governed AI assistants out to an entire workforce in
minutes, no code required: administrators centrally manage member permissions,
models and knowledge bases, messenger channels (WeCom, WeChat, QQ, DingTalk,
Feishu), and running costs — behind a four-layer AI security posture that is
visible, auditable, strictly governed, and fully scanned.

> **Learn more about ClawPro** · [product overview](https://cloud.tencent.com/document/product/213/129156)
> · [activation guide](https://cloud.tencent.com/document/product/213/129186)
> · [console](https://console.cloud.tencent.com/cvm/clawpro)

Running agents at enterprise scale makes one requirement non-negotiable:
**every model call must be governed** — who may call which model, with which
upstream credential, under which quota, and with what audit trail — without
asking teams to operate an extra proxy fleet or to hand out upstream keys. We
built that layer inside ClawPro; when it proved generally useful, we extracted
it into a standalone project and open-sourced it: AIGate.

## Design principles

AIGate is built around one question: what does it take for an enterprise to
actually operate a model gateway? The answers became its design principles:

- **Governance is built in, not bolted on.** Model routing, four-level token
  quotas, per-request usage accounting, key lifecycle, and webhook events are
  core capabilities — not features reserved for an enterprise edition.
- **Protocols are contracts.** OpenAI and Anthropic formats are accepted on
  ingress and supported as upstreams. When both sides match, traffic passes
  through natively (Anthropic-only fields such as `cache_control` survive);
  when they differ, the gateway translates — request, response, and streaming
  alike — without silently dropping semantics.
- **Operate it like a CLI, not a platform.** One static binary; SQLite for a
  laptop and MySQL for production; no sidecars and no proxy fleet. Small teams
  should be able to run it, and enterprises to standardize on it.
- **Secure by default.** Every outbound request is SSRF-hardened (URL
  validation, dial-time re-resolution, redirect checks); data-plane keys are
  stored as SHA-256 hashes only; upstream secrets are encrypted at rest when a
  master key is configured.
- **Observable by construction.** Every request is accounted for; usage is
  written asynchronously so forwarding is never blocked; logs, daily
  aggregates, exports, and signed webhook events keep consumption auditable.

## Production validation

AIGate is not a green-field design. It was extracted from a production-verified
LLM proxy, so its behaviour reflects request patterns and failure modes that
have already run in production, and it was validated end-to-end against real
upstream services across both protocol families — streaming and non-streaming —
before being open-sourced.

## Where to go next

- [README](../README.md) — what AIGate does and how to run it
- [API reference](API.md) — the data, admin, and user APIs
- [Deployment](deploy.md) — configuration, encryption, Docker
- [Roadmap](ROADMAP.md) — where the project is heading
