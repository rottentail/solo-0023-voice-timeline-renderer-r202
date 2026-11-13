# Voice Timeline Renderer

Voice Timeline Renderer is a self-hosted Go service for turning validated PCM
WAV recordings into a crossfaded, peak-normalized mixdown. The service exposes
an idempotent HTTP surface, keeps media and metadata under one data directory,
and records every render transition.

The implementation covers:

- 16-bit integer PCM WAV validation and signal measurement.
- Ordered timeline spans with source-frame selection, gain, and crossfade.
- Linear sample-rate conversion and mono/stereo channel mapping.
- Equal-power neighboring-span crossfades.
- Registered room impulse responses with per-span convolution reverb,
  including rate-matched responses, preserved stereo pairing, streamed
  reverb tails, and explicit project-duration semantics.
- Peak normalization into a requested dBFS target.
- Durable render states, receipts, and atomically committed output folders.

The spatial stage ships with focused unit tests (convolution accuracy against
a direct reference, bounded working sets, independent span history,
cancellation) plus an end-to-end `render-spatial` workflow check.


## Requirements

- Go 1.23 or newer.
- A writable local directory for source media, timeline metadata, render jobs,
  output WAV files, and receipts.

No network service, database server, or third-party Go module is required.

## Build

```bash
go build ./...
```

## Run

```bash
go run ./cmd/voiceservice \
  -addr :8093 \
  -data ./var/voice-timeline
```

The health endpoint is `GET http://127.0.0.1:8093/healthz`.

Runtime settings are supplied through arguments:

- `-addr`: HTTP listen address, default `:8093`.
- `-data`: persistent data root, default `./var/voice-timeline`.

No environment variable is required. Do not put credentials in the repository;
the service does not consume any remote credential.

## HTTP interface

### Register a source

`PUT /v1/sources/{source_id}`

```json
{
  "name": "opening take",
  "wav_base64": "UklGR..."
}
```

The source ID is chosen by the caller and must be 3-64 characters using
letters, digits, underscore, or hyphen. Repeating the same ID with exactly the
same name and bytes is idempotent. Reusing it with different content returns
the `source_conflict` error.

### Register an impulse response

`PUT /v1/impulses/{impulse_id}`

```json
{
  "name": "studio booth",
  "wav_base64": "UklGR..."
}
```

An impulse response is a separate, immutable read-only asset from source
recordings: it is stored under its own `impulses/` namespace and has the same
16-bit PCM mono/stereo constraints. Its sample rate does not have to match the
timeline; responses are resampled to the timeline rate at render time, and the
original stored bytes are never modified.

### Compose a timeline

`PUT /v1/timelines/{timeline_id}`

```json
{
  "name": "two part narration",
  "sample_rate": 48000,
  "output_channels": 2,
  "target_peak_dbfs": -1,
  "spans": [
    {
      "source_id": "opening_take_01",
      "source_start_frame": 0,
      "source_end_frame": 48000,
      "gain_db": -2,
      "crossfade_frames": 0,
      "spatial": {
        "impulse_id": "studio_booth_01",
        "wet_gain_db": -3,
        "dry_gain_db": -6
      }
    },
    {
      "source_id": "closing_take_01",
      "source_start_frame": 0,
      "source_end_frame": 44100,
      "gain_db": 0,
      "crossfade_frames": 4800
    }
  ]
}
```

Spans are sequential. A later span overlaps its predecessor by
`crossfade_frames`; the overlap is mixed with equal-power weights. A crossfade
must fit within both neighboring spans.

The optional per-span `spatial` block enables convolution reverb. Omitting it
keeps the legacy gain/fade-only behavior. Each spatially processed span keeps
an independent, fresh convolution state, so spans that reuse the same
recording never inherit one another's processing history. A stereo signal
stays a coherent left/right pair (a mono response is applied to both channels
independently; a stereo response pairs left with left and right with right)
and is never split into two unrelated mono signals.

