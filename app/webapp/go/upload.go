package isureal

import (
	"errors"
	"io"
	"net/http"
)

// maxUploadBytes は nginx の client_max_body_size (32m) と同じ。これより大きい本文は
// nginx が 413 で止めるので、アプリには届かない。届いた場合の保険として同じ 413 を返す。
const maxUploadBytes = 32 << 20

var errUploadTooLarge = NewAPIError(http.StatusRequestEntityTooLarge, "request_entity_too_large")

// readUpload はアップロードされた本文を最後まで読んで返す。
//
// size (PUT なら Content-Length、multipart なら FileHeader.Size) が分かっていれば、
// その長さの切片を 1 回だけ確保して io.ReadFull で埋める。io.ReadAll は 512 バイトから
// 約 1.25 倍ずつ伸ばしながら読むので、本体の約 5 倍を確保してコピーも繰り返し、
// 返る切片も cap > len だった (imageDataStore は len で数えるので予算が過小になる)。
//
// size が分からない (chunked など) か上限を超える申告なら、上限 + 1 バイトまでに
// 抑えた io.ReadAll に戻る。本文が size より短いときは io.ReadAll と同じく
// io.ErrUnexpectedEOF を返す。
func readUpload(r io.Reader, size int64) ([]byte, error) {
	if size <= 0 || size > maxUploadBytes {
		data, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1))
		if err != nil {
			return nil, err
		}
		if len(data) > maxUploadBytes {
			return nil, errUploadTooLarge
		}
		return data, nil
	}
	buf := make([]byte, size)
	if _, err := io.ReadFull(r, buf); err != nil {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF
		}
		return nil, err
	}
	// 申告が実際より短かったときだけ (通常は 1 回の Read が EOF を返して終わる) 残りを読み足す。
	var probe [1]byte
	n, err := r.Read(probe[:])
	if n == 0 && errors.Is(err, io.EOF) {
		return buf, nil
	}
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	rest, err := io.ReadAll(io.LimitReader(r, maxUploadBytes+1-int64(len(buf))-int64(n)))
	if err != nil {
		return nil, err
	}
	buf = append(buf, probe[:n]...)
	buf = append(buf, rest...)
	if len(buf) > maxUploadBytes {
		return nil, errUploadTooLarge
	}
	return buf, nil
}
