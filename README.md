# Lightweight Security Policy Gateway for REST APIs

This Go gateway enforces a shared, configurable security policy before requests reach REST APIs. It runs its own HTTP server and reverse proxy using the Go standard library.

## Security controls

The gateway has five security controls:

- **JWT authentication** checks the access token of each caller.
- **Role-based access control** allows or blocks each call by the role of the caller.
- **Rate limiting** counts the requests of each checked caller for each operation over a sliding time window. A request above the limit gets the answer 429 and goes no further.
- **Request validation** checks the JSON body and selected headers of a request against a JSON Schema set for the operation. An unclear body, such as one with a repeated field, is rejected.
- **Security event logging** records each security decision in a local file. A request goes to an API only after its record is safely on disk. If the log cannot be written, the gateway stops letting requests through.

Each request uses one policy version from start to end. When one control rejects a request, the request does not go to the next control and never reaches an API. The gateway checks tokens from an outside identity provider. It does not issue tokens itself. It accepts only RS256 tokens signed by one trusted public key. The roles of each caller come from the gateway's own policy, never from the token.

## Safe policy changes

A policy can pass all checks and still cause problems when it is in use. Administrators can change the shared policy through two workflows:

- **Confirmed Policy Activation.** A new permanent policy starts as provisional and is enforced at once, with a confirmation deadline of 1 second to 24 hours. An administrator confirms it or rolls it back. If nobody confirms it in time, the gateway goes back to the previous policy by itself, also when it was stopped over the deadline. A provisional policy never admits a request after its deadline, even if the rollback runs late. Only one provisional policy can exist at a time, and each one ends exactly once.
- **Temporary Policy Override.** An administrator can give a role one more operation or take one away, raise or lower an operation's rate limit, or block an operation, for 1 second to 24 hours. The change is applied on top of the permanent policy without changing it, and it ends by itself at its expiry time or when it is revoked. It cannot skip token checks or request checks, reduce logging, open a blocked operation or change where requests go. Two overrides cannot change the same thing, and a new permanent policy that would clash with an override is refused. While a provisional policy waits for confirmation, no new override can be made. Rate counts carry over, so an override never gives a fresh quota. A restored backup never brings an old override back.

## How it is built

- The gateway has its own HTTP server and reverse proxy, built on the Go standard library. It serves HTTP/1.0 and HTTP/1.1 in plain text. Each request passes all five controls in one pipeline and then goes only to the API that the policy names, once.
- The project implements the policy model and the five controls itself.
- SQLite stores each policy version, including the callers and their roles, the administrators and a record of every change. No request waits for a database query. The gateway refuses to start serving from a database whose schema or stored policy it cannot verify.
- Only the project controller changes the policy in use. The data plane has no administration endpoint.
- A protected admin API listens on a local address only. Administrators sign in with a password, and each one holds only the permissions they are given. The API can read the policy and its history, change which roles each caller holds, store and check a proposed new policy, and then activate, confirm or roll it back. It also creates, lists and revokes temporary overrides.
- Local commands set up the first administrator, add or reset administrators, make and restore checked database backups, and upgrade a database made by an earlier version. A restored backup serves nothing until an administrator confirms a new policy that allows nothing, and a provisional policy in the backup never comes back.
- A web dashboard for administrators (in `dashboard/`) is served by the admin listener itself, so it uses the same sign-in, origin and CSRF checks as the admin API. It shows the confirmed policy, any provisional policy and its deadline, the temporary overrides in force and the change history. It stages and activates new policies, confirms or rolls them back, changes caller roles and creates or revokes overrides. Each change is shown as done only when the gateway's receipt arrives, and a request with no answer is shown as having an unknown outcome.
- A demonstration website for ordinary API callers (in `client/`) is served by the gateway on a second loopback address, so the administrator's session cookie never reaches it. It is a small shop with customer, staff and manager views, and every call it makes goes through the gateway to the three test APIs with a test token pasted by the operator. Its inspector page shows permitted and refused requests, the rate limit and the request checks.

