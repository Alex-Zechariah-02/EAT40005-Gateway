# Lightweight Security Policy Gateway for REST APIs

This Go gateway enforces a shared, configurable security policy before requests reach REST APIs. It runs its own HTTP server and reverse proxy using the Go standard library.

## Security controls

The gateway has five security controls:

- **JWT authentication** checks RS256 access tokens with a trusted issuer, audience, expiry, subject and named signing key.
- **Role-based access control** allows or blocks each call by the role of the caller.
- **Rate limiting** counts requests per account or direct client address for each operation over a sliding time window. Public operations use the client address. A request above the limit gets the answer 429 and goes no further.
- **Request validation** checks the JSON body and selected headers against the operation's bounded JSON Schema. Bodies can include arrays with item schemas and item-count limits. Ambiguous fields and malformed Unicode are rejected.
- **Security event logging** records each security decision in a local file. A request goes to an API only after its record is safely on disk. If the log cannot be written, the gateway stops letting requests through.

Each request uses one policy version from start to end. When one control rejects a request, it never reaches an API. The gateway checks tokens from an outside identity provider and does not issue tokens itself. A token's `kid` selects one of the trusted public keys. Roles come from verified token claims and are kept only when the captured policy defines them. A separate blocked-account list refuses a verified account immediately and survives policy rollback. Public keys can be reloaded from operator files; an invalid reload keeps the current key set.

Policy documents use schema version 4 and contain roles and permits without per-user membership. Version 3 policies are refused rather than converted automatically. A fresh demonstration creates a version 4 policy.

## Safe policy changes

A policy can pass all checks and still cause problems when it is in use. Administrators can change the shared policy through two workflows:

- **Confirmed Policy Activation.** A new permanent policy starts as provisional and is enforced at once, with a confirmation deadline of 1 second to 24 hours. An administrator confirms it or rolls it back. If nobody confirms it in time, the gateway goes back to the previous policy by itself, also when it was stopped over the deadline. A provisional policy never admits a request after its deadline, even if the rollback runs late. Only one provisional policy can exist at a time, and each one ends exactly once.
- **Temporary Policy Override.** An administrator can give a role one more operation or take one away, raise or lower an operation's rate limit, or block an operation, for 1 second to 24 hours. The change is applied on top of the permanent policy without changing it, and it ends by itself at its expiry time or when it is revoked. It cannot skip token checks or request checks, reduce logging, open a blocked operation or change where requests go. Two overrides cannot change the same thing, and a new permanent policy that would clash with an override is refused. While a provisional policy waits for confirmation, no new override can be made. Rate counts carry over, so an override never gives a fresh quota. A restored backup never brings an old override back.

## Import an OpenAPI description

`gateway policy import-openapi` turns an OpenAPI 3.0 or 3.1 description (JSON, or YAML 1.2) into a new policy file for review. It runs offline: it reads only the files it is given, never fetches a `$ref`, and never activates anything. Stage the result as a draft, trial it and confirm it like any other change.

```
gateway policy import-openapi --spec <file.json|.yaml|.yml> --api <id> --upstream <id>
  --base <policy.json> [--max-body-bytes <n>] [--with-permits] --out <new file> --report <new file>
```

| Flag | Meaning |
|---|---|
| `--spec` | The description, at most 1 MiB; the extension chooses JSON or YAML |
| `--api` | The API to create, or to replace with its operations and the permits that name them |
| `--upstream` | An upstream already in `--base` that serves the API |
| `--base` | The policy to build on, for example the output of `demo policy export` |
| `--max-body-bytes` | Body limit for imported request bodies without `x-apiward-max-bytes`: 1 to 1,048,576, default 16,384 |
| `--with-permits` | Read `x-apiward-roles` into permits, adding missing roles; without it no permit is created |
| `--out`, `--report` | New files; the command never overwrites a file and writes both or neither |

Exit status: `0` every operation mapped; `2` both files written, but some operations were refused or widened (read the report); `1` nothing written (bad flags, unreadable or refused description, an output file exists, or the result does not compile).

