# navitape

Share Navidrome playlists with friends — time-limited, no account required.

## Features

- Share any playlist or set of tracks via a short link
- Optional TTL (e.g. `7d`, `24h`)
- Web player included — no login needed for listeners
- Admin API for creating/revoking shares

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/s/{token}` | Web player page |
| GET | `/s/{token}/stream/{songID}` | Audio stream proxy |
| GET | `/s/{token}/art/{songID}` | Cover art proxy |
| POST | `/admin/share` | Create share |
| GET | `/admin/shares` | List shares |
| DELETE | `/admin/share/{token}` | Revoke share |

## Config (env vars)

| Var | Description |
|-----|-------------|
| `NAVIDROME_URL` | e.g. `http://192.168.50.247:4533` |
| `NAVIDROME_USER` | Subsonic username |
| `NAVIDROME_PASS` | Subsonic password |
| `DATABASE_URL` | PostgreSQL DSN |
| `BASE_URL` | Public base URL for share links |
| `PORT` | Listen port (default: `8765`) |

## Create a share

```bash
curl -X POST http://localhost:8765/admin/share \
  -H 'Content-Type: application/json' \
  -d '{"playlist_id": "abc123", "expires_in": "7d"}'
```
