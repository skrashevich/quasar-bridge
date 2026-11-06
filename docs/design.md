# Implementation decisions

Binding intent: Go selective native Quasar proxy; home locally through HA,
complex/refused questions through a configured OpenAI-compatible LLM, other requests to Yandex.
ASR and TTS stay in Yandex. The application handles one authenticated speaker connection at a time per session.

Custom MCP tools are attached only to the conversational LLM agent, never to
the classifier or the HA planner. The official Go SDK handles Streamable HTTP,
legacy SSE and stdio. Sessions are isolated per answer and closed after it;
discovery is parallel and bounded. Server names and stable hashed tool aliases
prevent collisions, and the original input schema is forwarded to the model.
JSON arguments preserve large numbers. Explicit allow/deny lists control exposure.
Identical non-read-only calls within one answer reuse their outcome, including
unknown/failed outcomes; transport failures do not cause action replay.

MCP endpoints and credentials may come from env. HTTP credentials and legacy
SSE posts stay on the configured origin; redirects are not followed. Stdio has
a minimal inherited environment plus explicit configured additions. Raw SDK
errors, process stderr, endpoints and tool arguments/results are not logged.
History privacy carries across turns: external MCP calls are blocked after
search_history unless that server explicitly allows private history. Discovery
failure removes only that server's tools. Config changes require restart.

Source schemas: v3rt3p/quasar-protos, distributed as the separate Go module
github.com/skrashevich/quasar-go-genproto
Ruling: use a transparent WebSocket proxy with per-connection native state, not
an HTTP backend replacement — actual speech uses uniproxy.alice.yandex.net/uni.ws.
Unknown events, audio and ordinary cloud replies retain original bytes.

Ruling: text is held locally until routing; voice uses the native approval flag
so cloud ASR can finish before any cloud execution. Changing only that leaf
retains duplicate/unknown protobuf fields. Cloud receives original Continue
when available; otherwise an equivalent control with a new Yandex GUID.
ServerRequestReady follows Continue on the actual text test, so it cannot be
used as the pre-routing readiness prerequisite.

Investigation correction: an initial 2-byte AA marker interpretation was wrong.
AAPI is 4 bytes. PI happened to parse as an unknown protobuf varint, so reads
appeared valid while mutations moved those bytes and broke the protocol.
Regression now requires the entire marker to remain outside protobuf.
Earlier failed gating tests with the 2-byte interpretation are invalid evidence
about the server's approval support.

Ruling: HA is optional. An absent/empty QUASAR_HOME_MCP_URL disables the client,
warm-up and HA diagnostics. Every home route then resolves to Yandex before
Native starts local progress, regardless of its rule/classifier/default source.
A nonempty invalid URL is rejected; configured HA failures never retry in Yandex.

Ruling: HA calls use the configured local MCP capability URL in runtime.env,
not a new HA token. Honor Assist exposure; inventory states are fresh, exposure
is briefly cached for discovery and rechecked before actions. Batch validation
prevents duplicate/group-overlapping targets. A home failure never retries in
Yandex because the service might already have acted.

Ruling: refuse fallback substitution if cloud audio has already started or
executor directives are present (the observed client_action:tts_play_placeholder
is explicitly allowed). Otherwise suppress the original refusal,
its TTS controls/audio, and generate a replacement response/TTS. This is a
bounded textual refusal detector; ambiguous dialogues remain in Yandex.

Native state is per socket with cancellation, request limits and 90-second
expiry. Stream ownership clears on close/reuse. Payloads and credentials are
not recorded. TLS and exact device backups are separate from source artifacts.

Linux LLM reachability was blocked by Docker's overlapping /20 route. An
explicit /32 route via the existing LAN gateway restores only the requested
API destination; a separate systemd unit persists it without changing Docker.

Fixture verification, live text protocol checks and physical voice evidence
must be reported separately. See validation.md for what was actually tested.

Routing configuration is compiled into an immutable policy and atomically
published through the shared Engine. An absent routing object preserves legacy
defaults; explicit rule arrays replace the built-in array. SIGHUP reloads only
routing and rejects changed deployment fields. Failed reads/validation retain
the last good snapshot. Existing sessions share the Engine; in-flight classifier
calls retain their snapshot, while later decisions and refusal checks use the
current policy. Health reports a consistent revision/active-rule count.
