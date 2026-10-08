# Notifications

Sentinel sends events to **channels** defined in the central file
(`notifications.channels`, see [configuration.md](configuration.md#notifications)).
This page is the contract for receivers: what is sent, when, and how.
Only the `webhook` channel type exists; `slack` and `teams` are planned.

## Payload

Every delivery is one HTTP request (`POST` by default) whose JSON body is
a versioned envelope around the event:

```json
{
  "payload_version": 1,
  "event": {
    "event_id": "6f1c2a9e0b7d4e3f8a5c1d2e3f4a5b6c",
    "timestamp": "2026-10-08T12:00:00Z",
    "hostname": "web1",
    "sentinel_version": "1.0.0",
    "module": "supervisor",
    "source": "nginx",
    "source_type": "systemd",
    "event_type": "service_failed",
    "severity": "error",
    "state": "failed",
    "previous_state": "healthy",
    "message": "nginx.service failed (exit code 1)",
    "correlation_id": "6f1c2a9e0b7d4e3f8a5c1d2e3f4a5b6c",
    "metadata": {"unit": "nginx.service"},
    "attributes": {"exit_code": 1, "failure_count": 3}
  },
  "suppressed_count": 2
}
```

| Field | Meaning |
|---|---|
| `payload_version` | Format of this body; `1`. A change that is not backwards compatible increments it. |
| `event` | The event (below). |
| `suppressed_count` | Present when `repeat_interval` held back deliveries of the same event since the last one: how many. |

Event fields:

| Field | Always | Meaning |
|---|---|---|
| `event_id` | yes | Random 128-bit identifier, 32 hex digits. |
| `timestamp` | yes | When it happened, UTC (RFC 3339). |
| `hostname`, `sentinel_version` | yes | The emitting host and Sentinel version. |
| `module` | yes | `core` or the module that emitted it (`supervisor`, `firewall`, …). |
| `source` | yes | The emitter inside the module: a service name, a provider name, `daemon`. |
| `source_type` | yes | Kind of emitter: `daemon`, `systemd`, `cron`, … |
| `event_type` | yes | What happened (below). |
| `severity` | yes | `info`, `warning`, `error` or `critical`. |
| `state`, `previous_state` | no | The emitter's state after and before, in its source type's vocabulary. |
| `message` | yes | Human-readable summary. |
| `correlation_id` | no | Groups the events of one incident or operation (a failure and its recovery attempts, a firewall transaction). |
| `metadata` | no | Small string labels. |
| `attributes` | no | Structured details: numbers, booleans, strings, lists of strings, one nested level. Each event type documents its keys. `attributes_truncated: true` means they were dropped to respect the size limit. |

Receivers should ignore fields they do not know: new fields can be added
within the same `payload_version`.

### Event types

Every module registers the types it emits; there are no free-form types.

| Module | Event type | Default severity | Emitted when |
|---|---|---|---|
| `core` | `configuration_error` | error | a configuration could not be loaded or applied |
| `core` | `daemon_error` | error | sentineld itself failed |

Module event types are listed with each module (Phase 3 adds the
supervisor's `service_*`, `recovery_*` and `job_*` types).

### Content and limits

Text can come from outside the host (unit descriptions, container labels,
HTTP bodies), so before an event is delivered Sentinel:

- removes control characters and invalid UTF-8 from every string;
- shortens identifiers to 256 bytes, the message to 2048 bytes, metadata
  values and attribute strings to 1024 bytes, looking at no more than
  four times that limit (a field made only of control characters there
  ends up empty; an event without a source is rejected);
- keeps the encoded event under 16 KiB: if needed, attributes are
  replaced by `{"attributes_truncated": true}`, then metadata is dropped,
  then the message is shortened.

Secrets do not belong in events: emitters redact them, and environment
values never appear in configuration problems. The receiver is trusted
with the event content (threat model T-06).

## Routing

A channel receives an event only if a route sends it there:

- **core events** follow `notifications.core` in the central file;
- **module events** follow the `notifications` of the item that produced
  them (a service, a job), in the module's directory.

A route is a list of channel names, or a mapping that also filters event
types (D-004):

```yaml
notifications: [ops]
notifications: {channels: [ops, oncall], events: [service_failed, recovery_exhausted]}
```

Without a route an event is only logged. A route to a disabled channel
delivers nothing.

## Delivery

- **Asynchronous and bounded.** Each channel has a queue of 256 events and
  one worker. A slow or failing receiver never delays other channels or
  the module that emitted the event. When a queue is full, its oldest
  event is dropped and counted.
- **Order.** Events reach a channel in the order they were emitted by one
  source; there is no global order.
- **Retries.** A failed attempt (network error, timeout, a status not
  accepted) is retried up to `retry.attempts` times in total, after
  `retry.delay`, doubled each time with `backoff: exponential`, never more
  than `retry.max_delay`. After the last attempt the event is counted as
  failed for that channel.
- **Accepted status.** Any 2xx, or exactly the codes in
  `success_status_codes`. A 3xx is never a success.
- **Repeat interval.** With `repeat_interval` set, the same event (same
  module, source, source type and event type) is delivered at most once
  per interval; the next delivery carries `suppressed_count`. Default `0s`:
  every event is delivered. A channel tracks at most 4096 distinct events
  at a time: when all of them are inside their interval, a new event is
  delivered without being tracked (open windows and counts not yet
  reported are kept).
- **Not persisted.** Delivery is in memory: events
  queued when sentineld stops are lost. A receiver that needs every event
  should also read the journal or the state.

## Security

- **No redirects.** A 3xx response is a failure, never followed: a
  redirect could carry the channel's headers (often a token) to another
  host.
- **TLS.** TLS 1.2 or newer; the system roots or `tls.ca_file`;
  `tls.insecure_skip_verify` exists for test setups and warns. Whoever
  can change the CA bundle can intercept the channel, so `tls.ca_file`
  must be a regular file owned by root or the daemon user, not writable
  by group or others, in a directory checked like the configuration's; it
  may be a link only to another file of the same directory.
- **Timeouts.** `timeout` bounds each attempt, connection included. Only
  64 KiB of a response body are read, and discarded.
- **Secrets.** Put tokens in the environment (`${VAR}`), never in YAML.
  Errors never quote the channel URL; `sentinelctl config show` reduces
  URLs to scheme and host and masks header values.
- **Plain HTTP** sends events unencrypted and warns.