## Build and test

You need Go 1.26 or newer. `go.mod` names Go 1.27.3 as the toolchain, because it fixes security issues in the standard HTTP, TLS and certificate packages the gateway uses; an older `go` command downloads and uses it automatically. The repository follows the standard Go layout: the three programs live in `cmd/`, and the code they share is in `internal/gateway` (the gateway) and `internal/fixture` (the test APIs), which no other module can import. The two web applications are in `dashboard/` and `client/`.

- `cmd/gateway` runs the gateway from one strict JSON configuration file. It has the commands `run`, `install`, `enrol`, `backup`, `restore` and `migrate`, each with `--config <file>`. The local commands `install`, `enrol`, `backup`, `restore` and `migrate` run while the gateway is stopped.
- `cmd/fixture` runs one of the three test APIs: Orders, Inventory or Reports.
- `cmd/demo` sets up and runs the local demonstration with `cmd/gateway`. `up` does all of it with one command (see below). `setup` writes `gateway.json`, policies and a signing key into a new identified directory and installs the first administrator. `run` starts the three test APIs and the gateway, reports protected admission separately from listening, and stops them gracefully. `token` issues a short-lived test token for a test caller. `reset` restores the identified test APIs' data, and `events` prints request events from the active audit file. Rotated files are retained but are not searched by this command.

```
go build -trimpath -o <out-dir>/gateway.exe ./cmd/gateway
go build -trimpath -o <out-dir>/fixture.exe ./cmd/fixture
go build -trimpath -o <out-dir>/demo.exe ./cmd/demo
go test ./... -count=1
```

`-trimpath` keeps local directory names out of the programs, so the same source gives the same files wherever it is built.

The tests start the real gateway and the three test APIs as separate processes on this computer. Each test API keeps its own record of every request it receives. The tests use these records to show that a rejected request never reaches an API. The tests sign their own tokens with keys made in memory for each run, so no signing key is stored anywhere. Administrator passwords in the tests are also made fresh for each run.

The dashboard is in `dashboard/` and the website in `client/` (React and TypeScript, built with Vite). Each has its own browser tests, which drive a real demonstration made by `cmd/demo` (see their READMEs). To run the demonstration by hand, put the dashboard build in `<web-dir>/admin` and the website build in `<web-dir>/demo`, then:

```
demo setup -dir <new-dir> -web <web-dir> -gateway <out-dir>/gateway.exe
demo run -dir <new-dir> -gateway <out-dir>/gateway.exe -fixture <out-dir>/fixture.exe
```

The dashboard is at `http://127.0.0.1:9440/` and the website at `http://127.0.0.2:9480/demo/`.

### One command

With Go and Node.js installed, run from the repository root:

```
go run ./cmd/demo up
```

Each run builds the gateway, the test APIs, the dashboard and the website from the current sources, starts them and prints an hour-long shop token for the Customer, Staff and Manager slots. The first run also asks you to choose a password for the dashboard administrator `root`. It then sets up the starting policy through the Admin API, as an administrator would in the dashboard. Later runs keep that administrator, the policy and the data. Programs, builds and state are kept in `EAT40005-demo` in the user cache folder (`%LOCALAPPDATA%` on Windows); `-home <dir>` chooses another folder. `-fresh` deletes the earlier state and starts over. Press Ctrl+C to stop everything.

The gateway rejects a Caddy JSON configuration file and never converts one by itself. Keep the gateway stopped when running local operator commands; a deployment lock prevents two processes from sharing a database. Successful operator commands exit 0; refusals and failures exit 1. `install` and `enrol` print a one-time secret intentionally: keep that output private. On Windows, file modes do not establish confidentiality. Existing deployment and backup directories need operator-managed DACLs. New demonstration directories receive a protected DACL for the current user, SYSTEM and administrators; this does not isolate them from a privileged host administrator.

## Out of scope

The project does not cover the full OWASP API Security Top 10, distributed systems, Kubernetes or new cryptographic methods.
