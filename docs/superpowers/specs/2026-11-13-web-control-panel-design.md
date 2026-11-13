# Quasar Bridge web control panel

The embedded `/diagnostics/` page becomes an offline Russian control panel for
the running Bridge. Reuse the existing configuration, routing, history and
AirPlay controls rather than maintaining a second configuration implementation.

## Access

- `diagnostics.network_access` defaults to `false` and is voice editable.
- The explicit built-in commands “Алиса, включи веб-интерфейс” and
  “Алиса, выключи веб-интерфейс” toggle network access in hybrid mode without
  calling the LLM or depending on assistant tools. They preserve other pending
  configuration changes and retain the previous access state on failure.
- A dedicated HTTP listener defaults to `127.0.0.1:8080`, independently of the
  speaker's loopback UniProxy listener and `allowed_ips`.
- Setting network access binds this listener to `0.0.0.0` on the same port.
  Reload applies this change without restarting the voice connection.
- `diagnostics.port` defaults to `8080`, accepts `1..65535`, and requires restart.
- Keep the original `/diagnostics/` routes on the main listener for existing
  tunnels. The dedicated listener serves panel routes only.
- Apply access checks to every asset and API; reject cross-site requests and
  mismatched Origin. Network access accepts numeric-IP browser hosts.
- The panel has no account system. Network access grants local network users
  the panel's management rights; explain this at the toggle and in the guide.

## Functions

- Overview: running version, uptime, mode, model, frame counts, routing and
  feature status, connected speaker subscriptions, AirPlay state.
- Settings: safe existing catalog; search, type-aware editors, active/saved
  values, environment overrides, unsaved edits and pending restart state.
  Saving and applying are distinct actions. Restart requires a deliberate click.
- Request testing: exact CLI `-route` selection with classifier and built-in
  handlers, static active/saved rules comparison, and separate CLI `-query`
  execution. Live route selection never calls the selected action.
  Execution uses isolated dashboard dialogue state and can call real tools.
  Yandex cannot produce an answer outside a live speaker session.
- AirPlay: receiver start/stop and available sender media controls.
- History: bounded search through the existing opt-in conversation journal.
- Logs: retain bounded incremental polling, pause, filters, search and download.

Credentials, unrestricted endpoints, filesystem paths and full configuration
files are excluded from panel APIs. JSON mutations have bounded bodies and
strict schemas; expensive query execution is bounded and serialized.

## Presentation

Use an appliance control desk with the voice signal path as its visual anchor.
Palette: background `#EDF3F5`, surface `#FFFFFF`, text `#193541`, secondary
`#647A85`, action `#176D79`, error `#B34332`. Use local Trebuchet/Segoe UI/system
fonts and monospace only for logs and JSON. A quiet left navigation becomes a
horizontal navigation on mobile. Keep visible focus and readable field labels.

## Evidence

Verify defaults, config rejection, voice save/reload, live access changes,
same-origin guards, failed binds, API validation, unavailable services, isolated
query context and bounded history. Run the full Go suite and focused race
checks. Inspect the actual panel on desktop and mobile with a local fixture;
Record hardware and microphone evidence separately in `docs/validation.md`.
