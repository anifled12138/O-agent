# ADR 0001: Plugin-first runtime

Status: Accepted for milestone 1

## Context

O needs to evolve its reasoning loop without coupling model providers,
storage, tools, transports, and policy. Dynamic Go shared objects are not a
portable option on Windows and would make upgrades unsafe.

## Decision

The runtime is a small plugin kernel with manifests, dependency ordering,
lifecycle states, and a typed service registry. Built-in capabilities and
future external adapters use the same contract. Milestone 1 uses compiled-in
plugin factories; package distribution and process-isolated third-party
plugins will be added after the host protocol stabilizes.

The plugin boundary is architectural, not an excuse to invent replacements for
MCP or Skills. Those will be compatibility plugins using their standard
protocols.

## Reasoning direction

The initial agent loop persists the user turn, constructs provider-neutral
messages, calls a configured model, and persists the response. The next runtime
iteration will split this into explicit phases:

1. task contract and constraints;
2. frontier construction and action selection;
3. evidence-bearing tool execution;
4. state checkpoint and context compaction;
5. verifier-driven continuation or completion.

Each phase will be a policy interface so the harness can evolve independently
from provider APIs and tool protocols.

