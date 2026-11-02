// isureal-datagen は ISUReal の初期データを PostgreSQL に作り、ベンチが検証に使う manifest.json を書く。
//
// 本番の初期データ (利用者 5.6k、投稿 63 万、通知 185 万、画像 16 GB) と同じ構造を、
// 手元で作れる大きさで用意する。本番の初期ユーザーの SecretKey は分からないので、
// 手元の環境ではこのツールが作った利用者 (鍵を manifest に残す) で公式ベンチの流れを再現する。
//
//	isureal-datagen -dsn postgres://isureal:isureal@127.0.0.1:15432/isureal -schema ../app/webapp/sql -manifest manifest.json
package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	mrand "math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"isureal-bench/internal/jpegs"
	"isureal-bench/internal/manifest"
)

type user struct {
	m        manifest.User
	created  time.Time
	profile  string // image id
	friends  map[string]bool
	windowIx []int // 投稿した window のインデックス
}

type window struct {
	id        string
	region    string
	localDate time.Time
	startsAt  time.Time
	deadline  time.Time
	endedAt   time.Time
}

type post struct {
	id, author, windowID, imageID, caption, visibility, locKind string
	lat, lng                                                    *float64
	locality                                                    *string
	postedAt                                                    time.Time
	realmojis, comments                                         int
}

func uuid4() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

func secretKey() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:])
}

func sha256hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func parseMix(s string) map[string]float64 {
	out := map[string]float64{}
	for _, kv := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(kv), "=")
		if !ok {
			continue
		}
		var f float64
		fmt.Sscanf(v, "%g", &f)
		out[k] = f
	}
	return out
}

func parseCounts(s string) map[string]int {
	out := map[string]int{}
	for k, v := range parseMix(s) {
		out[k] = int(v)
	}
	return out
}

var regionOffset = map[string]time.Duration{"asia": 9 * time.Hour, "europe": 1 * time.Hour, "america": -5 * time.Hour}
var regions = []string{"asia", "europe", "america"}

var captions = []string{"今日の椅子", "朝ごはん", "散歩中", "office chair", "my favorite seat", "Bonjour!", "Guten Morgen", "coffee time", "", "雨の日", "sunset", "work from home"}
var localities = []string{"Tokyo", "Osaka", "Seoul", "Sydney", "Paris", "Berlin", "London", "New York", "Toronto", "Mexico City"}
var bios = []string{"", "椅子が好きです", "Photographer", "Hello!", "coffee & chairs", "🪑🪑🪑", "Travel, food, chairs"}
var reactions = []string{"like", "smile", "surprise", "heart", "laugh", "instant"}
var commentBodies = []string{"いいね", "nice!", "素敵な椅子", "Where is this?", "👍", "Magnifique", "lol", "I want this chair"}
var familyNames = []string{"佐藤", "鈴木", "高橋", "田中", "伊藤", "Smith", "Johnson", "Müller", "Dubois", "Rossi", "García", "Brown"}
var givenNames = []string{"悠真", "さくら", "陽翔", "結衣", "Alex", "Emma", "Noah", "Mia", "Lukas", "Marie", "Sofia", "Liam"}

