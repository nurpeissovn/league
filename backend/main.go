package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"time"
)

// Add this struct
type Period struct {
	ID        int64      `json:"id"`
	Name      string     `json:"name"`
	StartTime time.Time  `json:"start_time"`
	EndTime   *time.Time `json:"end_time,omitempty"`
	IsActive  bool       `json:"is_active"`
}

// Global variable to cache current period (reduces DB queries)
var (
	cachedPeriod    *Period
	lastPeriodCheck time.Time
)

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

// Modified handler
func handleAddMatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	// Ensure active period exists (auto-creates/rotates)
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

	ctx := r.Context()

	// Verify both teams belong to current active period
	var pid1, pid2 int64
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id=$1`, req.Team1ID).Scan(&pid1); err != nil {
		http.Error(w, "team1 not found", 400)
		return
	}
	if err := db.QueryRowContext(ctx, `SELECT period_id FROM teams WHERE id=$1`, req.Team2ID).Scan(&pid2); err != nil {
		http.Error(w, "team2 not found", 400)
		return
	}

	// Ensure teams are from the same period
	if pid1 != pid2 {
		http.Error(w, "teams from different periods", 400)
		return
	}

	// Optionally: ensure teams are from current active period
	if pid1 != period.ID {
		http.Error(w, "teams not from current active period", 400)
		return
	}

	// Insert match
	var id int64
	err = db.QueryRowContext(ctx, `
		INSERT INTO matches(team1_id, team2_id, score1, score2, period_id)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id
	`, req.Team1ID, req.Team2ID, req.Score1, req.Score2, period.ID).Scan(&id)

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]any{"id": id})
}

// Helper function to add team to current period
func handleAddTeam(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, "period error: "+err.Error(), 500)
		return
	}

	var req AddTeamReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", 400)
		return
	}

	var teamID int64
	err = db.QueryRowContext(r.Context(), `
		INSERT INTO teams (name, period_id)
		VALUES ($1, $2)
		RETURNING id
	`, req.Name, period.ID).Scan(&teamID)

	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	writeJSON(w, 200, map[string]any{"id": teamID, "period_id": period.ID})
}

// Add endpoint to list all periods (for history)
func handleListPeriods(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
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

	writeJSON(w, 200, periods)
}

// Add endpoint to get current active period info
func handleCurrentPeriod(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}

	period, err := GetOrCreateActivePeriod(r.Context(), db)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}

	// Calculate time remaining
	elapsed := time.Since(period.StartTime)
	remaining := 24*time.Hour - elapsed

	writeJSON(w, 200, map[string]any{
		"period":          period,
		"elapsed_hours":   elapsed.Hours(),
		"remaining_hours": remaining.Hours(),
		"auto_reset_in":   remaining.String(),
	})
}

// Helper function
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

// Add to your main() function's route setup:
/*
mux.HandleFunc("/api/current-period", handleCurrentPeriod)
mux.HandleFunc("/api/list-periods", handleListPeriods)
*/
