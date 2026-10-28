# Fetch Gateway Dashboard

The administrator dashboard for the EAT40005 Gateway, a security policy gateway for REST APIs. It is a React and TypeScript single-page application that the gateway's admin listener serves at `/`, on the same origin as the protected Admin API.

The dashboard holds no authority of its own. The gateway's Admin API authenticates administrators, checks their permissions, the session's CSRF token and the request's `Origin` and `Host` on every call, and decides every change. The dashboard only shows what the Admin API returns and sends what the administrator confirms. Hiding an action an administrator lacks the permission for is a convenience; the server still refuses it.

## What it does

The views are grouped by job.

**Monitor**

- **Overview:** the policy in force, any change on trial with its countdown and **Undo change** and **Review and keep**, the temporary rules in force, traffic in a chosen window, why requests were refused, the latest refusals with the check that decided each, figures per API, and a timeline of policy changes. A new or restored installation is told how to establish its first policy, and every page warns at the top while the gateway is not accepting requests. Once a policy is in force, a Getting started guide ticks off the first steps (a policy in force, a first trial, a trial kept or undone) from what the gateway reports; **Hide this guide** removes it until My account shows it again.
- **Live traffic:** every request as the gateway decides it, streamed as it happens, with a pause for inspection and filters that show only changes (anything but reads) or only refusals. It keeps the latest 1,000 requests and draws the newest 200 that match.
- **Request lookup:** find a request by the reference (Ref) a person quotes from an error, by account or by endpoint, and see each check it passed, the one that decided it and those it never reached. The **Search** box in every page header finds a full Ref, an account or endpoint ID, a draft change (“change 12”), a temporary rule (“rule 3”) or a version (“version 4”) and opens its page; an account searched for never enters the address.
- **APIs:** each API's endpoints, who may call them, their rate limits and bodies, its traffic figures, and each endpoint's request rules in words with the exact schema under More details.

**Policy**

- **Policy:** the Policy Designer, one draft in Canvas, Form and JSON views (below). Its "Versions and tables" link opens the policy in force as tables and every version, each of which can be inspected and used to start a draft change.
- **Roles:** what each role can do under the policy in force, in plain sentences, with any temporary rule that changes it listed beside it and the accounts seen with it in recent requests. The page only reads.
- **Changes:** the guided way a change goes live. Saving a draft change runs the gateway's own compiler over the whole document; the review lists the differences member by member; an impact preview replays recent requests to show which would now be decided differently; the change then goes on trial with a deadline and is undone by itself unless someone keeps it. Before a trial starts, its dialog says in plain sentences what the change adds, changes and removes; a draft that denies every request or removes a whole API is confirmed by typing “deny all” or the API's ID, and establishing a first policy by typing “deny all”. Past trials are listed with how each ended.
- **Temporary rules:** allow or restrict something temporarily for a fixed lifetime, checked with the gateway before the rule is added, plus incident quick actions such as freezing ordering for 10 minutes.
- **Accounts:** block an account instantly, unblock it, and see the accounts and roles in recent traffic. A block is not a policy change: undoing a change never lifts it.

**Admin**

- **History:** every administrative transition, filtered by area, outcome and administrator, read page by page or all at once.
- **Policy as code:** the policy in force as one JSON document to copy or download, and the Admin API requests a script uses to read it and to save a draft change from a file.
- **Team:** the gateway's administrators and their permissions, and the token signing keys it trusts, with a reload from the operator's settings. Adding an administrator stays an operator command.
- **My account:** session details, your permissions in plain words, the gateway's address, and password change.

The Changes, Temporary rules, Accounts, Roles and APIs pages open with the same list toolbar: a **Filter** box that narrows the rows already read (it sends nothing to the gateway), how many rows it leaves ("2 of 5 roles"; a paged list says "loaded"), and the list's main action at the right. **Add a temporary rule** there opens a step-by-step version of the rule form that ends on a review and then asks the usual confirmation.

The sidebar names the environment ("Local gateway" when the Dashboard is served from this computer); its exact address is one click away.

A notification bar at the top of every page names what changed between the gateway's status reads: a trial starting or ending, temporary rules added or ended, the signing keys reloaded, and a security log that cannot be written. It makes no requests of its own. A notice stays until it is dismissed or another page opens; a problem stays on every page until it is dismissed.

Every change is confirmed in a dialog that states its consequence. A change whose answer is lost is reported as an unknown outcome and never resent by itself; resending it explicitly reuses its idempotency key, so the gateway applies it at most once. Every refusal, warning and unknown outcome reads the same way: a plain title, what to do next, and a collapsed **More details** with the exact error code, HTTP status, field and IDs, each with a copy button. "Refused", "Applied, but …" and "No answer from the gateway" stay distinct.

