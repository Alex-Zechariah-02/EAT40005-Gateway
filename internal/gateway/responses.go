// Error replies and response status recording.

package gateway

import "net/http"

// writeError answers a data-plane refusal; its JSON body is never sniffed
// as another type.
func writeError(w http.ResponseWriter, status int, code, requestID string) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	writeJSON(w, status, map[string]string{"error": code, "request_id": requestID})
}

// statusRecorder notes the final status the forwarder wrote, while passing
// everything through unchanged.
type statusRecorder struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusRecorder) WriteHeader(status int) {
	// Informational (1xx) responses precede the final status.
	if !s.wrote && status >= 200 {
		s.status, s.wrote = status, true
	}
	s.ResponseWriter.WriteHeader(status)
}

func (s *statusRecorder) Write(data []byte) (int, error) {
	if !s.wrote {
		s.status, s.wrote = http.StatusOK, true
	}
	return s.ResponseWriter.Write(data)
}

// Unwrap lets http.ResponseController reach the underlying writer, so a
// forwarder can flush a streamed response through the recorder.
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// Interface guard
var _ http.Handler = (*Handler)(nil)
