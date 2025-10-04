package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	_ "github.com/lib/pq"
)

type Team struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type Player struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	TeamID  int64  `json:"team_id"`
	Goals   int    `json:"goals"`
	Assists int    `json:"assists"`
}

type Match struct {
	ID      int64     `json:"id"`
	Team1ID int64     `json:"team1_id"`
	Team2ID int64     `json:"team2_id"`
	Score1  int       `json:"score1"`
	Score2  int       `json:"score2"`
	Played  time.Time `json:"played_at"`
}

type AddTeamReq struct {
	Name string `json:"name"`
}

type AddPlayerReq struct {
	Name    string `json:"name"`
	TeamID  int64  `json:"team_id"`
	Goals   int    `json:"goals"`
	Assists int    `json:"assists"`
}

type AddMatchReq struct {
	Team1ID int64 `json:"team1_id"`
	Team2ID int64 `json:"team2_id"`
	Score1  int   `json:"score1"`
	Score2  int   `json:"score2"`
}

var db *sql.DB

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func mustInitDB() {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		log.Println("WARNING: DATABASE_URL is empty; using local defaults if any. Set it in your env.")
	}
	var err error
	db, err = sql.Open("postgres", dbURL)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	if err = db.Ping(); err != nil {
		log.Fatalf("ping db: %v", err)
	}
	if err := initSchema(db); err != nil {
		log.Fatalf("init schema: %v", err)
	}
}

func initSchema(db *sql.DB) error {
	// Note: idempotent schema init (CREATE IF NOT EXISTS / ALTER ADD IF NOT EXISTS)
	schema := `
CREATE EXTENSION IF NOT EXISTS btree_gist;

CREATE TABLE IF NOT EXISTS periods (
  id BIGSERIAL PRIMARY KEY,
  label TEXT NOT NULL,
  start_at TIMESTAMPTZ NOT NULL,
  end_at   TIMESTAMPTZ NOT NULL,
  EXCLUDE USING gist (tstzrange(start_at, end_at, '[)') WITH &&)
);

CREATE OR REPLACE VIEW current_period AS
SELECT id, label, start_at, end_at
FROM periods
WHERE now() >= start_at AND now() < end_at
ORDER BY start_at DESC
LIMIT 1;

CREATE OR REPLACE FUNCTION ensure_period_for(date_local date) RETURNS BIGINT AS $$
DECLARE
  tz TEXT := 'Asia/Almaty';
  s  timestamptz := (date_local::timestamp at time zone tz);
  e  timestamptz := ((date_local + 1)::timestamp at time zone tz);
  pid BIGINT;
BEGIN
  INSERT INTO periods(label, start_at, end_at)
  VALUES (to_char(date_local, 'YYYY-MM-DD'), s, e)
  ON CONFLICT DO NOTHING;
  SELECT id INTO pid FROM periods WHERE start_at = s AND end_at = e;
  RETURN pid;
END; $$ LANGUAGE plpgsql;

CREATE TABLE IF NOT EXISTS teams (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  period_id BIGINT REFERENCES periods(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_teams_name_period ON teams(period_id, name);

CREATE TABLE IF NOT EXISTS players (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  team_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
  goals INT NOT NULL DEFAULT 0,
  assists INT NOT NULL DEFAULT 0,
  period_id BIGINT REFERENCES periods(id) ON DELETE RESTRICT
);
CREATE UNIQUE INDEX IF NOT EXISTS ux_players_name_team_period ON players(period_id, team_id, name);

CREATE TABLE IF NOT EXISTS matches (
  id BIGSERIAL PRIMARY KEY,
  team1_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
  team2_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
  score1 INT NOT NULL,
  score2 INT NOT NULL,
  played_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  period_id BIGINT REFERENCES periods(id) ON DELETE RESTRICT
);
CREATE INDEX IF NOT EXISTS ix_matches_period_time ON matches(period_id, played_at);
`
	_, err := db.Exec(schema)
	return err
}

func currentPeriodID(ctx context.Context) (int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, `SELECT id FROM current_period`).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		// lazily create today's period in Asia/Almaty
		_, err = db.ExecContext(ctx, `SELECT ensure_period_for((now() at time zone 'Asia/Almaty')::date)`)
		if err != nil {
			return 0, err
		}
		err = db.QueryRowContext(ctx, `SELECT id FROM current_period`).Scan(&id)
	}
	return id, err
}

func periodIDFromRequest(r *http.Request, ctx context.Context) (int64, error) {
	// ?period=YYYY-MM-DD -> view/archive specific day
	q := r.URL.Query().Get("period")
	if q == "" {
		return currentPeriodID(ctx)
	}
	// ensure period exists for that date; do not auto-create for future
	// but harmless if created; keep simple and idempotent
	_, err := db.ExecContext(ctx, `SELECT ensure_period_for($1::date)`, q)
	if err != nil {
		return 0, err
	}
	var id int64
	err = db.QueryRowContext(ctx, `
		SELECT id FROM periods WHERE label = $1
	`, q).Scan(&id)
	return id, err
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
}

