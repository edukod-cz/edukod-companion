# Contributing

Bug reports and focused pull requests are welcome. Before submitting a change:

1. Do not include school credentials, pairing codes, model prompts, student
   data, or production logs containing private metadata.
2. Keep all model traffic restricted to a loopback OpenAI-compatible endpoint.
3. Preserve the outbound-only connection model and the request allowlist.
4. Run `make check` and include tests for protocol or security-boundary changes.

Protocol changes must remain backward compatible or increment the wire protocol
version with an explicit migration plan.
