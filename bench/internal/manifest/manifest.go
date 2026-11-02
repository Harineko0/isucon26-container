// Package manifest は初期データの「正解」。isureal-datagen が DB と同時に書き出し、
// isureal-bench が初期ユーザーのログイン・Memories・エクスポートの検証に使う。
// 公式ベンチは初期データの期待値を内部に持っている (manual.md「負荷走行の打ち切り」)。
// 本番サーバの初期データの SecretKey は分からないので、本番に対しては manifest 無しで走らせる。
package manifest

import (
	"encoding/json"
	"os"
	"time"
)

type Manifest struct {
	Version     int       `json:"version"`
	GeneratedAt time.Time `json:"generatedAt"`
	Seed        uint64    `json:"seed"`
	Admin       Admin     `json:"admin"`
	// region → 初期データの最後の投稿期間。ベンチはこの翌日から投稿期間を開く
	Windows map[string]Window `json:"windows"`
	Users   []User            `json:"users"`
}

type Admin struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type Window struct {
	LastLocalDate string `json:"lastLocalDate"`
	Days          int    `json:"days"`
}

type User struct {
	ID          string `json:"id"`
	Username    string `json:"username"`
	SecretKey   string `json:"secretKey"`
	DisplayName string `json:"displayName"`
	Bio         string `json:"bio"`
	Region      string `json:"region"`
	AccountType string `json:"accountType"`
	// 日 1 の負荷に使う利用者 (公式は初期データの一部の利用者で始める)
	Seed               bool     `json:"seed"`
	ProfileImageSHA256 string   `json:"profileImageSha256,omitempty"`
	Friends            []string `json:"friends"`
	Follows            []string `json:"follows"`
	Blocks             []string `json:"blocks"`
	Posts              []Post   `json:"posts"`
}

type Post struct {
	ID            string    `json:"id"`
	LocalDate     string    `json:"localDate"`
	Caption       string    `json:"caption"`
	Visibility    string    `json:"visibility"`
	LocationKind  string    `json:"locationKind"`
	ImageSHA256   string    `json:"imageSha256"`
	PostedAt      time.Time `json:"postedAt"`
	RealMojiCount int       `json:"realmojiCount"`
	CommentCount  int       `json:"commentCount"`
}

func Load(path string) (*Manifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

func (m *Manifest) Save(path string) error {
	data, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
