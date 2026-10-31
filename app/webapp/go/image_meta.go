package isureal

import (
	"context"
	"crypto/hmac"
	"database/sql"
	"errors"

	"github.com/jmoiron/sqlx"
)

// imageMeta は画像 id ごとに不変な値 (image_objects.owner_user_id と media_type は UPDATE されない)。
// 本体 (imageDataStore) とこれが手元にあれば、画像取得の可否確認は image_objects を読まずに
// 所有者の users 行だけで済む (imageOwnerQuery)。
//
// image_objects は月別 + ハッシュで約 45 の葉パーティションに分かれていて、汎用プランは PG16 では
// 実行時に刈り込んでも全パーティションと索引をロックする (実測 107 リレーション、fast-path は 16 まで)。
// これが LWLock:LockManager 待ちの原因になっていた。imageOwnerQuery は 17 リレーションで済む。
type imageMeta struct {
	ownerID   string
	mediaType string
}

// imageMetas は画像 id → imageMeta。image id は newTimeID (ランダム部分を含む) で再利用されないので、
// initialize の後も他のリージョンに残った値が別の画像を指すことはない (initialize を受けたサーバーは消す)。
// 1 件 150 バイト程度。上限を超えて捨てても従来の検索に戻るだけ。
var imageMetas = &boundedMap{limit: 2_000_000}

func loadImageMeta(id string) (imageMeta, bool) {
	v, ok := imageMetas.Load(id)
	if !ok {
		return imageMeta{}, false
	}
	return v.(imageMeta), true
}

// imageOwnerQuery は画像取得の検索 (image_api.go) の可否の式と同じ。image_objects の行の代わりに、
// 覚えている所有者 ($2) と画像 id ($1) を使う。所有者が active でなければ行が無い (従来も行が無く 403)。
const imageOwnerQuery = `
   SELECT u.secret_key_hash, COALESCE(
     u.profile_image_id = $1
     OR EXISTS (
       SELECT 1 FROM posts p
       WHERE p.image_id = $1 AND p.author_id = u.id AND p.deleted_at IS NULL
     )
     OR EXISTS (
       SELECT 1 FROM realmojis r
       JOIN posts p ON p.id = r.post_id AND p.deleted_at IS NULL
       JOIN users author ON author.id = p.author_id AND author.status = 'active'
       WHERE r.image_id = $1 AND r.reactor_id = u.id AND r.replaced_at IS NULL AND r.deleted_at IS NULL
     )
   , false) AS allowed
   FROM users u
   WHERE u.id = $2 AND u.status = 'active'`

type imageVerdict int

const (
	imageServe            imageVerdict = iota // 200 で本体を返す
	imageForbidden                            // 403 (abuse フラグ無し)
	imageInvalidSignature                     // 403 + abuse=invalid_signature
	imageFullLookup                           // 判定できないので従来の image_objects の検索に任せる
)

// ownerLookupVerdict は imageOwnerQuery の結果から、従来の検索と同じ応答を決める。
//
// 従来は「画像の行があり所有者が active」のときだけ署名を確かめ、合わなければ abuse フラグ付きの 403。
// imageOwnerQuery は画像の行の有無を見ないが、allowed が真なら行はある (行が消えるのは差し替え・削除後の
// プロフィール画像と initialize で消える画像だけで、どちらも allowed は偽になる)。
// 署名が合わず allowed が偽のときだけは行の有無で abuse フラグが変わるので、従来の検索に任せる。
func ownerLookupVerdict(found, signatureOK, allowed bool) imageVerdict {
	switch {
	case !found:
		return imageForbidden
	case signatureOK && allowed:
		return imageServe
	case signatureOK:
		return imageForbidden
	case allowed:
		return imageInvalidSignature
	default:
		return imageFullLookup
	}
}

// lookupImageByOwner は imageOwnerQuery で可否と署名を確かめる。
func lookupImageByOwner(ctx context.Context, conn *sqlx.Conn, imageID, ownerID string, sig []byte, exp int64) (imageVerdict, error) {
	var row struct {
		SecretKeyHash string `db:"secret_key_hash"`
		Allowed       bool   `db:"allowed"`
	}
	err := conn.GetContext(ctx, &row, imageOwnerQuery, imageID, ownerID)
	if errors.Is(err, sql.ErrNoRows) {
		return ownerLookupVerdict(false, false, false), nil
	}
	if err != nil {
		return 0, err
	}
	key, err := imageSigningKey(row.SecretKeyHash)
	if err != nil {
		return 0, err
	}
	return ownerLookupVerdict(true, hmac.Equal(sig, imageSignature(key, imageID, exp)), row.Allowed), nil
}
