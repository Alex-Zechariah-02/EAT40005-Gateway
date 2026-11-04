# Fetch

Fetch is the demonstration platform for the EAT40005 Gateway: a food and grocery delivery website in the style of GrabFood and Fetch Mart, set in Malaysia with prices in ringgit. Every shop, person and order is fictional. The gateway serves this React and TypeScript application at `/` on its data plane, on the same origin as the APIs it protects: Inventory, Orders, Reports, Support and Fetch Accounts.

The gateway decides every request. The browser sends only relative API paths, never contacts a service directly or the Admin API, and sends no cookies. Every screen shows real API answers (no browser-only data), and nothing on it is invented: no ratings, reviews, scarcity or countdowns.

## Who uses it

One website for every role; after signing in each person lands on their own home.

| Role | Home | What they do |
|---|---|---|
| Customer | `/` | Browse Fetch Food and Fetch Mart, sort shops by delivery time or fee and show only open ones, search and sort results by price, add from one shop at a time, check out with cash on delivery or the Fetch wallet, follow the order (the page says when its status changes), print a receipt, cancel it while it is new, ask for help with a reference |
| Merchant | `/merchant` | Accept new orders and mark them ready; change prices, availability and stock; see their own sales |
| Rider | `/rider` | Take a ready order from the job board, then mark it delivered |
| Support agent | `/support` | Answer and resolve tickets, with the linked order and the request reference |
| Operations | `/ops` | Platform reports and every order |
| Anyone | `/lab` | The security lab: buttons that send deliberately wrong requests and show what the gateway answered |

The demonstration accounts and their shared password are on the sign-in page. Fetch Accounts checks every sign-in.

## How it behaves

- **Sign-in:** Fetch Accounts issues a 15-minute token. It lives only in this tab's `sessionStorage`, so it survives a reload, ends when the tab closes, and each tab can be a different person. The app refreshes it silently before it expires. Changing person cancels pending requests and clears what was shown.
- **Storage:** `sessionStorage` holds the session, the basket and the delivery address for this tab; `localStorage` holds only the chosen appearance (`fetch.theme`). No cookies.
- **Errors:** every problem is said in plain words with who answered (the gateway or a named service) and the gateway's reference, **Ref**, shown in a short form; its copy button copies the full reference, which support pastes into the Dashboard's header search to open that request (the Security Log finds older ones).
- **Demo badge:** the header's **Demo** badge is the one place that says the site is a demonstration; it holds the note that every shop, person and order is fictional, and the link to the security lab.
- **Unknown outcomes:** when a change gets no clear answer, the app says it may or may not have been applied and never sends it again by itself. Checkout sends an `Idempotency-Key`, so an explicit "Send the same order again" is safe.
- **Roles only choose screens.** The gateway decides what each request may do, and the APIs check ownership.

## Build

Requires Node.js 24 and npm 11. The lockfile pins every tool.

```powershell
npm ci
npm run check
npm run build
npm audit
```

`dist/` is the website directory the gateway serves. It uses only its own files: no external fonts, images or scripts, and no inline script or style, under the gateway's content policy. Plus Jakarta Sans is bundled into the script and registered with the CSS Font Loading API; its licence is in `src/fonts/`. The photos are WebP files named by merchant and item ID; their sources and licences are in `src/photos/CREDITS.md`.

## Local development

- `npm run dev` serves the app at `http://localhost:5174/` and forwards the API paths to a running gateway at `http://127.0.0.2:9480` (start it with `go run ./cmd/demo up -home <folder>` from the repository root).
- `npm run dev:mock` runs the app on a development stand-in for the gateway and services (`src/dev/`), for building screens without the backend. It is never part of a production build and is never test evidence.
- `npx vite preview` serves an existing build at `http://127.0.0.1:5175/` with the gateway's exact content policy header, to catch policy violations early. There is no `preview` npm script; this invokes the locally installed Vite command.

## Browser tests

`npm run e2e` runs the browser tests in `e2e/` against the real gateway through the repository's demo tool. By default, the backend is built from an isolated export of `HEAD`; uncommitted Go changes are excluded. Set `E2E_BACKEND_SOURCE=worktree` to snapshot the current `cmd/`, `internal/`, `go.mod` and `go.sum`, including non-ignored new files. The shared helpers in the root `e2e/` directory record the selected commit, source mode, input-file hashes and diff hash, and refuse a worktree snapshot that changes during copying.

`EAT_PORT_OFFSET` shifts all gateway, service and control addresses and the development proxy target. `E2E_DEV_PORT` selects the development server's port (default 5174). Run one browser suite at a time; its demonstration owns the selected backend ports. For example, to verify local backend changes on isolated ports:

```powershell
$env:EAT_PORT_OFFSET='1000'
$env:E2E_BACKEND_SOURCE='worktree'
$env:E2E_DEV_PORT='15174'
npm run build
npm run e2e
```

- `site.spec.ts` checks the app as the gateway serves it before any policy exists: the page at `/` and every page route, each asset byte for byte with its fixed type, the content policy and security headers, each listener on its own loopback host, the bundled typeface, the gateway's refusal shown with the reference of the very answer, same-origin requests only, the tab's storage, the built files free of secrets and development hosts, and accessibility at phone, tablet and desktop widths.
- `flows.spec.ts` signs in as every Fetch role through Fetch Accounts, with the baseline policy confirmed, and checks each step against the services' own receipts: sorting, filtering and searching, ordering with cash and the wallet, an order moved through every status by the shop, a rider and the customer (whose open order page announces the change) and its printed receipt, menu, price and stock changes, refused cancels and out-of-stock orders with their references, support tickets, operations reports, a lost checkout answer that is never resent by itself, a blocked account, the security lab, a policy trial and its rollback, refused refreshes and other customers' orders shown with their references, the per-tab session, accessibility when signed in, a service that is down and a gateway that cannot record.
- `dev.spec.ts` checks that the development server sends API paths to the gateway; it is a development observation, not acceptance.

The tests serve `dist/`, or the existing build named by `E2E_APP_DIST`. They also build `../dashboard` for the admin listener, unless `DASHBOARD_DIST` names a built dashboard to use instead. Optional: `E2E_EVIDENCE` names a file for JSON-lines evidence (backend source manifests, statuses, references and receipts, never secrets), and `E2E_SHOTS` a directory for screenshots.
