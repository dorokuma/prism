# Contributing Guide

## Versioning

- Format: `v{major}.{minor}.{patch}`
- **major**: incompatible API changes (not triggered during the current 0.x phase)
- **minor**: new features (prism setup, provider routing, model cache, etc.)
- **patch**: bug fixes, internal refactors, doc updates (no change to external interfaces)

On each release, also update:
- README.md version line
- README.md Changelog
- push git tag to remote

## Changelog

All changes are logged in reverse chronological order in the "Changelog" section at the bottom of README.md.
Format: `- **date** — v{version} — change summary`

## Commit messages

Commit messages must follow Conventional Commits (`TYPE: subject` or `TYPE(scope): subject`).

- **Types**: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `build`, `ci`, `chore`, `revert`.
- **Scope**: Optional (`[a-z0-9._-]+`).
- **Subject**: Non-empty, ≤ 72 characters (Chinese and English permitted).
- **Exemptions**: Merge, revert, `fixup!`, and `squash!` commits.
- **Enforcement**: Enforced locally by the `commit-msg` hook (includes secret scanning and noise word checks).

