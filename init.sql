-- Periods table for 24-hour cycles
CREATE TABLE IF NOT EXISTS periods (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  start_time TIMESTAMP NOT NULL DEFAULT NOW(),
  end_time TIMESTAMP,
  is_active BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Index for quick active period lookup
CREATE INDEX IF NOT EXISTS idx_periods_active 
ON periods(is_active, start_time DESC) 
WHERE is_active = true;

-- Teams table with period reference
CREATE TABLE IF NOT EXISTS teams (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  period_id INT NOT NULL REFERENCES periods(id) ON DELETE CASCADE,
  created_at TIMESTAMP NOT NULL DEFAULT NOW(),
  UNIQUE (name, period_id)
);

CREATE INDEX IF NOT EXISTS idx_teams_period ON teams(period_id);

-- Players table with period reference
CREATE TABLE IF NOT EXISTS players (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  team_id INT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  goals INT NOT NULL DEFAULT 0,
  assists INT NOT NULL DEFAULT 0,
  created_at TIMESTAMP NOT NULL DEFAULT NOW(),
  UNIQUE (name, team_id)
);

CREATE INDEX IF NOT EXISTS idx_players_team ON players(team_id);

-- Matches table with period reference
CREATE TABLE IF NOT EXISTS matches (
  id SERIAL PRIMARY KEY,
  team1_id INT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  team2_id INT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  score1 INT NOT NULL,
  score2 INT NOT NULL,
  period_id INT NOT NULL REFERENCES periods(id) ON DELETE CASCADE,
  played_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_matches_period ON matches(period_id);
CREATE INDEX IF NOT EXISTS idx_matches_played_at ON matches(played_at);

-- View for current period data (for frontend)
CREATE OR REPLACE VIEW current_period_summary AS
SELECT 
  p.id as period_id,
  p.name as period_name,
  p.start_time,
  p.end_time,
  COUNT(DISTINCT t.id) as total_teams,
  COUNT(DISTINCT pl.id) as total_players,
  COUNT(DISTINCT m.id) as total_matches
FROM periods p
LEFT JOIN teams t ON t.period_id = p.id
LEFT JOIN players pl ON pl.team_id = t.id
LEFT JOIN matches m ON m.period_id = p.id
WHERE p.is_active = true
GROUP BY p.id, p.name, p.start_time, p.end_time;

-- Optional: Archive old periods (run manually or via cron)
-- DELETE FROM periods WHERE end_time < NOW() - INTERVAL '90 days';