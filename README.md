# Company Context

A small Go service for sharing company architecture, terminology, telemetry
locations, and investigation skills. Markdown in Git is the content source.
HTTP and MCP expose the same in-memory snapshot. Live inventory stays in your
observability systems.

## Run locally

Requires Go 1.25+; Git is required only for Git mode.

```sh
go build -o bin/contextd ./cmd/contextd
./bin/contextd -check
./bin/contextd
```

The service listens at `http://127.0.0.1:8080`. Content comes from `./content`
and refreshes every 15 minutes. Use `-refresh 5s` while editing locally.

```sh
curl http://127.0.0.1:8080/api/catalog
curl 'http://127.0.0.1:8080/api/search?q=architecture'
curl 'http://127.0.0.1:8080/api/catalog?kind=skill'
curl 'http://127.0.0.1:8080/api/content?path=context/overview.md'
curl http://127.0.0.1:8080/content/skills/investigate-alert/SKILL.md
```

## MCP clients

Connect a Streamable HTTP client to `http://127.0.0.1:8080/mcp`.
The official Go MCP SDK handles protocol negotiation.

| Tool | Arguments |
| --- | --- |
| `search_content` | Optional `query`, `kind`, `limit`, `offset` |
| `read_content` | Required `path` returned by search |

An empty query browses the catalog. Kind is `context`, `skill`, or `reference`.
Results include the snapshot revision, refresh timestamp, and any stale-cache
warning. Search ranks matches in titles and aliases above body matches; it uses
case-insensitive text matching with no embeddings or LLM calls.

For local clients, launch stdio mode using absolute paths:

```json
{
  "mcpServers": {
    "company-context": {
      "command": "/absolute/path/company-context/bin/contextd",
      "args": [
        "-transport", "stdio",
        "-content", "/absolute/path/company-context/content"
      ]
    }
  }
}
```

Client configuration formats vary. The example shows the command and arguments
to register. Logs go to stderr so stdout remains an MCP transport.

## Content

```text
content/
  context/                  # Company explanations and telemetry pointers
  skills/
    investigate-alert/
      SKILL.md              # Procedure with name/description frontmatter
      references/           # Supporting Markdown
```

Context files need only Markdown. Optional YAML frontmatter fields are `title`,
`description`, and `aliases` (a list). Skills require `name` matching the
directory and a nonempty `description`. Link to other files with relative paths.

Only Markdown under `context/` and `skills/` is published. Dotfiles and other
file types are excluded; published symlinks are rejected. Limits are 256 KiB per
file, 16 MiB total Markdown, and 2,000 files. A Git archive is capped at 32 MiB.
Use a dedicated content tree without unrelated large files.

This MVP serves skills as instructions and Markdown references. It does not
install skills, run scripts, package complete skill archives, or implement the
draft native MCP Skills extension. An investigator needs instructions to consult
the library; an automation can fetch the selected skill before calling its model.
Telemetry tools and credentials are provided by the consuming agent.

## Git mode

```sh
./bin/contextd \
  -git https://git.example.internal/team/company-context.git \
  -branch main \
  -git-root content \
  -state .state \
  -refresh 15m
```

Set `-git-root .` for a data repository with `context/` and `skills/` at its root.
Git mode uses committed content from the specified branch, ignoring `-content`.
Use your Git credential helper or SSH agent; interactive prompts are disabled.
Each refresh has a 30-second timeout, configurable with `-sync-timeout`.

The service fetches into a private bare repository, validates all Markdown,
then atomically publishes a new snapshot. Invalid or unavailable updates leave
the previous snapshot active. Git mode persists the last valid snapshot under
`-state` and can start from it during an outage. Local mode has no persistent
fallback. An initial load failure without usable cached content exits with error.
`-check` always requires a successful current load.

Every response uses one snapshot, but separate calls may see different revisions.
Compare revisions if consistency matters; historical revision reads are not
implemented. A recent refresh does not imply the guidance was recently reviewed.

## Hosting and status

Set `CONTEXT_TOKEN` to require `Authorization: Bearer <token>` for HTTP/MCP
content access. It is required when binding beyond a literal loopback address:

```sh
# Supply CONTEXT_TOKEN through your environment or secret manager.
./bin/contextd -listen 0.0.0.0:8080
```

Use a TLS reverse proxy for shared hosting. This MVP has one shared bearer token,
no OAuth discovery or per-document permissions. Clients must support an explicit
Authorization header. Browser-origin requests are rejected.

| Endpoint | Purpose |
| --- | --- |
| `/healthz` | Process liveness; no authentication |
| `/readyz` | Valid snapshot available; no authentication |
| `/status` | Revision, file count, refresh times, and last error |
| `/api/catalog` | Paginated entries; `kind`, `limit`, `offset` |
| `/api/search` | Same pagination and filters, plus `q` |
| `/api/content?path=…` | Full document with metadata as JSON |
| `/content/{path}` | Raw Markdown with revision and content-hash headers |
| `/mcp` | Streamable HTTP MCP |

Pagination defaults to 20 entries and is capped at 100. Readiness stays healthy
while a valid cached snapshot is being served; monitor `last_error` and
`last_successful_refresh` in `/status` for degraded freshness.

## Verify

```sh
go test ./...
go vet ./...
go run ./cmd/contextd -check
```

Tests cover HTTP/MCP retrieval, access checks, path isolation, validation,
concurrent refresh/read operations, Git updates, and offline recovery. Network
integration tests use temporary loopback listeners; Git tests use local repos.
