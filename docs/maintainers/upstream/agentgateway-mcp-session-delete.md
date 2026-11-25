# Notes for an agentgateway issue: a closed MCP session stays in the other replicas, and its idle time is not configurable

For the project owner. agentgateway's rules forbid issues written by AI: take the facts below and write the issue in your
own words. Nothing has been posted. Version: agentgateway v1.7.0, Kubernetes mode (controller, xDS), 3 proxy replicas.
Ambit issue: #383.

## What was seen

Three proxy pods, one MCP backend, steady traffic. Each round: a new client token, `initialize`, `tools/list`, then
`DELETE` of the session (a well-behaved client), and one LLM request. Memory of each proxy pod (`kubectl top`, working set):

- Unchanged `test/step10.sh` soak (about 45 minutes): 15 MiB at the start, 41 MiB after 30 minutes, then level (40 to 42 MiB
  for the next 15 minutes). All three pods alike.
- Phased test (36 rounds a minute, `/memory` of the admin server): live heap (jemalloc `allocated`) 6.5 MiB at the start, 12
  MiB after 40 minutes, then 9 to 10 MiB; working set 12 to 19 MiB, then 17 to 20 MiB for 75 minutes. While only MCP sessions were
  created (20 minutes) the memory stayed level; with only LLM requests it fell by about 3 MiB (the sessions of the earlier phase
  expired); with no traffic it stayed level.

So it is a rise for the first 30 minutes, then a level: bounded, not a leak.

## Why (from the source of v1.7.0, `crates/agentgateway/src/mcp/session.rs`)

- `get_or_resume_session`: a replica that does not know the session id decodes it (the session state is in the id) and
  **inserts** the resumed session into its own map, with a new upstream relay (`build_new_connections`).
- `delete_session`: removes the session from the map of the replica that receives the `DELETE` only.
- Every other replica that resumed the session keeps the entry until `idle_ttl` passes (`reap_expired_entries`, every 30 s).
- `idle_ttl` is `DEFAULT_SESSION_IDLE_TTL` (30 minutes) when the backend comes through xDS
  (`types/agent_xds.rs`: `session_idle_ttl: crate::mcp::DEFAULT_SESSION_IDLE_TTL`). Only the local file configuration
  (`LocalMcpBackend`, `config.mcp.sessionTtl`) can change it. `AgentgatewayBackend` has no field for it.

So with N replicas, up to N-1 copies of every closed session stay for 30 minutes after their last use on that replica. The
memory of one pod is its own baseline plus (sessions a minute that this pod served and did not delete) x 30 x (the size
of a session). With the 36 sessions a minute of the phased test, about 7 KiB and about 0.9 copies kept per pod, that is about
7 MiB: the rise that was measured.

## What to ask

1. Is it intended that `DELETE` does not remove the session from the replicas that resumed it? If a replica can not know, a
   short idle time for a resumed session (or an entry that is not kept after the request when the session id carries all state)
   would bound the memory without a shared store.
2. Please expose `session_idle_ttl` in `AgentgatewayBackend` (the `mcp` backend), so an operator can lower it.

## What Ambit does

Nothing can be configured in Ambit for this. `docs/alerts.md` and the comment on `gateway.resources` in
`charts/ambit/values.yaml` say that the memory rises for 30 minutes and then stays level, and that the limit must fit the level.
