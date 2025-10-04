package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
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

var (
	db              *sql.DB
	cachedPeriod    *Period
	lastPeriodCheck time.Time
)

func main() {
	port := getenv("PORT", "3000")
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Println("WARNING: DATABASE_URL is empty (set it in Railway)")
	}

	var err error
	if dbURL != "" {
		db, err = sql.Open("postgres", dbURL)
		if err != nil {
			log.Fatal("open db:", err)
		}
		if err := pingWithRetry(db, 10, 2*time.Second); err != nil {
			log.Fatal("db ping failed:", err)
		}
		if err := runInitSQL(db, "./init.sql"); err != nil {
			log.Fatal("init.sql failed:", err)
		}
		log.Println("DB ready ✅")
	}

	root := http.Dir("./public")
	fs := http.FileServer(root)
	handler := withSecurityHeaders(withCacheControl(stripDirListing(root, fs)))

	mux := http.NewServeMux()
	mux.Handle("/api/add-match", withJSON(db, addMatchHandler))
	mux.Handle("/api/delete-match", withJSON(db, deleteMatchHandler))
	mux.Handle("/api/add-player", withJSON(db, addPlayerHandler))
	mux.Handle("/api/delete-player", withJSON(db, deletePlayerHandler))
	mux.Handle("/api/add-team", withJSON(db, addTeamHandler))
	mux.HandleFunc("/api/list-teams", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		listTeamsHandler(db, w, r)
	})
	mux.HandleFunc("/api/current-period", handleCurrentPeriod)
	mux.HandleFunc("/api/list-periods", handleListPeriods)
	mux.HandleFunc("/api/players", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		listPlayersHandler(db, w, r)
	})
	mux.HandleFunc("/api/matches", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		listMatchesHandler(db, w, r)
	})

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

// --- Period helpers ---

func GetOrCreateActivePeriod(ctx context.Context, db *sql.DB) (*Period, error) {
	if cachedPeriod != nil && time.Since(lastPeriodCheck) < time.Minute {
		return cachedPeriod, nil
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var p Period
	err = tx.QueryRowContext(ctx, `
		SELECT id, name, start_time, end_time, is_active
		FROM periods
		WHERE is_active = true
		LIMIT 1
	`).Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive)

	if err == nil {
		if time.Since(p.StartTime) < 24*time.Hour {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			cachedPeriod = &p
			lastPeriodCheck = time.Now()
			return &p, nil
		}

		now := time.Now()
		if _, err := tx.ExecContext(ctx, `
			UPDATE periods
			SET is_active = false, end_time = $1
			WHERE id = $2
		`, now, p.ID); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}

	err = tx.QueryRowContext(ctx, `
		INSERT INTO periods (name, start_time, is_active)
		VALUES ($1, NOW(), true)
		RETURNING id, name, start_time, end_time, is_active
	`, time.Now().Format("Period 2006-01-02 15:04")).Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive)
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

