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
- `internal/gateway`: the gateway itself, which uses the packages above.

## Build and test

You need Go 1.26 or newer. `go.mod` selects the Go 1.27.3 toolchain; with `GOTOOLCHAIN=auto`, Go uses or downloads that toolchain when needed. Programs live in `cmd/`, with shared code in `internal/gateway` and `internal/services`. The preserved controlled APIs for gateway regression tests live in `internal/services/testfixture`. The two web applications are in `dashboard/` and `client/`.

- `cmd/gateway` runs the gateway from one strict JSON configuration file. It has the commands `run`, `install`, `enrol`, `backup`, `restore` and `migrate`, each with `--config <file>`. The local commands `install`, `enrol`, `backup`, `restore` and `migrate` run while the gateway is stopped.
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
