---
title: Scion web frontend instructions
summary: Lit and Shoelace contracts, experiment flags, backend integration and project browser-validation setup.
---

# Scion Web Frontend - Agent Instructions

Read [root project instructions](../AGENTS.md). Use shared [workflow](/../memory/rules/workflow.md) for execution and proportional checks, [visual review](/../memory/rules/visual-review.md) for rendered validation, and [browser operations](/../memory/rules/tool-discovery.md#browser-and-chatgpt-operations) for browser selection, authentication and recovery. This file owns Scion frontend contracts and setup.

## Design Documents

Before making changes, review the relevant design documentation:

- **[Web Frontend Design](../.design/hosted/web-frontend-design.md)** - Architecture, technology stack, component patterns

## Experimental features

New experimental user-facing features must be launched behind a registered experiment (`pkg/experiments/registry.go`); see `docs-site/src/content/docs/reference/experiments.md`. Browser-side flag values are user-editable and only affect presentation; if an experiment changes hub behaviour, register it with the server layer and check it in the hub (`requireExperiment` / `experimentEnabled`). Do not add ad-hoc `scion:feature:` string literals or new booleans to `/api/v1/settings/public`.

## Architecture Overview

The web frontend is a **client-side SPA** built with Lit web components. There is no Node.js server at runtime. The Go `scion` binary serves the compiled client assets and handles all server-side concerns (OAuth, sessions, SSE real-time events, API routing) via `pkg/hub/web.go` and `pkg/hub/events.go`.

Node.js and npm are used **only at build time** to compile and bundle client assets via Vite.

## Development Workflow

### Building and Running

```bash
cd web
npm install    # First time only, or after package.json changes

# Build client assets
npm run build

# Run the Go server with dev auth (from repository root)
scion server start --enable-hub --enable-web --dev-auth \
  --web-assets-dir ./web/dist/client
```

Dev auth bypasses OAuth and auto-creates a session with admin privileges. The `--web-assets-dir` flag loads assets from disk so you can rebuild and refresh without restarting the server.

### Using Vite Dev Server

For client-side development with hot module reload:

```bash
npm run dev
```

Note: The Vite dev server only serves client assets. API calls and SSE require the Go server to be running.

### Common Commands

| Command | Purpose |
|---------|---------|
| `npm run dev` | Start Vite dev server with hot reload |
| `npm run build` | Build client assets for production |
| `npm run build:dev` | Build client assets in development mode |
| `npm run lint` | Check for linting errors |
| `npm run lint:fix` | Auto-fix linting errors |
| `npm run format` | Format code with Prettier |
| `npm run typecheck` | Run TypeScript type checking |
| `npm run test` | Run Vitest test suite |

### Verifying Changes

Use the shared workflow's proportional verification boundary with the project-native typecheck, lint, test and build commands listed above. Real-time/API validation additionally needs the Go backend; a standalone Vite page does not establish those behaviors.

## Project Structure

```
web/
├── src/
│   ├── client/              # Browser-side code
│   │   ├── main.ts          # Client entry point, routing setup
│   │   ├── state.ts         # State manager with SSE subscriptions
│   │   └── sse-client.ts    # SSE client for real-time updates
│   ├── components/          # Lit web components
│   │   ├── index.ts         # Component exports
│   │   ├── app-shell.ts     # Main application shell (sidebar, header, content)
│   │   ├── shared/          # Reusable UI components
│   │   │   ├── index.ts         # Shared component exports
│   │   │   ├── nav.ts           # Sidebar navigation
│   │   │   ├── header.ts       # Top header bar with user menu
│   │   │   ├── breadcrumb.ts   # Breadcrumb navigation
│   │   │   ├── debug-panel.ts  # Debug panel component
│   │   │   └── status-badge.ts # Status indicator badges
│   │   └── pages/           # Page components
│   │       ├── home.ts          # Dashboard page
│   │       ├── login.ts         # OAuth login page
│   │       ├── agents.ts       # Agents list page
│   │       ├── agent-detail.ts # Agent details page
│   │       ├── projects.ts       # Projects list page
│   │       ├── project-detail.ts # Project details page
│   │       ├── terminal.ts     # Terminal/session page (xterm.js)
│   │       ├── unauthorized.ts # 401/403 page
│   │       └── not-found.ts    # 404 page
│   ├── styles/              # CSS theme and utilities
│   │   ├── theme.css        # CSS custom properties, light/dark mode
│   │   └── utilities.css    # Utility classes
│   └── shared/              # Shared types between components
│       └── types.ts         # Type definitions (User, Project, Agent, etc.)
├── public/                  # Static assets
│   └── assets/              # Built client assets (CSS, JS)
├── dist/                    # Build output (gitignored)
├── vite.config.ts           # Vite build configuration
├── tsconfig.json            # TypeScript configuration
└── package.json
```

## Technology Stack

- **Components:** Lit 3.x with TypeScript decorators
- **UI Library:** Shoelace 2.x
- **Build:** Vite for client-side bundling
- **Routing:** Client-side via History API (click interception in `main.ts`)
- **Terminal:** xterm.js for terminal sessions
- **Server:** Go (`scion` binary with `--enable-web`)

### Shoelace component registration

Shoelace components, themes and icons ship in the client bundle; the SPA shell loads nothing from a CDN. Every `sl-*` component the app renders must be registered by an import in `src/client/main.ts` (`import '@shoelace-style/shoelace/dist/components/<name>/<name>.js';`). An unregistered tag renders as an unknown, unstyled element, in dev and production alike. `src/client/shoelace-registration.test.ts` fails when a tag used in `src/` has no such import.

## Icon Reference

All icons use the Shoelace `<sl-icon>` component, which provides [Bootstrap Icons](https://icons.getbootstrap.com/). Use these consistently when building new UI features.

**Important:** Only icons listed in the `USED_ICONS` array in `scripts/copy-shoelace-icons.mjs` are included in production builds. When you add a new `<sl-icon name="...">` reference, you **must** also add the icon name to that array, then run `npm run copy:shoelace-icons`. Icons will render in dev mode but appear blank in production if this step is missed.

### Resource Type Icons

| Resource Type | Icon Name | Usage |
|---------------|-----------|-------|
| **Agents** | `cpu` | Agent lists, detail pages, breadcrumbs, group members |
| **Projects** | `folder` | Navigation, dashboard, breadcrumbs |
| **Brokers** | `hdd-rack` | Navigation, broker lists, broker detail |
| **Users** | `people` | Navigation, user lists, user groups |
| **Groups** | `diagram-3` | Navigation, group lists, group detail |
| **Settings** | `gear` | Navigation, project settings |
| **Dashboard** | `house` | Navigation |

### Project Variant Icons

| Variant | Icon Name | Usage |
|---------|-----------|-------|
| **Git-backed project** | `diagram-3` | Project lists, project detail header |
| **Hub-managed workspace** | `folder-fill` | Project lists, project detail header |
| **Empty state** | `folder2-open` | No-projects placeholder |

### Profile & Config Icons

| Resource Type | Icon Name | Usage |
|---------------|-----------|-------|
| **Environment Variables** | `terminal` | Profile nav, env var pages, dashboard |
| **Secrets** | `shield-lock` | Profile nav, secrets pages |

### Individual vs. Collection Icons

| Context | Icon Name | Usage |
|---------|-----------|-------|
| **Single user** | `person` | Group member lists |
| **User avatar** | `person-circle` | Header, profile nav |
| **User collection** | `people` | Navigation, admin pages |

### Common Action Icons

| Action | Icon Name | Usage |
|--------|-----------|-------|
| **Create/Add** | `plus-lg` | Create agent, add items |
| **Create project** | `folder-plus` | Create project action |
| **Back/Return** | `arrow-left-circle` | Return links |
| **Recent activity** | `clock-history` | Dashboard activity section |

## Page Title Registration

The `PAGE_TITLES` map in `web/src/components/app-shell.ts` controls the h1 header text displayed on each page. It maps route paths to display titles (e.g. `'/health': 'Health'`).

**When adding a new route, you MUST add an entry to PAGE_TITLES (or handle it in the pattern matches of getPageTitle()).** If a route is missing from both, the page header will display "Page Not Found" even though the page itself renders correctly. This is a common pitfall — the page works but the header is wrong, which confuses users.

## Key Patterns

### Creating Lit Components

Components use standard Lit patterns with TypeScript decorators:

```typescript
import { LitElement, html, css } from 'lit';
import { customElement, property } from 'lit/decorators.js';

@customElement('my-component')
export class MyComponent extends LitElement {
  @property({ type: String })
  myProp = 'default';

  static override styles = css`
    :host { display: block; }
  `;

  override render() {
    return html`<div>${this.myProp}</div>`;
  }
}
```

### Using Shoelace Components

```typescript
render() {
  return html`
    <sl-button variant="primary" @click=${() => this.handleClick()}>
      <sl-icon slot="prefix" name="plus-lg"></sl-icon>
      Create Agent
    </sl-button>

    <sl-badge variant="success">Running</sl-badge>
  `;
}
```

### Theme Variables

Use CSS custom properties with the `--scion-` prefix for consistent theming:

```css
:host {
  background: var(--scion-surface);
  color: var(--scion-text);
  border: 1px solid var(--scion-border);
  border-radius: var(--scion-radius);
}
```

### Dark Mode

Dark mode is handled automatically via CSS custom properties. The theme toggle in the navigation saves the preference to localStorage. Components should use the semantic color variables (e.g., `--scion-surface`, `--scion-text`) which automatically adjust for dark mode.

## Testing Real-Time (SSE) Events

Test scripts for validating real-time event delivery are in `web/test-scripts/`. These were used during the initial validation of the SSE pipeline and remain useful for regression testing.

| Script | Purpose |
|--------|---------|
| `sse-curl-test.sh` | Server-side SSE validation with curl (no browser) |
| `realtime-lifecycle-test.js` | Full browser test with Playwright screenshots |
| `screenshot-debug.js` | Debug tool for blank/broken pages |

### Quick SSE smoke test (no browser needed)

```bash
TOKEN=<dev-token> PROJECT_ID=<uuid> ./web/test-scripts/sse-curl-test.sh
```

### Full browser lifecycle test

```bash
PROJECT_ID=<uuid> TOKEN=<dev-token> node web/test-scripts/realtime-lifecycle-test.js
```

## Containerized / Sandboxed Environments

When working in a containerized or sandboxed agent environment (e.g., scion agents), keep these points in mind:

- **Vite dev server is available.** You can run `npm run dev` to start the Vite dev server for client-side development and visual inspection. API calls and SSE will not work without the Go backend.
- **Use `--dev-auth` for local testing.** When a Go server is available, `--dev-auth` bypasses OAuth and auto-creates a dev session, which is the simplest way to test the frontend end-to-end. See the README for details.
- **Go server** the golang server can be started as a background process, but OAuth flows cannot be used in a container.

### Tool ownership

Use the existing package scripts listed above rather than resolving an unrelated same-name package through an ad-hoc runner. Shared [TypeScript runtime guidance](/../memory/.agents/skills/languages/typescript/SKILL.md) owns executable/tool resolution; changing this project's checked-in toolchain requires scoped work.

## Tips for End-to-End Web Validation

These tips were collected during validation work and are useful for agents debugging or testing the web frontend against the Go backend.

### Server startup

- **Combined mode** runs the Hub API on the web port (default 8080). When `--enable-hub` and `--enable-web` are both set, there is no separate listener on port 9810. All API routes are at `http://localhost:8080/api/v1/`.
- **Runtime broker** must be enabled (`--enable-runtime-broker`) and linked to the project as a provider before agents can be created. With the co-located broker, use `POST /api/v1/projects/{id}/providers` with `{"brokerId":"<id>"}` to link.
- **Dev token** is printed in the server startup logs. Use it as `Authorization: Bearer scion_dev_...` for API calls.
- **`--web-assets-dir`** loads assets from disk so you can rebuild the frontend (`npm run build`) and refresh the browser without restarting the Go server.

### API gotchas

- **Agent status updates** use `POST /api/v1/agents/{id}/status`, not `PATCH`. The handler only accepts POST.
- **Agent creation response** wraps the agent under an `"agent"` key: `{ "agent": { "id": "...", ... }, "warnings": [...] }`.
- **SSE endpoint** (`/events?sub=...`) requires a session cookie, not a Bearer token. To get a session cookie via curl: `curl -c - -H "Authorization: Bearer $TOKEN" http://localhost:8080/api/v1/projects` then pass the `scion_sess` cookie with `-b`.

### SSE event format

The SSE stream uses a single event type `update` with the subject embedded in the data payload:

```
event: update
data: {"subject":"project.xxx.agent.created","data":{"agentId":"...","projectId":"..."}}
```

The client `SSEClient` listens for `event: update` and the `StateManager` parses the `subject` field to route events. If the event type is changed or the subject is used as the SSE event type directly, the client will silently drop events.

### Browser testing

Follow the shared browser operations' long-lived-connection readiness guidance for Scion's SSE pages. Inspect the `[SSE] Connected` console event and the actual resulting UI state when validating API-driven updates. Use the project test scripts above for the supported browser fixture setup.

### Common failure modes

- **Blank page**: Check that web assets are built (`npm run build`) and the `--web-assets-dir` flag points to `web/dist/client`. Use `screenshot-debug.js` to see console errors and 404s.
- **SSE events not updating UI**: Check the SSE event type. The client only listens for `event: update`. If the server sends events with the subject as the type, they are silently dropped.
- **Agent delete not reflected**: The `onAgentsUpdated()` handler in `project-detail.ts` must run even when the state manager's agent map is empty (after the last agent is deleted).

## Browser-Based QA with Chrome DevTools MCP

Use shared browser operations and rendered visual review; the following setup and component details are Scion-specific.

### Starting the Go server

1. **Settings must exist first.** The server requires `image_registry` in `~/.scion/settings.json` with `schema_version`. Create this before starting:
   ```json
   {
     "schema_version": "1",
     "image_registry": "dummy.registry.io"
   }
   ```
   Without `schema_version`, the settings file is silently ignored and the server exits with `image_registry is not configured`.

2. **Start the Go server with the fixture's built binary and client assets**, using the current tool's supported process/session handling:
   ```bash
   /tmp/scion-test server start --enable-hub --enable-web --dev-auth \
     --web-assets-dir ./web/dist/client --foreground
   ```

3. **Capture the dev token** from the server startup output. It appears as:
   ```
   Dev token: scion_dev_<hex>
   ```

4. **Build assets before starting the server**: Run `npm install && npm run build` in `web/` first. The server serves from `web/dist/client`.

### Browser authentication

- **Navigate directly to `http://localhost:8080`** — dev-auth mode sets a session cookie automatically on the first page load, no manual cookie setup needed.
- **For API calls via curl**, use the dev token as `Authorization: Bearer scion_dev_...` to get a `scion_sess` cookie, then pass it to subsequent requests.

### Project QA captures and dialogs

Store Scion QA screenshots under ignored `.scratch/` and follow shared visual review for inspection and relevant viewport/theme states. Shoelace `<sl-dialog>` modals are DOM components; distinguish them from browser-native confirmation dialogs when choosing the available tool operation.

### Filling forms in Lit/Shoelace components

- **Use `fill` for `<sl-input>` fields** — it dispatches the correct input events.
- **Use `click` on `<sl-checkbox>` elements** — they toggle on click.
- **`<sl-select>` with a single option** may auto-select; verify via snapshot before filling.
- **After form submission**, take a snapshot to confirm the dialog closed and the list updated.

