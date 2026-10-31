package isureal

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"mime/quotedprintable"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/labstack/echo/v5"
)

const uploadBoundary = "image-test-boundary"

func testImageForm(t testing.TB, img []byte, caption string, encoded bool) ([]byte, string) {
	t.Helper()
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	if err := w.SetBoundary(uploadBoundary); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteField("caption", caption); err != nil {
		t.Fatal(err)
	}
	h := textproto.MIMEHeader{"Content-Disposition": {`form-data; name="image"; filename="photo.jpg"`}}
	if encoded {
		h.Set("Content-Transfer-Encoding", "quoted-printable")
	}
	p, err := w.CreatePart(h)
	if err != nil {
		t.Fatal(err)
	}
	if encoded {
		q := quotedprintable.NewWriter(p)
		q.Binary = true
		if _, err := q.Write(img); err != nil {
			t.Fatal(err)
		}
		if err := q.Close(); err != nil {
			t.Fatal(err)
		}
	} else if _, err := p.Write(img); err != nil {
		t.Fatal(err)
	}
	// Echo binds the first file named image, even when another follows.
	p, err = w.CreateFormFile("image", "other.jpg")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(p, "second file"); err != nil {
		t.Fatal(err)
	}
	for _, field := range [][2]string{{"visibility", "public"}, {"latitude", "12.5"}, {"reactionType", "smile"}} {
		if err := w.WriteField(field[0], field[1]); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), w.FormDataContentType()
}

func TestBindImageForm(t *testing.T) {
	img := randomBody(360 << 10)
	copy(img[160:], "\r\n--"+uploadBoundary+"suffix that is not a boundary")
	plain, contentType := testImageForm(t, img, "雪", false)
	encoded, _ := testImageForm(t, img, "qp", true)
	largeFields, _ := testImageForm(t, img, strings.Repeat("x", 8192), false)
	repeated, _ := testImageForm(t, img, string(img[:64]), false)
	badField := bytes.Replace(plain, []byte("\r\n12.5\r\n"), []byte("\r\noops\r\n"), 1)
	tests := []struct {
		name   string
		wire   []byte
		length int64
		view   bool
	}{
		{"ordinary", plain, int64(len(plain)), true},
		{"chunked", plain, -1, false},
		{"truncated", plain[:len(plain)-15], int64(len(plain) - 15), true},
		{"shorter than declared", plain, int64(len(plain)) + 31, false},
		{"longer than declared", plain, int64(len(plain)) - 31, false},
		{"invalid field", badField, int64(len(badField)), true},
		{"encoded file", encoded, int64(len(encoded)), false},
		{"large surrounding fields", largeFields, int64(len(largeFields)), false},
		{"ambiguous prefix", repeated, int64(len(repeated)), false},
	}
	e := echo.New()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			context := func() *echo.Context {
				req := httptest.NewRequest(http.MethodPost, "/api/posts", bytes.NewReader(tt.wire))
				req.Header.Set("Content-Type", contentType)
				req.ContentLength = tt.length
				return e.NewContext(req, httptest.NewRecorder())
			}
			var got, want createPostRequest
			c, ref := context(), context()
			imgView, err := bindImageForm(c, &got)
			wantErr := ref.Bind(&want)
			if fmt.Sprint(err) != fmt.Sprint(wantErr) {
				t.Fatalf("bind error %v, want %v", err, wantErr)
			}
			if err != nil {
				return
			}
			if (imgView != nil) != tt.view {
				t.Fatalf("image view = %v, want %v", imgView != nil, tt.view)
			}
			if got.Caption != want.Caption || got.Visibility != want.Visibility || got.Latitude == nil || *got.Latitude != *want.Latitude {
				t.Fatal("bound post fields differ")
			}
			if got.Image == nil || got.Image.Filename != want.Image.Filename {
				t.Fatal("bound file differs")
			}
			imageData, err := readFormImage(got.Image, imgView)
			if err != nil || !bytes.Equal(imageData, img) {
				t.Fatalf("image mismatch, err %v", err)
			}
			// Other readers of the original request still see the genuine file.
			file, _, err := c.Request().FormFile("image")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			original, err := io.ReadAll(file)
			if err != nil || !bytes.Equal(original, imageData) || !bytes.Equal(imageData, img) {
				t.Fatal("original or retained image changed")
			}
			c = context()
			c.SetPathValues(echo.PathValues{{Name: "postId", Value: "the-post"}})
			var moji replaceRealMojiRequest
			if _, err := bindImageForm(c, &moji); err != nil || moji.PostID != "the-post" || moji.ReactionType != "smile" {
				t.Fatalf("RealMoji binding: post %q reaction %q err %v", moji.PostID, moji.ReactionType, err)
			}
		})
	}
}

func TestBindImageFormReadError(t *testing.T) {
	wire, contentType := testImageForm(t, randomBody(64<<10), "caption", false)
	failure := fmt.Errorf("upload interrupted")
	e := echo.New()
	context := func() *echo.Context {
		r := httptest.NewRequest(http.MethodPost, "/api/posts",
			io.MultiReader(bytes.NewReader(wire[:len(wire)/2]), iotest.ErrReader(failure)))
		r.Header.Set("Content-Type", contentType)
		r.ContentLength = int64(len(wire))
		return e.NewContext(r, httptest.NewRecorder())
	}
	_, err := bindImageForm(context(), &createPostRequest{})
	want := context().Bind(&createPostRequest{})
	if want == nil || fmt.Sprint(err) != want.Error() {
		t.Fatalf("read error %v, want %v", err, want)
	}
}

func BenchmarkMultipartImage(b *testing.B) {
	e := echo.New()
	for _, size := range []int{64 << 10, 360 << 10, 900 << 10} {
		wire, contentType := testImageForm(b, randomBody(size), "caption", false)
		for _, mode := range []string{"standard", "view"} {
			b.Run(fmt.Sprintf("%dKiB/%s", size>>10, mode), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					req := httptest.NewRequest(http.MethodPost, "/api/posts", bytes.NewReader(wire))
					req.Header.Set("Content-Type", contentType)
					c := e.NewContext(req, httptest.NewRecorder())
					var input createPostRequest
					var data []byte
					var err error
					if mode == "view" {
						data, err = bindImageForm(c, &input)
					} else {
						err = c.Bind(&input)
					}
					if err != nil {
						b.Fatal(err)
					}
					data, err = readFormImage(input.Image, data)
					if err != nil || len(data) != size {
						b.Fatal(err, len(data))
					}
				}
			})
		}
	}
}
