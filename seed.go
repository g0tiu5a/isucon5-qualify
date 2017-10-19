package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"time"

	redis "github.com/garyburd/redigo/redis"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/sessions"
)

var (
	cache redis.Conn
	db    *sql.DB
	store *sessions.CookieStore
)

type User struct {
	ID          int
	AccountName string
	NickName    string
	Email       string
}

type Profile struct {
	UserID    int
	FirstName string
	LastName  string
	Sex       string
	Birthday  mysql.NullTime
	Pref      string
	UpdatedAt time.Time
}

type Entry struct {
	ID        int
	UserID    int
	Private   bool
	Title     string
	Content   string
	CreatedAt time.Time
}

type Comment struct {
	ID        int
	EntryID   int
	UserID    int
	Comment   string
	CreatedAt time.Time
}

type Friend struct {
	ID        int
	CreatedAt time.Time
}

type Footprint struct {
	UserID    int       `json:"user_id"`
	OwnerID   int       `json:"owner_id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type FootprintGroup struct {
	UserID  int
	OwnerID int
}

type Relation struct {
	One       int       `json:"one"`
	Another   int       `json:"another"`
	CreatedAt time.Time `json:"created_at"`
}

func InitializeFootprints() {
	var isNotRequired map[FootprintGroup]bool = map[FootprintGroup]bool{}
	var maxCreatedAt map[FootprintGroup]time.Time = map[FootprintGroup]time.Time{}
	var fps []Footprint

	rows, err := db.Query(`SELECT user_id, owner_id, created_at FROM footprints`)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	defer rows.Close()

	for rows.Next() {
		fp := Footprint{}
		checkErr(rows.Scan(&fp.UserID, &fp.OwnerID, &fp.CreatedAt))

		group := FootprintGroup{
			UserID:  fp.UserID,
			OwnerID: fp.OwnerID,
		}

		if fp.CreatedAt.UnixNano() > maxCreatedAt[group].UnixNano() {
			maxCreatedAt[group] = fp.CreatedAt
		}

		fps = append(fps, fp)
	}

	for _, fp := range fps {
		group := FootprintGroup{
			UserID:  fp.UserID,
			OwnerID: fp.OwnerID,
		}

		if isNotRequired[group] {
			continue
		}
		isNotRequired[group] = true

		var tmpFp Footprint
		tmpFp = Footprint{
			UserID:    fp.UserID,
			OwnerID:   fp.OwnerID,
			CreatedAt: maxCreatedAt[group],
			UpdatedAt: maxCreatedAt[group],
		}

		tmpFpJson, err := json.Marshal(tmpFp)
		if err != nil {
			log.Fatalf("Can not marshal footprint to json.: %s\n", err.Error())
		}
		cache.Do("ZADD", fmt.Sprintf("footprints:user_id:%d", tmpFp.UserID), -tmpFp.CreatedAt.UnixNano(), tmpFpJson)
	}
}

func AddRelationCache(relation *Relation) {
	cache.Do("ZADD", fmt.Sprintf("relations:one:%d", relation.One), -relation.CreatedAt.UnixNano(), relation.Another)
}

func InitializeRelations() {
	rows, err := db.Query(`SELECT one, another, created_at FROM relations`)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	defer rows.Close()

	for rows.Next() {
		relation := Relation{}
		checkErr(rows.Scan(&relation.One, &relation.Another, &relation.CreatedAt))

		//cache.Do("ZADD", fmt.Sprintf("relations:one:%d", relation.One), -relation.CreatedAt.UnixNano(), relation.Another)
		AddRelationCache(&relation)
	}
}

func LoadRedisAof() {
	// Clear redis caches
	err := exec.Command("redis-cli", "flushall").Run()
	if err != nil {
		log.Fatalf("Failed to flushall redis caches.: %s\n", err.Error())
	}

	if _, err = os.Stat("/var/lib/redis/appendonly.aof"); err == nil {
		loadErr := exec.Command("sh", "-c", "redis-cli --pipe < /var/lib/redis/appendonly.aof").Run()
		if loadErr != nil {
			log.Fatalf("Failed to load aof: %s\n", loadErr.Error())
		}
	} else {
		log.Fatalf("appendonly.aof isn't exists.: %s", err.Error())
	}
}

func main() {
	var err error

	cache, err = redis.Dial("unix", "/var/run/redis/redis.sock")
	if err != nil {
		log.Fatalf("Failed to connect redis with unixsock: %s.", err.Error())
	}

	user := os.Getenv("ISUCON5_DB_USER")
	if user == "" {
		user = "isucon"
	}

	password := os.Getenv("ISUCON5_DB_PASSWORD")
	if password == "" {
		password = "isucon"
	}

	dbname := os.Getenv("ISUCON5_DB_NAME")
	if dbname == "" {
		dbname = "isucon5q"
	}

	ssecret := os.Getenv("ISUCON5_SESSION_SECRET")
	if ssecret == "" {
		ssecret = "beermoris"
	}

	db, err = sql.Open("mysql", user+":"+password+"@unix(/var/run/mysqld/mysqld.sock)/"+dbname+"?loc=Local&parseTime=true")
	if err != nil {
		log.Fatalf("Failed to connect to DB: %s.", err.Error())
	}
	defer db.Close()

	// Initialize
	err = exec.Command("redis-cli", "config", "set", "appendonly", "yes").Run()
	if err != nil {
		log.Fatalf("Failed to enable appendonly")
	}
	InitializeFootprints()
	// InitializeRelations()
	err = exec.Command("redis-cli", "config", "set", "appendonly", "no").Run()
	if err != nil {
		log.Fatalf("Failed to disable appendonly")
	}

	// footprints := FetchFootprints(3011, 10)
	// for idx, fp := range footprints {
	// 	fmt.Printf("[footprints:%d] %v\n", idx, fp)
	// }

	// friends := FetchRelations(1995)
	// for idx, friend := range friends {
	// 	fmt.Printf("[relations:%d] %v\n", idx, friend)
	// }

	LoadRedisAof()
}

func checkErr(err error) {
	if err != nil {
		panic(err)
	}
}
