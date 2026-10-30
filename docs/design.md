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

Source schemas: freequasar/quasar-go-genproto at
2f85025877ff367b10bc6e93e089207b8fca9a46.

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
is briefly cached for discovery and rechecked before actions. A selected group
covers its members, so those member actions are dropped; a remaining overlap is
rejected before any call. At most eight actions run. Light, switch and fan
on/off state is reread for about two seconds. A home failure never retries in
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

Ruling: `llm_system_prompt` is optional operator text appended to the
conversational system prompt and both Home Assistant planner prompts. The
classifier does not receive it. An empty value leaves the built-in prompts
unchanged. The note stays inside the single system message. When the request
names no room, known HA areas mentioned in the note narrow the planner
inventory; a room named in the request wins. Generic words such as «комната»
do not select an area. Changes require a process restart.

Ruling: optional `home_assistant` runs the Home Assistant Assist pipeline over
the WebSocket command `assist_pipeline/run`, from stage `intent` through
`intent`, and speaks `response.speech.plain.speech`. The websocket stays open
for the life of the process and is redialed only after it drops. ASR and TTS
stay with Yandex. `pipeline_id` selects the voice assistant, so local intent
handling follows that pipeline. `device_id` is the device-registry id sent with the
run. An empty pipeline uses Home Assistant's preferred one. An empty device
id is omitted. `agent_id` is accepted and not sent. The long-lived token is
only `QUASAR_HOME_ASSISTANT_TOKEN`. An empty URL disables the client and does
not require a token. A successful turn is not planned again through MCP. A
transport failure or `response_type` of `error` falls back to the MCP planner
when that client is configured. The conversation id is kept per speaker until
`ttl_seconds` or a dialogue reset. `continue_conversation` defaults to false.
An Assist answer then sets Alice `ShouldListen` false, including when
`listen_after_reply` is true and when the pipeline event asks to continue.
True follows `intent_output.continue_conversation` from `intent-end` and
ignores `listen_after_reply` for that answer. Planner fallback does not
set the override. Elliptical follow-ups during an active
conversation route to home after explicit routing rules. Redirects are
refused. URL, token and request text are not logged. Changes require a
process restart.

Ruling: `spotter_wait_ms` defaults to 1500 and requires a restart. Zero disables
the wait. A voice request routed to home, llm, music, or playback waits for
`SpotterValidation` before any local work. `Ok` runs the route.
`SpottingRejected` or `CanceledCauseOfMultiactivation` does not call the
executor: the cloud receives UserStop and the speaker receives a voiceless
completion. If no frame arrives, the route runs after the wait, so one speaker
in the room still answers. A rejection that arrives after the wait and before
local speech starts cancels that work and completes the dialog the same way.
The Yandex route does not wait. A validation frame that arrived early is
forwarded when that route is chosen, and a later frame is forwarded as it
arrives. Text input does not wait. The log records the result enum and the
multiactivation flag, not the phrase. Values outside 0–5000 are rejected.

Ruling: `routing.unknown_to_home` defaults to false. When true, text that
matches no rule goes to home without the classifier. Phrases that ask the
neural net go to home even if a rule chose llm. Timer phrases that a rule
would send to Yandex go to home, except «таймер на …» and a timer control
such as resume, pause, cancel, or delete, which stay on Yandex.
Alarm phrases stay on Yandex. An explicit home rule, weather, music,
clock questions, dialogue history, page reading, code requests, and an open
local follow-up stay on their existing routes. The decision source is
`unknown`. SIGHUP reloads the flag with the rest of routing.

Ruling: a whole conversion request goes to Yandex through `unit_conversion`.
It is a quantity, a currency or measurement unit, «в», and another currency
or unit. A currency may be followed by its country or code.
Examples: «50 евро в рублях», «5 долларов США в рублях», «40 вон в долларах», «20 кг в граммах»,
«1 метр в сантиметрах». A longer sentence is not this rule, and
`unknown_to_home` does not redirect a matched conversion.

Ruling: a whole playback command still stops music. That includes «Алиса,
хватит», «хватит», «стоп», pause, and resume. An active local queue handles
it; otherwise the `speaker_playback` rule sends it to Yandex. A longer
recognized phrase that contains «алиса хватит» is dropped. Punctuation
between the words is ignored. That longer utterance is not sent to Yandex,
Home Assistant, or the local model, and nothing is spoken. The word
«хватит» alone does not match the drop. The drop runs after the playback
exemption and before other rules, `unknown_to_home`, follow-ups, retry, and
dialogue reset. Local dialogue history is left in place. The speaker request
is finished with a voiceless AliceResponse (`ShouldListen` false, no TTS).
Abandoning that request makes the firmware time out and speak its generic
error. A gated cloud request is canceled with `UserStop`, not
`LocalExecution`.

Routing configuration is compiled into an immutable policy and atomically
published through the shared Engine. An absent routing object preserves legacy
defaults; explicit rule arrays replace the built-in array. SIGHUP reloads only
routing and rejects changed deployment fields. Failed reads/validation retain
the last good snapshot. Existing sessions share the Engine; in-flight classifier
calls retain their snapshot, while later decisions and refusal checks use the
current policy. Health reports a consistent revision/active-rule count.
