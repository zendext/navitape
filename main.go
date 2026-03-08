package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	_ "embed"
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
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
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

var (
	db       *sql.DB
	dbDriver string // "pgx" | "sqlite"
)

// ph returns the nth placeholder ($n for postgres, ? for sqlite)
func ph(n int) string {
	if dbDriver == "sqlite" {
		return "?"
	}
	return fmt.Sprintf("$%d", n)
}

// jsonArrayLen returns DB-appropriate json array length expression
func jsonArrayLen(col string) string {
	if dbDriver == "sqlite" {
		return "json_array_length(" + col + ")"
	}
	return "jsonb_array_length(" + col + ")"
}

func initDB(ctx context.Context) {
	dsn := mustEnv("DATABASE_URL")

	var driverDSN string
	switch {
	case strings.HasPrefix(dsn, "postgres://"), strings.HasPrefix(dsn, "postgresql://"):
		dbDriver = "pgx"
		driverDSN = dsn
	case strings.HasPrefix(dsn, "sqlite://"):
		dbDriver = "sqlite"
		driverDSN = strings.TrimPrefix(dsn, "sqlite://")
	default:
		// bare path → sqlite
		dbDriver = "sqlite"
		driverDSN = dsn
	}

	var err error
	db, err = sql.Open(dbDriver, driverDSN)
	if err != nil {
		log.Fatalf("db open: %v", err)
	}
	if err = db.PingContext(ctx); err != nil {
		log.Fatalf("db ping: %v", err)
	}

	tracksColDef := "JSONB NOT NULL DEFAULT '[]'"
	timeDef := "TIMESTAMPTZ"
	if dbDriver == "sqlite" {
		tracksColDef = "TEXT NOT NULL DEFAULT '[]'"
		timeDef = "DATETIME"
	}

	_, err = db.ExecContext(ctx, fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS navidrome_shares (
			token       TEXT PRIMARY KEY,
			label       TEXT NOT NULL DEFAULT '',
			tracks      %s,
			created_at  %s NOT NULL DEFAULT CURRENT_TIMESTAMP,
			expires_at  %s
		)`, tracksColDef, timeDef, timeDef))
	if err != nil {
		log.Fatalf("db init: %v", err)
	}
	log.Printf("db ready (%s)", dbDriver)
}

// ── Model ─────────────────────────────────────────────────────────

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

func getShare(ctx context.Context, token string) (*Share, error) {
	var s Share
	var tracksJSON string
	var expiresAt sql.NullTime

	err := db.QueryRowContext(ctx,
		fmt.Sprintf("SELECT token, label, tracks, created_at, expires_at FROM navidrome_shares WHERE token=%s", ph(1)),
		token,
	).Scan(&s.Token, &s.Label, &tracksJSON, &s.CreatedAt, &expiresAt)
	if err != nil {
		return nil, err
	}
	if expiresAt.Valid {
		s.ExpiresAt = &expiresAt.Time
	}
	if err := json.Unmarshal([]byte(tracksJSON), &s.Tracks); err != nil {
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

// ── Template ──────────────────────────────────────────────────────

//go:embed templates/player.html
var playerTmplStr string

var playerTmpl *template.Template

func init() {
	playerTmpl = template.Must(template.New("player").Funcs(template.FuncMap{
		"durStr": func(secs int) string {
			return fmt.Sprintf("%d:%02d", secs/60, secs%60)
		},
	}).Parse(playerTmplStr))
}

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
	if r.URL.Query().Get("format") == "mp3" {
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
		title, ext := songID, "flac"
		if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "mpeg") {
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
		entries, _ := pl["entry"].([]any)
		for _, e := range entries {
			entry := e.(map[string]any)
			t := Track{ID: strVal(entry, "id"), Title: strVal(entry, "title"), Artist: strVal(entry, "artist"), Album: strVal(entry, "album")}
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
			t := Track{ID: strVal(s, "id"), Title: strVal(s, "title"), Artist: strVal(s, "artist"), Album: strVal(s, "album")}
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

	var expiresVal any
	if expiresAt != nil {
		expiresVal = *expiresAt
	}

	if _, err = db.ExecContext(r.Context(),
		fmt.Sprintf("INSERT INTO navidrome_shares (token, label, tracks, expires_at) VALUES (%s,%s,%s,%s)",
			ph(1), ph(2), ph(3), ph(4)),
		token, label, string(tracksJSON), expiresVal,
	); err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token": token, "url": fmt.Sprintf("%s/s/%s", baseURL, token),
		"label": label, "tracks": len(tracks), "expires_at": expiresAt,
	})
}

func handleListShares(w http.ResponseWriter, r *http.Request) {
	rows, err := db.QueryContext(r.Context(),
		fmt.Sprintf("SELECT token, label, created_at, expires_at, %s FROM navidrome_shares ORDER BY created_at DESC",
			jsonArrayLen("tracks")))
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	var result []map[string]any
	for rows.Next() {
		var token, label string
		var createdAt time.Time
		var expiresAt sql.NullTime
		var trackCount int
		rows.Scan(&token, &label, &createdAt, &expiresAt, &trackCount)
		var exp any
		if expiresAt.Valid {
			exp = expiresAt.Time
		}
		result = append(result, map[string]any{
			"token": token, "label": label,
			"created_at": createdAt, "expires_at": exp,
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
	res, err := db.ExecContext(r.Context(),
		fmt.Sprintf("DELETE FROM navidrome_shares WHERE token=%s", ph(1)), token)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
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
