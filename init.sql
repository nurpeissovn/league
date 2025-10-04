-- Step 1: Base tables for periods, teams, matches
CREATE TABLE IF NOT EXISTS periods (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  start_time TIMESTAMP NOT NULL DEFAULT NOW(),
  end_time TIMESTAMP,
  is_active BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS teams (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  period_id INT NOT NULL,
  created_at TIMESTAMP NOT NULL DEFAULT NOW(),
  UNIQUE (name, period_id),
  CONSTRAINT fk_teams_period FOREIGN KEY (period_id)
    REFERENCES periods(id) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS matches (
  id SERIAL PRIMARY KEY,
  team1_id INT NOT NULL,
  team2_id INT NOT NULL,
  score1 INT NOT NULL DEFAULT 0,
  score2 INT NOT NULL DEFAULT 0,
  played_at TIMESTAMP NOT NULL DEFAULT NOW(),
  period_id INT NOT NULL,
  CONSTRAINT fk_matches_period FOREIGN KEY (period_id)
    REFERENCES periods(id) ON DELETE CASCADE,
  CONSTRAINT fk_matches_team1 FOREIGN KEY (team1_id)
    REFERENCES teams(id) ON DELETE CASCADE,
  CONSTRAINT fk_matches_team2 FOREIGN KEY (team2_id)
    REFERENCES teams(id) ON DELETE CASCADE
);

-- Step 1b: Patch legacy periods table structure
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables WHERE table_name = 'periods'
  ) THEN
    -- Ensure created_at column exists before other updates
    IF NOT EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_name = 'periods' AND column_name = 'created_at'
    ) THEN
      ALTER TABLE periods ADD COLUMN created_at TIMESTAMP NOT NULL DEFAULT NOW();
    END IF;

    -- Ensure start_time column exists
    IF NOT EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_name = 'periods' AND column_name = 'start_time'
    ) THEN
      ALTER TABLE periods ADD COLUMN start_time TIMESTAMP;
      UPDATE periods SET start_time = COALESCE(start_time, NOW());
      ALTER TABLE periods ALTER COLUMN start_time SET DEFAULT NOW();
      ALTER TABLE periods ALTER COLUMN start_time SET NOT NULL;
    END IF;

    -- Ensure end_time column exists
    IF NOT EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_name = 'periods' AND column_name = 'end_time'
    ) THEN
      ALTER TABLE periods ADD COLUMN end_time TIMESTAMP;
    END IF;

    -- Ensure is_active column exists
    IF NOT EXISTS (
      SELECT 1 FROM information_schema.columns
      WHERE table_name = 'periods' AND column_name = 'is_active'
    ) THEN
      ALTER TABLE periods ADD COLUMN is_active BOOLEAN;
      UPDATE periods SET is_active = true WHERE is_active IS NULL;
      ALTER TABLE periods ALTER COLUMN is_active SET DEFAULT true;
      ALTER TABLE periods ALTER COLUMN is_active SET NOT NULL;
    END IF;
  END IF;
END $$;

-- Step 2: Add period_id to teams if it doesn't exist
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables WHERE table_name = 'teams'
  ) AND NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'teams' AND column_name = 'period_id'
  ) THEN
    INSERT INTO periods (name, start_time, is_active)
    SELECT 'Initial Period', NOW(), true
    WHERE NOT EXISTS (SELECT 1 FROM periods);

    ALTER TABLE teams ADD COLUMN period_id INT;
    UPDATE teams SET period_id = (SELECT id FROM periods WHERE is_active = true LIMIT 1);
    ALTER TABLE teams ALTER COLUMN period_id SET NOT NULL;
    ALTER TABLE teams ADD CONSTRAINT fk_teams_period
      FOREIGN KEY (period_id) REFERENCES periods(id) ON DELETE CASCADE;

    ALTER TABLE teams DROP CONSTRAINT IF EXISTS teams_name_key;
    ALTER TABLE teams ADD CONSTRAINT teams_name_period_key UNIQUE (name, period_id);
  END IF;
END $$;

-- Step 3: Add period_id to matches if it doesn't exist
DO $$
BEGIN
  IF EXISTS (
    SELECT 1 FROM information_schema.tables WHERE table_name = 'matches'
  ) AND NOT EXISTS (
    SELECT 1 FROM information_schema.columns
    WHERE table_name = 'matches' AND column_name = 'period_id'
  ) THEN
    ALTER TABLE matches ADD COLUMN period_id INT;
    UPDATE matches SET period_id = (SELECT id FROM periods WHERE is_active = true LIMIT 1);
    ALTER TABLE matches ALTER COLUMN period_id SET NOT NULL;
    ALTER TABLE matches ADD CONSTRAINT fk_matches_period
      FOREIGN KEY (period_id) REFERENCES periods(id) ON DELETE CASCADE;
  END IF;
END $$;

-- Step 4: Create players table if it doesn't exist
CREATE TABLE IF NOT EXISTS players (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  team_id INT NOT NULL REFERENCES teams(id) ON DELETE CASCADE,
  goals INT NOT NULL DEFAULT 0,
  assists INT NOT NULL DEFAULT 0,
  UNIQUE (name, team_id)
);

-- Step 5: Create indexes
CREATE INDEX IF NOT EXISTS idx_periods_active
  ON periods(is_active, start_time DESC)
  WHERE is_active = true;

CREATE INDEX IF NOT EXISTS idx_teams_period ON teams(period_id);
CREATE INDEX IF NOT EXISTS idx_players_team ON players(team_id);
CREATE INDEX IF NOT EXISTS idx_matches_period ON matches(period_id);
CREATE INDEX IF NOT EXISTS idx_matches_played_at ON matches(played_at);

-- Step 6: Ensure there's always one active period
INSERT INTO periods (name, start_time, is_active)
SELECT 'Initial Period', NOW(), true
WHERE NOT EXISTS (SELECT 1 FROM periods WHERE is_active = true);
