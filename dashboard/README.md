# Fetch Gateway Dashboard

The administrator dashboard for the EAT40005 Gateway, a security policy gateway for REST APIs. It is a React and TypeScript single-page application that the gateway's admin listener serves at `/`, on the same origin as the protected Admin API.

The dashboard holds no authority of its own. The gateway's Admin API authenticates administrators, checks their permissions, the session's CSRF token and the request's `Origin` and `Host` on every call, and decides every change. The dashboard only shows what the Admin API returns and sends what the administrator confirms. Hiding an action an administrator lacks the permission for is a convenience; the server still refuses it.

## What it does

The views are grouped by job.

**Monitor**

- **Overview:** the policy in force, any change on trial with its rollback countdown and Confirm and Roll back, the temporary rules in force, traffic in a chosen window, why requests were refused, the latest refusals with the check that decided each, figures per API, and a timeline of policy changes. A new or restored installation is told how to establish its first policy, and every page warns at the top while the gateway admits nothing.
- **Live traffic:** every request as the gateway decides it, streamed as it happens, with a pause for inspection and a filter for refusals.
- **Request lookup:** find a request by the reference (Ref) a person quotes from an error, by account or by operation, and see each check it passed, the one that decided it and those it never reached. Every page header has the same Ref search.
- **APIs:** each API's operations, who may call them, their rate limits and bodies, and its traffic figures.

**Policy**

- **Policy:** the Policy Designer, one draft in Canvas, Form and JSON views (below). Its "Revisions and tables" link opens the confirmed policy as tables and every revision, each of which can be inspected and used to start a change.
- **Changes:** the guided way a change goes live. Staging runs the gateway's own compiler over the whole document; the review lists the differences member by member; an impact preview replays recent requests to show which would now be decided differently; the change then goes on trial with a deadline and rolls back by itself unless confirmed. Past trials are listed with how each ended.
- **Temporary rules:** exceptions and containments with a fixed lifetime, checked with the gateway before they are added, plus incident quick actions such as freezing ordering for 10 minutes.
- **Accounts:** block an account instantly, unblock it, and see the accounts and roles in recent traffic. A block is not a policy change: no trial or rollback undoes it.

**Admin**

- **History:** every administrative transition, filtered by area, outcome and administrator, read page by page or all at once.
- **Policy as code:** the policy in force as one JSON document to copy or download, and how to stage a change from a file.
- **Team:** the gateway's administrators and their permissions, and the token signing keys it trusts, with a reload from the operator's settings. Adding an administrator stays an operator command.
- **My account:** session details and password change.

Every change is confirmed in a dialog that states its consequence. A change whose answer is lost is reported as an unknown outcome and never resent by itself; resending it explicitly reuses its idempotency key, so the gateway applies it at most once. A refused change shows the gateway's own error code with a plain explanation.

Sign-in uses a password, and first sign-in an operator-issued enrolment secret. The session is an HttpOnly, `Secure`, `SameSite=Strict`, host-only cookie that page scripts cannot read. Browser storage holds only the Policy Designer's box positions (`eat40005.policy-designer.layout.v1`) and, when chosen, the appearance (`gateway.theme`, light or dark; the default follows the computer).

## Policy Designer

Edit one draft in Canvas, Form or JSON. Boxes show roles, operations, APIs and upstreams; arrows show permits and routing. The checks in an operation stay in the gateway's enforcement order. The Form tab offers every editing option by keyboard.

The draft stays in the signed-in page's memory while you change pages and ends with the session. **Stage as a change** asks the gateway to check and store the whole document; the change then continues on the Changes page. A staging request with no answer remains an unknown outcome; only an explicit identical resend reuses its request key.

Confirmed and on-trial modes are read-only. Live markers show temporary permits, removals, rates, operation denial and changes on trial without rewriting the draft. Trace explains the selected document and sends no request; it does not apply temporary rules or decide actual access. **Reset layout** removes the stored box positions.

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

- `npm run dev` serves the dashboard at `http://localhost:5173/` (strict port). `/admin` calls pass through a development proxy to a running gateway's admin listener at `127.0.0.1:9440`, rewriting Host and Origin to that listener.
- `npm run dev:mock` does the same, but a development stand-in (`src/dev/stand-in.ts`) answers the traffic, event, impact, blocked-account, key and administrator endpoints when the gateway does not serve them yet. Every other `/admin` path still reaches the real gateway.

Neither the proxy nor the stand-in is part of the production build, and neither is test evidence: the browser tests use the built files served by the real gateway.

## Browser tests

The tests in `e2e/` build the gateway programs from a clean export of the last commit (`git archive HEAD`), so uncommitted work never reaches the programs under test, and drive a real gateway with its fixture APIs through the repository's demo tool. They check what the dashboard shows against the Admin API, the fixtures' own receipts and the gateway's security events, including the content policy, session cookie, CSRF, Origin and Host rules, unknown outcomes, storage and accessibility at phone, tablet and desktop widths. They run serially on fixed loopback ports (`127.0.0.1:9440`, `127.0.0.2:9480`, `127.0.0.1:9441`, `127.0.0.2:9481` and the fixtures' ports), so run one suite at a time.

```powershell
npm run build
npm run e2e
```

The demo tool also needs a website build; the helper builds `../client`, unless `WEBSITE_DIST` names a built client to use instead. Optional: `E2E_EVIDENCE` names a file for JSON-lines evidence (statuses, receipts and event fields, never secrets), and `E2E_SHOTS` a directory for screenshots. Pages taller than 16,000 pixels are captured as viewport images with a JSON manifest of their scroll positions, avoiding Chrome's tall-image repetition.
