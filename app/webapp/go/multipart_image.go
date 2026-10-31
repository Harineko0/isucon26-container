package isureal

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"

	"github.com/labstack/echo/v5"
)

// multipart.Reader treats a read error as terminal. Remember it when looking
// ahead so the fallback parser sees the same error after the replayed bytes.
type multipartSource struct {
	io.ReadCloser
	err error
}

func (s *multipartSource) Read(p []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	n, err := s.ReadCloser.Read(p)
	s.err = err
	return n, err
}

// bindImageForm binds the post/RealMoji fields. For an ordinary image upload,
// keep the image in the request-sized allocation and let the usual binder parse
// just the framing and fields. All other forms use the original binding path.
func bindImageForm(c *echo.Context, input any) ([]byte, error) {
	req := c.Request()
	if req.MultipartForm != nil || req.Form != nil || req.ContentLength < 32<<10 || req.ContentLength > 2<<20 {
		return nil, c.Bind(input)
	}
	mediaType, params, err := mime.ParseMediaType(req.Header.Get("Content-Type"))
	if err != nil || mediaType != "multipart/form-data" || params["boundary"] == "" {
		return nil, c.Bind(input)
	}

	// Look one byte beyond Content-Length. Replay a short read, extra bytes
	// or an I/O error to the binder so its normal error handling still applies.
	source := &multipartSource{ReadCloser: req.Body}
	body := make([]byte, req.ContentLength+1)
	n, _ := io.ReadFull(source, body)
	body = body[:n]
	req.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(body), source), source}
	if int64(n) != req.ContentLength || source.err != io.EOF {
		return nil, c.Bind(input)
	}
	start, end, ok := multipartImageRange(body, params["boundary"])
	if !ok {
		return nil, c.Bind(input)
	}

	// Bind on a private request. FormFile on the original request can still
	// parse the complete image. The binder checks all headers, fields, part
	// counts and the closing boundary as usual.
	fields := req.WithContext(req.Context())
	fields.Body = io.NopCloser(io.MultiReader(bytes.NewReader(body[:start]), bytes.NewReader(body[end:])))
	fields.ContentLength -= int64(end - start)
	c.SetRequest(fields)
	defer c.SetRequest(req)
	err = c.Bind(input)
	if fields.MultipartForm != nil {
		fields.MultipartForm.RemoveAll()
	}
	return body[start:end:end], err
}

// multipartImageRange locates the first file named image. NextRawPart gives
// verbatim bytes and recognizes legal MIME boundaries, including in binary
// data. Only use the view if its prefix identifies one position in the bytes
// the parser has already read. Otherwise leave the whole form to the binder.
func multipartImageRange(body []byte, boundary string) (int, int, bool) {
	source := bytes.NewReader(body)
	reader := multipart.NewReader(source, boundary)
	// Bound the extra lookahead on forms with many unrelated parts.
	for range 16 {
		part, err := reader.NextRawPart()
		if err != nil {
			return 0, 0, false
		}
		if part.FormName() != "image" || part.FileName() == "" {
			continue
		}
		if part.Header.Get("Content-Transfer-Encoding") != "" {
			return 0, 0, false
		}
		var prefix [64]byte
		if _, err := io.ReadFull(part, prefix[:]); err != nil {
			return 0, 0, false
		}
		seen := body[:len(body)-source.Len()]
		start := bytes.Index(seen, prefix[:])
		if start < 0 || bytes.Contains(seen[start+1:], prefix[:]) {
			return 0, 0, false
		}
		rest, err := io.Copy(io.Discard, part)
		end := start + len(prefix) + int(rest)
		// Caches charge by image length. Keep at most 4 KiB of form
		// metadata, and at most 1/16th of the image, in its backing array.
		extra := len(body) - (end - start)
		return start, end, err == nil && extra <= 4096 && extra <= (end-start)/16
	}
	return 0, 0, false
}

func readFormImage(file *multipart.FileHeader, data []byte) ([]byte, error) {
	if data != nil {
		return data, nil
	}
	f, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readUpload(f, file.Size)
}
