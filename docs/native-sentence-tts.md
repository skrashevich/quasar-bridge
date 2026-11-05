# Native sentence streaming TTS (opt-in)

This proposal lets Quasar Bridge start speaking the first complete phrase of a plain LLM answer while the remaining answer is still being generated. It reuses Yandex Station's existing Alice speech synthesizer, microphone, ASR, UniProxy transport and Opus player. No firmware patches, custom voices, audio transcoding or new playback driver are needed.

## Why this is different from existing local_progress

The existing full-answer route uses Chat Completions with stream=false and eventually calls TtsGenerate once with the complete text. The local_progress option can keep a silent/placeholder audio stream alive until that answer is ready, but does not start speaking the text as the model generates it.

The optional sentence_tts mode is different:
1. For a tool-free Chat Completions LLM route, read SSE text deltas as they arrive.
2. Buffer until a real sentence end. For a long opening sentence, a natural clause pause (comma, colon or semicolon) after approximately 32 runes is permitted. Subsequent sentences are kept complete, with natural pauses for unusually long sentences.
3. Open one initial TtsGenerate with IsStreaming=true and empty Text.
4. Send the first spoken character and all later generated text through successive DeferredTtsGenerate messages, sharing RequestMessageId and incrementing ChunkNum.
5. Finish once with IsLast=true, and forward the stock Opus/Ogg data unchanged to the firmware's own player.

The initial Text is deliberately empty. In controlled tests with IsStreaming=true, UniProxy **ignored the initial TtsGenerate.Text**. This previously caused the entire opening phrase to be missing when it was sent in the initial request rather than through DeferredTtsGenerate.

The client-to-server event is DeferredTtsGenerate (event field 34). Its payload contains RequestMessageId (field 1), Text (field 2), IsLast (field 3), and ChunkNum (field 4). TtsGenerate.IsStreaming is field 12 of the TTS message. Current pinned Go proto definitions do not expose the deferred client event, so the implementation encodes its known fields via protobuf unknown-field support; ordinary non-streaming requests use the existing generated types.

## Evidence from the stock Alice firmware and cloud (Station Lite 1)

These are **separate** tests from the new Bridge implementation, captured using an unmodified stock Yandex response during a controlled long-story voice request on 2026-11-04.

| Observation | Measured result |
| --- | --- |
| ASR final | 16:38:39.272 UTC |
| First partial text (DeferredAliceResponse) | 16:38:41.086 UTC |
| TtsSpeak began | 16:38:41.093 UTC |
| First encoded audio packet | 16:38:41.132 UTC |
| Firmware began playback | 16:38:41.235 UTC |
| Final partial text | 16:38:41.689 UTC |
| Time from ASR final to player start | ~1.963 seconds |
| Partial server-side text responses | 10 |
| TtsSpeak network audio streams | 1 |
| Logical Ogg/Opus sequences inside it | 9 |
| Lazy TTS streaming flag in that capture | false |

The final text arrived **after audible playback began**. Stock Alice therefore demonstrably supports incremental delivery of both text and audio. The nine Ogg logical sequences in a single network audio stream prove that one network stream does not imply one acoustic segment. The listener described that stock response as smooth.

We also ran an **isolated synthetic UniProxy A/B experiment** (same Russian paragraph; different boundaries; one TtsGenerate with IsStreaming=true in each run):

| Input segmentation | Logical Ogg/Opus sequences | Summed encoded audio duration | Encoded bytes |
| --- | ---: | ---: | ---: |
| Whole paragraph as one DeferredTtsGenerate | 1 | 11.571 s | 107,871 |
| 3 sentence-sized deferred inputs | 3 | 11.165 s | 108,736 |
| 10 inputs of ~16 characters | 10 | 13.794 s | 133,877 |

The 16-character strategy produced 10 separate logical audio sequences and about 2.22 seconds more encoded speech than the one-piece input for the same words. This is consistent with the audible resets of intonation reported for small arbitrary chunks.

**Limits of the evidence:** It has not been proven that native Alice's internal cloud model always uses exactly one Ogg segment per sentence, or that the segments share neural/prosody state. The same network transport can contain many acoustic segments. The sentence boundary strategy is an empirically motivated latency-versus-naturalness compromise, not a claim to reproduce Yandex's undisclosed internal TTS synthesis algorithm. Stock Alice used server-generated DeferredAliceResponse; our local Bridge uses client-generated DeferredTtsGenerate. These are different protocol directions.

## Enable and rollback

The feature defaults **off**, preserving existing behavior. For hybrid mode with a tool-free OpenAI-compatible Chat Completions LLM, add this JSON setting:

    "sentence_tts": { "enabled": true }

Keep all other existing configuration unchanged. Restart Bridge to apply settings. The option is intentionally bypassed for assistant tool calls, ChatGPT subscription Responses, Yandex-native routes and special dialogue retry/reset routes; those retain their current, full-response behavior. No extra TTS service is required.

To roll back, change enabled to false and restart Bridge. The original full-answer and local_progress routes remain available.

## Verification and portability

The regression tests verify a single opening TtsGenerate with empty text, one shared RequestMessageId, monotonically increasing ChunkNum, exactly one final IsLast, full preservation of leading and trailing words (including counting 1 to 30), and that the first deferred speech text goes out before the LLM has generated the remainder. Input boundary tests address numbers, punctuation, quotation marks, long clauses and partial model tokens.

The protocol and synthetic experiments were performed with a rooted Yandex Station Lite 1. **Audible quality, barge-in, interruptions, and firmware support on other Station families still require device-side acceptance testing** before enabling this experimentally in production.

Related protocol information:
- https://github.com/v3rt3p/quasar-protos
- https://hackalice.me/internals/voice/
