package isureal

import (
	"context"
	"runtime"

	"example.com/isureal-company/isureal/internal/jpegcheck"
)

// cpuAdmission は CPU を使い切る処理の同時実行数を抑える枠。
//
// JPEG の検証 (jpegcheck.ValidateBytes、1 枚 2〜3 ms の CPU) は、asia の最終日に 1,044 人が通知の直後に
// 一斉に投稿すると 1,044 本の goroutine で同時に runnable になる。Go のスケジューラに優先度は無いので、
// DB の応答を受け取って接続をプールに返す goroutine も、フィード・画像 GET・SSE の goroutine も同じ
// run queue に並び、1,044 × 2〜3 ms ÷ 2 コア ≈ 1.5 秒の順番待ちをする (asia1 は 2 vCPU)。接続が返るのが
// 遅れた分だけプールの待ち (job #31089: 33,058 回・合計 3h58m・平均 430 ms) と POST /api/posts の
// ゲート待ちが伸び、投稿期間の中で友達フィードの確認まで終わる人数 (10/20 点) を減らしていた。
//
// 検証を GOMAXPROCS 本に絞ると、順番を待つ投稿は channel で寝ていて runnable ではないので run queue は
// 短く保たれ、DB の応答や他の要求はすぐに CPU を得る。検証の総量は変わらず、全部の検証が終わる時刻も
// 同じ (もとから 2 コア分しか進まない) で、並びは到着順 (channel の待ちは FIFO) になる。
// 待っている間に要求が取り消されたら、検証せずに ctx の誤りを返す。
type cpuAdmission chan struct{}

func newCPUAdmission(n int) cpuAdmission { return make(cpuAdmission, max(n, 1)) }

// acquire は枠が空くか ctx が取り消されるまで待つ。取り消し済みなら枠が空いていても待たずに返す。
func (a cpuAdmission) acquire(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	select {
	case a <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (a cpuAdmission) release() { <-a }

// jpegValidation は JPEG 検証の枠。コア数ぶんあれば検証は CPU を使い切れる。
var jpegValidation = newCPUAdmission(runtime.GOMAXPROCS(0))

// validateJPEGFunc はテストで差し替える。
var validateJPEGFunc = jpegcheck.ValidateBytes

// validateJPEGBytes は jpegcheck.ValidateBytes を jpegValidation の枠の中で実行する。
// 枠を待つ間に ctx が取り消されたら、検証せずにその誤り (context.Canceled / DeadlineExceeded) を返す
// (呼び出し側は ctx.Err() != nil で見分け、不正な JPEG の 400 や abuse の記録と区別する)。
// それ以外の誤りは jpegcheck のもので、JPEG が不正。
func validateJPEGBytes(ctx context.Context, data []byte) (width, height int, err error) {
	if err := jpegValidation.acquire(ctx); err != nil {
		return 0, 0, err
	}
	defer jpegValidation.release()
	return validateJPEGFunc(data)
}
