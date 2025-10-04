
-- init.sql — robust daily-period schema + migrations

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

-- Create core tables if missing
CREATE TABLE IF NOT EXISTS teams (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS players (
  id BIGSERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  team_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
  goals INT NOT NULL DEFAULT 0,
  assists INT NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS matches (
  id BIGSERIAL PRIMARY KEY,
  team1_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
  team2_id BIGINT REFERENCES teams(id) ON DELETE CASCADE,
  score1 INT NOT NULL,
  score2 INT NOT NULL,
  played_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Ensure period_id columns exist
ALTER TABLE teams   ADD COLUMN IF NOT EXISTS period_id BIGINT REFERENCES periods(id) ON DELETE RESTRICT;
ALTER TABLE players ADD COLUMN IF NOT EXISTS period_id BIGINT REFERENCES periods(id) ON DELETE RESTRICT;
ALTER TABLE matches ADD COLUMN IF NOT EXISTS period_id BIGINT REFERENCES periods(id) ON DELETE RESTRICT;

-- Seed today's period
SELECT ensure_period_for((now() at time zone 'Asia/Almaty')::date);

-- Backfill existing rows
WITH p AS (
  SELECT id FROM periods
  WHERE label = to_char((now() at time zone 'Asia/Almaty')::date,'YYYY-MM-DD')
  LIMIT 1
)
UPDATE teams   SET period_id = (SELECT id FROM p) WHERE period_id IS NULL;

WITH p AS (
  SELECT id FROM periods
  WHERE label = to_char((now() at time zone 'Asia/Almaty')::date,'YYYY-MM-DD')
  LIMIT 1
)
UPDATE players SET period_id = (SELECT id FROM p) WHERE period_id IS NULL;

UPDATE matches m
SET period_id = ensure_period_for((m.played_at at time zone 'Asia/Almaty')::date)
WHERE m.period_id IS NULL;

-- Indexes
CREATE UNIQUE INDEX IF NOT EXISTS ux_teams_name_period ON teams(period_id, name);
CREATE UNIQUE INDEX IF NOT EXISTS ux_players_name_team_period ON players(period_id, team_id, name);
CREATE INDEX IF NOT EXISTS ix_matches_period_time ON matches(period_id, played_at);
