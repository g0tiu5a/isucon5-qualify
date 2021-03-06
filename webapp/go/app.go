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
		redisConn.Do("ZADD", fmt.Sprintf("footprints:user_id:%d", tmpFp.UserID), -tmpFp.CreatedAt.UnixNano(), tmpFpJson)
	}
}

func FetchFootprints(userId int, limit int) (footprints []Footprint) {
	fps, err := redis.Values(redisConn.Do("ZRANGE", fmt.Sprintf("footprints:user_id:%d", userId), 0, limit-1))
	if err != nil {
		log.Fatalf("Can not fetch data from cache: %s.", err.Error())
	}

	for _, fpJson := range fps {
		fp := Footprint{}
		json.Unmarshal(fpJson.([]byte), &fp)
		footprints = append(footprints, fp)
	}

	return
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
	entrie_ids := []string{}
	for rows.Next() {
		var id, userID, private int
		var body string
		var createdAt time.Time
		var title string
		checkErr(rows.Scan(&id, &userID, &private, &body, &createdAt, &title))
		entries = append(entries, Entry{id, userID, private == 1, title, body, createdAt})
		entrie_ids = append(entrie_ids, strconv.Itoa(id))
	}
	rows.Close()

	stmtGetCommentsForMe := `SELECT * FROM comments WHERE entry_id IN (%s)`
	rows, err = db.Query(fmt.Sprintf(stmtGetCommentsForMe, strings.Join(entrie_ids, ",")))
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

	var friendsCnt int
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
			friendsCnt += 1
			friendID = another
		} else {
			friendID = one
		}
		if _, ok := friendsMap[friendID]; !ok {
			friendsMap[friendID] = createdAt
		}
	}

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

	rows, err = db.Query(`SELECT user_id, owner_id, DATE(created_at) AS date, MAX(created_at) AS updated
FROM footprints
WHERE user_id = ?
GROUP BY user_id, owner_id, DATE(created_at)
ORDER BY updated DESC
LIMIT 10`, user.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	footprints := make([]Footprint, 0, 10)
	for rows.Next() {
		fp := Footprint{}
		checkErr(rows.Scan(&fp.UserID, &fp.OwnerID, &fp.CreatedAt, &fp.UpdatedAt))
		footprints = append(footprints, fp)
	}
	rows.Close()

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
	footprints := make([]Footprint, 0, 50)
	rows, err := db.Query(`SELECT user_id, owner_id, DATE(created_at) AS date, MAX(created_at) as updated
FROM footprints
WHERE user_id = ?
GROUP BY user_id, owner_id, DATE(created_at)
ORDER BY updated DESC
LIMIT 50`, user.ID)
	if err != sql.ErrNoRows {
		checkErr(err)
	}
	for rows.Next() {
		fp := Footprint{}
		checkErr(rows.Scan(&fp.UserID, &fp.OwnerID, &fp.CreatedAt, &fp.UpdatedAt))
		footprints = append(footprints, fp)
	}
	rows.Close()
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
	runtime.GOMAXPROCS(32)
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
	if dbUseTcp == "0" {
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
	if redisUseTcp == "0" {
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
