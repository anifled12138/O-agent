# HTTP API rules

- A success response represents the completed domain mutation, including any
  linked metadata update. Do not ignore an inner storage or runtime error.
- Mutation responses should return authoritative read-back state rather than
  echoing the requested value.
- Streaming endpoints must recognize every terminal turn state.
- Connection tests must validate the capability the UI claims was tested, not
  merely that an endpoint returned 2xx JSON.

