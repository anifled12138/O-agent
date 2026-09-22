# Plugin catalog rules

- `enabled` means the feature affects runtime behavior now. If an item is only
  configured, installed, or unhealthy, expose that state instead.
- Every catalog toggle must either change authoritative state and read it back,
  or return an error. Never implement a successful no-op toggle.
- Persist user-visible core and skill switches. Test that they survive manager
  reconstruction.
- Do not publish duplicate catalog entries for one runtime feature.

