# Voice Timeline Renderer

## Service goal

Voice Timeline Renderer is a backend service for assembling narrated PCM audio
into a deterministic mixdown. It gives audio-platform engineers a small HTTP
surface for registering 16-bit WAV sources, composing ordered timeline spans,
and rendering a normalized output along with a machine-readable render
receipt.

The project is intentionally production-only. Automated tests, fixtures, and a
test command are deferred to the downstream task stage, which will add focused
unit, integration, and end-to-end verification.

## Users

- Audio-platform engineers who need a self-hosted assembly stage.
- Audio editing tooling that submits WAV sources and timeline operations.
- Quality engineers who replay a render and compare the receipt with the output.

## Project shape and stack

- Shape: backend.
- Language: Go.
- Runtime: Go bundled HTTP packages.
- Persistence: a local data directory containing metadata JSON and media files.
- External services: none.

## Core entities

### Source asset

A source asset is an immutable uploaded PCM WAV. Its identity, SHA-256 digest,
frame count, sample rate, channel count, peak amplitude, RMS amplitude, and duration are
recorded after decoding. A source is never updated in place.

Invariants:

- The payload must be a RIFF/WAVE file with 16-bit integer PCM samples.
- Only mono and stereo layouts are accepted.
- The digest and signal measurements describe the exact bytes stored.
- Decoded payloads are bounded by configured byte and duration limits.

### Impulse response

An impulse response is an immutable, registered PCM WAV used for convolution
reverb. It is a distinct resource from a source recording and shares the same
16-bit mono/stereo format constraints. Its identity, digest, frame count,
sample rate, channel count, and signal measurements are recorded after
decoding. Stored response bytes are never rewritten.

Invariants:

- The payload must be a RIFF/WAVE file with 16-bit integer PCM samples.
- Only mono and stereo layouts are accepted, bounded by configured byte and
  frame limits.
- A response's sample rate may differ from the timeline rate; it is resampled
  to the timeline rate during render preparation without mutating stored media.
- Stereo responses keep an explicit left/right pairing and are never treated
  as two independent mono signals.

### Assembly timeline

A timeline is an ordered set of spans over existing source assets. A span
selects a half-open source frame interval, a gain in decibels, an optional
crossfade with the preceding span, and an optional per-span spatial
(convolution reverb) configuration referencing a registered impulse response.

Invariants:

- A timeline has at least one span.
- Every referenced source and impulse response exists.
- Source intervals are non-empty and remain inside the source frame count.
- The first span has no crossfade.
- A crossfade cannot exceed the shorter neighboring span.
- Spatial wet and dry gains are finite and inside the accepted gain range.
- Output channel count is explicit and independent from an input source's
  channel count.
- The projected frame count, including the final span's convolution tail,
  stays inside the configured output limit.

### Render job

A render job is the durable record of one timeline execution.

State machine:

`queued -> rendering -> finalizing -> completed`

Any active state may transition to `failed`. `completed` and `failed` are
terminal. A failed job stores a stable error code and message. A completed job
stores the output file, output metadata, and immutable render receipt.

### Render receipt

A receipt records source identifiers, span placement, gain adjustments, peak
and RMS measurements, final normalization gain, frame count, output digest, and
non-fatal warnings. It is written only after the output bytes have been
atomically committed.

Spatial renders use receipt schema version 2 and additionally record, per
span, the impulse digest and original rate/frame count, whether resampling or
channel adaptation was applied, the wet/dry gains, and the exact convolution
tail frame range. A `duration` block makes the final project duration explicit
and re-checkable: the dry timeline frame count, the tail extension added by
the final processed span, the total output frame count (which equals the
encoded WAV frame count), and a human-readable statement of the meaning. A
`convolution` block documents the streaming engine, block size, FFT size, and
the single-convolver working-set bound. Legacy timelines without spatial
processing keep a schema version 1 receipt and render their original bytes.

## Workflows

### 1. Register audio source

Entry point: `PUT /v1/sources/{source_id}`.

1. The HTTP boundary enforces request size and base64 decoding.
2. The service validates identity fields and decodes the WAV structure.
3. The codec layer verifies PCM format, frame alignment, and signal bounds.
4. Signal measurements are computed from the decoded samples.
5. Storage writes the media bytes and metadata using an atomic commit.

Success returns source metadata. Failure returns a stable validation error and
leaves no visible partial source.

### 2. Register impulse response

Entry point: `PUT /v1/impulses/{impulse_id}`.

1. The HTTP boundary enforces request size and base64 decoding.
2. The service validates identity fields and decodes the WAV structure.
3. The codec layer verifies PCM format, frame alignment, and signal bounds and
   rejects responses beyond the configured frame limit.
