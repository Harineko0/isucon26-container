package client

import (
	"bytes"
	"mime/multipart"
)

// Multipart は multipart/form-data の本文を組む。files は name → (filename, data)。
type FilePart struct {
	Field    string
	Filename string
	Data     []byte
	MIME     string
}

func Multipart(fields map[string]string, files ...FilePart) (body []byte, contentType string, err error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	for k, v := range fields {
		if err := w.WriteField(k, v); err != nil {
			return nil, "", err
		}
	}
	for _, f := range files {
		h := make(map[string][]string)
		h["Content-Disposition"] = []string{`form-data; name="` + f.Field + `"; filename="` + f.Filename + `"`}
		mime := f.MIME
		if mime == "" {
			mime = "image/jpeg"
		}
		h["Content-Type"] = []string{mime}
		part, err := w.CreatePart(h)
		if err != nil {
			return nil, "", err
		}
		if _, err := part.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	if err := w.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), w.FormDataContentType(), nil
}
