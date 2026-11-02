package isureal

import (
	"context"
	"runtime"

	"example.com/isureal-company/isureal/internal/jpegcheck"
)

// Bound runnable JPEG decoders during an upload burst so request and DB
// completion handlers can run on the same CPUs.
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
