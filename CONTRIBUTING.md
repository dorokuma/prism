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