Sign-in uses a password, and first sign-in an operator-issued enrolment secret. The session is an HttpOnly, `Secure`, `SameSite=Strict`, host-only cookie that page scripts cannot read. Browser storage holds only the Policy Designer's box positions (`eat40005.policy-designer.layout.v1`) and, when chosen, the appearance (`gateway.theme`, light or dark; the default follows the computer).

## Policy Designer

Edit one draft in Canvas, Form or JSON. Boxes show roles, endpoints, APIs and service addresses; arrows show permissions and routing. The checks on an endpoint stay in the gateway's enforcement order, with the same names as Live traffic. The Form tab offers every editing option by keyboard.

**Templates** prepares a common change in the draft: limit sign-in attempts, read-only access for a role to an API, removing a role's access to an API, or a tighter rate limit on an endpoint. A template only edits the unsaved draft and says what it changed; the draft still goes through the gateway's check, a trial and keeping like any other change.

**Step by step** walks through three common edits in a short dialog: give a role access, change a rate limit, or add an endpoint. Each ends on a review listing what the draft gains, changes and loses, and **Apply to the draft** only edits the unsaved draft.

The draft stays in the signed-in page's memory while you change pages and ends with the session. **Save draft change** asks the gateway to check and store the whole document; the change then continues on the Changes page. A save with no answer remains an unknown outcome; only an explicit identical resend reuses its request key.

The in-force and on-trial modes are read-only. Live markers show temporary permissions, removals, rates, endpoint denial and changes on trial without rewriting the draft. Trace explains the selected document and sends no request; it does not apply temporary rules or decide actual access. **Reset layout** removes the stored box positions.

## Requirements

- Node.js 24 and npm 11.
- For the browser tests: the Go toolchain, Git and an installed Google Chrome.

## Build

```sh
npm ci
npm run check   # type checks the application and the tests
npm run build   # writes dist/
```

Point the gateway configuration's `gateway.admin_ui` at the absolute path of `dist/`. The gateway serves `index.html` and the flat `assets/` directory, and nothing else.

The gateway serves the dashboard under a strict content policy (`default-src 'none'`; scripts, styles, images and connections from its own origin only). The build therefore puts no script or style into the page and calls only relative Admin API paths. The policy has no `font-src`, so Hanken Grotesk and Geist Mono are bundled into the script and registered with the CSS Font Loading API before the page renders; no font file is fetched. Their SIL Open Font License 1.1 notices are in `src/fonts/`.

## Development

`npm run dev` serves the dashboard at `http://localhost:5173/` (strict port). `/admin` calls pass through a development proxy to a running gateway's admin listener at `127.0.0.1:9440`, rewriting Host and Origin to that listener. The proxy is not part of the production build and is not test evidence: the browser tests use the built files served by the real gateway.

## Browser tests

The tests in `e2e/` run the gateway, Fetch services and Fetch Accounts through the repository's demo tool with the Fetch baseline policy. They check what the dashboard shows against the Admin API, the services' own receipts and the gateway's security events, including the content policy, session cookie, CSRF, Origin and Host rules, unknown outcomes, storage and accessibility at phone, tablet and desktop widths.

By default, the backend is built from an isolated export of `HEAD`; uncommitted Go changes are excluded. Set `E2E_BACKEND_SOURCE=worktree` to snapshot the current `cmd/`, `internal/`, `go.mod` and `go.sum`, including non-ignored new files. The shared helpers in the root `e2e/` directory record the selected commit, source mode, input-file hashes and diff hash, and refuse a worktree snapshot that changes during copying.

`EAT_PORT_OFFSET` shifts every backend address, including the second gateway used for short-session tests and the development proxy target. `E2E_DEV_PORT` selects the development server's port (default 5173). Run one browser suite at a time; its demonstrations own the selected backend ports. For example, to verify local backend changes on isolated ports:

```powershell
$env:EAT_PORT_OFFSET='1000'
$env:E2E_BACKEND_SOURCE='worktree'
$env:E2E_DEV_PORT='15173'
npm run build
npm run e2e
```

The tests serve `dist/`, or the existing build named by `E2E_APP_DIST`. The demo tool also needs a website build; the helper builds `../client`, unless `WEBSITE_DIST` names a built client to use instead. Optional: `E2E_EVIDENCE` names a file for JSON-lines evidence (backend source manifests, statuses, receipts and event fields, never secrets), and `E2E_SHOTS` a directory for screenshots. Pages taller than 16,000 pixels are captured as viewport images with a JSON manifest of their scroll positions, avoiding Chrome's tall-image repetition.