**How it maps.** Each path and method becomes one operation `<api>.<operationId>`, allowed, with sign-in required unless the description says `security: []`. No permits are created unless `--with-permits` reads `x-apiward-roles`; grant roles afterwards. A path whose last segment is a parameter (`/orders/{id}`) becomes the prefix `/orders/*`, which also matches deeper paths. Header parameters become the header schema; query, path and cookie parameters are listed as not enforced. A JSON request body becomes a body rule. Schema `$ref`s inside the file are inlined, descriptive keywords (`description`, `example`, `x-…`) are removed, and an object without `additionalProperties` allows extra fields, as OpenAPI does. Every operation is checked by the gateway's own compiler, and the whole result is compiled before anything is written.

**What is refused.** An operation is left out, with a code in the report, when it cannot be expressed exactly: `TRACE` (`METHOD_REFUSED`); a missing or invalid `operationId` (`OPERATION_ID_INVALID`, with a suggestion) or one used twice (`OPERATION_ID_DUPLICATE`); a parameter in the middle of a path (`PATH_TEMPLATE_UNSUPPORTED`); two operations on the same method and target (`TARGET_COLLISION`); API keys, Basic, mutual TLS or optional sign-in (`SECURITY_SCHEME_UNSUPPORTED`); non-JSON bodies (`MEDIA_TYPE_UNSUPPORTED`); a schema keyword the gateway cannot enforce, such as `format`, `pattern` or `oneOf` (`SCHEMA_KEYWORD_UNSUPPORTED`; dropping it would accept more than described); a `$ref` to another file (`EXTERNAL_REF`), a missing one (`REF_INVALID`) or a circular one (`SCHEMA_CYCLE`); a schema deeper than 16 levels or larger than 4,096 values once inlined (`SCHEMA_LIMIT_EXCEEDED`); and anything else the compiler rejects (`COMPILER_REFUSED`, with its code and field). YAML anchors, aliases, merge keys, tags, several documents in one file, duplicate or non-string keys and `.nan`/`.inf` refuse the whole file, naming the line.

**Report.** JSON; every list is always present:

```json
{"spec_version": "3.1.0", "source_format": "yaml", "api": "orders", "operations_in_spec": 8,
 "mapped": [{"method": "GET", "path": "/orders", "operation": "orders.listOrders", "target": "/orders", "auth": "required"}],
 "widened": [{"method": "GET", "path": "/orders/{id}", "operation": "orders.getOrder", "target": "/orders/*", "auth": "required", "note": "matches /orders/<anything>, including deeper paths"}],
 "assumed": [{"operation": "orders.createOrder", "field": "body.max_bytes", "value": 16384}],
 "changed": [{"operation": "orders.updateOrder", "field": "body", "note": "The description allows no body; the gateway refuses a request without one (415 without a JSON Content-Type, else 400 malformed_json)."}],
 "refused": [{"method": "GET", "path": "/orders/{id}/items", "code": "PATH_TEMPLATE_UNSUPPORTED", "message": "A templated segment followed by more segments cannot be expressed."}],
 "needs_permissions": ["orders.listOrders", "orders.getOrder"],
 "scopes_not_mapped": ["orders:read"],
 "not_enforced": [{"operation": "orders.listOrders", "parameter": "page", "in": "query"}],
 "annotations_removed": [{"operation": "orders.createOrder", "field": "#/components/schemas/NewOrder/description"}],
 "removed_operations": ["orders.old"],
 "removed_permits": [{"role": "customer", "operation": "orders.old"}],
 "ignored": ["responses", "servers", "tags"]}
```

