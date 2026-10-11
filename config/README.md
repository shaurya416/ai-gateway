# Configuration

The gateway is configured by a YAML or JSON file (format auto-detected) named by
the `GATEWAY_CONFIG` environment variable. **A config file is read only when
that variable names it** — nothing is discovered, so a stray file on a host can
never change how a gateway routes. Decoding is strict: an unknown key is
rejected at load, by `ferrogw validate`, at startup, and on `PUT /admin/config`
alike.

This package holds the schema (`config.go`) and the loader/validator
(`load.go`). The full annotated reference — every option, with inline comments —
is [`../config.example.yaml`](../config.example.yaml) /
[`../config.example.json`](../config.example.json).

```yaml
strategy:
  mode: fallback            # how targets are ordered — see the strategies README

targets:                    # the providers this gateway may route to (an allowlist)
  - virtual_key: openai
    retry: { attempts: 3 }  # per target, applies under every routing mode
  - virtual_key: anthropic
  - virtual_key: gemini
    models: [gemini-2.5-flash]   # models declared by the operator — additive only

aliases:
  fast: gpt-4o-mini         # resolved before routing

plugins:                    # guardrails and middleware — see the plugin README
  - name: word-filter
    type: guardrail
    stage: before_request
    enabled: true
    config: { blocked_words: ["password"] }
```

## Section map

| Section | What it does | Reference |
|---|---|---|
| `strategy` | Target *ordering* — 8 modes in two families (pool vs named); `sticky` (load-balance, ab-test), `failover_on_status_codes`, and rule `target_keys` chains | [`../internal/strategies/README.md`](../internal/strategies/README.md) |
| `targets[]` | The provider allowlist; per-target `timeout` (one attempt), `retry`, `concurrency`, `circuit_breaker`, `weight`, declared `models`, and `model_map` | [`../config.example.yaml`](../config.example.yaml) |
| `aliases` | Model-name aliases, resolved before routing | — |
| `plugins[]` | Guardrails, caching, rate limits, budgets, logging | [`../plugin/README.md`](../plugin/README.md) |
| `mcp_servers[]` | MCP tool servers (stdio + Streamable HTTP) | [`../mcp/README.md`](../mcp/README.md) |
| `observability` | OpenTelemetry tracing + exporter plugins | [`../observability/README.md`](../observability/README.md) |
| `notifications` | Webhooks posted on circuit-breaker transitions, a budget's first refusal, and error-rate spikes | [below](#webhook-notifications-notificationswebhooks) |
| `compatibility` | `on_unsupported_param: warn \| drop \| reject` for parameters a provider cannot express | [`../providers/README.md`](../providers/README.md) |
| `request_timeout` | End-to-end bound on one non-streaming request, retries included | — |
| `batch_target` / `responses_target` | The single backend serving `/v1/files*` + `/v1/batches*`, and the Responses id sub-routes | [`../providers/README.md`](../providers/README.md) |

Validate any file without starting the server:

```bash
ferrogw validate            # every virtual_key, plugin name/stage, and strategy
                            # must resolve; credentials are deliberately NOT checked
```

## `${VAR}` environment references

Values in plugin `config`, MCP `headers`/`env`, and observability exporter
`config` may reference environment variables with the **braced form only**:
`${MY_TOKEN}`. References resolve **when the component is constructed**, never
at file load — so the config carries only the reference for its whole life, and
a secret is never written to the config-history store, never served by
`GET /admin/config`, and never restored by a rollback. A bare `$` is data
(`$100`, `pa$$w0rd` survive verbatim); an undefined variable is an error rather
than a silently blank secret.

## Declared models (`targets[].models`)

Names models a target serves that neither the model catalog nor live discovery
can see — an id newer than the catalog, a preview or regional name, a
self-hosted deployment behind `<PROVIDER>_BASE_URL`. Declared models join the
routing index and `/v1/models` alongside the automatic sources. The field is
**additive only**: declaring one model never hides the others a target serves.
Wildcards are rejected at load. The same id on two targets is how a model gets
a fallback.

## Cross-provider model mapping (`targets[].model_map`)

Maps the model key visible after global alias resolution to the exact model ID
sent to one target. The keys join both the routing index and `/v1/models`
inventory; `targets[].models` remains additive and can still declare other
models. Cost and pricing use the mapped upstream model, not the visible key.
Responses name the visible key as their `model` on every surface, streamed
chunks included; the upstream ID never reaches the client.

For fallback, every target intended to participate for that visible model must
contain the same key; unrelated configured targets need not. The client below
always requests `smart`, while each provider receives its own model ID:

