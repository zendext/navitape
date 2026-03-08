package main

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── Config ────────────────────────────────────────────────────────

var (
	naviURL  = mustEnv("NAVIDROME_URL")
	naviUser = mustEnv("NAVIDROME_USER")
	naviPass = mustEnv("NAVIDROME_PASS")
	baseURL  = strings.TrimRight(os.Getenv("BASE_URL"), "/")
	port     = getEnv("PORT", "8765")
)

func mustEnv(k string) string {
	v := os.Getenv(k)
	if v == "" {
		log.Fatalf("missing env: %s", k)
	}
	return v
}

func getEnv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// ── DB ────────────────────────────────────────────────────────────

var db *pgxpool.Pool

type Track struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Artist   string `json:"artist"`
	Album    string `json:"album"`
	Duration int    `json:"duration"`
}

type Share struct {
	Token     string
	Label     string
	Tracks    []Track
	CreatedAt time.Time
	ExpiresAt *time.Time
}

func initDB(ctx context.Context) {
	var err error
	db, err = pgxpool.New(ctx, mustEnv("DATABASE_URL"))
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}
	_, err = db.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS navidrome_shares (
			token       TEXT PRIMARY KEY,
			label       TEXT NOT NULL DEFAULT '',
			tracks      JSONB NOT NULL DEFAULT '[]',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			expires_at  TIMESTAMPTZ
		)
	`)
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	log.Println("db ready")
}

func getShare(ctx context.Context, token string) (*Share, error) {
	var s Share
	var tracksJSON []byte
	err := db.QueryRow(ctx,
		"SELECT token, label, tracks, created_at, expires_at FROM navidrome_shares WHERE token=$1",
		token,
	).Scan(&s.Token, &s.Label, &tracksJSON, &s.CreatedAt, &s.ExpiresAt)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(tracksJSON, &s.Tracks); err != nil {
		return nil, err
	}
	return &s, nil
}

func (s *Share) expired() bool {
	return s.ExpiresAt != nil && time.Now().UTC().After(*s.ExpiresAt)
}

func (s *Share) hasTrack(id string) bool {
	for _, t := range s.Tracks {
		if t.ID == id {
			return true
		}
	}
	return false
}

// ── Navidrome helpers ─────────────────────────────────────────────

func naviParams(extra map[string]string) url.Values {
	v := url.Values{
		"u": {naviUser}, "p": {naviPass},
		"v": {"1.16.1"}, "c": {"navitape"}, "f": {"json"},
	}
	for k, val := range extra {
		v.Set(k, val)
	}
	return v
}

func naviGet(ctx context.Context, path string, extra map[string]string) (map[string]any, error) {
	u := fmt.Sprintf("%s/rest/%s?%s", naviURL, path, naviParams(extra).Encode())
	req, _ := http.NewRequestWithContext(ctx, "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var envelope struct {
		Response map[string]any `json:"subsonic-response"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return nil, err
	}
	return envelope.Response, nil
}

