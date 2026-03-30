package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"go.uber.org/zap"
)

type Freezer interface {
	Freeze(ctx context.Context, podName string) error
}

type Thawer interface {
	Thaw(ctx context.Context, podName string) error
}

type FreezeThawer interface {
	Freezer
	Thawer
}

type Handler struct {
	Freezer Freezer
	Thawer  Thawer
	Logger  *zap.SugaredLogger
	APIKey  string // if empty, auth is disabled
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h.APIKey != "" {
		auth := r.Header.Get("Authorization")
		token := strings.TrimPrefix(auth, "Bearer ")
		if token != h.APIKey {
			h.Logger.Error("Unauthorized request: invalid or missing API key")
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
	}

	var m messageBody
	if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
		h.Logger.Errorf("Unable to decode message body: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if m.PodName == "" || m.Namespace == "" {
		h.Logger.Error("Missing podName or namespace in request body")
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	podKey := m.Namespace + "/" + m.PodName

	switch m.Action {
	case "pause":
		h.Logger.Infof("pause request received, freezing pod: %s", podKey)
		if err := h.Freezer.Freeze(r.Context(), podKey); err != nil {
			h.Logger.Errorf("freezing pod %s failed: %v", podKey, err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	case "resume":
		h.Logger.Infof("resume request received, thawing pod: %s", podKey)
		if err := h.Thawer.Thaw(r.Context(), podKey); err != nil {
			h.Logger.Errorf("thawing pod %s failed: %v", podKey, err)
			w.WriteHeader(http.StatusInternalServerError)
		}
	default:
		h.Logger.Infof("invalid action specified: %s", m.Action)
		w.WriteHeader(http.StatusBadRequest)
	}
}

type messageBody struct {
	Action    string `json:"action"`
	PodName   string `json:"podName"`
	Namespace string `json:"namespace"`
}
