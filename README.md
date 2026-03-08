# navitape

Share Navidrome playlists with friends — time-limited public links, no account required.

## Features

- Share any playlist or set of tracks via a short link
- Optional TTL (`7d`, `24h`, `30m`)
- Web player with FLAC / MP3 320k quality toggle
- Per-track download button (original format)
- Track metadata snapshot at share time — immune to playlist edits
- Multi-database: PostgreSQL and SQLite via standard DSN

## Endpoints

| Method | Path | Description |
|--------|------|-------------|
| GET | `/s/{token}` | Web player |
| GET | `/s/{token}/stream/{songID}` | Stream (`?format=mp3` for transcoded) |
| GET | `/s/{token}/download/{songID}` | Download original file |
| GET | `/s/{token}/art/{songID}` | Cover art |
| POST | `/admin/share` | Create share |
| GET | `/admin/shares` | List shares |
| DELETE | `/admin/share/{token}` | Revoke share |

`/admin/` endpoints are internal-only (block at reverse proxy).

## Config

| Env | Description |
|-----|-------------|
| `NAVIDROME_URL` | e.g. `http://192.168.1.100:4533` |
| `NAVIDROME_USER` | Subsonic username |
| `NAVIDROME_PASS` | Subsonic password |
| `DATABASE_URL` | `postgres://...` or `sqlite://./data.db` |
| `BASE_URL` | Public base URL for share links |
| `PORT` | Listen port (default: `8765`) |

## Create a share

```bash
# From a playlist
curl -X POST http://localhost:8765/admin/share \
  -H 'Content-Type: application/json' \
  -d '{"playlist_id": "abc123", "expires_in": "7d", "label": "Summer Mix"}'

# From specific songs
curl -X POST http://localhost:8765/admin/share \
  -H 'Content-Type: application/json' \
  -d '{"song_ids": ["id1", "id2"], "expires_in": "24h"}'
```

## Docker

```bash
docker run -d \
  -e NAVIDROME_URL=http://navidrome:4533 \
  -e NAVIDROME_USER=admin \
  -e NAVIDROME_PASS=secret \
  -e DATABASE_URL=sqlite:///data/navitape.db \
  -e BASE_URL=https://music.example.com \
  -v ./data:/data \
  -p 8765:8765 \
  navitape
```