Convolution of N span frames with an M-frame response produces N+M-1 frames, so
the reverb tail continues after the span's own audio and mixes into any
following sound. Only the tail of the final processed span extends the
project. The persisted timeline reports both `output_frames` (the dry span
length) and `projected_frames` (the length including the tail extension).

### Start a render

`PUT /v1/renders/{render_id}`

```json
{
  "timeline_id": "two_part_narration_01"
}
```

The render runs through the durable states
`queued -> rendering -> finalizing -> completed`. A failed or canceled render
records a terminal `failed` state with a stable error code and does not expose
a partial output folder; a canceled context never reaches artifact
publication.

### Read resources

- `GET /v1/sources/{source_id}`
- `GET /v1/impulses/{impulse_id}`
- `GET /v1/timelines/{timeline_id}`
- `GET /v1/renders/{render_id}`
- `GET /v1/renders/{render_id}/receipt`
- `GET /v1/renders/{render_id}/output`

The output route returns `audio/wav`. The receipt records source digests, span
placement, channel mapping, measured peak and RMS amplitude, normalization
gain, output digest, and non-fatal warnings. A spatial render uses receipt
schema version 2 and additionally records, per span, the impulse digest and
its original rate/frame count, resampling and channel actions, wet/dry gains,
and exact tail frame ranges, plus a `duration` block stating precisely what
the final project length means:

```json
{
  "schema_version": 2,
  "duration": {
    "dry_timeline_frames": 38400,
    "tail_extension_frames": 14399,
    "total_output_frames": 52799,
    "meaning": "total_output_frames is the dry timeline span length plus ..."
  },
  "convolution": {
    "method": "uniform_partition_overlap_save_fft",
    "block_frames": 1024,
    "fft_size": 2048,
    "prepared_impulse_count": 1,
    "max_partitions_per_impulse": 15,
    "concurrent_span_convolvers": 1
  }
}
```

Legacy timelines without spatial processing keep a schema version 1 receipt
and render their original bytes unchanged.

## Data directory

```text
var/voice-timeline/
  sources/<source_id>/
    source.wav
    metadata.json
  impulses/<impulse_id>/
    impulse.wav
    metadata.json
  timelines/<timeline_id>.json
  jobs/<render_id>.json
  renders/<render_id>/
    output.wav
    receipt.json
  tmp/
```

Source, impulse, and render folders are assembled in `tmp/` and moved into
place with one directory rename. Metadata updates use same-directory temporary
files followed by rename.

## Source layout

```text
cmd/voiceservice/       service entry point
cmd/timelinecheck/      workflow execution entry point
internal/httpapi/       routing, request limits, JSON errors, media response
internal/service/       workflow orchestration and idempotency
internal/domain/        entities, validation, errors, and render transitions
internal/audiocodec/    WAV handling, transforms, and signal measurement
internal/convolve/      streaming overlap-save FFT convolution engine
internal/render/        timeline preparation, mixing, spatial, normalization, receipt
internal/storage/       atomic local repository
internal/checkflow/     bounded production workflow checks
```

## Workflow checks

Run each declared production check from the repository root:

```bash
go run ./cmd/timelinecheck -workflow ingest-audio-source
go run ./cmd/timelinecheck -workflow compose-timeline
go run ./cmd/timelinecheck -workflow render-mixdown
go run ./cmd/timelinecheck -workflow render-spatial
```

Each check creates an isolated temporary data directory, starts the real HTTP
handler, sends generated WAV inputs, and verifies read-back or rendered output.
The checks are bounded and do not require network access. The `render-spatial`
check registers a different-rate stereo impulse, renders a two-span spatial
timeline that reuses one recording, verifies the tail-extended duration
semantics and v2 receipt against the decoded output, and confirms a missing
impulse is rejected without publishing anything.

## Tests

Focused Go unit tests cover the convolution engine (direct-reference
accuracy, stereo pairing, bounded working set, independent span history,
cancellation), the domain spatial validation and projected-duration math, the
render pipeline (legacy byte identity, tail math, cancellation, dry-only
behavior), and the service render lifecycle (a canceled render reaches the
terminal failed state and never commits artifacts). Run them with:

```bash
go test ./...
```