```yaml
strategy: { mode: fallback }
targets:
  - virtual_key: openai
    model_map: { smart: gpt-5 }
  - virtual_key: anthropic
    model_map: { smart: claude-sonnet-4-6 }
```

## Webhook notifications (`notifications.webhooks`)

Each webhook receives a JSON `POST` for the events it subscribes to:

```yaml
notifications:
  webhooks:
    - name: oncall                     # labels logs and gateway_webhook_deliveries_total
      url: ${ONCALL_WEBHOOK_URL}       # http(s); resolved at construction, never stored
      secret: ${ONCALL_WEBHOOK_SECRET} # optional; must be a ${VAR} reference
      timeout: 5s                      # per delivery attempt (default 5s)
      events: [circuit.opened, circuit.half_open, circuit.closed, budget.exceeded, error_rate.high]
      error_rate:                      # required with error_rate.high, refused without it
        threshold: 0.5                 # failing fraction in (0, 1]
        window: 1m                     # sliding window (default 1m)
        min_requests: 20               # fewest attempts judged (default 10)
```

| Event | Fires when | `details` |
|---|---|---|
| `circuit.opened` / `circuit.half_open` / `circuit.closed` | a target's breaker changes state — once per transition | `from`, `to` |
| `budget.exceeded` | the budget plugin refuses a key for the first time since it was last admitted (a reset re-arms it) | `plugin`, `store_id`, `api_key_id`, `limit_usd`, `spent_usd` |
| `error_rate.high` | a target's failing fraction of routing attempts over `window` rises to `threshold` with at least `min_requests` attempts — once per rise; falling back below re-arms it | `error_rate`, `errors`, `requests`, `threshold`, `window`, `min_requests` |

The body is `{"event", "time" (UTC RFC 3339), "target", "provider", "details"}`
and never carries a credential, a request body, or prompt text; a key is named
by its opaque id. Every delivery carries `X-Ferro-Event`, `X-Ferro-Delivery`
(the same id on each retry of one event) and `X-Ferro-Timestamp` (Unix
seconds). With a `secret`, `X-Ferro-Signature` is
`sha256=` + hex(HMAC-SHA256(secret, timestamp + "." + body)) — verify it over
the raw body, and refuse an old timestamp to reject a replay.

An unknown event, a duplicate name, a literal `secret`, a URL that is not
`http(s)`, or an `error_rate` block that does not match the event list is a load
error. A `${VAR}` that does not resolve at startup disables that webhook with an
`ERROR` log line; it does not stop the gateway.

Delivery never runs on the request path. Each webhook has its own queue (256
events); when it is full the oldest pending event is dropped and counted. A
`5xx` or a transport failure is retried up to three attempts in all with
exponential backoff; any other non-`2xx` answer — including a redirect — is
final. `gateway_webhook_deliveries_total{webhook,event,outcome}` counts each
event's end: `delivered`, `rejected`, `failed`, `dropped`, or `abandoned` (still
pending when the shutdown grace ran out). On shutdown queued events are
delivered for up to five seconds and the rest abandoned.

`GET /admin/config` serves a webhook URL as its scheme and host only — chat and
incident tools put the token in the path — so write the URL as a `${VAR}`
reference to keep the config editable round-trip.

## Trusted proxies (`TRUSTED_PROXIES`)

By default the gateway trusts `X-Forwarded-For` / `X-Real-IP` only from loopback,
so per-IP rate limiting and request logs see the real client IP without being
spoofable. Behind a reverse proxy or load balancer, set the proxy's CIDR(s):

```bash
TRUSTED_PROXIES=10.0.0.1/32                 # single upstream proxy
TRUSTED_PROXIES=10.0.0.0/8,172.16.0.0/12    # VPC / multiple ranges
```

| Deployment | Recommended value |
|---|---|
| Local dev (no proxy) | _(leave unset — loopback default)_ |
| Docker Compose with nginx | `172.16.0.0/12` or the bridge subnet |
| AWS ALB / GCP LB | your VPC CIDR (e.g. `10.0.0.0/8`) |
| Kubernetes cluster-internal | your pod/node CIDR |
| Cloudflare Tunnel | Cloudflare's published IP ranges |

Configure the proxy to **replace** `X-Forwarded-For`, not append: the gateway
resolves the client from the far end of the chain, and an appending proxy in
front of an untrusted hop lets a caller forge its address. A request from an IP
outside the trusted list has its forwarded headers ignored entirely.

## Live changes

`PUT`/`POST /admin/config` applies a config over the API with the same strict
decoding and validation as a file; history is recorded (with the acting
credential) and `POST /admin/config/rollback` restores a prior version. Free-form
secret-bearing values are withheld from `GET /admin/config` — see
[SECURITY.md](../SECURITY.md).
