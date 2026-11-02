package isureal

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"expvar"
	"io"
	"net/http"
	"os"
	"time"

	"example.com/isureal-company/isureal/internal/jpegworker"
)

var (
	jpegWorkerURL        = asiaJPEGWorkerURL()
	jpegWorkerCredential = jpegworker.Credential(os.Getenv("ISUREAL_DB_PASSWORD"))
	jpegWorkerSlots      = make(chan struct{}, 4)
	jpegWorkerStats      = expvar.NewMap("jpeg_worker")
	errWorkerInvalidJPEG = errors.New("invalid JPEG")
	jpegWorkerClient     = &http.Client{
		Transport:     &http.Transport{MaxIdleConnsPerHost: 4, IdleConnTimeout: time.Minute},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Timeout:       750 * time.Millisecond,
	}
)

func asiaJPEGWorkerURL() string {
	if os.Getenv("ISUREAL_REGION") == "asia" {
		return os.Getenv("ISUREAL_ASIA_JPEG_WORKER")
	}
	return ""
}

// ok is true only for a complete validator response. Any transport or protocol
// failure leaves the caller responsible for checking the original bytes.
func validateJPEGOnWorker(ctx context.Context, data []byte) (width, height int, err error, ok bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, jpegWorkerURL+jpegworker.Path, bytes.NewReader(data))
	if err != nil {
		return 0, 0, nil, false
	}
	req.Header.Set(internalSecretHeader, jpegWorkerCredential)
	resp, err := jpegWorkerClient.Do(req)
	if err != nil {
		return 0, 0, nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnprocessableEntity {
		return 0, 0, errWorkerInvalidJPEG, true
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength != 4 {
		return 0, 0, nil, false
	}
	var dims [4]byte
	if _, err := io.ReadFull(resp.Body, dims[:]); err != nil {
		return 0, 0, nil, false
	}
	return int(binary.BigEndian.Uint16(dims[:2])), int(binary.BigEndian.Uint16(dims[2:])), nil, true
}
