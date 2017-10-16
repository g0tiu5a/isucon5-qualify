package main

import (
	"crypto/sha512"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/exec"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	redis "github.com/garyburd/redigo/redis"
	"github.com/go-sql-driver/mysql"
	"github.com/gorilla/context"
	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
)

var (
	redisConn redis.Conn
	db        *sql.DB
	store     *sessions.CookieStore
	users     map[int]User
	salts     map[int]string
)

type User struct {
	ID          int
	AccountName string
	NickName    string
	Email       string
	PassHash    string
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

var prefs = []string{"未入力",
	"北海道", "青森県", "岩手県", "宮城県", "秋田県", "山形県", "福島県", "茨城県", "栃木県", "群馬県", "埼玉県", "千葉県", "東京都", "神奈川県", "新潟県", "富山県",
	"石川県", "福井県", "山梨県", "長野県", "岐阜県", "静岡県", "愛知県", "三重県", "滋賀県", "京都府", "大阪府", "兵庫県", "奈良県", "和歌山県", "鳥取県", "島根県",
	"岡山県", "広島県", "山口県", "徳島県", "香川県", "愛媛県", "高知県", "福岡県", "佐賀県", "長崎県", "熊本県", "大分県", "宮崎県", "鹿児島県", "沖縄県"}

var (
	ErrAuthentication   = errors.New("Authentication error.")
	ErrPermissionDenied = errors.New("Permission denied.")
	ErrContentNotFound  = errors.New("Content not found.")
)

// ===== Redis Seed Start =====
func AddFootprintCache(footprint Footprint) {
	fps, err := redis.Values(redisConn.Do("ZRANGE", fmt.Sprintf("footprints:user_id:%d", footprint.UserID), 0, -1))
	if err != nil {
		log.Fatalf("Failed to fetch footprint cache footprints:user_id:%d: %s\n", footprint.UserID, err.Error())
	}

	var maxCreatedAt int64 = footprint.CreatedAt.UnixNano()
	for _, fpJson := range fps {
		fp := Footprint{}
		json.Unmarshal(fpJson.([]byte), &fp)

		if fp.OwnerID == footprint.OwnerID && fp.CreatedAt.UnixNano() > maxCreatedAt {
			maxCreatedAt = fp.CreatedAt.UnixNano()
		}
	}
	footprint.CreatedAt, footprint.UpdatedAt = time.Unix(0, maxCreatedAt), time.Unix(maxCreatedAt, 0)

	footprintJson, err := json.Marshal(footprint)
	if err != nil {
		log.Fatalf("Failed to marshalize footprint: <%v> %s\n", footprint.CreatedAt, err.Error())
	}
	redisConn.Do("ZADD", fmt.Sprintf("footprints:user_id:%d", footprint.UserID), -maxCreatedAt, footprintJson)
}

func FetchFootprintsCache(userId int, limit int) (footprints []Footprint) {
	fps, err := redis.Values(redisConn.Do("ZRANGE", fmt.Sprintf("footprints:user_id:%d", userId), 0, limit-1))
	if err != nil {
		log.Fatalf("Can not fetch data from cache: %s.", err.Error())
	}

	for _, fpJson := range fps {
		fp := Footprint{}
		json.Unmarshal(fpJson.([]byte), &fp)

		// Datetime -> Dateに変換
		jst := time.FixedZone("Asia/Tokyo", 9*60*60)
		fp.CreatedAt = time.Date(
			fp.CreatedAt.Year(),
			fp.CreatedAt.Month(),
			fp.CreatedAt.Day(),
			0, 0, 0, 0,
			jst,
		)
		footprints = append(footprints, fp)
	}

	return
}

func LoadRedisAof() {
	if _, err := os.Stat("/var/lib/redis/appendonly.aof"); err == nil {
		loadErr := exec.Command("sh", "-c", "redis-cli --pipe < /var/lib/redis/appendonly.aof").Run()
		if loadErr != nil {
			log.Fatalf("Failed to load aof: %s\n", loadErr.Error())
		}
	} else {
		log.Fatalf("appendonly.aof isn't exists.: %s", err.Error())
	}
}

// ===== Redis Seed End =====

func authenticate(w http.ResponseWriter, r *http.Request, email, passwd string) {
	var user User
	for _, user = range users {
		if user.Email == email {
			break
		}
	}

	if user.Email == "" {
		checkErr(ErrAuthentication)
	}

	salt, _ := salts[user.ID]
	hash := fmt.Sprintf("%x", sha512.Sum512([]byte(fmt.Sprintf("%s%s", passwd, salt))))

	if hash != user.PassHash {
		checkErr(ErrAuthentication)
	}

	session := getSession(w, r)
	session.Values["user_id"] = user.ID
	session.Save(r, w)
}

func getCurrentUser(w http.ResponseWriter, r *http.Request) *User {
	u := context.Get(r, "user")
	if u != nil {
		user := u.(User)
		return &user
	}
	session := getSession(w, r)
	userID, ok := session.Values["user_id"]
	if !ok || userID == nil {
		return nil
	}

	user, _ := users[userID.(int)]
	context.Set(r, "user", user)
	return &user
}

func authenticated(w http.ResponseWriter, r *http.Request) bool {
	user := getCurrentUser(w, r)
	if user == nil {
		http.Redirect(w, r, "/login", http.StatusFound)
		return false
	}
	return true
}

func getUser(w http.ResponseWriter, userID int) *User {
	user, ok := users[userID]
	if ok != true {
		log.Fatalf("Cannot get user object from memory (userID:%d\n)", userID)
	}
	return &user
}

func getUserFromAccount(w http.ResponseWriter, name string) *User {
	for _, user := range users {
		if user.AccountName == name {
			return &user
		}
	}
	user := User{}
	return &user
}

func checkFriendFromSlice(friends []int, id int) bool {
	index := sort.SearchInts(friends, id)
	return index < len(friends) && friends[index] == id
}

func isFriend(w http.ResponseWriter, r *http.Request, anotherID int) bool {
	session := getSession(w, r)
	id := session.Values["user_id"]
	row := db.QueryRow(`SELECT COUNT(1) AS cnt FROM relations WHERE (one = ? AND another = ?)`, id, anotherID)
	cnt := new(int)
	err := row.Scan(cnt)
	checkErr(err)
	return *cnt > 0
}

func isFriendAccount(w http.ResponseWriter, r *http.Request, name string) bool {
	user := getUserFromAccount(w, name)
	if user == nil {
		return false
	}
	return isFriend(w, r, user.ID)
}

func permitted(w http.ResponseWriter, r *http.Request, anotherID int) bool {
	user := getCurrentUser(w, r)
	if anotherID == user.ID {
		return true
	}
	return isFriend(w, r, anotherID)
}

func markFootprint(w http.ResponseWriter, r *http.Request, id int) {
	user := getCurrentUser(w, r)
	if user.ID != id {
		_, err := db.Exec(`INSERT INTO footprints (user_id,owner_id) VALUES (?,?)`, id, user.ID)
		checkErr(err)

		// FIXME: ここでもう１回DBから取得しなくてはならない.
		//        できればこの処理をしたくない
		var footprint Footprint
		err = db.QueryRow(`SELECT user_id, owner_id, created_at FROM footprints WHERE user_id = ? AND owner_id = ?`, id, user.ID).Scan(
			&footprint.UserID,
			&footprint.OwnerID,
			&footprint.CreatedAt,
		)
		checkErr(err)
		footprint.UpdatedAt = footprint.CreatedAt
		AddFootprintCache(footprint)
	}
}

func myHandler(fn func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			rcv := recover()
			if rcv != nil {
				switch {
				case rcv == ErrAuthentication:
					session := getSession(w, r)
					delete(session.Values, "user_id")
					session.Save(r, w)
					render(w, r, http.StatusUnauthorized, "login.html", struct{ Message string }{"ログインに失敗しました"})
					return
				case rcv == ErrPermissionDenied:
					render(w, r, http.StatusForbidden, "error.html", struct{ Message string }{"友人のみしかアクセスできません"})
					return
				case rcv == ErrContentNotFound:
					render(w, r, http.StatusNotFound, "error.html", struct{ Message string }{"要求されたコンテンツは存在しません"})
					return
				default:
					var msg string
					if e, ok := rcv.(runtime.Error); ok {
						msg = e.Error()
					}
					if s, ok := rcv.(string); ok {
						msg = s
					}
					msg = rcv.(error).Error()
					http.Error(w, msg, http.StatusInternalServerError)
				}
			}
		}()
		fn(w, r)
	}
}

