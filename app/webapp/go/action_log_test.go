package isureal

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/labstack/echo/v5"
)

// Preserve the previous implementation as the guard-facing output oracle.
func legacyActionLogMiddleware(minLevel string, output io.Writer, onLogged func(action string, status int, ip, abuse string)) echo.MiddlewareFunc {
	return func(next echo.HandlerFunc) echo.HandlerFunc {
		return func(c *echo.Context) error {
			err := next(c)
			if err != nil {
				c.Echo().HTTPErrorHandler(c, err)
			}
			_, status := echo.ResolveResponseStatus(c.Response(), err)

			level := "info"
			switch {
			case status >= http.StatusInternalServerError:
				level = "error"
			case c.Request().Method == http.MethodGet && !(status == http.StatusForbidden && c.Get("abuse") == "invalid_signature"):
				level = "debug"
			}
			if logLevelRanks[level] < logLevelRanks[minLevel] {
				return nil
			}

			route := c.RouteInfo()
			entry := map[string]any{
				"level":   level,
				"action":  nil,
				"status":  status,
				"ip":      c.RealIP(),
				"user_id": nil,
			}
			action, ok := actionNames[route.Method+" "+route.Path]
			if ok {
				entry["action"] = action
			}
			if session := GetSession(c); session != nil {
				entry["user_id"] = session.UserID
			}
			if action == "login" || action == "register" {
				entry["username"] = nil
				if username, _ := c.Get("username").(string); usernamePattern.MatchString(username) {
					entry["username"] = username
				}
			}
			abuse, _ := c.Get("abuse").(string)
			if abuse != "" {
				entry["abuse"] = abuse
			}
			line, marshalErr := json.Marshal(entry)
			if marshalErr != nil {
				return marshalErr
			}
			_, writeErr := output.Write(append(line, '\n'))
			if onLogged != nil {
				onLogged(action, status, c.RealIP(), abuse)
			}
			return writeErr
		}
	}
}

func TestActionLogMatchesLegacy(t *testing.T) {
	for _, tc := range []struct {
		method, path, minimum, username, abuse string
		status                                 int
		session                                bool
	}{
		{"POST", "/api/auth/login", "info", "alice", "", 200, true},
		{"POST", "/api/auth/login", "info", "invalid username", "", 401, false},
		{"POST", "/api/auth/register", "info", "", "", 409, false},
		{"POST", "/api/posts", "info", "", "", 201, true},
		{"GET", "/api/images/i", "info", "", "invalid_signature", 403, false},
		{"GET", "/api/images/i", "info", "", "", 403, false},
		{"GET", "/api/unknown", "debug", "", "", 200, false},
		{"POST", "/api/unknown", "info", "", "invalid_image", 500, true},
		{"POST", "/api/posts", "error", "", "", 201, true},
	} {
		t.Run(tc.method+tc.path+tc.username+tc.abuse+tc.minimum, func(t *testing.T) {
			var outputs [][]byte
			var callbacks [][][]any
			for _, factory := range []func(string, io.Writer, func(string, int, string, string)) echo.MiddlewareFunc{legacyActionLogMiddleware, ActionLogMiddleware} {
				var output bytes.Buffer
				var calls [][]any
				e := echo.New()
				e.HTTPErrorHandler = APIErrorHandler
				e.IPExtractor = func(r *http.Request) string { return "192.0.2.1" }
				e.Use(factory(tc.minimum, &output, func(action string, status int, ip, abuse string) {
					calls = append(calls, []any{action, status, ip, abuse})
				}))
				route := tc.path
				if route == "/api/images/i" {
					route = "/api/images/:imageId"
				}
				e.Add(tc.method, route, func(c *echo.Context) error {
					if tc.session {
						c.Set("session", &Session{UserID: "user\"<>&"})
					}
					c.Set("username", tc.username)
					c.Set("abuse", tc.abuse)
					if tc.status >= 400 {
						return NewAPIError(tc.status, "test_error")
					}
					return c.NoContent(tc.status)
				})
				rec := httptest.NewRecorder()
				e.ServeHTTP(rec, httptest.NewRequest(tc.method, tc.path, nil))
				outputs = append(outputs, append([]byte(nil), output.Bytes()...))
				callbacks = append(callbacks, calls)
			}
			if !bytes.Equal(outputs[0], outputs[1]) {
				t.Fatalf("log differs: old %s new %s", outputs[0], outputs[1])
			}
			if !reflect.DeepEqual(callbacks[0], callbacks[1]) {
				t.Fatalf("callback differs: %v", callbacks)
			}
		})
	}
}

var logBytesSink []byte

func BenchmarkActionLogEncoding(b *testing.B) {
	action, user := "create_post", "user-id"
	for _, name := range []string{"old", "new"} {
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				var value any
				if name == "old" {
					value = map[string]any{"action": action, "ip": "192.0.2.1", "level": "info", "status": 201, "user_id": user}
				} else {
					value = actionLogEntry{Action: &action, IP: "192.0.2.1", Level: "info", Status: 201, UserID: &user}
				}
				logBytesSink, _ = json.Marshal(value)
			}
		})
	}
}
