
-- init.sql — daily-period schema
-- Run once to initialize DB. Safe to re-run.
-- Requires Postgres 12+

CREATE EXTENSION IF NOT EXISTS btree_gist;

-- Periods (daily windows in Asia/Almaty)
CREATE TABLE IF NOT EXISTS periods (
  id BIGSERIAL PRIMARY KEY,
  label TEXT NOT NULL,               -- e.g. '2025-10-04'
  start_at TIMESTAMPTZ NOT NULL,
  end_at   TIMESTAMPTZ NOT NULL,
  EXCLUDE USING gist (tstzrange(start_at, end_at, '[)') WITH &&)
);

-- View: current active period
CREATE OR REPLACE VIEW current_period AS
SELECT id, label, start_at, end_at
FROM periods
WHERE now() >= start_at AND now() < end_at
ORDER BY start_at DESC
LIMIT 1;

-- Helper: create/return a period for given local date in Asia/Almaty
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

-- Core domain tables
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

-- Optional: seed today's period (Asia/Almaty). Safe if it already exists.
SELECT ensure_period_for((now() at time zone 'Asia/Almaty')::date);