func getSession(w http.ResponseWriter, r *http.Request) *sessions.Session {
	session, _ := store.Get(r, "isucon5q-go.session")
	return session
}

func getTemplatePath(file string) string {
	return path.Join("templates", file)
}

func render(w http.ResponseWriter, r *http.Request, status int, file string, data interface{}) {
	fmap := template.FuncMap{
		"getUser": func(id int) *User {
			return getUser(w, id)
		},
		"getCurrentUser": func() *User {
			return getCurrentUser(w, r)
		},
		"isFriend": func(id int) bool {
			return isFriend(w, r, id)
		},
		"prefectures": func() []string {
			return prefs
		},
		"substring": func(s string, l int) string {
			if len(s) > l {
				return s[:l]
			}
			return s
		},
		"split": strings.Split,
		"getEntry": func(id int) Entry {
			row := db.QueryRow(`SELECT * FROM entries WHERE id=?`, id)
			var entryID, userID, private int
			var body string
			var createdAt time.Time
			var title string
			checkErr(row.Scan(&entryID, &userID, &private, &body, &createdAt, &title))
			return Entry{id, userID, private == 1, title, body, createdAt}
		},
		"numComments": func(id int) int {
			row := db.QueryRow(`SELECT COUNT(*) AS c FROM comments WHERE entry_id = ?`, id)
			var n int
			checkErr(row.Scan(&n))
			return n
		},
	}
	tpl := template.Must(template.New(file).Funcs(fmap).ParseFiles(getTemplatePath(file), getTemplatePath("header.html")))
	w.WriteHeader(status)
	checkErr(tpl.Execute(w, data))
}

