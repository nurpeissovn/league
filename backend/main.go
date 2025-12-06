package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	pq "github.com/lib/pq"
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

type UpdatePlayerReq struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
	TeamID  int    `json:"team_id"`
}

type UpdateTeamReq struct {
	OldName string `json:"old_name"`
	NewName string `json:"new_name"`
}

const defaultLeagueSlug = "default"

var (
	errNotFound       = errors.New("not found")
	errInactivePlayer = errors.New("player is inactive")
)

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
	mux.Handle("/api/delete-match", withJSON(db, deleteMatchHandler))
	mux.Handle("/api/add-player", withJSON(db, addPlayerHandler))
	mux.Handle("/api/update-player", withJSON(db, updatePlayerHandler))
	mux.Handle("/api/delete-player", withJSON(db, deletePlayerHandler))
	mux.Handle("/api/snapshot", withJSON(db, snapshotHandler))

	// Team routes
	mux.Handle("/api/add-team", withJSON(db, addTeamHandler))
	mux.Handle("/api/update-team", withJSON(db, updateTeamHandler))
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

	mux.HandleFunc("/ranking", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		getPlayerRanking(db, w, r)
	})
	mux.HandleFunc("/api/ranking-snapshots", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		listRankingSnapshotsHandler(db, w, r)
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

func execTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) (err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
			return
		}
		if commitErr := tx.Commit(); commitErr != nil {
			err = commitErr
		}
	}()

	err = fn(tx)
	return err
}

