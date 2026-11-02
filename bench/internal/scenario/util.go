package scenario

import (
	"errors"

	"isureal-bench/internal/client"
)

// reportStatusMismatch は期待外のステータスのうち、criticalStatus なら致命として記録する。
func (w *World) reportStatusMismatch(u *User, what string, err error, criticalStatus int, criticalMsg string) {
	var e *client.Error
	if errors.As(err, &e) && e.Got == criticalStatus {
		w.report(true, u, what, errors.New(criticalMsg))
		return
	}
	w.report(false, u, what, err)
}