func main() {
	dsn := flag.String("dsn", "postgres://isureal:isureal@127.0.0.1:15432/isureal?sslmode=disable", "PostgreSQL の接続先")
	schemaDir := flag.String("schema", "../app/webapp/sql", "001-schema.sql と 003-initial-snapshot.sql のあるディレクトリ")
	manifestPath := flag.String("manifest", "manifest.json", "書き出す manifest")
	nUsers := flag.Int("users", 600, "利用者数")
	days := flag.Int("days", 60, "各リージョンの投稿期間の日数")
	endDate := flag.String("end-date", "2026-10-27", "初期データの最後の投稿期間の localDate")
	seed := flag.Uint64("seed", 20261031, "乱数の種")
	mix := flag.String("region-mix", "asia=0.55,europe=0.25,america=0.20", "リージョンの比率")
	seedUsers := flag.String("seed-users", "asia=18,europe=7,america=6", "日 1 の負荷に使う利用者数 (manifest の seed)")
	avgFriends := flag.Int("friends", 12, "1 人あたりの友達数の目安")
	postRate := flag.Float64("post-rate", 0.6, "投稿期間ごとに投稿する確率")
	imageBytes := flag.Int("image-bytes", 0, "投稿画像の大きさの目安 (0 なら最小の JPEG)")
	certifiedRate := flag.Float64("certified", 0.01, "認定公開アカウントの割合")
	skipSchema := flag.Bool("skip-schema", false, "001-schema.sql を流さない (表が既にある)")
	flag.Parse()

	ctx := context.Background()
	rng := mrand.New(mrand.NewPCG(*seed, *seed^0xdeadbeef))
	start := time.Now()

	conn, err := pgx.Connect(ctx, *dsn)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	if !*skipSchema {
		schema, err := os.ReadFile(filepath.Join(*schemaDir, "001-schema.sql"))
		if err != nil {
			log.Fatal(err)
		}
		if _, err := conn.Exec(ctx, string(schema)); err != nil {
			log.Fatalf("001-schema.sql: %v", err)
		}
		// 以前の datagen / initialize が作った表も消す
		if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS initial_data_boundary, initial_users, initial_friend_requests, initial_friendships, initial_follows, initial_block_relations, user_search_norms, user_search_dirty CASCADE`); err != nil {
			log.Fatalf("drop leftovers: %v", err)
		}
		log.Printf("schema created")
	}

	end, err := time.Parse("2006-01-02", *endDate)
	if err != nil {
		log.Fatal(err)
	}
	baseCreated := end.AddDate(0, 0, -*days-30)

	// ---- 利用者 ----
	regionMix := parseMix(*mix)
	pickRegion := func() string {
		r := rng.Float64()
		acc := 0.0
		for _, reg := range regions {
			acc += regionMix[reg]
			if r < acc {
				return reg
			}
		}
		return "asia"
	}
	users := make([]*user, 0, *nUsers)
	byRegion := map[string][]*user{}
	usernames := map[string]bool{}
	for i := 0; i < *nUsers; i++ {
		region := pickRegion()
		var username string
		for {
			username = fmt.Sprintf("%s_%s_%d", strings.ToLower(romaji(rng)), strings.ToLower(romaji(rng)), rng.IntN(1000))
			if !usernames[username] {
				usernames[username] = true
				break
			}
		}
		key := secretKey()
		accountType := "regular"
		if rng.Float64() < *certifiedRate {
			accountType = "certified_public"
		}
		u := &user{
			m: manifest.User{
				ID: uuid4(), Username: username, SecretKey: key,
				DisplayName: fmt.Sprintf("%s %s %d", familyNames[rng.IntN(len(familyNames))], givenNames[rng.IntN(len(givenNames))], rng.IntN(1000)),
				Bio:         bios[rng.IntN(len(bios))], Region: region, AccountType: accountType,
				Friends: []string{}, Follows: []string{}, Blocks: []string{}, Posts: []manifest.Post{},
			},
			created: baseCreated.Add(time.Duration(rng.Int64N(int64(30 * 24 * time.Hour)))),
			friends: map[string]bool{},
		}
		users = append(users, u)
		byRegion[region] = append(byRegion[region], u)
	}
	log.Printf("users: %d (asia %d, europe %d, america %d)", len(users), len(byRegion["asia"]), len(byRegion["europe"]), len(byRegion["america"]))

	// プロフィール画像 (30%)
	type imageRow struct {
		id, owner string
		data      []byte
		created   time.Time
	}
	var images []imageRow
	var profileSHA = map[string]string{}
	mkImage := func(w, h int) *jpegs.Image {
		if *imageBytes <= 0 {
			return jpegs.Tiny(rng)
		}
		return jpegs.GenerateSized(rng, w, h, *imageBytes)
	}
	for _, u := range users {
		if rng.Float64() < 0.3 {
			img := mkImage(256, 256)
			u.profile = uuid4()
			images = append(images, imageRow{u.profile, u.m.ID, img.Data, u.created.Add(time.Hour)})
			profileSHA[u.m.ID] = hex.EncodeToString(img.SHA256[:])
			u.m.ProfileImageSHA256 = profileSHA[u.m.ID]
		}
	}

	// ---- 友達・申請・フォロー・ブロック ----
	type friendship struct {
		id, low, high, req string
		at                 time.Time
	}
	type request struct {
		id, from, to, status string
		at                   time.Time
	}
	var friendships []friendship
	var requests []request
	pair := func(a, b *user) {
		if a == b || a.friends[b.m.ID] {
			return
		}
		a.friends[b.m.ID] = true
		b.friends[a.m.ID] = true
		low, high := a.m.ID, b.m.ID
		if high < low {
			low, high = high, low
		}
		at := maxTime(a.created, b.created).Add(time.Duration(rng.Int64N(int64(72 * time.Hour))))
		req := request{uuid4(), a.m.ID, b.m.ID, "accepted", at}
		requests = append(requests, req)
		friendships = append(friendships, friendship{uuid4(), low, high, req.id, at.Add(time.Minute)})
		a.m.Friends = append(a.m.Friends, b.m.ID)
		b.m.Friends = append(b.m.Friends, a.m.ID)
	}
	for _, u := range users {
		want := *avgFriends/2 + rng.IntN(*avgFriends+1)
		for k := 0; k < want; k++ {
			pool := byRegion[u.m.Region]
			if rng.Float64() < 0.3 {
				pool = users
			}
			pair(u, pool[rng.IntN(len(pool))])
		}
	}
	// 未処理・拒否された申請
	for i := 0; i < len(users)/10; i++ {
		a, b := users[rng.IntN(len(users))], users[rng.IntN(len(users))]
		if a == b || a.friends[b.m.ID] {
			continue
		}
		status := []string{"pending", "rejected", "cancelled"}[rng.IntN(3)]
		requests = append(requests, request{uuid4(), a.m.ID, b.m.ID, status, end.Add(-time.Duration(rng.IntN(24*30)) * time.Hour)})
	}
	type follow struct {
		id, from, to string
		at           time.Time
	}
	var follows []follow
	var certified []*user
	for _, u := range users {
		if u.m.AccountType == "certified_public" {
			certified = append(certified, u)
		}
	}
	for _, u := range users {
		if len(certified) == 0 {
			break
		}
		seen := map[string]bool{}
		for k := 0; k < rng.IntN(3); k++ {
			c := certified[rng.IntN(len(certified))]
			if c == u || seen[c.m.ID] {
				continue
			}
			seen[c.m.ID] = true
			follows = append(follows, follow{uuid4(), u.m.ID, c.m.ID, u.created.Add(24 * time.Hour)})
			u.m.Follows = append(u.m.Follows, c.m.ID)
		}
	}
	type block struct {
		id, from, to string
		at           time.Time
	}
	var blocks []block
	for i := 0; i < len(users)/100+1; i++ {
		a, b := users[rng.IntN(len(users))], users[rng.IntN(len(users))]
		if a == b || a.friends[b.m.ID] {
			continue
		}
		blocks = append(blocks, block{uuid4(), a.m.ID, b.m.ID, end.Add(-time.Duration(rng.IntN(24*30)) * time.Hour)})
		a.m.Blocks = append(a.m.Blocks, b.m.ID)
	}
	log.Printf("friendships: %d, requests: %d, follows: %d, blocks: %d", len(friendships), len(requests), len(follows), len(blocks))

	// ---- 投稿期間 ----
	var windows []window
	windowsByRegion := map[string][]int{}
	for _, region := range regions {
		for d := *days - 1; d >= 0; d-- {
			local := end.AddDate(0, 0, -d)
			// 現地 09:00 に開く
			startsAt := time.Date(local.Year(), local.Month(), local.Day(), 9, 0, 0, 0, time.UTC).Add(-regionOffset[region])
			w := window{id: uuid4(), region: region, localDate: local, startsAt: startsAt, deadline: startsAt.Add(2 * time.Minute), endedAt: startsAt.Add(24 * time.Hour)}
			windowsByRegion[region] = append(windowsByRegion[region], len(windows))
			windows = append(windows, w)
		}
	}

	// ---- 投稿・RealMoji・コメント・通知 ----
	var posts []post
	type realmoji struct {
		id, post, reactor, image, reaction string
		at                                 time.Time
	}
	type comment struct {
		id, post, author, body string
		at                     time.Time
	}
	var realmojis []realmoji
	var comments []comment
	type notification struct {
		id, user, window string
		at               time.Time
	}
	var notifications []notification
	friendList := func(u *user) []*user {
		out := make([]*user, 0, len(u.friends))
		for _, v := range users {
			if u.friends[v.m.ID] {
				out = append(out, v)
			}
		}
		return out
	}
	friendCache := map[string][]*user{}
	for _, u := range users {
		friendCache[u.m.ID] = friendList(u)
	}
	for wi, w := range windows {
		for _, u := range byRegion[w.region] {
			if u.created.After(w.startsAt) {
				continue
			}
			notifications = append(notifications, notification{uuid4(), u.m.ID, w.id, w.startsAt})
			if rng.Float64() >= *postRate {
				continue
			}
			img := mkImage(1024, 768)
			imageID := uuid4()
			postedAt := w.startsAt.Add(time.Duration(rng.Int64N(int64(10 * time.Minute))))
			images = append(images, imageRow{imageID, u.m.ID, img.Data, postedAt})
			p := post{id: uuid4(), author: u.m.ID, windowID: w.id, imageID: imageID, caption: captions[rng.IntN(len(captions))],
				visibility: []string{"friends", "friends", "friends", "friends_of_friends", "public"}[rng.IntN(5)], postedAt: postedAt}
			switch rng.IntN(3) {
			case 0:
				p.locKind = "none"
			case 1:
				p.locKind = "exact"
				lat, lng := rng.Float64()*180-90, rng.Float64()*360-180
				p.lat, p.lng = &lat, &lng
			default:
				p.locKind = "approximate"
				loc := localities[rng.IntN(len(localities))]
				p.locality = &loc
			}
			friends := friendCache[u.m.ID]
			if len(friends) > 0 {
				for k := 0; k < rng.IntN(4); k++ {
					f := friends[rng.IntN(len(friends))]
					if f.created.After(postedAt) {
						continue
					}
					rimg := mkImage(256, 256)
					rid := uuid4()
					at := postedAt.Add(time.Duration(rng.Int64N(int64(6 * time.Hour))))
					images = append(images, imageRow{rid, f.m.ID, rimg.Data, at})
					realmojis = append(realmojis, realmoji{uuid4(), p.id, f.m.ID, rid, reactions[rng.IntN(len(reactions))], at})
					p.realmojis++
				}
				for k := 0; k < rng.IntN(4); k++ {
					f := friends[rng.IntN(len(friends))]
					if f.created.After(postedAt) {
						continue
					}
					comments = append(comments, comment{uuid4(), p.id, f.m.ID, commentBodies[rng.IntN(len(commentBodies))], postedAt.Add(time.Duration(rng.Int64N(int64(6 * time.Hour))))})
					p.comments++
				}
			}
			posts = append(posts, p)
			u.windowIx = append(u.windowIx, wi)
			u.m.Posts = append(u.m.Posts, manifest.Post{ID: p.id, LocalDate: w.localDate.Format("2006-01-02"), Caption: p.caption, Visibility: p.visibility,
				LocationKind: p.locKind, ImageSHA256: hex.EncodeToString(img.SHA256[:]), PostedAt: postedAt, RealMojiCount: p.realmojis, CommentCount: p.comments})
		}
	}
	// 同じ投稿への同じ人の RealMoji は 1 つだけ (realmojis_one_active)。重複は replaced_at を付ける
	replaced := map[string]bool{}
	seenPair := map[string]int{}
	for i := range realmojis {
		key := realmojis[i].post + "/" + realmojis[i].reactor
		if j, ok := seenPair[key]; ok {
			replaced[realmojis[j].id] = true
		}
		seenPair[key] = i
	}
	// manifest の件数も重複分を引く
	activeByPost := map[string]int{}
	for _, r := range realmojis {
		if !replaced[r.id] {
			activeByPost[r.post]++
		}
	}
	for _, u := range users {
		for i := range u.m.Posts {
			u.m.Posts[i].RealMojiCount = activeByPost[u.m.Posts[i].ID]
		}
	}
	log.Printf("windows: %d, posts: %d, realmojis: %d, comments: %d, notifications: %d, images: %d", len(windows), len(posts), len(realmojis), len(comments), len(notifications), len(images))

	// ---- 書き込み ----
	tx, err := conn.Begin(ctx)
	if err != nil {
		log.Fatal(err)
	}
	defer tx.Rollback(ctx)
	copyRows := func(table string, cols []string, rows [][]any) {
		n, err := tx.CopyFrom(ctx, pgx.Identifier{table}, cols, pgx.CopyFromRows(rows))
		if err != nil {
			log.Fatalf("COPY %s: %v", table, err)
		}
		log.Printf("%s: %d rows", table, n)
	}
	{
		rows := make([][]any, 0, len(users))
		for _, u := range users {
			var profile any
			if u.profile != "" {
				profile = u.profile
			}
			rows = append(rows, []any{u.m.ID, u.m.Username, sha256hex(u.m.SecretKey), u.m.DisplayName, u.m.Bio, profile, u.m.Region, u.m.AccountType, "active", u.created, u.created})
		}
		copyRows("users", []string{"id", "username", "secret_key_hash", "display_name", "bio", "profile_image_id", "region", "account_type", "status", "created_at", "updated_at"}, rows)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO administrators (id, username, password_hash) VALUES ($1, 'admin', $2)`, uuid4(), sha256hex("password1234")); err != nil {
		log.Fatal(err)
	}
	{
		rows := make([][]any, 0, len(images))
		for _, im := range images {
			rows = append(rows, []any{im.id, im.owner, im.data, "image/jpeg", im.created})
		}
		copyRows("image_objects", []string{"id", "owner_user_id", "data", "media_type", "created_at"}, rows)
	}
	{
		rows := make([][]any, 0, len(requests))
		for _, r := range requests {
			rows = append(rows, []any{r.id, r.from, r.to, r.status, r.at, r.at})
		}
		copyRows("friend_requests", []string{"id", "requester_id", "recipient_id", "status", "created_at", "updated_at"}, rows)
		rows = rows[:0]
		for _, f := range friendships {
			rows = append(rows, []any{f.id, f.low, f.high, f.req, f.at, f.at})
		}
		copyRows("friendships", []string{"id", "user_low_id", "user_high_id", "source_request_id", "created_at", "updated_at"}, rows)
		rows = rows[:0]
		for _, f := range follows {
			rows = append(rows, []any{f.id, f.from, f.to, f.at, f.at})
		}
		copyRows("follows", []string{"id", "follower_id", "followee_id", "created_at", "updated_at"}, rows)
		rows = rows[:0]
		for _, b := range blocks {
			rows = append(rows, []any{b.id, b.from, b.to, b.at})
		}
		copyRows("block_relations", []string{"id", "blocker_id", "blocked_id", "created_at"}, rows)
	}
	{
		rows := make([][]any, 0, len(windows))
		for _, w := range windows {
			rows = append(rows, []any{w.id, w.region, w.localDate, w.startsAt, w.deadline, w.endedAt, w.startsAt})
		}
		copyRows("posting_windows", []string{"id", "region", "local_date", "starts_at", "on_time_deadline", "ended_at", "created_at"}, rows)
	}
	{
		rows := make([][]any, 0, len(posts))
		for _, p := range posts {
			var lat, lng, loc any
			if p.lat != nil {
				lat, lng = *p.lat, *p.lng
			}
			if p.locality != nil {
				loc = *p.locality
			}
			rows = append(rows, []any{p.id, p.author, p.windowID, p.imageID, p.caption, p.visibility, p.locKind, lat, lng, loc, p.postedAt, p.postedAt})
		}
		copyRows("posts", []string{"id", "author_id", "posting_window_id", "image_id", "caption", "visibility", "location_kind", "latitude", "longitude", "locality", "posted_at", "created_at"}, rows)
	}
	{
		rows := make([][]any, 0, len(realmojis))
		for _, r := range realmojis {
			var rep any
			if replaced[r.id] {
				rep = r.at.Add(time.Second)
			}
			rows = append(rows, []any{r.id, r.post, r.reactor, r.image, r.reaction, r.at, rep})
		}
		copyRows("realmojis", []string{"id", "post_id", "reactor_id", "image_id", "reaction_type", "created_at", "replaced_at"}, rows)
		rows = rows[:0]
		for _, c := range comments {
			rows = append(rows, []any{c.id, c.post, c.author, c.body, c.at})
		}
		copyRows("comments", []string{"id", "post_id", "author_id", "body", "created_at"}, rows)
		rows = rows[:0]
		for _, n := range notifications {
			rows = append(rows, []any{n.id, n.user, n.window, n.at})
		}
		copyRows("notifications", []string{"id", "user_id", "posting_window_id", "created_at"}, rows)
	}
	if err := tx.Commit(ctx); err != nil {
		log.Fatal(err)
	}
	snapshot, err := os.ReadFile(filepath.Join(*schemaDir, "003-initial-snapshot.sql"))
	if err != nil {
		log.Fatal(err)
	}
	if _, err := conn.Exec(ctx, string(snapshot)); err != nil {
		log.Fatalf("003-initial-snapshot.sql: %v", err)
	}
	if _, err := conn.Exec(ctx, `ANALYZE`); err != nil {
		log.Fatal(err)
	}

	// ---- manifest ----
	seedCounts := parseCounts(*seedUsers)
	man := &manifest.Manifest{Version: 1, GeneratedAt: time.Now(), Seed: *seed, Admin: manifest.Admin{Username: "admin", Password: "password1234"}, Windows: map[string]manifest.Window{}}
	for _, region := range regions {
		man.Windows[region] = manifest.Window{LastLocalDate: end.Format("2006-01-02"), Days: *days}
	}
	for _, region := range regions {
		n := seedCounts[region]
		for _, u := range byRegion[region] {
			if n == 0 {
				break
			}
			if u.m.AccountType == "regular" && len(u.m.Friends) > 0 && len(u.m.Posts) > 0 {
				u.m.Seed = true
				n--
			}
		}
	}
	for _, u := range users {
		man.Users = append(man.Users, u.m)
	}
	if err := man.Save(*manifestPath); err != nil {
		log.Fatal(err)
	}
	log.Printf("manifest written to %s (%s)", *manifestPath, time.Since(start).Round(time.Millisecond))
}

func maxTime(a, b time.Time) time.Time {
	if a.After(b) {
		return a
	}
	return b
}

var syl = []string{"ka", "ki", "ku", "ke", "ko", "sa", "shi", "su", "se", "so", "ta", "chi", "tsu", "te", "to", "na", "ni", "nu", "ne", "no", "ha", "hi", "fu", "he", "ho", "ma", "mi", "mu", "me", "mo", "ya", "yu", "yo", "ra", "ri", "ru", "re", "ro", "wa", "n"}

func romaji(rng *mrand.Rand) string {
	n := 2 + rng.IntN(2)
	var b strings.Builder
	for i := 0; i < n; i++ {
		b.WriteString(syl[rng.IntN(len(syl))])
	}
	return b.String()
}