func strVal(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

// ── Helpers ───────────────────────────────────────────────────────

func newToken() string {
	b := make([]byte, 9)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func parseTTL(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	unit := s[len(s)-1]
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil {
		return nil, fmt.Errorf("invalid ttl: %s", s)
	}
	var d time.Duration
	switch unit {
	case 'd':
		d = time.Duration(n) * 24 * time.Hour
	case 'h':
		d = time.Duration(n) * time.Hour
	case 'm':
		d = time.Duration(n) * time.Minute
	default:
		return nil, fmt.Errorf("unknown unit: %c", unit)
	}
	t := time.Now().UTC().Add(d)
	return &t, nil
}

// ── Player template ───────────────────────────────────────────────

var playerTmpl = template.Must(template.New("player").Funcs(template.FuncMap{
	"durStr": func(secs int) string {
		return fmt.Sprintf("%d:%02d", secs/60, secs%60)
	},
}).Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1.0">
<title>{{.Label}}</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#121212;color:#e0e0e0;font-family:-apple-system,BlinkMacSystemFont,'Segoe UI',sans-serif}
.header{padding:32px 24px 16px;background:linear-gradient(180deg,#1e1e2e 0%,#121212 100%)}
.header h1{font-size:28px;font-weight:700}
.expires{font-size:12px;color:#888;margin-top:6px}
.player{position:sticky;top:0;background:#1a1a2e;padding:16px 24px;z-index:10;border-bottom:1px solid #333}
.now-playing{font-size:13px;color:#aaa;margin-bottom:8px;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
audio{width:100%;accent-color:#7c6af7}
.tracks{padding:8px 0 80px}
.track{display:flex;align-items:center;gap:12px;padding:10px 24px;cursor:pointer;transition:background 0.15s}
.track:hover{background:#1e1e1e}
.track.active{background:#2a2a3e}
.cover{width:48px;height:48px;border-radius:4px;object-fit:cover;background:#333;flex-shrink:0}
.info{flex:1;overflow:hidden}
.title{font-size:14px;font-weight:500;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.artist{font-size:12px;color:#888;margin-top:2px}
.duration{font-size:12px;color:#666;flex-shrink:0}
.track-actions{display:flex;align-items:center;gap:8px;flex-shrink:0}
.dl-btn{background:none;border:none;color:#888;cursor:pointer;font-size:16px;padding:4px;line-height:1;transition:color 0.15s}
.dl-btn:hover{color:#e0e0e0}
.quality-toggle{display:flex;gap:4px;margin-top:8px}
.quality-btn{background:none;border:1px solid #444;border-radius:4px;color:#888;cursor:pointer;font-size:11px;padding:3px 8px;transition:all 0.15s}
.quality-btn.active{background:#7c6af7;border-color:#7c6af7;color:#fff}
</style>
</head>
<body>
<div class="header">
  <h1>{{.Label}}</h1>
  {{if .ExpiresAt}}<p class="expires">Expires {{.ExpiresAt.Format "2006-01-02 15:04 UTC"}}</p>{{end}}
</div>
<div class="player">
  <div class="now-playing" id="np">Select a track to play</div>
  <audio id="audio" controls preload="none"></audio>
  <div class="quality-toggle">
    <button class="quality-btn active" id="q-flac" onclick="setQuality('flac')">FLAC</button>
    <button class="quality-btn" id="q-mp3" onclick="setQuality('mp3')">MP3 320k</button>
  </div>
</div>
<div class="tracks">
{{range $i, $t := .Tracks}}
<div class="track" data-index="{{$i}}" onclick="play({{$i}})">
  <img class="cover" src="/s/{{$.Token}}/art/{{$t.ID}}" onerror="this.style.display='none'">
  <div class="info">
    <div class="title">{{$t.Title}}</div>
    <div class="artist">{{$t.Artist}}</div>
  </div>
  <div class="track-actions">
    <div class="duration">{{durStr $t.Duration}}</div>
    <a class="dl-btn" href="/s/{{$.Token}}/download/{{$t.ID}}" download title="Download FLAC">⬇</a>
  </div>
</div>
{{end}}
</div>
<script>
const tracks={{.TracksJSON}};
const token="{{.Token}}";
let cur=-1;
let quality="flac";
const audio=document.getElementById('audio');

function setQuality(q){
  quality=q;
  document.getElementById('q-flac').classList.toggle('active',q==='flac');
  document.getElementById('q-mp3').classList.toggle('active',q==='mp3');
  if(cur>=0){
    const t=tracks[cur];
    const pos=audio.currentTime;
    audio.src=streamUrl(t.id);
    audio.currentTime=pos;
    audio.play();
  }
}

function streamUrl(id){
  return quality==='mp3'
    ? "/s/"+token+"/stream/"+id+"?format=mp3"
    : "/s/"+token+"/stream/"+id;
}

function play(i){
  if(i<0||i>=tracks.length)return;
  cur=i;
  const t=tracks[i];
  audio.src=streamUrl(t.id);
  audio.play();
  document.getElementById('np').textContent=t.title+(t.artist?' — '+t.artist:'');
  document.querySelectorAll('.track').forEach((el,idx)=>el.classList.toggle('active',idx===i));
}
audio.addEventListener('ended',()=>play(cur+1));
</script>
</body>
</html>`))

type playerData struct {
	*Share
	TracksJSON template.JS
}

// ── Public handlers ───────────────────────────────────────────────

func handleSharePage(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	share, err := getShare(r.Context(), token)
	if err != nil {
		http.Error(w, "Not found", 404)
		return
	}
	if share.expired() {
		http.Error(w, "This share has expired", 410)
		return
	}
	tj, _ := json.Marshal(share.Tracks)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	playerTmpl.Execute(w, playerData{share, template.JS(tj)})
}

func proxyTrack(w http.ResponseWriter, r *http.Request, token, songID string, download bool) {
	share, err := getShare(r.Context(), token)
	if err != nil || !share.hasTrack(songID) {
		http.Error(w, "Forbidden", 403)
		return
	}
	if share.expired() {
		http.Error(w, "Expired", 410)
		return
	}

	params := map[string]string{"id": songID}
	if f := r.URL.Query().Get("format"); f == "mp3" {
		params["format"] = "mp3"
		params["maxBitRate"] = "320"
	}

	u := fmt.Sprintf("%s/rest/stream.view?%s", naviURL, naviParams(params).Encode())
	req, _ := http.NewRequestWithContext(r.Context(), "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", 502)
		return
	}
	defer resp.Body.Close()

	for _, h := range []string{"Content-Type", "Content-Length", "Accept-Ranges"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if download {
		// find track title for filename
		title := songID
		ext := "flac"
		if ct := resp.Header.Get("Content-Type"); ct != "" && (ct == "audio/mpeg" || ct == "audio/mp3") {
			ext = "mp3"
		}
		for _, t := range share.Tracks {
			if t.ID == songID {
				title = t.Title
				break
			}
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.%s"`, title, ext))
	}
	w.WriteHeader(resp.StatusCode)
	io.Copy(w, resp.Body)
}

func handleStream(w http.ResponseWriter, r *http.Request) {
	proxyTrack(w, r, chi.URLParam(r, "token"), chi.URLParam(r, "songID"), false)
}

func handleDownload(w http.ResponseWriter, r *http.Request) {
	proxyTrack(w, r, chi.URLParam(r, "token"), chi.URLParam(r, "songID"), true)
}

func handleArt(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	songID := chi.URLParam(r, "songID")
	share, err := getShare(r.Context(), token)
	if err != nil || !share.hasTrack(songID) {
		http.Error(w, "Forbidden", 403)
		return
	}
	u := fmt.Sprintf("%s/rest/getCoverArt.view?%s", naviURL, naviParams(map[string]string{"id": songID, "size": "128"}).Encode())
	req, _ := http.NewRequestWithContext(r.Context(), "GET", u, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		http.Error(w, "upstream error", 502)
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.Header().Set("Cache-Control", "public, max-age=86400")
	io.Copy(w, resp.Body)
}

// ── Admin handlers ────────────────────────────────────────────────

type createReq struct {
	PlaylistID string   `json:"playlist_id"`
	SongIDs    []string `json:"song_ids"`
	Label      string   `json:"label"`
	ExpiresIn  string   `json:"expires_in"`
}

func handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var req createReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", 400)
		return
	}

	var tracks []Track
	label := req.Label

	switch {
	case req.PlaylistID != "":
		data, err := naviGet(r.Context(), "getPlaylist.view", map[string]string{"id": req.PlaylistID})
		if err != nil {
			http.Error(w, "navidrome error: "+err.Error(), 502)
			return
		}
		pl := data["playlist"].(map[string]any)
		if label == "" {
			label = strVal(pl, "name")
		}
		for _, e := range pl["entry"].([]any) {
			entry := e.(map[string]any)
			t := Track{
				ID:     strVal(entry, "id"),
				Title:  strVal(entry, "title"),
				Artist: strVal(entry, "artist"),
				Album:  strVal(entry, "album"),
			}
			if d, ok := entry["duration"].(float64); ok {
				t.Duration = int(d)
			}
			tracks = append(tracks, t)
		}

	case len(req.SongIDs) > 0:
		if label == "" {
			label = "Shared Tracks"
		}
		for _, sid := range req.SongIDs {
			data, err := naviGet(r.Context(), "getSong.view", map[string]string{"id": sid})
			if err != nil {
				http.Error(w, "navidrome error: "+err.Error(), 502)
				return
			}
			s := data["song"].(map[string]any)
			t := Track{
				ID:     strVal(s, "id"),
				Title:  strVal(s, "title"),
				Artist: strVal(s, "artist"),
				Album:  strVal(s, "album"),
			}
			if d, ok := s["duration"].(float64); ok {
				t.Duration = int(d)
			}
			tracks = append(tracks, t)
		}

	default:
		http.Error(w, "provide playlist_id or song_ids", 400)
		return
	}

	expiresAt, err := parseTTL(req.ExpiresIn)
	if err != nil {
		http.Error(w, err.Error(), 400)
		return
	}

	token := newToken()
	tracksJSON, _ := json.Marshal(tracks)

	if _, err = db.Exec(r.Context(),
		"INSERT INTO navidrome_shares (token, label, tracks, expires_at) VALUES ($1,$2,$3,$4)",
		token, label, tracksJSON, expiresAt,
	); err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":      token,
		"url":        fmt.Sprintf("%s/s/%s", baseURL, token),
		"label":      label,
		"tracks":     len(tracks),
		"expires_at": expiresAt,
	})
}

func handleListShares(w http.ResponseWriter, r *http.Request) {
	rows, err := db.Query(r.Context(),
		"SELECT token, label, created_at, expires_at, jsonb_array_length(tracks) FROM navidrome_shares ORDER BY created_at DESC")
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var result []map[string]any
	for rows.Next() {
		var token, label string
		var createdAt time.Time
		var expiresAt *time.Time
		var trackCount int
		rows.Scan(&token, &label, &createdAt, &expiresAt, &trackCount)
		result = append(result, map[string]any{
			"token": token, "label": label,
			"created_at": createdAt, "expires_at": expiresAt,
			"track_count": trackCount,
		})
	}
	if result == nil {
		result = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func handleDeleteShare(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	tag, err := db.Exec(r.Context(), "DELETE FROM navidrome_shares WHERE token=$1", token)
	if err != nil || tag.RowsAffected() == 0 {
		http.Error(w, "not found", 404)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"deleted": token})
}

// ── Main ──────────────────────────────────────────────────────────

func main() {
	ctx := context.Background()
	initDB(ctx)

	r := chi.NewRouter()
	r.Use(middleware.Logger)
	r.Use(middleware.Recoverer)

	r.Get("/s/{token}", handleSharePage)
	r.Get("/s/{token}/stream/{songID}", handleStream)
	r.Get("/s/{token}/download/{songID}", handleDownload)
	r.Get("/s/{token}/art/{songID}", handleArt)

	r.Post("/admin/share", handleCreateShare)
	r.Get("/admin/shares", handleListShares)
	r.Delete("/admin/share/{token}", handleDeleteShare)

	log.Printf("navitape listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, r))
}