func findPeriodByDate(ctx context.Context, db *sql.DB, date time.Time) (*Period, error) {
	start := time.Date(date.Year(), date.Month(), date.Day(), 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)

	var p Period
	err := db.QueryRowContext(ctx, `
		SELECT id, name, start_time, end_time, is_active
		FROM periods
		WHERE start_time >= $1 AND start_time < $2
		ORDER BY start_time DESC
		LIMIT 1
	`, start, end).Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func periodFromRequest(ctx context.Context, db *sql.DB, r *http.Request) (*Period, error) {
	periodParam := strings.TrimSpace(r.URL.Query().Get("period"))
	if periodParam == "" {
		return GetOrCreateActivePeriod(ctx, db)
	}
	date, err := time.Parse("2006-01-02", periodParam)
	if err != nil {
		return nil, fmt.Errorf("invalid period date: %w", err)
	}
	return findPeriodByDate(ctx, db, date)
}

// --- HTTP handlers ---

func handleCurrentPeriod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	period, err := periodFromRequest(r.Context(), db, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	now := time.Now()
	elapsed := now.Sub(period.StartTime)
	remaining := 24*time.Hour - elapsed
	if !period.IsActive {
		if period.EndTime != nil {
			elapsed = period.EndTime.Sub(period.StartTime)
		}
		if elapsed < 0 {
			elapsed = 0
		}
		remaining = 0
	} else if remaining < 0 {
		remaining = 0
	}

	reply := map[string]interface{}{
		"period":          period,
		"elapsed_hours":   elapsed.Hours(),
		"remaining_hours": remaining.Hours(),
		"auto_reset_in":   remaining.String(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(reply)
}

func handleListPeriods(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT id, name, start_time, end_time, is_active
		FROM periods
		ORDER BY start_time DESC
	`)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	periods := []Period{}
	for rows.Next() {
		var p Period
		if err := rows.Scan(&p.ID, &p.Name, &p.StartTime, &p.EndTime, &p.IsActive); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		periods = append(periods, p)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(periods)
}

func listTeamsHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	period, err := periodFromRequest(r.Context(), db, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT id, name
		FROM teams
		WHERE period_id = $1
		ORDER BY name
	`, period.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []TeamDTO{}
	for rows.Next() {
		var t TeamDTO
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, t)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func listPlayersHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	period, err := periodFromRequest(r.Context(), db, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT p.name, p.team_id, p.goals, p.assists
		FROM players p
		JOIN teams t ON p.team_id = t.id
		WHERE t.period_id = $1
		ORDER BY (p.goals + p.assists) DESC, p.goals DESC
	`, period.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type player struct {
		Name    string `json:"name"`
		TeamID  int    `json:"team_id"`
		Goals   int    `json:"goals"`
		Assists int    `json:"assists"`
	}

	out := []player{}
	for rows.Next() {
		var p player
		if err := rows.Scan(&p.Name, &p.TeamID, &p.Goals, &p.Assists); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, p)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func listMatchesHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	period, err := periodFromRequest(r.Context(), db, r)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		http.Error(w, err.Error(), status)
		return
	}

	rows, err := db.QueryContext(r.Context(), `
		SELECT id, team1_id, team2_id, score1, score2, played_at
		FROM matches
		WHERE period_id = $1
		ORDER BY played_at ASC
	`, period.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	type match struct {
		ID       int       `json:"id"`
		Team1ID  int       `json:"team1_id"`
		Team2ID  int       `json:"team2_id"`
		Score1   int       `json:"score1"`
		Score2   int       `json:"score2"`
		PlayedAt time.Time `json:"played_at"`
	}

	out := []match{}
	for rows.Next() {
		var m match
		if err := rows.Scan(&m.ID, &m.Team1ID, &m.Team2ID, &m.Score1, &m.Score2, &m.PlayedAt); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, m)
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func addTeamHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req AddTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "bad json or empty name", http.StatusBadRequest)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	row := db.QueryRowContext(r.Context(), `
		INSERT INTO teams (name, period_id)
		VALUES ($1, $2)
		ON CONFLICT (name, period_id) DO UPDATE SET name = EXCLUDED.name
		RETURNING id, name
	`, req.Name, period.ID)

	var team TeamDTO
	if err := row.Scan(&team.ID, &team.Name); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(team)
}

func addMatchHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req AddMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.Team1ID == 0 || req.Team2ID == 0 || req.Team1ID == req.Team2ID {
		http.Error(w, "invalid teams", http.StatusBadRequest)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	ctx := r.Context()

	var pid1, pid2 int64
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id = $1`, req.Team1ID).Scan(&pid1); err != nil {
		http.Error(w, "team1 not found", http.StatusBadRequest)
		return
	}
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id = $1`, req.Team2ID).Scan(&pid2); err != nil {
		http.Error(w, "team2 not found", http.StatusBadRequest)
		return
	}
	if pid1 != pid2 {
		http.Error(w, "teams from different periods", http.StatusBadRequest)
		return
	}
	if pid1 != period.ID {
		http.Error(w, "teams not in current active period", http.StatusBadRequest)
		return
	}

	var matchID int
	err = db.QueryRowContext(ctx, `
		INSERT INTO matches (team1_id, team2_id, score1, score2, period_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id
	`, req.Team1ID, req.Team2ID, req.Score1, req.Score2, period.ID).Scan(&matchID)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]int{"id": matchID})
}

func deleteMatchHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req DeleteMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.ID == 0 {
		http.Error(w, "invalid match id", http.StatusBadRequest)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	res, err := db.ExecContext(r.Context(), `
		DELETE FROM matches
		WHERE id = $1 AND period_id = $2
	`, req.ID, period.ID)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		http.Error(w, "match not found in active period", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func addPlayerHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req AddPlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.TeamID == 0 || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "invalid player", http.StatusBadRequest)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	var teamPeriod int64
	if err := db.QueryRowContext(r.Context(), `SELECT period_id FROM teams WHERE id = $1`, req.TeamID).Scan(&teamPeriod); err != nil {
		http.Error(w, "team not found", http.StatusBadRequest)
		return
	}
	if teamPeriod != period.ID {
		http.Error(w, "team not in current active period", http.StatusBadRequest)
		return
	}

	tx, err := db.BeginTx(r.Context(), nil)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer tx.Rollback()

	_, err = tx.ExecContext(r.Context(), `
		INSERT INTO players (name, team_id, goals, assists)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (name, team_id)
		DO UPDATE SET goals = EXCLUDED.goals, assists = EXCLUDED.assists
	`, req.Name, req.TeamID, req.Goals, req.Assists)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}

	if err := tx.Commit(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func deletePlayerHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req DeletePlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	if req.TeamID == 0 || strings.TrimSpace(req.Name) == "" {
		http.Error(w, "invalid player", http.StatusBadRequest)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	res, err := db.ExecContext(r.Context(), `
		DELETE FROM players
		USING teams
		WHERE players.team_id = teams.id
		  AND players.name = $1
		  AND players.team_id = $2
		  AND teams.period_id = $3
	`, req.Name, req.TeamID, period.ID)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if rows, _ := res.RowsAffected(); rows == 0 {
		http.Error(w, "player not found in active period", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// --- middleware / static helpers ---

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