func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func handleAddTeam(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req AddTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		http.Error(w, "empty name", 400)
		return
	}
	ctx := r.Context()
	pid, err := currentPeriodID(ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	var id int64
	err = db.QueryRowContext(ctx, `
		INSERT INTO teams(name, period_id)
		VALUES ($1,$2)
		ON CONFLICT (period_id, name) DO UPDATE SET name = EXCLUDED.name
		RETURNING id
	`, req.Name, pid).Scan(&id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id, "name": req.Name, "period_id": pid})
}

func handleListTeams(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid, err := periodIDFromRequest(r, ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rows, err := db.QueryContext(ctx, `SELECT id, name FROM teams WHERE period_id=$1 ORDER BY name`, pid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var items []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		items = append(items, t)
	}
	writeJSON(w, 200, items)
}

func handleAddPlayer(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req AddPlayerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	if req.Name == "" || req.TeamID == 0 {
		http.Error(w, "name/team required", 400)
		return
	}
	ctx := r.Context()
	// trust period from team
	var pid int64
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id=$1`, req.TeamID).Scan(&pid); err != nil {
		http.Error(w, "team not found", 400)
		return
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO players(name, team_id, goals, assists, period_id)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (period_id, team_id, name)
		DO UPDATE SET goals=EXCLUDED.goals, assists=EXCLUDED.assists
	`, req.Name, req.TeamID, req.Goals, req.Assists, pid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleListPlayers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid, err := periodIDFromRequest(r, ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rows, err := db.QueryContext(ctx, `
		SELECT p.id, p.name, p.team_id, p.goals, p.assists
		FROM players p
		WHERE p.period_id=$1
		ORDER BY (p.goals + p.assists) DESC, p.goals DESC, p.name ASC
	`, pid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var items []Player
	for rows.Next() {
		var it Player
		if err := rows.Scan(&it.ID, &it.Name, &it.TeamID, &it.Goals, &it.Assists); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		items = append(items, it)
	}
	writeJSON(w, 200, items)
}

func handleAddMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var req AddMatchReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}
	ctx := r.Context()
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
	var id int64
	err := db.QueryRowContext(ctx, `
		INSERT INTO matches(team1_id, team2_id, score1, score2, period_id)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id
	`, req.Team1ID, req.Team2ID, req.Score1, req.Score2, pid1).Scan(&id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"id": id})
}

func handleListMatches(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pid, err := periodIDFromRequest(r, ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	rows, err := db.QueryContext(ctx, `
		SELECT id, team1_id, team2_id, score1, score2, played_at
		FROM matches
		WHERE period_id=$1
		ORDER BY played_at ASC, id ASC
	`, pid)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer rows.Close()
	var items []Match
	for rows.Next() {
		var m Match
		if err := rows.Scan(&m.ID, &m.Team1ID, &m.Team2ID, &m.Score1, &m.Score2, &m.Played); err != nil {
			http.Error(w, err.Error(), 500)
			return
		}
		items = append(items, m)
	}
	writeJSON(w, 200, items)
}

func handleDeleteMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	idStr := strings.TrimPrefix(r.URL.Path, "/api/match/")
	id, err := parseInt64(idStr)
	if err != nil || id <= 0 {
		http.Error(w, "bad id", 400)
		return
	}
	if _, err := db.Exec(`DELETE FROM matches WHERE id=$1`, id); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func handleGetPeriod(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	var p struct {
		ID    int64     `json:"id"`
		Label string    `json:"label"`
		Start time.Time `json:"start_at"`
		End   time.Time `json:"end_at"`
	}
	// current or specific
	pid, err := periodIDFromRequest(r, ctx)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	err = db.QueryRowContext(ctx, `SELECT id, label, start_at, end_at FROM periods WHERE id=$1`, pid).
		Scan(&p.ID, &p.Label, &p.Start, &p.End)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	writeJSON(w, 200, p)
}

// Simple file server for static demo (index.html, register.html)
func serveStatic() {
	fs := http.FileServer(http.Dir("."))
	http.Handle("/", fs)
}

func main() {
	port := getenv("PORT", "3000")
	mustInitDB()

	http.HandleFunc("/api/add-team", handleAddTeam)
	http.HandleFunc("/api/list-teams", handleListTeams)
	http.HandleFunc("/api/add-player", handleAddPlayer)
	http.HandleFunc("/api/players", handleListPlayers)
	http.HandleFunc("/api/add-match", handleAddMatch)
	http.HandleFunc("/api/matches", handleListMatches)
	http.HandleFunc("/api/match/", handleDeleteMatch) // DELETE /api/match/{id}
	http.HandleFunc("/api/period", handleGetPeriod)

	serveStatic()

	log.Printf("listening on :%s", port)
	if err := http.ListenAndServe(":"+port, nil); err != nil {
		log.Fatal(err)
	}
}
