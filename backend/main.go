package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type AddMatchReq struct {
	Team1ID int `json:"team1_id"`
	Team2ID int `json:"team2_id"`
	Score1  int `json:"score1"`
	Score2  int `json:"score2"`
}

type AddPlayerReq struct {
	Name    string `json:"name"`
	TeamID  int    `json:"team_id"`
	Goals   int    `json:"goals"`
	Assists int    `json:"assists"`
}

type DeletePlayerReq struct {
	Name   string `json:"name"`
	TeamID int    `json:"team_id"`
}

func main() {
	port := getenv("PORT", "3000")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Println("WARNING: DATABASE_URL is empty (set it in Railway)")
	}

	// connect DB
	var db *sql.DB
	var err error
	if dbURL != "" {
		db, err = sql.Open("postgres", dbURL)
		if err != nil {
			log.Fatal("open db:", err)
		}
		if err := pingWithRetry(db, 10, 2*time.Second); err != nil {
			log.Fatal("db ping failed:", err)
		}
		// run init.sql
		if err := runInitSQL(db, "./init.sql"); err != nil {
			log.Fatal("init.sql failed:", err)
		}
		log.Println("DB ready ✅")
	}

	root := http.Dir("./public")
	fs := http.FileServer(root)
	handler := withSecurityHeaders(withCacheControl(stripDirListing(root, fs)))

	mux := http.NewServeMux()
	// API
	mux.Handle("/api/add-match", withJSON(db, addMatchHandler))
	mux.Handle("/api/add-player", withJSON(db, addPlayerHandler))
	mux.Handle("/api/delete-player", withJSON(db, deletePlayerHandler))

	// Team routes
	mux.Handle("/api/add-team", withJSON(db, addTeamHandler))
	mux.HandleFunc("/api/list-teams", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		listTeamsHandler(db, w, r)
	})

	mux.HandleFunc("/api/players", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		listPlayersHandler(db, w, r)
	})

	mux.HandleFunc("/api/matches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		listMatchesHandler(db, w, r)
	})

	// static
	mux.Handle("/", handler)

	srv := &http.Server{
		Addr:              ":" + port,
		Handler:           withLogging(mux),
		ReadHeaderTimeout: 5 * time.Second,
	}
	log.Printf("Listening on :%s …", port)
	log.Fatal(srv.ListenAndServe())
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func pingWithRetry(db *sql.DB, tries int, delay time.Duration) error {
	var err error
	for i := 0; i < tries; i++ {
		if err = db.Ping(); err == nil {
			return nil
		}
		time.Sleep(delay)
	}
	return err
}

// ---- Team endpoints ----

type AddTeamReq struct {
	Name string `json:"name"`
}
type TeamDTO struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

func addTeamHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	var req AddTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "bad json or empty name", 400)
		return
	}
	row := db.QueryRow(`
		INSERT INTO teams (name) VALUES ($1)
		ON CONFLICT (name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, name
	`, req.Name)
	var t TeamDTO
	if err := row.Scan(&t.ID, &t.Name); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(t)
}

func listTeamsHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	rows, err := db.Query(`SELECT id, name FROM teams ORDER BY name`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	out := []TeamDTO{} // Initialize empty slice instead of nil
	for rows.Next() {
		var t TeamDTO
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out = append(out, t)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func runInitSQL(db *sql.DB, path string) error {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("init.sql not found, skip")
			return nil
		}
		return err
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	_, err = db.Exec(string(b))
	return err
}

// GET /api/players -> [{name, team_id, goals, assists}]
func listPlayersHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	rows, err := db.Query(`SELECT name, team_id, goals, assists FROM players ORDER BY (goals + assists) DESC, goals DESC`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	type P struct {
		Name    string `json:"name"`
		TeamID  int    `json:"team_id"`
		Goals   int    `json:"goals"`
		Assists int    `json:"assists"`
	}
	out := []P{} // Initialize empty slice instead of nil
	for rows.Next() {
		var p P
		if err := rows.Scan(&p.Name, &p.TeamID, &p.Goals, &p.Assists); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out = append(out, p)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// GET /api/matches -> [{team1_id,team2_id,score1,score2,played_at}]
func listMatchesHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	rows, err := db.Query(`SELECT team1_id, team2_id, score1, score2, played_at FROM matches ORDER BY played_at ASC`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	type M struct {
		Team1ID  int       `json:"team1_id"`
		Team2ID  int       `json:"team2_id"`
		Score1   int       `json:"score1"`
		Score2   int       `json:"score2"`
		PlayedAt time.Time `json:"played_at"`
	}
	out := []M{} // Initialize empty slice instead of nil
	for rows.Next() {
		var m M
		if err := rows.Scan(&m.Team1ID, &m.Team2ID, &m.Score1, &m.Score2, &m.PlayedAt); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		out = append(out, m)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

// ---------- API handlers ----------

func withJSON(db *sql.DB, h func(db *sql.DB, w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h(db, w, r)
	})
}

func addMatchHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	var req AddMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.Team1ID == 0 || req.Team2ID == 0 || req.Team1ID == req.Team2ID {
		http.Error(w, "invalid teams", 400)
		return
	}
	_, err := db.ExecContext(r.Context(),
		`INSERT INTO matches (team1_id, team2_id, score1, score2) VALUES ($1,$2,$3,$4)`,
		req.Team1ID, req.Team2ID, req.Score1, req.Score2)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func addPlayerHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	var req AddPlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.TeamID == 0 || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "invalid player", 400)
		return
	}
	ctx := context.Background()
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(ctx, `
		INSERT INTO players (name, team_id, goals, assists)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (name, team_id)
		DO UPDATE SET goals = EXCLUDED.goals, assists = EXCLUDED.assists
	`, req.Name, req.TeamID, req.Goals, req.Assists)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func deletePlayerHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	var req DeletePlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.TeamID == 0 || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "invalid player", 400)
		return
	}

	_, err := db.ExecContext(r.Context(),
		`DELETE FROM players WHERE name = $1 AND team_id = $2`,
		req.Name, req.TeamID)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

// ---------- static helpers ----------

func withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start))
	})
}

func withSecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("X-XSS-Protection", "0")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data: https:; style-src 'self' 'unsafe-inline' https:; script-src 'self' 'unsafe-inline' https:; connect-src 'self' https:")
		next.ServeHTTP(w, r)
	})
}

func withCacheControl(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" {
			w.Header().Set("Cache-Control", "no-cache")
		} else {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
		next.ServeHTTP(w, r)
	})
}

func stripDirListing(root http.Dir, fs http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		full := filepath.Join(".", "public", filepath.FromSlash(p))
		if info, err := os.Stat(full); err == nil && info.IsDir() {
			index := filepath.Join(full, "index.html")
			if _, err := os.Stat(index); err == nil {
				http.ServeFile(w, r, index)
				return
			}
			http.NotFound(w, r)
			return
		}
		fs.ServeHTTP(w, r)
	})
}
