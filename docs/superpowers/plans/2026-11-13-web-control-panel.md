# Web control panel implementation plan

**Goal:** Make Bridge manageable and debuggable from its embedded panel, with
voice-controlled network access and localhost as the default.

**Architecture:** Reuse `configurationControl`, `Engine`, `SearchHistory` and
`airPlayControl`. Serve the embedded assets and guarded JSON APIs on the existing
listener and an independently bound diagnostics listener.

**Spec:** `docs/superpowers/specs/2026-11-13-web-control-panel-design.md`

## Constraints and review focus

Go 1.27.3; no frontend build or external assets. Preserve the main checkout's
unfinished rebase. Never expose the complete config or environment. Verify
network disable on established connections, failed port binds, atomic config
validation, pending changes and reload failures, and explicit query side effects.

## Tasks

- [x] Access/config: add validated diagnostics settings; add the network flag to
  the voice catalog; apply the flag live through a callback; guard every route.
  Test omitted/invalid fields, Set + reload and cross-site/Origin rejection.
- [x] Listener: create a dedicated loopback/network listener and preserve the
  request applying its rebind. Test bind changes, port conflicts and shutdown.
- [x] API: expose safe configuration, status, route preview, explicit query,
  history and AirPlay operations. Test malformed/oversized requests, secret
  exclusion, unavailable features and concurrent-query rejection.
- [x] UI: replace the three embedded assets with responsive overview, settings,
  query testing, AirPlay, history and logs. Preserve polling and cursor behavior;
  show server errors and active/saved/unsaved state clearly.
- [x] Integration/docs: register APIs on both guarded panel muxes, manage the
  dedicated listener lifecycle, document config/voice/URL behavior and examples.
- [x] Verify: full Go tests, full race checks, build, JavaScript syntax check,
  desktop/mobile browser workflows and final independent branch review.
