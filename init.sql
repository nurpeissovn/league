-- Step 1: Create periods table if it doesn't exist
CREATE TABLE IF NOT EXISTS periods (
  id SERIAL PRIMARY KEY,
  name TEXT NOT NULL,
  start_time TIMESTAMP NOT NULL DEFAULT NOW(),
  end_time TIMESTAMP,
  is_active BOOLEAN NOT NULL DEFAULT true,
  created_at TIMESTAMP NOT NULL DEFAULT NOW()
);

-- Step 2: Add period_id to teams if it doesn't exist
DO $$ 
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns 
    WHERE table_name = 'teams' AND column_name = 'period_id'
  ) THEN
    -- First ensure we have an active period
    INSERT INTO periods (name, start_time, is_active)
    SELECT 'Initial Period', NOW(), true
    WHERE NOT EXISTS (SELECT 1 FROM periods);
    
    -- Add period_id column
    ALTER TABLE teams ADD COLUMN period_id INT;
    
    -- Set existing teams to the first/active period
    UPDATE teams SET period_id = (SELECT id FROM periods WHERE is_active = true LIMIT 1);
    
    -- Make it NOT NULL and add foreign key
    ALTER TABLE teams ALTER COLUMN period_id SET NOT NULL;
    ALTER TABLE teams ADD CONSTRAINT fk_teams_period 
      FOREIGN KEY (period_id) REFERENCES periods(id) ON DELETE CASCADE;
    
    -- Drop old unique constraint if exists and add new one with period_id
    ALTER TABLE teams DROP CONSTRAINT IF EXISTS teams_name_key;
    ALTER TABLE teams ADD CONSTRAINT teams_name_period_key UNIQUE (name, period_id);
  END IF;
END $$;

-- Step 3: Add period_id to matches if it doesn't exist
DO $$ 
BEGIN
  IF NOT EXISTS (
    SELECT 1 FROM information_schema.columns 
    WHERE table_name = 'matches' AND column_name = 'period_id'
  ) THEN
    -- Add period_id column
    ALTER TABLE matches ADD COLUMN period_id INT;
    
    -- Set existing matches to the active period
    UPDATE matches SET period_id = (SELECT id FROM periods WHERE is_active = true LIMIT 1);
    
    -- Make it NOT NULL and add foreign key
    ALTER TABLE matches ALTER COLUMN period_id SET NOT NULL;
    ALTER TABLE matches ADD CONSTRAINT fk_matches_period 
      FOREIGN KEY (period_id) REFERENCES periods(id) ON DELETE CASCADE;
  END IF;
END $$;

-- Step 4: Create players table if it doesn't exist (unchanged)
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