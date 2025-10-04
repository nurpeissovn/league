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

type DeleteMatchReq struct {
	ID int `json:"id"`
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

type AddTeamReq struct {
	Name string `json:"name"`
}

type TeamDTO struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type Period struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	IsActive  bool       `json:"is_active"`
}

// Global DB variable
var db *sql.DB

// Cache for current period (reduces DB queries)
var (
	cachedPeriod    *Period
	lastPeriodCheck time.Time
)

func main() {
	port := getenv("PORT", "3000")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Println("WARNING: DATABASE_URL is empty (set it in Railway)")
	}

	// connect DB
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
	mux.Handle("/api/delete-match", withJSON(db, deleteMatchHandler))
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
	
	// Period routes
	mux.HandleFunc("/api/current-period", handleCurrentPeriod)
	mux.HandleFunc("/api/list-periods", handleListPeriods)
	
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

// GetOrCreateActivePeriod ensures there's an active period and auto-rotates after 24h
func GetOrCreateActivePeriod(ctx context.Context, db *sql.DB) (*Period, error) {
	// Use cached period if checked within last minute
	if cachedPeriod != nil && time.Since(lastPeriodCheck) < time.Minute {
		return cachedPeriod, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Check for active period
	var p Period
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, start_time, end_time, is_active 
		FROM periods 
		WHERE is_active = true 
		LIMIT 1
	`).Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive)

	// If active period exists and is < 24 hours old, return it
	if err == nil {
		if time.Since(p.StartTime) < 24*time.Hour {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			cachedPeriod = &p
			lastPeriodCheck = time.Now()
			return &p, nil
		}
		
		// Period is > 24 hours old, close it and create new one
		now := time.Now()
		_, err = tx.ExecContext(ctx, `
			UPDATE periods 
			SET is_active = false, end_time = $1 
			WHERE id = $2
		`, now, p.ID)
		if err != nil {
			return nil, err
		}
	}

	// Create new period
	newName := time.Now().Format("Period 2006-01-02 15:04")
	err = tx.QueryRowContext(ctx, `
		INSERT INTO periods (name, start_time, is_active)
		VALUES ($1, NOW(), true)
		RETURNING id, name, start_time, end_time, is_active
	`, newName).Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive)
	
	if err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}

	cachedPeriod = &p
	lastPeriodCheck = time.Now()
	return &p, nil
}

// ---- Period endpoints ----

func handleCurrentPeriod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	elapsed := time.Since(period.StartTime)
	remaining := 24*time.Hour - elapsed
	
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"period":          period,
		"elapsed_hours":   elapsed.Hours(),
		"remaining_hours": remaining.Hours(),
		"auto_reset_in":   remaining.String(),
	})
}

func handleListPeriods(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT id, name, start_time, end_time, is_active
		FROM periods
		ORDER BY start_time DESC
	`)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	periods := []Period{}
	for rows.Next() {
		var p Period
		if err := rows.Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		periods = append(periods, p)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(periods)
}

// ---- Team endpoints ----

func addTeamHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}

	// Get or create active period
	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, "period error: "+err.Error(), 500)
		return
	}

	var req AddTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "bad json or empty name", 400)
		return
	}

	row := db.QueryRow(`
		INSERT INTO teams (name, period_id) VALUES ($1, $2)
		ON CONFLICT (name, period_id) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, name
	`, req.Name, period.ID)
	
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

	// Get current period
	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, "period error: "+err.Error(), 500)
		return
	}

	rows, err := db.Query(`SELECT id, name FROM teams WHERE period_id = $1 ORDER BY name`, period.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	
	out := []TeamDTO{}
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

// GET /api/players
func listPlayersHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}

	// Get current period
	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, "period error: "+err.Error(), 500)
		return
	}

	rows, err := db.Query(`
		SELECT p.name, p.team_id, p.goals, p.assists 
		FROM players p
		JOIN teams t ON p.team_id = t.id
		WHERE t.period_id = $1
		ORDER BY (p.goals + p.assists) DESC, p.goals DESC
	`, period.ID)
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
	out := []P{}
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

// GET /api/matches
func listMatchesHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}

	// Get current period
	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, "period error: "+err.Error(), 500)
		return
	}

	rows, err := db.Query(`
		SELECT id, team1_id, team2_id, score1, score2, played_at 
		FROM matches 
		WHERE period_id = $1
		ORDER BY played_at ASC
	`, period.ID)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	
	type M struct {
		ID       int       `json:"id"`
		Team1ID  int       `json:"team1_id"`
		Team2ID  int       `json:"team2_id"`
		Score1   int       `json:"score1"`
		Score2   int       `json:"score2"`
		PlayedAt time.Time `json:"played_at"`
	}
	out := []M{}
	for rows.Next() {
		var m M
		if err := rows.Scan(&m.ID, &m.Team1ID, &m.Team2ID, &m.Score1, &m.Score2, &m.PlayedAt); err != nil {
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

	// Get or create active period
	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, "period error: "+err.Error(), 500)
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

	ctx := r.Context()
	
	// Verify both teams exist and belong to current period
	var pid1, pid2 int64
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id=$1`, req.Team1ID).Scan(&pid1); err != nil {
		http.Error(w, "team1 not found", 400)
		return
	}
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id=$1`, req.Team2ID).Scan(&pid2); err != nil {
		http.Error(w, "team2 not found", 400)
		return
	}
	if pid1 != pid2 {
		http.Error(w, "teams from different periods", 400)
		return
	}
	if pid1 != period.ID {
		http.Error(w, "teams not from current active period", 400)
		return
	}

	var matchID int
	err = db.QueryRowContext(ctx,
		`INSERT INTO matches (team1_id, team2_id, score1, score2, period_id) VALUES ($1,$2,$3,$4,$5) RETURNING id`,
		req.Team1ID, req.Team2ID, req.Score1, req.Score2, period.ID).Scan(&matchID)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]int{"id": matchID})
}

func deleteMatchHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	var req DeleteMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.ID == 0 {
		http.Error(w, "invalid match id", 400)
		return
	}
	_, err := db.ExecContext(r.Context(),
		`DELETE FROM matches WHERE id = $1`, req.ID)
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