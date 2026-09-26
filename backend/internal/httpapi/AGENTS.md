# HTTP API rules

This package maps local HTTP routes to provider, Agent, plugin, and Evolution services, including SSE turn-event delivery.

- Develop from `backend/`; use `go test ./internal/httpapi` for focused handler changes and `go test ./...` for shared contracts.
- Validate request bodies, return service errors, and read back authoritative state after mutations. Do not put secrets or request bodies in access logs.
- Streaming routes must handle every terminal Turn state and resume from the durable event cursor.

- A success response represents the completed domain mutation, including any
  linked metadata update. Do not ignore an inner storage or runtime error.
- Mutation responses should return authoritative read-back state rather than
  echoing the requested value.
- Streaming endpoints must recognize every terminal turn state.
- Approval endpoints must bind the authenticated workspace user to the pending
  request and accept only an explicit one-time approve or deny decision.
- Observability endpoints must return bounded aggregates, never raw unredacted
  tool arguments or credentials.
- Connection tests must validate the capability the UI claims was tested, not
  merely that an endpoint returned 2xx JSON.
