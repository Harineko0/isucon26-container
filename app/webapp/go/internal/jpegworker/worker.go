package jpegworker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"io"
	"net/http"

	"example.com/isureal-company/isureal/internal/jpegcheck"
)

const (
	Path    = "/v1/validate"
	MaxSize = 2 << 20
)

func Credential(password string) string {
	if password == "" {
		return ""
	}
	mac := hmac.New(sha256.New, []byte(password))
	mac.Write([]byte("isureal-jpeg-validation-v1"))
	return hex.EncodeToString(mac.Sum(nil))
}

func Handler(credential string) http.Handler {
	slots := make(chan struct{}, 6)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("POST "+Path, func(w http.ResponseWriter, r *http.Request) {
		if credential == "" || !hmac.Equal([]byte(r.Header.Get("X-Isureal-Internal")), []byte(credential)) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
		default:
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.ContentLength < 0 || r.ContentLength > MaxSize {
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		data := make([]byte, int(r.ContentLength))
		if _, err := io.ReadFull(r.Body, data); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		width, height, err := jpegcheck.ValidateBytes(data)
		if err != nil {
			w.WriteHeader(http.StatusUnprocessableEntity)
			return
		}
		var dims [4]byte
		binary.BigEndian.PutUint16(dims[:2], uint16(width))
		binary.BigEndian.PutUint16(dims[2:], uint16(height))
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", "4")
		w.Write(dims[:])
	})
	return mux
}