func GetLogin(w http.ResponseWriter, r *http.Request) {
	render(w, r, http.StatusOK, "login.html", struct{ Message string }{"高負荷に耐えられるSNSコミュニティサイトへようこそ!"})
}

func PostLogin(w http.ResponseWriter, r *http.Request) {
	email := r.FormValue("email")
	passwd := r.FormValue("password")
	authenticate(w, r, email, passwd)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func GetLogout(w http.ResponseWriter, r *http.Request) {
	session := getSession(w, r)
	delete(session.Values, "user_id")
	session.Options = &sessions.Options{MaxAge: -1}
	session.Save(r, w)
	http.Redirect(w, r, "/login", http.StatusFound)
}

func GetIndex(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	user := getCurrentUser(w, r)

	prof := Profile{}
	row := db.QueryRow(`SELECT * FROM profiles WHERE user_id = ?`, user.ID)
	err := row.Scan(&prof.UserID, &prof.FirstName, &prof.LastName, &prof.Sex, &prof.Birthday, &prof.Pref, &prof.UpdatedAt)
	if err != sql.ErrNoRows {
		checkErr(err)
	}

	rows, err := db.Query(`SELECT * FROM entries WHERE user_id = ? ORDER BY created_at LIMIT 5`, user.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	entries := make([]Entry, 0, 5)
	for rows.Next() {
		var id, userID, private int
		var body string
		var createdAt time.Time
		var title string
		checkErr(rows.Scan(&id, &userID, &private, &body, &createdAt, &title))
		entries = append(entries, Entry{id, userID, private == 1, title, body, createdAt})
	}
	rows.Close()

	rows, err = db.Query(`SELECT c.id AS id, c.entry_id AS entry_id, c.user_id AS user_id, c.comment AS comment, c.created_at AS created_at
FROM comments c
JOIN entries e ON c.entry_id = e.id
WHERE e.user_id = ?
ORDER BY c.created_at DESC
LIMIT 10`, user.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	commentsForMe := make([]Comment, 0, 10)
	for rows.Next() {
		c := Comment{}
		checkErr(rows.Scan(&c.ID, &c.EntryID, &c.UserID, &c.Comment, &c.CreatedAt))
		commentsForMe = append(commentsForMe, c)
	}
	rows.Close()

	rows, err = db.Query(`SELECT * FROM relations WHERE one = ? OR another = ? ORDER BY created_at DESC`, user.ID, user.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	friendsMap := make(map[int]time.Time)
	for rows.Next() {
		var id, one, another int
		var createdAt time.Time
		checkErr(rows.Scan(&id, &one, &another, &createdAt))
		var friendID int
		if one == user.ID {
			friendID = another
		} else {
			friendID = one
		}
		if _, ok := friendsMap[friendID]; !ok {
			friendsMap[friendID] = createdAt
		}
	}

	row = db.QueryRow(`SELECT COUNT(*) AS friendCnt FROM relations WHERE one = ?`, user.ID)
	var friendsCnt int
	checkErr(row.Scan(&friendsCnt))

	friendIds := make([]int, 0, len(friendsMap))
	for key := range friendsMap {
		friendIds = append(friendIds, key)
	}
	rows.Close()

	sort.Ints(friendIds)

	rows, err = db.Query(`SELECT * FROM entries ORDER BY created_at DESC LIMIT 1000`)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	entriesOfFriends := make([]Entry, 0, 10)
	for rows.Next() {
		var id, userID, private int
		var body string
		var createdAt time.Time
		var title string
		checkErr(rows.Scan(&id, &userID, &private, &body, &createdAt, &title))
		if !checkFriendFromSlice(friendIds, userID) {
			continue
		}
		entriesOfFriends = append(entriesOfFriends, Entry{id, userID, private == 1, title, body, createdAt})
		if len(entriesOfFriends) >= 10 {
			break
		}
	}
	rows.Close()

	rows, err = db.Query(`SELECT * FROM comments ORDER BY created_at DESC LIMIT 1000`)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	commentsOfFriends := make([]Comment, 0, 10)
	for rows.Next() {
		c := Comment{}
		checkErr(rows.Scan(&c.ID, &c.EntryID, &c.UserID, &c.Comment, &c.CreatedAt))
		if !checkFriendFromSlice(friendIds, c.UserID) {
			continue
		}
		row := db.QueryRow(`SELECT * FROM entries WHERE id = ?`, c.EntryID)
		var id, userID, private int
		var body string
		var createdAt time.Time
		var title string
		checkErr(row.Scan(&id, &userID, &private, &body, &createdAt, &title))
		entry := Entry{id, userID, private == 1, title, body, createdAt}
		if entry.Private {
			if !permitted(w, r, entry.UserID) {
				continue
			}
		}
		commentsOfFriends = append(commentsOfFriends, c)
		if len(commentsOfFriends) >= 10 {
			break
		}
	}
	rows.Close()

	footprints := FetchFootprintsCache(user.ID, 10)

	render(w, r, http.StatusOK, "index.html", struct {
		User              User
		Profile           Profile
		Entries           []Entry
		CommentsForMe     []Comment
		EntriesOfFriends  []Entry
		CommentsOfFriends []Comment
		FriendsCnt        int
		Footprints        []Footprint
	}{
		*user, prof, entries, commentsForMe, entriesOfFriends, commentsOfFriends, friendsCnt, footprints,
	})
}

func GetProfile(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	account := mux.Vars(r)["account_name"]
	owner := getUserFromAccount(w, account)
	row := db.QueryRow(`SELECT * FROM profiles WHERE user_id = ?`, owner.ID)
	prof := Profile{}
	err := row.Scan(&prof.UserID, &prof.FirstName, &prof.LastName, &prof.Sex, &prof.Birthday, &prof.Pref, &prof.UpdatedAt)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	var query string
	if permitted(w, r, owner.ID) {
		query = `SELECT * FROM entries WHERE user_id = ? ORDER BY created_at LIMIT 5`
	} else {
		query = `SELECT * FROM entries WHERE user_id = ? AND private=0 ORDER BY created_at LIMIT 5`
	}
	rows, err := db.Query(query, owner.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	entries := make([]Entry, 0, 5)
	for rows.Next() {
		var id, userID, private int
		var body string
		var createdAt time.Time
		var title string
		checkErr(rows.Scan(&id, &userID, &private, &body, &createdAt, &title))
		entry := Entry{id, userID, private == 1, title, body, createdAt}
		entries = append(entries, entry)
	}
	rows.Close()

	markFootprint(w, r, owner.ID)

	render(w, r, http.StatusOK, "profile.html", struct {
		Owner   User
		Profile Profile
		Entries []Entry
		Private bool
	}{
		*owner, prof, entries, permitted(w, r, owner.ID),
	})
}

func PostProfile(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}
	user := getCurrentUser(w, r)
	account := mux.Vars(r)["account_name"]
	if account != user.AccountName {
		checkErr(ErrPermissionDenied)
	}
	query := `UPDATE profiles
SET first_name=?, last_name=?, sex=?, birthday=?, pref=?, updated_at=CURRENT_TIMESTAMP()
WHERE user_id = ?`
	birth := r.FormValue("birthday")
	firstName := r.FormValue("first_name")
	lastName := r.FormValue("last_name")
	sex := r.FormValue("sex")
	pref := r.FormValue("pref")
	_, err := db.Exec(query, firstName, lastName, sex, birth, pref, user.ID)
	checkErr(err)
	// TODO should escape the account name?
	http.Redirect(w, r, "/profile/"+account, http.StatusSeeOther)
}

func ListEntries(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	account := mux.Vars(r)["account_name"]
	owner := getUserFromAccount(w, account)
	var query string
	if permitted(w, r, owner.ID) {
		query = `SELECT * FROM entries WHERE user_id = ? ORDER BY created_at DESC LIMIT 20`
	} else {
		query = `SELECT * FROM entries WHERE user_id = ? AND private=0 ORDER BY created_at DESC LIMIT 20`
	}
	rows, err := db.Query(query, owner.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	entries := make([]Entry, 0, 20)
	for rows.Next() {
		var id, userID, private int
		var body string
		var createdAt time.Time
		var title string
		checkErr(rows.Scan(&id, &userID, &private, &body, &createdAt, &title))
		entry := Entry{id, userID, private == 1, title, body, createdAt}
		entries = append(entries, entry)
	}
	rows.Close()

	markFootprint(w, r, owner.ID)

	render(w, r, http.StatusOK, "entries.html", struct {
		Owner   *User
		Entries []Entry
		Myself  bool
	}{owner, entries, getCurrentUser(w, r).ID == owner.ID})
}

func GetEntry(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}
	entryID := mux.Vars(r)["entry_id"]
	row := db.QueryRow(`SELECT * FROM entries WHERE id = ?`, entryID)
	var id, userID, private int
	var body string
	var createdAt time.Time
	var title string
	err := row.Scan(&id, &userID, &private, &body, &createdAt, &title)
	if err == sql.ErrNoRows {
		checkErr(ErrContentNotFound)
	}
	checkErr(err)
	entry := Entry{id, userID, private == 1, title, body, createdAt}
	owner := getUser(w, entry.UserID)
	if entry.Private {
		if !permitted(w, r, owner.ID) {
			checkErr(ErrPermissionDenied)
		}
	}
	rows, err := db.Query(`SELECT * FROM comments WHERE entry_id = ?`, entry.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	comments := make([]Comment, 0, 10)
	for rows.Next() {
		c := Comment{}
		checkErr(rows.Scan(&c.ID, &c.EntryID, &c.UserID, &c.Comment, &c.CreatedAt))
		comments = append(comments, c)
	}
	rows.Close()

	markFootprint(w, r, owner.ID)

	render(w, r, http.StatusOK, "entry.html", struct {
		Owner    *User
		Entry    Entry
		Comments []Comment
	}{owner, entry, comments})
}

func PostEntry(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	user := getCurrentUser(w, r)
	title := r.FormValue("title")
	if title == "" {
		title = "タイトルなし"
	}
	content := r.FormValue("content")
	var private int
	if r.FormValue("private") == "" {
		private = 0
	} else {
		private = 1
	}
	_, err := db.Exec(`INSERT INTO entries (user_id, private, body, title) VALUES (?,?,?,?)`, user.ID, private, content, title)
	checkErr(err)
	http.Redirect(w, r, "/diary/entries/"+user.AccountName, http.StatusSeeOther)
}

func PostComment(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	entryID := mux.Vars(r)["entry_id"]
	row := db.QueryRow(`SELECT * FROM entries WHERE id = ?`, entryID)
	var id, userID, private int
	var body string
	var createdAt time.Time
	var title string
	err := row.Scan(&id, &userID, &private, &body, &createdAt, &title)
	if err == sql.ErrNoRows {
		checkErr(ErrContentNotFound)
	}
	checkErr(err)

	entry := Entry{id, userID, private == 1, title, body, createdAt}
	owner := getUser(w, entry.UserID)
	if entry.Private {
		if !permitted(w, r, owner.ID) {
			checkErr(ErrPermissionDenied)
		}
	}
	user := getCurrentUser(w, r)

	_, err = db.Exec(`INSERT INTO comments (entry_id, user_id, comment) VALUES (?,?,?)`, entry.ID, user.ID, r.FormValue("comment"))
	checkErr(err)
	http.Redirect(w, r, "/diary/entry/"+strconv.Itoa(entry.ID), http.StatusSeeOther)
}

func GetFootprints(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	user := getCurrentUser(w, r)
	footprints := FetchFootprintsCache(user.ID, 50)

	render(w, r, http.StatusOK, "footprints.html", struct{ Footprints []Footprint }{footprints})
}
func GetFriends(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	user := getCurrentUser(w, r)
	rows, err := db.Query(`SELECT * FROM relations WHERE one = ? OR another = ? ORDER BY created_at DESC`, user.ID, user.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	friendsMap := make(map[int]time.Time)
	for rows.Next() {
		var id, one, another int
		var createdAt time.Time
		checkErr(rows.Scan(&id, &one, &another, &createdAt))
		var friendID int
		if one == user.ID {
			friendID = another
		} else {
			friendID = one
		}
		if _, ok := friendsMap[friendID]; !ok {
			friendsMap[friendID] = createdAt
		}
	}
	rows.Close()
	friends := make([]Friend, 0, len(friendsMap))
	for key, val := range friendsMap {
		friends = append(friends, Friend{key, val})
	}
	render(w, r, http.StatusOK, "friends.html", struct{ Friends []Friend }{friends})
}

func PostFriends(w http.ResponseWriter, r *http.Request) {
	if !authenticated(w, r) {
		return
	}

	user := getCurrentUser(w, r)
	anotherAccount := mux.Vars(r)["account_name"]
	if !isFriendAccount(w, r, anotherAccount) {
		another := getUserFromAccount(w, anotherAccount)
		_, err := db.Exec(`INSERT INTO relations (one, another) VALUES (?,?), (?,?)`, user.ID, another.ID, another.ID, user.ID)
		checkErr(err)
		http.Redirect(w, r, "/friends", http.StatusSeeOther)
	}
}

func GetInitialize(w http.ResponseWriter, r *http.Request) {
	db.Exec("DELETE FROM relations WHERE id > 500000")
	db.Exec("DELETE FROM footprints WHERE id > 500000")
	db.Exec("DELETE FROM entries WHERE id > 500000")
	db.Exec("DELETE FROM comments WHERE id > 1500000")

	loadAof := os.Getenv("ISUCON5_REDIS_LOAD_AOF")
	if loadAof == "" {
		LoadRedisAof()
	}

	rows, _ := db.Query(`SELECT * FROM users`)
	users = map[int]User{}
	for rows.Next() {
		u := User{}
		checkErr(rows.Scan(&u.ID, &u.AccountName, &u.NickName, &u.Email, &u.PassHash))
		users[u.ID] = u
	}
	rows.Close()

	rows, _ = db.Query(`SELECT * FROM salts`)
	salts = map[int]string{}
	for rows.Next() {
		var id int
		var s string
		checkErr(rows.Scan(&id, &s))
		salts[id] = s
	}
	rows.Close()
}

func main() {
	host := os.Getenv("ISUCON5_DB_HOST")
	if host == "" {
		host = "localhost"
	}
	portstr := os.Getenv("ISUCON5_DB_PORT")
	if portstr == "" {
		portstr = "3306"
	}
	port, err := strconv.Atoi(portstr)
	if err != nil {
		log.Fatalf("Failed to read DB port number from an environment variable ISUCON5_DB_PORT.\nError: %s", err.Error())
	}
	user := os.Getenv("ISUCON5_DB_USER")
	if user == "" {
		user = "root"
	}
	password := os.Getenv("ISUCON5_DB_PASSWORD")
	dbname := os.Getenv("ISUCON5_DB_NAME")
	if dbname == "" {
		dbname = "isucon5q"
	}
	ssecret := os.Getenv("ISUCON5_SESSION_SECRET")
	if ssecret == "" {
		ssecret = "beermoris"
	}
	dbUseTcp := os.Getenv("ISUCON5_DB_USE_TCP")
	if dbUseTcp == "" {
		db, err = sql.Open("mysql", user+":"+password+"@unix(/var/run/mysqld/mysqld.sock)/"+dbname+"?loc=Local&parseTime=true")
		if err != nil {
			log.Fatalf("Failed to connect to DB with Unix domain socket: %s.", err.Error())
		}
	} else {
		db, err = sql.Open("mysql", user+":"+password+"@tcp("+host+":"+strconv.Itoa(port)+")/"+dbname+"?loc=Local&parseTime=true")
		if err != nil {
			log.Fatalf("Failed to connect to DB with TCP: %s.", err.Error())
		}
	}
	defer db.Close()

	redisHost := os.Getenv("ISUCON5_REDIS_HOST")
	if redisHost == "" {
		redisHost = "localhost"
	}

	redisUseTcp := os.Getenv("ISUCON5_REDIS_USE_TCP")
	if redisUseTcp == "" {
		redisConn, err = redis.Dial("unix", "/var/run/redis/redis.sock")
		if err != nil {
			log.Fatalf("Failed to connect to Redis with Unix domain socket: %s.", err.Error())
		}
	} else {
		redisConn, err = redis.Dial("tcp", fmt.Sprintf("%v:6379", redisHost))
		if err != nil {
			log.Fatalf("Failed to connect to Redis with TCP: %s.", err.Error())
		}
	}
	defer redisConn.Close()

	store = sessions.NewCookieStore([]byte(ssecret))

	r := mux.NewRouter()

	l := r.Path("/login").Subrouter()
	l.Methods("GET").HandlerFunc(myHandler(GetLogin))
	l.Methods("POST").HandlerFunc(myHandler(PostLogin))
	r.Path("/logout").Methods("GET").HandlerFunc(myHandler(GetLogout))

	p := r.Path("/profile/{account_name}").Subrouter()
	p.Methods("GET").HandlerFunc(myHandler(GetProfile))
	p.Methods("POST").HandlerFunc(myHandler(PostProfile))

	d := r.PathPrefix("/diary").Subrouter()
	d.HandleFunc("/entries/{account_name}", myHandler(ListEntries)).Methods("GET")
	d.HandleFunc("/entry", myHandler(PostEntry)).Methods("POST")
	d.HandleFunc("/entry/{entry_id}", myHandler(GetEntry)).Methods("GET")

	d.HandleFunc("/comment/{entry_id}", myHandler(PostComment)).Methods("POST")

	r.HandleFunc("/footprints", myHandler(GetFootprints)).Methods("GET")

	r.HandleFunc("/friends", myHandler(GetFriends)).Methods("GET")
	r.HandleFunc("/friends/{account_name}", myHandler(PostFriends)).Methods("POST")

	r.HandleFunc("/initialize", myHandler(GetInitialize))
	r.HandleFunc("/", myHandler(GetIndex))

	go func() {
		log.Println(http.ListenAndServe("localhost:6060", nil))
	}()

	log.Fatal(http.ListenAndServe(":8080", r))
}

func checkErr(err error) {
	if err != nil {
		panic(err)
	}
}
