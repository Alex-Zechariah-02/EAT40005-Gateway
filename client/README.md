# Fetch

Fetch is the demonstration platform for the EAT40005 Gateway: a food and grocery delivery website in the style of GrabFood and Fetch Mart, set in Malaysia with prices in ringgit. Every shop, person and order is fictional. The gateway serves this React and TypeScript application at `/` on its data plane, on the same origin as the APIs it protects: Inventory, Orders, Reports, Support and Fetch Accounts.

The gateway decides every request. The browser sends only relative API paths, never contacts a service directly or the Admin API, and sends no cookies. Every screen shows real API answers (no browser-only data), and nothing on it is invented: no ratings, reviews, scarcity or countdowns.

## Who uses it

One website for every role; after signing in each person lands on their own home.

| Role | Home | What they do |
|---|---|---|
| Customer | `/` | Browse Fetch Food and Fetch Mart, search, add from one shop at a time, check out with cash on delivery or the Fetch wallet, follow the order, cancel it while it is new, ask for help with a reference |
| Merchant | `/merchant` | Accept new orders and mark them ready; change prices, availability and stock; see their own sales |
| Rider | `/rider` | Take a ready order from the job board, then mark it delivered |
| Support agent | `/support` | Answer and resolve tickets, with the linked order and the request reference |
| Operations | `/ops` | Platform reports and every order |
| Anyone | `/lab` | The security lab: buttons that send deliberately wrong requests and show what the gateway answered |

The demonstration accounts and their shared password are on the sign-in page. Fetch Accounts checks every sign-in.

## How it behaves

- **Sign-in:** Fetch Accounts issues a 15-minute token. It lives only in this tab's `sessionStorage`, so it survives a reload, ends when the tab closes, and each tab can be a different person. The app refreshes it silently before it expires. Changing person cancels pending requests and clears what was shown.
- **Storage:** `sessionStorage` holds the session, the basket and the delivery address for this tab; `localStorage` holds only the chosen appearance (`fetch.theme`). No cookies.
- **Errors:** every problem is said in plain words with who answered (the gateway or a named service) and the gateway's reference, **Ref**, which support and the Dashboard's request lookup use.
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
- `npm run preview` serves a build at `http://127.0.0.1:5175/` with the gateway's exact content policy header, to catch policy violations early.

## Browser tests

Browser tests in `e2e/` exercise the shop against the real gateway and services. They build the gateway programs from a clean export of the last commit.