func leagueFromRequest(r *http.Request) string {
	raw := strings.TrimSpace(r.Header.Get("X-League-Slug"))
	if raw == "" {
		raw = strings.TrimSpace(r.URL.Query().Get("league"))
	}
	raw = strings.ToLower(raw)
	var b strings.Builder
	for _, ch := range raw {
		if (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '-' {
			b.WriteRune(ch)
		}
	}
	if b.Len() == 0 {
		return defaultLeagueSlug
	}
	return b.String()
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
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	league := leagueFromRequest(r)
	if name == "" {
		http.Error(w, "bad json or empty name", 400)
		return
	}
	row := db.QueryRow(`
		INSERT INTO teams (name, league_slug) VALUES ($1, $2)
		ON CONFLICT (name, league_slug) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()
		RETURNING id, name
	`, name, league)
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
	league := leagueFromRequest(r)
	rows, err := db.Query(`SELECT id, name FROM teams WHERE league_slug = $1 ORDER BY name`, league)
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

func updateTeamHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req UpdateTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	oldName := strings.TrimSpace(req.OldName)
	newName := strings.TrimSpace(req.NewName)
	league := leagueFromRequest(r)
	if oldName == "" || newName == "" {
		http.Error(w, "invalid team name", http.StatusBadRequest)
		return
	}

	var updated TeamDTO
	ctx := r.Context()
	err := execTx(ctx, db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `SELECT id FROM teams WHERE name = $1 AND league_slug = $2 FOR UPDATE`, oldName, league)
		if err := row.Scan(&updated.ID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNotFound
			}
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE teams
			SET name = $1,
			    updated_at = NOW()
			WHERE id = $2 AND league_slug = $3
		`, newName, updated.ID, league); err != nil {
			return err
		}

		updated.Name = newName
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "team not found", http.StatusNotFound)
		default:
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				http.Error(w, "team name already exists", http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(updated)
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
	league := leagueFromRequest(r)
	rows, err := db.Query(`
		SELECT p.name, p.team_id, p.goals, p.assists
		FROM players p
		JOIN teams t ON p.team_id = t.id
		WHERE t.league_slug = $1 AND p.deleted = FALSE
		ORDER BY (p.goals + p.assists) DESC, p.goals DESC
	`, league)
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

// GET /api/matches -> [{id,team1_id,team2_id,score1,score2,played_at}]
func listMatchesHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", 500)
		return
	}
	league := leagueFromRequest(r)
	rows, err := db.Query(`
		SELECT m.id, m.team1_id, m.team2_id, m.score1, m.score2, m.played_at
		FROM matches m
		JOIN teams t1 ON m.team1_id = t1.id
		JOIN teams t2 ON m.team2_id = t2.id
		WHERE t1.league_slug = $1 AND t2.league_slug = $1
		ORDER BY m.played_at ASC
	`, league)
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
	out := []M{} // Initialize empty slice instead of nil
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

type rankingDailyStat struct {
	Date    string `json:"date"`
	Goals   int    `json:"goals"`
	Assists int    `json:"assists"`
	Points  int    `json:"points"`
}

type rankingEntry struct {
	Name         string             `json:"name"`
	DailyStats   []rankingDailyStat `json:"daily_stats"`
	TotalPoints  int                `json:"total_points"`
	LastSnapshot string             `json:"last_snapshot"`
}

type rankingSnapshotDTO struct {
	PlayerName   string `json:"player_name"`
	TeamID       int    `json:"team_id"`
	Status       string `json:"status"`
	SnapshotDate string `json:"snapshot_date"`
}

func getPlayerRanking(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	league := leagueFromRequest(r)

	rankings, err := fetchRankingFromPlayerStats(r.Context(), db, league)
	if err != nil {
		if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "42P01" {
			rankings, err = fetchRankingFromPlayers(r.Context(), db, league)
		}
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(rankings); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

func fetchRankingFromPlayerStats(ctx context.Context, db *sql.DB, league string) ([]rankingEntry, error) {
	const query = `
		SELECT
			p.name,
			COALESCE(
				json_agg(
					json_build_object(
						'date', ps.snapshot_date,
						'goals', ps.goals,
						'assists', ps.assists,
						'points', ps.goals + ps.assists
					)
					ORDER BY ps.snapshot_date
				)
				FILTER (WHERE ps.snapshot_date IS NOT NULL),
				'[]'::json
			) AS daily_stats,
			COALESCE(SUM(ps.goals + ps.assists), 0) AS total_points,
			MAX(ps.snapshot_date) AS last_snapshot
		FROM players p
		JOIN teams t ON p.team_id = t.id
		LEFT JOIN player_stats ps ON p.id = ps.player_id
		WHERE t.league_slug = $1
		GROUP BY p.id, p.name
		ORDER BY total_points DESC, p.name ASC`

	rows, err := db.QueryContext(ctx, query, league)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rankings []rankingEntry
	for rows.Next() {
		var (
			name         string
			dailyJSON    []byte
			totalPoints  int
			lastSnapshot sql.NullTime
		)
		if err := rows.Scan(&name, &dailyJSON, &totalPoints, &lastSnapshot); err != nil {
			return nil, err
		}

		entry := rankingEntry{Name: name, TotalPoints: totalPoints}
		if len(dailyJSON) > 0 {
			if err := json.Unmarshal(dailyJSON, &entry.DailyStats); err != nil {
				return nil, err
			}
		}
		if entry.DailyStats == nil {
			entry.DailyStats = []rankingDailyStat{}
		}
		if lastSnapshot.Valid {
			entry.LastSnapshot = lastSnapshot.Time.Format("2006-01-02")
		}
		rankings = append(rankings, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return rankings, nil
}

func fetchRankingFromPlayers(ctx context.Context, db *sql.DB, league string) ([]rankingEntry, error) {
	const query = `
		SELECT
			p.name,
			COALESCE(p.goals + p.assists, 0) AS total_points
		FROM players p
		JOIN teams t ON p.team_id = t.id
		WHERE t.league_slug = $1
		ORDER BY total_points DESC, p.name ASC`

	rows, err := db.QueryContext(ctx, query, league)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var rankings []rankingEntry
	for rows.Next() {
		var entry rankingEntry
		if err := rows.Scan(&entry.Name, &entry.TotalPoints); err != nil {
			return nil, err
		}
		entry.DailyStats = []rankingDailyStat{}
		entry.LastSnapshot = ""
		rankings = append(rankings, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return rankings, nil
}

func listRankingSnapshotsHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	dateFilter := strings.TrimSpace(r.URL.Query().Get("date"))
	league := leagueFromRequest(r)
	var (
		rows *sql.Rows
		err  error
	)

	if dateFilter != "" {
		if _, parseErr := time.Parse("2006-01-02", dateFilter); parseErr != nil {
			http.Error(w, "invalid date", http.StatusBadRequest)
			return
		}
		rows, err = db.QueryContext(ctx, `
			SELECT player_name, team_id, status, snapshot_date
			FROM ranking_snapshots rs
			JOIN teams t ON rs.team_id = t.id
			WHERE rs.snapshot_date = $1 AND t.league_slug = $2
			ORDER BY player_name
		`, dateFilter, league)
	} else {
		rows, err = db.QueryContext(ctx, `
			SELECT player_name, team_id, status, snapshot_date
			FROM ranking_snapshots rs
			JOIN teams t ON rs.team_id = t.id
			WHERE t.league_slug = $1
			ORDER BY snapshot_date, player_name
		`, league)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	var out []rankingSnapshotDTO
	for rows.Next() {
		var dto rankingSnapshotDTO
		if err := rows.Scan(&dto.PlayerName, &dto.TeamID, &dto.Status, &dto.SnapshotDate); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		out = append(out, dto)
	}
	if err := rows.Err(); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(out); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// ---------- API handlers ----------

func withJSON(db *sql.DB, h func(db *sql.DB, w http.ResponseWriter, r *http.Request)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-League-Slug")
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
	league := leagueFromRequest(r)
	var req AddMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.Team1ID == 0 || req.Team2ID == 0 || req.Team1ID == req.Team2ID {
		http.Error(w, "invalid teams", 400)
		return
	}
	var matchID int
	ctx := r.Context()

	var team1League, team2League string
	if err := db.QueryRowContext(ctx, `SELECT league_slug FROM teams WHERE id = $1`, req.Team1ID).Scan(&team1League); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "team1 not found", http.StatusNotFound)
			return
		}
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	if err := db.QueryRowContext(ctx, `SELECT league_slug FROM teams WHERE id = $1`, req.Team2ID).Scan(&team2League); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "team2 not found", http.StatusNotFound)
			return
		}
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	if team1League != team2League {
		http.Error(w, "teams belong to different leagues", http.StatusBadRequest)
		return
	}
	if team1League != league {
		http.Error(w, "team does not belong to this league", http.StatusForbidden)
		return
	}

	err := db.QueryRowContext(ctx,
		`INSERT INTO matches (team1_id, team2_id, score1, score2) VALUES ($1,$2,$3,$4) RETURNING id`,
		req.Team1ID, req.Team2ID, req.Score1, req.Score2).Scan(&matchID)
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
	league := leagueFromRequest(r)
	var req DeleteMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.ID == 0 {
		http.Error(w, "invalid match id", 400)
		return
	}
	ctx := r.Context()
	res, err := db.ExecContext(ctx, `
		DELETE FROM matches m
		USING teams t1, teams t2
		WHERE m.team1_id = t1.id
		  AND m.team2_id = t2.id
		  AND m.id = $1
		  AND t1.league_slug = $2
		  AND t2.league_slug = $2
	`, req.ID, league)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	if count, _ := res.RowsAffected(); count == 0 {
		http.Error(w, "match not found", http.StatusNotFound)
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
	league := leagueFromRequest(r)
	var req AddPlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	if req.TeamID == 0 || name == "" {
		http.Error(w, "invalid player", 400)
		return
	}
	ctx := r.Context()
	var teamLeague string
	if err := db.QueryRowContext(ctx, `SELECT league_slug FROM teams WHERE id = $1`, req.TeamID).Scan(&teamLeague); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			http.Error(w, "team not found", http.StatusNotFound)
			return
		}
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	if teamLeague != league {
		http.Error(w, "team does not belong to this league", http.StatusForbidden)
		return
	}
	err := execTx(ctx, db, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `
		INSERT INTO players (name, team_id, goals, assists)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (name, team_id)
		DO UPDATE SET goals = EXCLUDED.goals,
			assists = EXCLUDED.assists,
			deleted = FALSE,
			updated_at = NOW()
	`, name, req.TeamID, req.Goals, req.Assists)
		return err
	})
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
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
	league := leagueFromRequest(r)
	var req DeletePlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	name := strings.TrimSpace(req.Name)
	if req.TeamID == 0 || name == "" {
		http.Error(w, "invalid player", 400)
		return
	}

	ctx := r.Context()
	res, err := db.ExecContext(ctx,
		`UPDATE players p
		SET deleted = TRUE,
			updated_at = NOW()
		FROM teams t
		WHERE p.name = $1 AND p.team_id = $2 AND p.deleted = FALSE
			AND t.id = p.team_id AND t.league_slug = $3`,
		name, req.TeamID, league)
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	affected, err := res.RowsAffected()
	if err != nil {
		http.Error(w, "db error: "+err.Error(), 500)
		return
	}
	if affected == 0 {
		http.Error(w, "player not found", http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

func updatePlayerHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	var req UpdatePlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	oldName := strings.TrimSpace(req.OldName)
	newName := strings.TrimSpace(req.NewName)
	league := leagueFromRequest(r)
	if req.TeamID == 0 || oldName == "" || newName == "" {
		http.Error(w, "invalid player", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	err := execTx(ctx, db, func(tx *sql.Tx) error {
		row := tx.QueryRowContext(ctx, `
			SELECT p.id, p.deleted
			FROM players p
			JOIN teams t ON p.team_id = t.id
			WHERE p.name = $1 AND p.team_id = $2 AND t.league_slug = $3
			FOR UPDATE
		`, oldName, req.TeamID, league)

		var (
			playerID int
			deleted  bool
		)
		if err := row.Scan(&playerID, &deleted); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errNotFound
			}
			return err
		}
		if deleted {
			return errInactivePlayer
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE players
			SET name = $1,
				updated_at = NOW()
			WHERE id = $2
		`, newName, playerID); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE ranking_snapshots
			SET player_name = $1
			WHERE player_name = $2 AND team_id = $3
		`, newName, oldName, req.TeamID); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		switch {
		case errors.Is(err, errNotFound):
			http.Error(w, "player not found", http.StatusNotFound)
		case errors.Is(err, errInactivePlayer):
			http.Error(w, "player is inactive", http.StatusBadRequest)
		default:
			if pqErr, ok := err.(*pq.Error); ok && pqErr.Code == "23505" {
				http.Error(w, "player name already exists", http.StatusConflict)
			} else {
				http.Error(w, err.Error(), http.StatusInternalServerError)
			}
		}
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.Write([]byte(`{"ok":true}`))
}

type snapshotRequest struct {
	SnapshotAt string `json:"snapshot_at"`
}

func snapshotHandler(db *sql.DB, w http.ResponseWriter, r *http.Request) {
	if db == nil {
		http.Error(w, "DB not configured", http.StatusInternalServerError)
		return
	}

	league := leagueFromRequest(r)
	var req snapshotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}

	snapshotTime := time.Now().UTC()
	if strings.TrimSpace(req.SnapshotAt) != "" {
		parsed, err := time.Parse(time.RFC3339, req.SnapshotAt)
		if err != nil {
			http.Error(w, "invalid snapshot_at", http.StatusBadRequest)
			return
		}
		snapshotTime = parsed.UTC()
	}
	snapshotDate := snapshotTime.Format("2006-01-02")

	ctx := r.Context()
	err := execTx(ctx, db, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO player_stats (player_id, snapshot_date, goals, assists, points)
			SELECT p.id, $1, p.goals, p.assists, p.goals + p.assists
			FROM players p
			JOIN teams t ON p.team_id = t.id
			WHERE p.deleted = FALSE AND t.league_slug = $2
			ON CONFLICT (player_id, snapshot_date)
			DO UPDATE SET goals = player_stats.goals + EXCLUDED.goals,
				assists = player_stats.assists + EXCLUDED.assists,
				points = player_stats.points + EXCLUDED.points
		`, snapshotDate, league); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ranking_snapshots (player_name, team_id, status, snapshot_date)
			SELECT p.name, p.team_id, 'active', $1
			FROM players p
			JOIN teams t ON p.team_id = t.id
			WHERE p.deleted = FALSE AND t.league_slug = $2
			ON CONFLICT (player_name, team_id, snapshot_date)
			DO UPDATE SET status = EXCLUDED.status
		`, snapshotDate, league); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			INSERT INTO ranking_snapshots (player_name, team_id, status, snapshot_date)
			SELECT p.name, p.team_id, 'missed', $1
			FROM players p
			JOIN teams t ON p.team_id = t.id
			WHERE p.deleted = TRUE AND t.league_slug = $2
			ON CONFLICT (player_name, team_id, snapshot_date)
			DO UPDATE SET status = EXCLUDED.status
		`, snapshotDate, league); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			UPDATE players
			SET goals = 0,
				assists = 0,
				updated_at = NOW()
			WHERE deleted = FALSE AND team_id IN (SELECT id FROM teams WHERE league_slug = $1)
		`, league); err != nil {
			return err
		}

		if _, err := tx.ExecContext(ctx, `
			DELETE FROM matches m
			USING teams t1, teams t2
			WHERE m.team1_id = t1.id
			  AND m.team2_id = t2.id
			  AND t1.league_slug = $1
			  AND t2.league_slug = $1
		`, league); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"snapshot_date": snapshotDate,
	})
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
		if strings.HasSuffix(r.URL.Path, ".html") || r.URL.Path == "/" || strings.HasSuffix(r.URL.Path, "auth.js") {
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