`mapped` and `widened` list the imported operations (`note` explains a default they rely on); `assumed` lists values the import chose; `changed` lists behaviour the gateway enforces differently from the description; `refused` entries may add `field` (a JSON Pointer into the description, or the compiler's field) and `suggestion`; `needs_permissions` lists the signed-in operations no role can call yet; `removed_operations` and `removed_permits` are what replacing `--api` took out of the base; `ignored` names description members with no effect on the policy (`servers`, `responses`, `callbacks`, `webhooks`, `links`, `tags`).

**Gateway extensions.** An operation may carry the members the export writes; each is checked, and any other `x-apiward-…` member refuses the operation (`EXTENSION_INVALID`):

| Member | Value | Effect |
|---|---|---|
| `x-apiward-target` | the prefix target the path stands for, such as `/orders/*` for `/orders/{rest}` | the operation is mapped, not widened: the description states that deeper paths match |
| `x-apiward-access` | `allow` or `deny` | the operation's access; absent means `allow` |
| `x-apiward-rate` | `{"algorithm": "sliding-window", "count", "window_seconds", "key"}` | the operation's rate rule, as in a policy |
| `x-apiward-max-bytes` | 1 to 1,048,576 | the body limit; needs a request body |
| `x-apiward-roles` | list of role identifiers | the roles permitted, read only with `--with-permits` |

## Export a policy as OpenAPI

`gateway policy export-openapi` writes one API of a policy as an OpenAPI 3.1 description (JSON, or YAML by the `--out` extension), for review or for tools that read OpenAPI. Importing that file onto the same policy with `--with-permits` gives the same policy back.

```
gateway policy export-openapi --policy <policy.json> --api <id>
  --out <new file.json|.yaml|.yml> --report <new file> [--with-upstream]
```

Each operation becomes the path and method of its target, with `operationId` = the operation identifier without the `<api>.` prefix. A prefix target `/orders/*` becomes `/orders/{rest}` with `x-apiward-target: /orders/*`. Sign-in required becomes the bearer scheme `apiward`; public becomes `security: []`. A body rule becomes a required `application/json` request body with its schema and `x-apiward-max-bytes`; a header schema becomes header parameters (its root `additionalProperties` is not written, since the gateway checks declared headers only). Access, rate rule and permitted roles go into `x-apiward-access`, `x-apiward-rate` and `x-apiward-roles`. The upstream address is written as a server only with `--with-upstream`.

Exit status: `0` the description means exactly what the policy does; `2` both files written, but something differs (read `changed`): a prefix target, which OpenAPI reads as one segment while the gateway also matches deeper paths; an identifier without the `<api>.` prefix, which comes back with it; a target containing `{` or `}`, which is not exported; `1` nothing written. The report lists `exported` (`operation`, `method`, `path`), `changed` (`operation`, `field`, `note`) and `omitted` (what a flag would add).

## Migrate from another gateway

Migration runs in two offline steps with a file to review between them. `gateway migrate import` reads a configuration and writes an **exchange file**: the configuration as an OpenAPI 3.1 description with the gateway extensions above, plus a ledger that classifies every setting it found. `gateway migrate policy` turns an exchange file into a new policy file. Neither command activates anything: read the reports, then stage, trial and confirm the policy like any other change. The supported source format is `openapi`.

```
gateway migrate import --from openapi --in <file.json|.yaml|.yml>
  --out <new exchange file.json|.yaml|.yml> --report <new file> [--decisions <overlay>]
gateway migrate policy --exchange <file> --base <policy.json> --api <id> --upstream <id>
  --out <new file> --report <new file> [--decisions <overlay>] [--with-permits]
```

Each input is read only from the named path and may be at most 8 MiB, nested at most 16 levels; YAML passes the same gate as the OpenAPI import. Outputs must not exist yet; both are created together or neither is, and the output and the report must be different files.

**The ledger** is the root member `x-apiward-migration`: `version` (`1`); `source` (`format`, `format_version`, `sha256` of the input); `items`, one per setting; `carried`, settings kept as opaque data for the gateway they came from (`vendor`, `scope`, `path`, `value`), never compiled into a policy; `credentials_skipped` (`type`, `count`), credentials counted but never read or copied; and `trust.issuers` (`issuer`, `audience`, `public_key_ids`). Each item has `id` (`<format>:<kind>:<name>`), `facet` (`route`, `access`, `auth`, `roles`, `rate`, `validation`, `size`, `upstream`, `transform`, `failure` or `other`), `class`, `direction`, `source` and `target` (JSON Pointers into the input and the exchange file), `note` and `accepted`. The class is `exact`, `equivalent`, `approximated` or `not_expressible`; an approximation also states whether the gateway is `stricter`, `looser` or differs in `both` directions, and a setting that cannot be expressed has an empty `target`. Unknown members anywhere in the ledger, or another `x-apiward-…` root member, refuse the file.

**Looser settings are not imported by default.** `migrate policy` leaves out each operation that a looser item points to, reporting `LOOSER_NOT_ACCEPTED` with the item's `id` in `field`. Accepting that item with a decision (below) imports the operation as described. Only a looser approximation can have `accepted` set to `true`.

**Decisions** are a file of repeatable choices, given with `--decisions`: an [OpenAPI Overlay 1.2.0](https://spec.openapis.org/overlay/v1.2.0.html) document (JSON or YAML, through the same gate) with `overlay: 1.2.0`, an `info` object with `title` and `version`, and a list of `actions`. `migrate import` applies them to the exchange file before writing it, so the file records them; `migrate policy` applies them to the exchange file it reads, before converting it. Actions apply in order, each to the result of the one before, and each must select exactly one node.

- Only `update` actions are accepted (each with a `target`, an `update` and an optional `description`); `remove`, `copy`, reusable actions, `components` and extension members refuse the file.
- A `target` is one of three forms: a normalized JSONPath into an operation, such as `$['paths']['/orders']['get']['x-apiward-rate']`; `$.paths[*][?@.operationId=='<id>']`; or `$['x-apiward-migration'].items[?@.id=='<id>']` (or the item's normalized path, `$['x-apiward-migration']['items'][<n>]`). Write the forms exactly, with single quotes and no spaces.
- An update merges as the Overlay specification states: objects member by member, lists by appending, single values by replacing.
- On an operation a decision may only make `x-apiward-access`, `x-apiward-rate` and `x-apiward-max-bytes` stricter: set access to `deny`; add a rate rule, or lower its `count` or lengthen its `window_seconds` with the same `algorithm` and `key`; lower the body limit (16384 bytes when none is stated). Any other change, including one that would widen access, refuses the file.
- On a ledger item the only decision is to accept a looser approximation, with exactly `{"accepted": true, "direction": "looser"}`.
- An action that selects nothing is **stale**: it changes nothing, is listed in the report's `stale_decisions` as `actions[<n>]: <target>`, and makes the exit status `2`. An action that selects more than one node, a refused action or an unreadable file writes nothing (exit `1`).

**Policy limits.** If the new policy would exceed a limit of the policy document (its size, nesting, or the number of upstreams, APIs, operations, roles or permits), nothing is written and the error starts with `POLICY_LIMIT_EXCEEDED` and names the limit. Import fewer operations, or one API at a time.

Both reports extend the import report with `ledger_summary` (counts of `exact`, `equivalent`, `approximated` by `stricter`, `looser` and `both`, and `not_expressible`), `stale_decisions`, `credentials_skipped` and `carried_count`. Every list is present, possibly empty.

Exit status: `0` every setting is exact or equivalent and nothing was changed, refused, widened, carried, skipped or stale; `2` both files written, but at least one of those happened (read the report); `1` nothing written.

## How it is built

- The gateway has its own HTTP server and reverse proxy, built on the Go standard library. It serves HTTP/1.0 and HTTP/1.1 in plain text. Each request passes all five controls in one pipeline and then goes only to the API that the policy names, once.
- The project implements the policy model and the five controls itself.
- One SQLite controller stores complete policy versions, roles, permits, independent account blocks, administrators and durable change receipts. Protected requests read immutable snapshots rather than querying the database. The gateway refuses protected admission from state it cannot verify.
- Only the project controller changes the policy in use. The data plane has no administration endpoint.
- A protected admin API listens on a local address only. Administrators sign in with a password and hold explicit permissions. The API reads policy and history, stages and checks complete candidates, and activates, confirms or rolls them back. It also manages temporary overrides, independent account blocks and disk-only key reloads. Read-only checks answer whether a role may call an endpoint (with the request pipeline's own matching and permission code, for the policy in force or a draft), report each built-in limit with its current use and how many tokens each trusted key verified, and give fixed-rule advice: risky settings, unused permissions and alerts. Advice never changes anything.
- The Dashboard's header search by Ref and the live events use an in-memory window of the latest 10,000 API outcomes. A security log search reads the active and rotated audit files, newest first and bounded per page, and exports matching requests as CSV or JSON; it names any log file that has been moved away. Event streams have bounded queues, disclose dropped events and stop with the administrator's session. Traffic summaries and a separate loopback Prometheus listener expose bounded operational measurements without token, subject or address labels.
- Local commands set up the first administrator, add or reset administrators, make and restore checked database backups, and upgrade a database made by an earlier version. A restored backup serves nothing until an administrator confirms a new policy that allows nothing, and a provisional policy in the backup never comes back.
- A web dashboard for administrators (in `dashboard/`) is served by the admin listener itself, so it uses the same sign-in, origin and CSRF checks as the admin API. It shows the confirmed policy, any provisional policy and its deadline, the temporary overrides in force and the change history. It stages and activates new policies, confirms or rolls them back and creates or revokes overrides. It also has pages for the security log search, the access checker, limits and advice, and shows active alerts on its overview. Each change is shown as done only when the gateway's receipt arrives, and a request with no answer is shown as having an unknown outcome.
- Fetch, the demonstration application in `client/`, is served on a second loopback address, so the administrator's session cookie never reaches it. Customers, merchants, riders, support agents and operations staff sign in through Fetch Accounts. Application calls go through the gateway to Inventory, Orders, Reports, Support and Accounts. Business ownership, stock, wallet and order transitions stay in those services.

### Code layout

Each package under `internal/` imports only the packages listed before it; a test enforces the order.

- `internal/fault`: the fault-injection points tests use to crash, delay or fail the gateway at exact places. They act only in binaries built with the `faultinject` tag; normal builds compile them to no-ops.
- `internal/policy`: the policy document: its types, strict JSON decoding, and the limits and identifier formats every policy must meet.
- `internal/compile`: turns a policy into an immutable generation (routes, permits, rate rules, body and header schemas) that requests are decided against, readable only through methods; it also owns the trusted clock for deadlines.
- `internal/forward`: sends an admitted request to the one upstream it is bound to, on a new connection each time; a missing or non-loopback binding is sent nowhere.
- `internal/store`: the one SQLite database (policy versions, draft changes, temporary rules, blocked accounts, administrators, change receipts); the only code that runs SQL, holding the database exclusively.
- `internal/audit`: the durable security log; an admitted request is acknowledged only after its record is synced to disk, and the writer never deletes a file.
- `internal/openapi`: the offline OpenAPI import and export; it turns a description into a candidate policy, checks it with the compiler, and writes one API of a policy back as a description.
- `internal/exchange`: the migration exchange file's ledger: its types, strict reading and the rules every ledger must meet.
- `internal/overlay`: decisions files: a restricted Overlay 1.2.0 profile that may only make operations stricter or accept a looser ledger item, applied in order to a copy of an exchange file.
- `internal/semantics`: the meaning of settings other gateways use (path matching and rewriting, identity, roles, rate algorithms, failure modes) and how far each is from the gateway's own, with the bounds of each rate algorithm.
- `internal/migrate`: the offline `gateway migrate` commands, which classify a configuration into an exchange file and turn an exchange file into a candidate policy.
- `internal/gateway`: the gateway itself, which uses the packages above.

## Build and test

You need Go 1.26 or newer. `go.mod` selects the Go 1.27.3 toolchain; with `GOTOOLCHAIN=auto`, Go uses or downloads that toolchain when needed. Programs live in `cmd/`, with shared code in `internal/gateway` and `internal/services`. The preserved controlled APIs for gateway regression tests live in `internal/services/testfixture`. The two web applications are in `dashboard/` and `client/`.

- `cmd/gateway` runs the gateway from one strict JSON configuration file. It has the commands `run`, `install`, `enrol`, `backup`, `restore` and `migrate`, each with `--config <file>`, and the offline `policy import-openapi` and `policy export-openapi` (see above). The local commands `install`, `enrol`, `backup`, `restore` and `migrate` run while the gateway is stopped.
- `cmd/services -service inventory|orders|reports|support -secret-file <service.secret>` runs a Fetch business API, with independent authenticated receipts, state and reset. Defaults honor `EAT_PORT_OFFSET`; `-listen` and `-control` select explicit loopback addresses. Orders uses `-inventory <http-origin>` and Reports uses `-orders <http-origin>` for authenticated internal calls.
- `cmd/accounts -secret-file <service.secret> -signing-settings <accounts.json>` runs Fetch Accounts. Signing settings contain `kid` and an absolute `private_key_file`. Only Accounts signs application tokens, which last 15 minutes. Account and signing secrets stay in the private demonstration state.
- `cmd/testfixture` preserves the three earlier controlled APIs for gateway regression tests. The Fetch demonstration does not build or run it.
- `cmd/demo` builds, sets up and runs the full local stack with `up`. `setup` writes `gateway.json`, complete baseline and deny-all policies, `service.secret`, a signing key pair and `accounts.json` into a new identified private directory, then installs the first administrator. `run` starts the five services and gateway, reports protected admission separately from listening, and stops its own children gracefully. `token` is an optional client of Accounts for a seeded subject; it cannot change the fixed 15-minute lifetime. `reset` restores every service's seed and clears its receipts. `events` prints request events from the active audit file; retained rotated files are not searched.
- `cmd/measure -samples 30 -account cust-001` collects preliminary paired wallet latency against an isolated running Fetch stack, directly and through the gateway. It honors `EAT_PORT_OFFSET`, keeps credentials out of output, checks equivalent successful responses, and emits raw observations and aggregate JSON.

```
go build -trimpath -o <out-dir>/gateway.exe ./cmd/gateway
go build -trimpath -o <out-dir>/services.exe ./cmd/services
go build -trimpath -o <out-dir>/accounts.exe ./cmd/accounts
go build -trimpath -o <out-dir>/testfixture.exe ./cmd/testfixture
go build -trimpath -o <out-dir>/demo.exe ./cmd/demo
go build -trimpath -o <out-dir>/measure.exe ./cmd/measure
```

`-trimpath` keeps local build directory names out of the programs. Reproducing an identical binary also requires the same source, toolchain, dependencies, build flags and environment.

Run the required Go checks from the repository root. In PowerShell, give backend verification its isolated port offset:

```powershell
$env:EAT_PORT_OFFSET='1000'
go mod verify
gofmt -l cmd internal
go vet ./...
go vet -tags faultinject ./...
go test ./... -count=1
```

`gofmt -l` must print nothing. The gateway regression tests start real gateway and controlled API processes and check independent request receipts. The Fetch integration test starts all five services and walks the role flows, refusals, policy changes, blocks, keys and resets. Tests use isolated synthetic identities and temporary state: the controlled-test issuer is in memory, while real Accounts tests create private signing files in temporary directories. No application signing key is shipped in source. Race and optional WSL checks have separate evidence; an unrun platform check is not implied by a passing Windows suite.

The dashboard is in `dashboard/` and the website in `client/` (React and TypeScript, built with Vite). Each has its own browser tests, which drive a real demonstration made by `cmd/demo` (see their READMEs). To run the demonstration by hand, put the dashboard build in `<web-dir>/admin` and the website build in `<web-dir>/demo`, then:

```
demo setup -dir <new-dir> -web <web-dir> -gateway <out-dir>/gateway.exe
demo run -dir <new-dir> -gateway <out-dir>/gateway.exe -services <out-dir>/services.exe -accounts <out-dir>/accounts.exe
```

The dashboard is at `http://127.0.0.1:9440/` and the website at `http://127.0.0.2:9480/`. Metrics are at `http://127.0.0.1:9450/metrics` on a separate loopback listener. `EAT_PORT_OFFSET` shifts the demonstration ports; for example, `1000` selects data port 10480, admin port 10440 and metrics port 10450.

### One command

With Go and Node.js installed, run from the repository root:

```
go run ./cmd/demo up
```

Each run builds the gateway, services, Accounts and both websites from the current sources. It prints all fourteen fictional demo accounts; their shared public password is `FetchDemo2026!`. The first run asks you to choose a separate password for the Dashboard administrator `root`, then establishes deny-all genesis and confirms the Fetch baseline through the Admin API. Later runs keep the administrator and policy; service business data starts from its deterministic seed on restart. Programs, builds and state are kept in `EAT40005-demo` in the user cache folder (`%LOCALAPPDATA%` on Windows); `-home <dir>` chooses another folder. `-fresh` replaces identified earlier state and creates a fresh version 4 deployment; it never migrates a version 3 policy. Press Ctrl+C to stop everything. `-stdin` also stops when standard input closes, after any password prompts.

If an earlier native setup stopped before any administrator, candidate or policy existed, `up` recognizes that unfinished state, asks for the initial Dashboard password and preserves the complete earlier directory under `state-before-fetch-<id>` in the same home before creating the Fetch setup at `state/`. This needs no alternate home or reset flag. An earlier deployment with saved authority, mismatched files, a corrupt database or another process holding it is refused before building. Current Fetch deployments continue to reuse their administrator and policy.

At the default offset, Inventory, Orders, Reports, Support and Accounts listen on `127.0.0.1:9501` through `9505`, with protected control listeners on `9601` through `9605`. Orders reserves Inventory stock and Reports reads Orders history through fixed `/internal/` endpoints with `X-Service-Key` from `service.secret`. Those endpoints have no gateway routes. Every service response carries `X-Service`; gateway refusals do not. The controls expose `/state`, `/receipts` and `/reset` with a separate bearer secret. Keep the state directory and all service/control listeners private to the local demonstration.

While the stack is running, these commands use its identified `state` directory:

```
go run ./cmd/demo reset -dir <home>/state
go run ./cmd/demo attack -dir <home>/state -scenario flood-search -from 127.0.0.50 -count 65
go run ./cmd/demo attack -dir <home>/state -scenario flood-login -from 127.0.0.51 -count 12
go run ./cmd/demo attack -dir <home>/state -scenario flood-orders -from 127.0.0.52 -count 12
go run ./cmd/demo keys rotate -dir <home>/state
go run ./cmd/demo keys retire -dir <home>/state -kid <old-key-id>
go run ./cmd/demo policy export -dir <home>/state -out <new-policy-file>
go run ./cmd/demo policy stage -dir <home>/state -file <policy-file>
go run ./cmd/demo events -dir <home>/state -last 20
```

The order flood signs in as Customer 2 (`cust-002`), keeping Customer 1's (`cust-001`) checkout quota separate. It consumes Customer 2's per-account quota, including any checkout requests from its lab tab. It creates real demonstration cash orders until limited; reset the isolated demo afterwards. Different source addresses isolate the public search and login quotas.

Key and policy commands prompt for the `root` Dashboard password and use its session, Origin and CSRF protections. Rotation writes a new pair locally, adds public trust and reloads it before switching Accounts; the old key remains trusted until retirement. The current Accounts key cannot be retired. A failed or unconfirmed reload leaves Accounts signing unchanged and reports the uncertainty. Retirement removes public trust but retains key files for the operator. Policy export writes only to a new file. Policy staging validates a complete candidate against the current confirmed revision and does not activate it. No command automatically repeats a write without an answer.

Flood commands send bounded serial traffic from the chosen IPv4 loopback address and print response counts. Search and login exercise per-address quotas; checkout exercises the signed-in customer's quota and creates real synthetic cash orders until it is limited. Reset afterwards for another demonstration. Money uses integer sen, wallets start at 10000 sen and checkout keys replay the first exact result without repeating the charge or reservation. Business state is in memory; policy, administrators, blocks and change receipts remain in the gateway's single SQLite controller.

The gateway rejects a Caddy JSON configuration file and never converts one by itself. Keep the gateway stopped when running local operator commands; a deployment lock prevents two processes from sharing a database. Successful operator commands exit 0; refusals and failures exit 1. `install` and `enrol` print a one-time secret intentionally: keep that output private. On Windows, file modes do not establish confidentiality. Existing deployment and backup directories need operator-managed DACLs. New demonstration directories receive a protected DACL for the current user, SYSTEM and administrators; this does not isolate them from a privileged host administrator.

## Out of scope

The project does not cover the full OWASP API Security Top 10, distributed systems, Kubernetes or new cryptographic methods.
