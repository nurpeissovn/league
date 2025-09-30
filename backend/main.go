package main

import (
	"fmt"
	"net/http"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	_ "github.com/lib/pq"
)

var db *sqlx.DB

func main() {
	host := os.Getenv("DB_HOST")
	port := os.Getenv("DB_PORT")
	user := os.Getenv("DB_USER")
	password := os.Getenv("DB_PASSWORD")
	dbname := os.Getenv("DB_NAME")

	dsn := fmt.Sprintf("host=%s port=%s user=%s password=%s dbname=%s sslmode=disable",
		host, port, user, password, dbname)

	var err error
	db, err = sqlx.Connect("postgres", dsn)
	if err != nil {
		panic(err)
	}

	r := gin.Default()
	r.Static("/static", "./") // serve static HTML

	// Serve pages
	r.GET("/", func(c *gin.Context) { c.File("./index.html") })
	r.GET("/register", func(c *gin.Context) { c.File("./player_reg.html") })

	// APIs
	r.GET("/teams", getTeams)
	r.GET("/players", getPlayers)
	r.POST("/add-team", addTeam)
	r.POST("/add-player", addPlayer)
	r.POST("/add-match", addMatch)

	r.Run(":8080")
}

// --- Handlers ---
func getTeams(c *gin.Context) {
	var teams []struct {
		ID   int
		Name string
	}
	db.Select(&teams, "SELECT id, name FROM teams")
	c.JSON(http.StatusOK, teams)
}

func getPlayers(c *gin.Context) {
	var players []struct {
		ID      int
		Name    string
		TeamID  int `db:"team_id"`
		Goals   int
		Assists int
	}
	db.Select(&players, "SELECT * FROM players")
	c.JSON(http.StatusOK, players)
}

func addTeam(c *gin.Context) {
	var body struct{ Name string }
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, err := db.Exec("INSERT INTO teams(name) VALUES($1) ON CONFLICT DO NOTHING", body.Name)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func addPlayer(c *gin.Context) {
	var body struct {
		Name    string `json:"name"`
		TeamID  int    `json:"team_id"`
		Goals   int    `json:"goals"`
		Assists int    `json:"assists"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, err := db.Exec("INSERT INTO players(name, team_id, goals, assists) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING",
		body.Name, body.TeamID, body.Goals, body.Assists)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}

func addMatch(c *gin.Context) {
	var body struct {
		Team1ID int `json:"team1_id"`
		Team2ID int `json:"team2_id"`
		Score1  int `json:"score1"`
		Score2  int `json:"score2"`
	}
	if err := c.BindJSON(&body); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	_, err := db.Exec("INSERT INTO matches(team1_id,team2_id,score1,score2) VALUES($1,$2,$3,$4)",
		body.Team1ID, body.Team2ID, body.Score1, body.Score2)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
