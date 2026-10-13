# pageforge

Static site hosting for [Forgejo](https://forgejo.org) repositories — a
GitHub Pages-style server with per-repo settings and OAuth login.

Users log in with their Forgejo account, pick which repositories to publish,
and choose a branch and optional subdirectory per repo. pageforge serves the
latest files from those branches through the Forgejo API. No build step, no
deploy pipeline, no webhook: a push to the branch is live within seconds.

Published sites live at `https://pages.example.com/:owner/:repo/`.

## How it works

- **OAuth login.** pageforge registers as an OAuth2 application on your
  Forgejo instance and requests `read:user` + `read:repository`. Access
  tokens are encrypted at rest (AES-256-GCM) and refreshed automatically.
- **Per-repo settings.** Branch (defaults to the repo's default branch),
  subdirectory (e.g. `docs` or `public`), and an optional SPA fallback that
  serves `index.html` for unknown paths.
- **Freshness without webhooks.** File blobs are cached keyed by the branch
  HEAD commit SHA, which is re-checked every 15 seconds. A new push changes
  the HEAD and the cache naturally misses.
- **Two Forgejo URLs.** Browser-facing OAuth goes through the public URL;
  server-side API calls and the token exchange go through an internal
  docker-network URL, so API traffic never leaves your network.

## Quick start

1. **Register an OAuth2 application** on your Forgejo instance under
   *Settings → Applications → Manage OAuth2 Applications*:
   - Application name: `pageforge` (anything works)
   - Redirect URI: `https://pages.example.com/oauth/callback`
2. **Run the container** next to Forgejo (compose example below).
3. Open `https://pages.example.com`, log in with Forgejo, publish a repo.

```yaml
services:
  pageforge:
    image: ghcr.io/sola-ryu/pageforge:latest
    restart: unless-stopped
    environment:
      PAGEFORGE_FORGEJO_URL: https://git.example.com          # public, for OAuth
      PAGEFORGE_FORGEJO_INTERNAL_URL: http://forgejo:3000     # docker network, for API
      PAGEFORGE_PUBLIC_URL: https://pages.example.com        # this app, for callbacks/links
      PAGEFORGE_CLIENT_ID: <from step 1>
      PAGEFORGE_CLIENT_SECRET: <from step 1>
      PAGEFORGE_SESSION_SECRET: <random 32+ characters>
    volumes:
      - pageforge-data:/data
    networks:
      - proxy        # Traefik
      - forgejo-net  # must reach the forgejo container
    labels:
      - "traefik.enable=true"
      - "traefik.http.routers.pageforge.rule=Host(`pages.example.com`)"
      - "traefik.http.routers.pageforge.entrypoints=websecure"
      - "traefik.http.routers.pageforge.tls.certresolver=letsencrypt"
      - "traefik.http.services.pageforge.loadbalancer.server.port=8080"
```

## Configuration

| Variable | Required | Default | Description |
|---|---|---|---|
| `PAGEFORGE_FORGEJO_URL` | yes | — | Public Forgejo URL (browser-facing OAuth) |
| `PAGEFORGE_FORGEJO_INTERNAL_URL` | no | `PAGEFORGE_FORGEJO_URL` | Forgejo URL for server-side API calls |
| `PAGEFORGE_PUBLIC_URL` | yes | — | This app's public URL |
| `PAGEFORGE_CLIENT_ID` | yes | — | OAuth2 client ID |
| `PAGEFORGE_CLIENT_SECRET` | yes | — | OAuth2 client secret |
| `PAGEFORGE_SESSION_SECRET` | yes | — | Signs sessions; encrypts stored tokens |
| `PAGEFORGE_ADDR` | no | `:8080` | Listen address |
| `PAGEFORGE_DB` | no | `./pageforge.db` | SQLite database path |

## Security notes

- **Publishing a private repository makes its files public** to anyone who
  knows the site URL — pageforge serves through the publisher's token and
  does not gate readers. Only publish repos you're comfortable exposing.
- OAuth tokens are encrypted at rest and never logged. Sessions are
  random 256-bit IDs in `HttpOnly`, `Secure`, `SameSite=Lax` cookies.
- One repo can only be published by one user (enforced in the database).

## Development

Requires Go 1.26+.

```sh
go build -o pageforge .
PAGEFORGE_FORGEJO_URL=https://git.example.com \
PAGEFORGE_PUBLIC_URL=http://localhost:8080 \
PAGEFORGE_CLIENT_ID=... \
PAGEFORGE_CLIENT_SECRET=... \
PAGEFORGE_SESSION_SECRET=... \
./pageforge
```

Note: the OAuth callback must be reachable by the browser at
`PAGEFORGE_PUBLIC_URL/oauth/callback`, so local development needs the
registered redirect URI to match.
