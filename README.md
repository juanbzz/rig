# rig

A disposable app stack for integration tests, from one config file. rig reads
`.rig.toml` at the repository root, starts the databases in containers, runs
the migrations, starts the project's own apps on free ports, and hands the
tests one URL. A sibling to [bbq](https://github.com/j0lvera/bbq): bbq makes a
workspace per ticket, and rig gives each workspace a stack that doesn't collide
with the others.

```
$ rig run -- playwright test
→ service db  postgres:16-alpine
✓ service db  ready in 4.6s
→ step migrate
2026/10/01 15:16:54 goose: successfully migrated database to version: 1
✓ step migrate  done in 900ms
→ build api
✓ build api  done in 2.1s
→ app api  http://localhost:52156
✓ app api  healthy in 300ms
→ app web  http://localhost:52157
✓ app web  healthy in 600ms
✓ ready in 8.3s  http://localhost:52157

Running 12 tests using 1 worker
  12 passed (9.1s)
→ stopping
✓ stopped in 1.7s
```

rig exits with the test command's status, so CI needs only that one line.

## Install

Requires Docker (Docker Desktop, colima, or OrbStack). Go and go-task are
needed to build, not to run.

```fish
task build               # builds into ~/.bin
fish_add_path -m ~/.bin  # once, if ~/.bin is not on PATH yet
rig --version
```

`task build` stamps the binary with `git describe`. Override the destination
with `RIG_INSTALL_DIR`.

## Usage

```fish
rig run -- playwright test   # up → run → down; exits with the command's status
rig up                       # start, print the URL, block until Ctrl-C
rig up --logs                # …and stream app output, prefixed by app name
rig down                     # stop a `rig up` running in another shell
```

`rig up` prints only the URL to stdout. Progress and the summary go to
stderr, so `rig up > url.txt &` leaves the URL in a file and the terminal
readable. Once it is ready, `.rig/state.json` describes the whole stack.

### What tests get

`rig run` passes these to its command:

| Variable | Value |
| --- | --- |
| `RIG_URL` | the exposed app, e.g. `http://localhost:52157` |
| `RIG_<APP>_URL` | each app, e.g. `RIG_API_URL` |
| `RIG_<SERVICE>_URL` | each service, e.g. `RIG_DB_URL=postgres://rig:rig@localhost:32999/rig?sslmode=disable` |
| `RIG_STATE` | path to `.rig/state.json` |

Names are uppercased with `-` turned into `_`, so `web-admin` becomes
`RIG_WEB_ADMIN_URL`. Anything driving a stack started with `rig up` reads the
same values from `.rig/state.json`: `url`, `apps.<name>.url`,
`services.<name>.url`, and an `env` map holding the variables above.

### Playwright

`rig run` replaces `global-setup.ts`, `global-teardown.ts`, and `webServer`:

```ts
// playwright.config.ts
export default defineConfig({
  use: { baseURL: process.env.RIG_URL },
  // no globalSetup, globalTeardown, or webServer: rig owns the stack
});
```

```json
"test:e2e": "rig run -- playwright test"
```

A test that needs the database directly reads `process.env.RIG_DB_URL`.

## Configuration

[`.rig.toml.example`](.rig.toml.example) is startapp's harness with every
option annotated. The shape:

```toml
expose = "web"                # the app whose URL is the result

[services.db]                 # a container: preset or image + port
preset = "postgres"

[[steps]]                     # one-shot commands, in order, before any app
run = "goose -dir migrations up"
env = { GOOSE_DRIVER = "postgres", GOOSE_DBSTRING = "{{ services.db.url }}" }

[apps.api]                    # the project's code, as a host process
run    = "go run ./cmd/api"
health = "/health"
env    = { DATABASE_URL = "{{ services.db.url }}", PORT = "{{ port }}", ORIGINS = "{{ apps.web.url }}" }

[apps.web]
run        = "bun run dev --port {{ port }} --strictPort"
env        = { VITE_API_URL = "{{ apps.api.url }}" }
depends_on = ["api"]
```

Presets are `postgres` (16-alpine) and `mysql` (8.4). Both use the user
`rig`, the password `rig`, and the database `rig`, exposed as
`{{ services.db.user }}`, `.password`, and `.database` for frameworks that
want the parts rather than a URL. Any other image works as a generic service
with `image`, `port`, an optional `ready` check, and a `url` template.

rig checks the whole file before it starts anything. An unknown key, a
placeholder naming something that doesn't exist, or a `depends_on` cycle are
all reported at once, with exit status 2.

## How it behaves

**Order.** Every app gets a free port first, so templates can refer to any
app in any order: the API's CORS origin can be the web app's URL while the
web app proxies to the API. Services then start in parallel. Steps run one at
a time, then app builds, then apps. An app waits only for the apps in its
`depends_on`. `rig up` and `rig run` return once every app is healthy.

**No fixed ports.** Apps listen on ports the kernel hands out, and services
on random host ports. Stacks in different bbq workspaces, and the normal dev
servers, run side by side. Only one stack runs per project root; a second
`rig up` there refuses and names the pid that owns it.

**Teardown.** Each app runs in its own process group, and stopping sends
SIGTERM to the whole group, then SIGKILL 10 seconds later. That reaches the
server `go run` or `bun run` started, not just the wrapper. Containers stop
after the apps. testcontainers' Ryuk reaper also removes the containers if rig
is killed outright, even with SIGKILL. A second Ctrl-C kills rig right away.
Its containers are still reaped, but app processes can be left behind; the
next `rig up` warns about any it finds.

**Failures.** If an app exits before it is healthy, rig fails right away
instead of waiting out the timeout, and quotes the end of its log. If an app
dies while `rig up` is running, the stack comes down and rig exits 1. Under
`rig run`, rig reports the crash and lets the test command fail with its own
report.

**Environment.** Steps and apps inherit rig's environment, and their `env`
is added on top. A connection string your shell exports, such as a
`DATABASE_URL` from direnv, reaches the apps unless the config sets it. Set
every connection variable explicitly so tests never touch a live database.

**Files.** rig writes only under `.rig/`: `state.json` while a stack is up,
and `logs/<app>.log`, which are kept after teardown for reading after a
failure. `.rig/` gitignores itself.

**Docker.** testcontainers doesn't read `docker context`, so when
`DOCKER_HOST` is unset, rig resolves the selected context itself. It also
points the reaper at the VM's own socket, which colima and OrbStack need.
Settings you export yourself always win. Whatever rig sets this way
(`DOCKER_HOST`, `TESTCONTAINERS_DOCKER_SOCKET_OVERRIDE`) is also passed to
steps and apps.

## Exit status

| Status | Meaning |
| --- | --- |
| 0 | success, or `rig up` stopped by Ctrl-C / `rig down` |
| 1 | the stack failed to start, or an app crashed |
| 2 | no `.rig.toml`, or it is invalid |
| 130 | interrupted while starting |
| *n* | `rig run`: the command's own status |

## Environment variables

| Variable | Default | Purpose |
| --- | --- | --- |
| `RIG_LOG_LEVEL` | `info` | debug, info, warn, error |
| `RIG_VERBOSE` | `false` | same as `--verbose`: debug logging, including testcontainers' |
| `RIG_INSTALL_DIR` | `~/.bin` | where `task build` installs |

## Not yet

v1 covers what startapp's harness needs. These come when a project needs them
(BYTE-1192):

- apps built from a Dockerfile and run as containers (pgm)
- `[[files]]` rendered from templates, such as Laravel's `.env.rig` (shop)
- apps with no port, such as queue workers (`port = "none"`) (shop)
- more presets: redis, minio, mailpit, meilisearch
- `rig init`, profiles, compose import, `rig status`, `rig env`, an agent skill

## Development

```fish
task test     # go test ./...; the preset tests start real containers
task check    # fmt, vet, test
go test -short ./...  # skip anything that needs Docker
```
