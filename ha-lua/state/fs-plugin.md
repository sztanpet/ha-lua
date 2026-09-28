# State: filesystem plugin (fs-plugin-spec.md)

Spec: `fs-plugin-spec.md`. Global decisions live in `../AI.state`.

Status: **COMPLETE.** Read API in 1.2.0, write API and the log-dir-rooted
`log_file` in 3.0.0 (BREAKING: `log_file` paths became relative to log_dir).

## Decisions and why
- One process-wide `os.Root` per tree, shared across LStates. os.Root rejects
  symlink and `..` escapes at the syscall layer, so bindings pass user paths
  straight through; `TestRequireRejectsSymlinkEscape` shows the old lexical
  check did not.
- `log_file` writes through a SECOND root over log_dir, not the scripts root:
  logs do not belong in the watched scripts tree. A bad path or unset log_dir
  raises at registration, because a broken error sink must fail while someone
  is looking.
- `LoadAll` with a nil root is an error, not a fallback to `os.ReadDir`: main
  always opens the root, so nil there is a wiring bug.
- `fs.exists` treats ANY Stat error as false (never raises), and `fs.write`
  does not create parent directories. Both deliberate.
- Deferred until a real use case: a separate `scripts/assets/` root, asset hot
  reload.
