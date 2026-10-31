package isureal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const imageURLLifetime = 24 * time.Hour
const imageSignatureIterations = 1000

// 署名は 1000 回の HMAC で重いので、exp を丸めて (鍵, 画像, exp) ごとに使い回す。
// exp は常に「今 + 24 時間」以上に切り上げるので、URL の有効期間は短くならない。
// 丸めの幅を 10 秒から 30 秒にした: 幅ごとに全画像の署名を計算し直すので、asia1 (SHA 命令なし) では
// フィード 1 回あたり最大 100 署名 × 1000 回の HMAC を 10 秒ごとに払っていた。
// さらに 300 秒にした: 17:14 のベンチ中の asia1 の CPU プロファイルで、署名 (sha256x16 + HMAC) が Go の CPU の
// 約 33% (Memories 28%、エクスポートの一部) を占め、asia1 の CPU が張り付いていた。30 秒ごとに切り替わると
// 約 100 秒のベンチで同じ (鍵, 画像) を 3〜4 回署名し直す。300 秒なら多くても 2 回。URL の期限は 24 時間 + 最大 300 秒
// (短くはならない。署名と期限の形式・検証は変えない)。
const imageURLExpBucket = 300

func imageSigningKey(secretKeyHash string) ([]byte, error) {
	key, err := hex.DecodeString(secretKeyHash)
	if err != nil || len(key) != sha256.Size {
		return nil, fmt.Errorf("invalid image signing key")
	}
	return key, nil
}

func computeImageSignature(key []byte, imageID string, exp int64) []byte {
	// hmac.New を 1 回だけにして Reset で使い回す(鍵の前処理を毎回やり直さない)。結果は同じ。
	mac := hmac.New(sha256.New, key)
	value := []byte(strconv.FormatInt(exp, 10) + ":" + imageID)
	var buf [sha256.Size]byte
	for range imageSignatureIterations {
		mac.Reset()
		mac.Write(value)
		value = mac.Sum(buf[:0])
	}
	return append([]byte(nil), value...)
}

type signatureCacheKey struct {
	key     string
	imageID string
	exp     int64
}

// 署名の結果は鍵・画像・exp だけで決まるので、古い世代を捨てるだけで正しさは保たれる。
type signatureCache struct {
	mu       sync.RWMutex
	current  map[signatureCacheKey][]byte
	previous map[signatureCacheKey][]byte
	rotateAt time.Time
}

var imageSignatures = &signatureCache{current: map[signatureCacheKey][]byte{}, previous: map[signatureCacheKey][]byte{}}

func (s *signatureCache) get(k signatureCacheKey) ([]byte, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if v, ok := s.current[k]; ok {
		return v, true
	}
	v, ok := s.previous[k]
	return v, ok
}

func (s *signatureCache) put(k signatureCacheKey, v []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if now.After(s.rotateAt) {
		s.previous = s.current
		s.current = make(map[signatureCacheKey][]byte, len(s.previous))
		s.rotateAt = now.Add(10 * time.Minute)
	}
	s.current[k] = v
}

func imageSignature(key []byte, imageID string, exp int64) []byte {
	k := signatureCacheKey{key: string(key), imageID: imageID, exp: exp}
	if v, ok := imageSignatures.get(k); ok {
		return v
	}
	v := computeImageSignature(key, imageID, exp)
	imageSignatures.put(k, v)
	return v
}

func currentImageExp() int64 {
	exp := time.Now().Add(imageURLLifetime).Unix()
	return (exp + imageURLExpBucket - 1) / imageURLExpBucket * imageURLExpBucket
}

// signaturePlan は応答に含める署名を先に集めてまとめて計算し、キャッシュに入れておく。
type signaturePlan struct {
	exp      int64
	requests []signatureRequest
}

func newSignaturePlan() *signaturePlan {
	return &signaturePlan{exp: currentImageExp()}
}

func (p *signaturePlan) add(user User, imageID string) {
	p.requests = append(p.requests, signatureRequest{key: user.ImageKey, imageID: imageID, exp: p.exp})
}

func (p *signaturePlan) addProfile(user User) {
	if user.ProfileImageID != nil {
		p.add(user, *user.ProfileImageID)
	}
}

func (p *signaturePlan) run() {
	computeImageSignatures(p.requests)
}

func signedImageURL(user User, imageID string) string {
	exp := currentImageExp()
	query := url.Values{
		"exp": {strconv.FormatInt(exp, 10)},
		"sig": {base64.RawURLEncoding.EncodeToString(imageSignature(user.ImageKey, imageID, exp))},
	}
	return "/api/images/" + url.PathEscape(imageID) + "?" + query.Encode()
}