4. Signal measurements are computed from the decoded samples.
5. Storage writes the media bytes and metadata under the separate `impulses/`
   namespace using an atomic commit.

Success returns impulse metadata. Failure returns a stable validation error and
leaves no visible partial impulse. Source recordings are never touched.

### 3. Compose assembly timeline

Entry point: `PUT /v1/timelines/{timeline_id}`.

1. The HTTP boundary decodes and sizes the span list.
2. Domain validation checks ranges, gain limits, crossfade limits, temporal
   ordering, and every spatial impulse reference and spatial gain.
3. The service resolves every source and impulse reference from storage.
4. The dry output frames and the projected frames including convolution tails
   are computed and checked against the configured limit.
5. A timeline record is persisted once all spans are valid.

Success returns a ready timeline. Failure reports the first stable validation
issue without persisting a partial timeline.

### 4. Render timeline mixdown

Entry point: `PUT /v1/renders/{render_id}` with a `timeline_id` body field.

1. The service creates a durable `queued` job.
2. The renderer moves to `rendering` and loads all referenced sources and
   impulse responses.
3. Each source interval is resampled to the timeline sample rate, mapped to the
   requested channel layout, gain-adjusted, faded, and placed on the timeline.
4. Spatially processed spans stream through a fresh per-span overlap-save
   convolution using a shared, read-only, rate-matched response working set.
   Reverb tails are mixed into following frames; the final span's tail extends
   the project. Spans without spatial config keep the legacy dry path.
5. Overlapping neighboring spans are mixed with equal-power crossfade weights.
6. The finalizing stage applies peak normalization to the requested target,
   writes a WAV and receipt through temporary files, and renames them into
   place atomically.
7. The job moves to `completed`. Any error, including request cancellation,
   records a failed terminal state and removes unpublished temporary output; a
   canceled render never commits a finished artifact.

Success is observable through the job resource, the downloaded WAV, and the
receipt. Failure is observable through the terminal job error code.

## HTTP interface

- `GET /healthz`
- `PUT /v1/sources/{source_id}`
- `GET /v1/sources/{id}`
- `PUT /v1/impulses/{impulse_id}`
- `GET /v1/impulses/{id}`
- `PUT /v1/timelines/{timeline_id}`
- `GET /v1/timelines/{id}`
- `PUT /v1/renders/{render_id}`
- `GET /v1/renders/{id}`
- `GET /v1/renders/{id}/receipt`
- `GET /v1/renders/{id}/output`

Errors use a JSON envelope:

```json
{
  "error": {
    "code": "invalid_source_interval",
    "message": "source interval ends beyond the source frame count",
    "field": "spans[0].source_end_frame"
  }
}
```

## Modules

| Module | Responsibility |
|---|---|
| `httpapi` | HTTP routing, request limits, JSON encoding, media responses |
| `service` | Workflow orchestration and job lifecycle |
| `domain` | Entities, validation rules, stable errors, state transitions |
| `audiocodec` | WAV parsing and encoding, resampling, channel mapping, signal metrics |
| `convolve` | Memory-bounded streaming overlap-save FFT convolution |
| `render` | Span evaluation, crossfade mixing, spatial convolution, normalization, receipt construction |
| `storage` | Atomic source, impulse, timeline, job, output, and receipt persistence |

## Engineering mechanisms

- State transitions: render jobs persist every active and terminal transition.
- Error propagation: domain errors retain stable codes through HTTP responses.
- Resource lifecycle: temporary files are cleaned after failed publication; a
  canceled render fails terminally and never reaches artifact publication.
- Concurrency: storage guards indexes and registry updates; unique IDs and
  atomic renames prevent duplicate or half-visible records.
- Validation boundaries: request size, PCM layout, timeline bounds, impulse
  references, and gain ranges are checked before allocation-heavy work.
- Streaming convolution: impulse partitions are prepared once and shared
  read-only; each span owns a fresh, fixed-size convolver that is released
  before the next span starts, so long responses never retain a per-span full
  convolution working set and span histories never leak between spans.

## Validation plan

The runtime manifest exposes one bounded production smoke path per workflow:

- Register a generated WAV, read it back, and confirm a malformed payload fails.
- Compose a two-span timeline, read it back, and confirm an invalid crossfade
  fails.
- Render a crossfaded timeline, download the output, decode it, and match core
  receipt measurements.
- Register a different-rate stereo impulse response, render a spatial timeline
  reusing one recording across two spans, and verify the tail-extended
  duration semantics, v2 receipt facts, stereo image, and rejection of an
  unknown impulse.

These checks use the real HTTP entry points and local storage. Unit tests
additionally pin the convolution math to a direct-reference result, the
streaming working-set bound, independent span histories, projected-duration
math, legacy render byte identity, and the canceled-render lifecycle.
