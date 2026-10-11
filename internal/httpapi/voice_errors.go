package httpapi

import (
	"context"
	"errors"
	"net/http"

	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func writeVoiceSynthesisError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ttsclient.ErrOverloaded):
		w.Header().Set("Retry-After", "1")
		writeError(w, http.StatusServiceUnavailable, "TTS_OVERLOADED")
	case errors.Is(err, ttsclient.ErrTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "TTS_TIMEOUT")
	default:
		writeError(w, http.StatusBadGateway, "TTS_UNAVAILABLE")
	}
}
