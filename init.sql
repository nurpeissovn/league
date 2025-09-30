CREATE TABLE IF NOT EXISTS teams (
    id SERIAL PRIMARY KEY,
    name TEXT UNIQUE NOT NULL
);

CREATE TABLE IF NOT EXISTS players (
    id SERIAL PRIMARY KEY,
    name TEXT NOT NULL,
    team_id INT REFERENCES teams(id),
    goals INT DEFAULT 0,
    assists INT DEFAULT 0
);

CREATE TABLE IF NOT EXISTS matches (
    id SERIAL PRIMARY KEY,
    team1_id INT REFERENCES teams(id),
    team2_id INT REFERENCES teams(id),
    score1 INT,
    score2 INT,
    played_at TIMESTAMP DEFAULT NOW()
);
