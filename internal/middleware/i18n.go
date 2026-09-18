package middleware

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/quizzzone/backend/internal/pkg/i18n"
)

// localeKey is where the resolved locale is stashed for handlers that want it.
const localeKey = "locale"

// Localize rewrites the "error" field of failed responses into the caller's
// language.
//
// It works at the edge rather than at each call site because the handlers emit
// 126 distinct messages through `gin.H{"error": ...}` and threading a
// translator through all of them would be a far larger change than the problem
// warrants. Only responses with status >= 400 are buffered and re-encoded, so
// the success path pays nothing but one wrapper allocation.
//
// A message with no entry in the catalog passes through untouched, which is
// also what keeps protocol sentinels (NO_MORE_QUESTIONS, ROOM_FINISHED) intact.
func Localize() gin.HandlerFunc {
	return func(c *gin.Context) {
		locale := localeFromRequest(c)
		c.Set(localeKey, locale)

		if locale != "vi" {
			c.Next()
			return
		}

		rec := &errorBodyRecorder{ResponseWriter: c.Writer, buf: &bytes.Buffer{}}
		c.Writer = rec
		c.Next()
		rec.flush()
	}
}

// localeFromRequest reads the locale the frontend forwards. The browser's
// Accept-Language is the fallback, so a client that calls the API directly
// still gets a sensible language.
func localeFromRequest(c *gin.Context) string {
	if v := c.GetHeader("X-Locale"); v != "" {
		if v == "vi" || v == "en" {
			return v
		}
	}
	accept := c.GetHeader("Accept-Language")
	for _, part := range strings.Split(accept, ",") {
		tag := strings.TrimSpace(strings.SplitN(part, ";", 2)[0])
		switch strings.ToLower(strings.SplitN(tag, "-", 2)[0]) {
		case "vi":
			return "vi"
		case "en":
			return "en"
		}
	}
	return "en"
}

// errorBodyRecorder buffers a failed response so its message can be rewritten.
// Successful responses are written straight through — streaming bodies and
// large payloads must not be held in memory.
type errorBodyRecorder struct {
	gin.ResponseWriter
	buf      *bytes.Buffer
	status   int
	capture  bool
	resolved bool
}

func (r *errorBodyRecorder) WriteHeader(status int) {
	if !r.resolved {
		r.status = status
		r.capture = status >= http.StatusBadRequest
		r.resolved = true
	}
	if !r.capture {
		r.ResponseWriter.WriteHeader(status)
	}
}

func (r *errorBodyRecorder) Write(b []byte) (int, error) {
	if !r.resolved {
		// gin writes a body without an explicit WriteHeader on 200.
		r.status = http.StatusOK
		r.capture = false
		r.resolved = true
	}
	if !r.capture {
		return r.ResponseWriter.Write(b)
	}
	return r.buf.Write(b)
}

func (r *errorBodyRecorder) WriteString(s string) (int, error) {
	return r.Write([]byte(s))
}

// flush emits the buffered failure, with its message translated when the body
// is the JSON object shape the handlers use. Anything else is passed through
// byte for byte — an HTML error page or a stream must not be mangled.
func (r *errorBodyRecorder) flush() {
	if !r.capture {
		return
	}
	body := r.buf.Bytes()

	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err == nil {
		if msg, ok := payload["error"].(string); ok {
			if translated := i18n.Translate("vi", msg); translated != msg {
				payload["error"] = translated
				if out, err := json.Marshal(payload); err == nil {
					body = out
				}
			}
		}
	}

	r.Header().Set("Content-Length", strconv.Itoa(len(body)))
	r.ResponseWriter.WriteHeader(r.status)
	_, _ = r.ResponseWriter.Write(body)
}
