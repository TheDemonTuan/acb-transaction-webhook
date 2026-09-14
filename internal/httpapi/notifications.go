package httpapi

import (
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/thedemontuan/acb-transaction-webhook/internal/notification"
	"github.com/thedemontuan/acb-transaction-webhook/internal/security"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func (s *Server) notificationProviders(w http.ResponseWriter, r *http.Request) {
	if s.providerReader != nil {
		res, err := s.providerReader.NotificationProviderMetadata(r.Context())
		if err != nil {
			slog.Warn("failed to fetch notification provider metadata from worker", "error", err)
			writeError(w, http.StatusServiceUnavailable, "worker unavailable: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, res)
		return
	}

	barkConfigured := (s.barkSender != nil) || (s.cfg.BarkServerURL != "")
	providers := []map[string]any{
		{
			"id":          "WEBHOOK",
			"name":        "Webhook",
			"description": "Gửi JSON có chữ ký HMAC tới hệ thống khác.",
			"configured":  true,
			"status":      "configured",
		},
		{
			"id":          "BARK",
			"name":        "Bark (iOS)",
			"description": "Đẩy thông báo trực tiếp tới iPhone qua Bark self-host.",
			"configured":  barkConfigured,
			"publicUrl":   s.cfg.BarkPublicURL,
			"status":      map[bool]string{true: "configured", false: "unconfigured"}[barkConfigured],
		},
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": providers})
}

func (s *Server) notificationChannels(w http.ResponseWriter, r *http.Request) {
	items, err := s.store.NotificationChannels(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}

func (s *Server) createNotificationChannel(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider   string              `json:"provider"`
		Name       string              `json:"name"`
		URL        string              `json:"url,omitempty"`
		DeviceKey  string              `json:"deviceKey,omitempty"`
		BarkConfig *storage.BarkConfig `json:"barkConfig,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}

	in.Provider = strings.ToUpper(strings.TrimSpace(in.Provider))
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}

	if in.Provider == "WEBHOOK" {
		if _, err := security.ValidateWebhookURL(in.URL); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		ep, err := s.store.CreateEndpointWithSecret(r.Context(), in.Name, in.URL)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		audit(s.store, r, "webhook.create", ep.ID)
		s.publishStateEvent("notification.changed", ep.ID, map[string]any{"id": ep.ID, "status": ep.Status, "provider": "WEBHOOK"})
		s.publishStateEvent("webhook.changed", ep.ID, map[string]any{"id": ep.ID, "status": ep.Status})

		ch := storage.NotificationChannel{
			ID:        ep.ID,
			Name:      ep.Name,
			Provider:  "WEBHOOK",
			Status:    ep.Status,
			Revision:  ep.Revision,
			URL:       ep.URL,
			Secret:    ep.Secret,
			CreatedAt: ep.CreatedAt,
			UpdatedAt: ep.UpdatedAt,
		}
		writeJSON(w, http.StatusCreated, ch)
		return

	} else if in.Provider == "BARK" {
		in.DeviceKey = strings.TrimSpace(in.DeviceKey)
		if in.DeviceKey == "" {
			writeError(w, http.StatusBadRequest, "deviceKey is required for Bark channel")
			return
		}

		ch, err := s.store.CreateBarkChannel(r.Context(), in.Name, in.DeviceKey, in.BarkConfig)
		if err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		audit(s.store, r, "notification_channel.create", ch.ID)
		s.publishStateEvent("notification.changed", ch.ID, map[string]any{"id": ch.ID, "status": ch.Status, "provider": "BARK"})
		writeJSON(w, http.StatusCreated, ch)
		return
	}

	writeError(w, http.StatusBadRequest, "invalid provider: must be WEBHOOK or BARK")
}

func (s *Server) updateNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		ExpectedRevision int                 `json:"expectedRevision"`
		Name             string              `json:"name"`
		URL              string              `json:"url,omitempty"`
		BarkConfig       *storage.BarkConfig `json:"barkConfig,omitempty"`
	}
	if !decode(w, r, &in) {
		return
	}

	updated, err := s.store.UpdateChannel(r.Context(), id, in.ExpectedRevision, in.Name, in.URL, in.BarkConfig)
	if errors.Is(err, storage.ErrRevisionConflict) {
		writeError(w, http.StatusConflict, err.Error())
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	audit(s.store, r, "notification_channel.update", id)
	s.publishStateEvent("notification.changed", id, map[string]any{"id": id, "status": updated.Status, "provider": updated.Provider})
	if updated.Provider == "WEBHOOK" {
		s.publishStateEvent("webhook.changed", id, map[string]any{"id": id, "status": updated.Status})
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) toggleNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	action := chi.URLParam(r, "action")
	status := "ACTIVE"
	if action == "disable" {
		status = "DISABLED"
	}

	err := s.store.SetEndpointStatus(r.Context(), id, status)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	ch, _ := s.store.NotificationChannelByID(r.Context(), id)

	audit(s.store, r, "notification_channel."+strings.ToLower(status), id)
	s.publishStateEvent("notification.changed", id, map[string]any{"id": id, "status": status, "provider": ch.Provider})
	s.publishStateEvent("webhook.changed", id, map[string]any{"id": id, "status": status})
	writeJSON(w, http.StatusOK, map[string]string{"status": status})
}

func (s *Server) rotateChannelSecret(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	var in struct {
		DeviceKey string `json:"deviceKey,omitempty"`
	}
	if r.ContentLength != 0 && !decode(w, r, &in) {
		return
	}

	newSecret, err := s.store.RotateSecret(r.Context(), id, in.DeviceKey)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	audit(s.store, r, "notification_channel.rotate_secret", id)
	s.publishStateEvent("notification.changed", id, map[string]any{"id": id})

	if newSecret != "" {
		writeJSON(w, http.StatusOK, map[string]string{"secret": newSecret, "status": "ROTATED"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ROTATED"})
}

func (s *Server) testNotificationChannel(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")

	// Cooldown: 5s per channel
	s.testCooldownMu.Lock()
	if s.lastTestPerCh == nil {
		s.lastTestPerCh = make(map[string]time.Time)
	}
	lastTest := s.lastTestPerCh[id]
	if time.Since(lastTest) < 5*time.Second {
		s.testCooldownMu.Unlock()
		writeError(w, http.StatusTooManyRequests, "vui lòng đợi vài giây trước khi gửi thử lại")
		return
	}
	s.lastTestPerCh[id] = time.Now()
	s.testCooldownMu.Unlock()

	ch, err := s.store.NotificationChannelByID(r.Context(), id)
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "channel_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage_error")
		return
	}

	if s.channelTester != nil {
		res, err := s.channelTester.TestNotificationChannel(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusBadGateway, "worker RPC failure: "+err.Error())
			return
		}
		audit(s.store, r, "notification_channel.test", id)
		if res.Success || res.Status == "DELIVERED" {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":    "DELIVERED",
				"latencyMs": res.LatencyMs,
				"message":   res.Message,
			})
			return
		}
		if res.ProviderErrorCode == storage.ErrCodeBarkKeyDecryptionFailed {
			writeStandardError(w, r, http.StatusBadRequest, res.ProviderErrorCode, res.SanitizedError)
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"status": "FAILED",
			"code":   res.ProviderErrorCode,
			"error":  res.SanitizedError,
		})
		return
	}

	target, err := s.store.DeliveryTargetForDelivery(r.Context(), storage.Delivery{
		EndpointID:       ch.ID,
		EndpointRevision: ch.Revision,
	})
	if err != nil {
		if ch.Provider == "BARK" && (errors.Is(err, storage.ErrBarkDecryptionFailed) || errors.Is(err, security.ErrAuthenticationFailed)) {
			writeStandardError(w, r, http.StatusBadRequest, storage.ErrCodeBarkKeyDecryptionFailed, storage.ErrMsgBarkKeyDecryptionFailed)
			return
		}
		writeError(w, http.StatusBadRequest, "cannot decrypt channel target: "+err.Error())
		return
	}

	if ch.Provider == "BARK" {
		if s.barkSender == nil {
			writeError(w, http.StatusServiceUnavailable, "Bark server URL chưa được cấu hình trên gateway")
			return
		}
		res := s.barkSender.SendTestNotification(r.Context(), target)
		audit(s.store, r, "notification_channel.test", id)
		if res.Outcome == notification.OutcomeSuccess {
			writeJSON(w, http.StatusOK, map[string]any{
				"status":    "DELIVERED",
				"latencyMs": res.LatencyMs,
				"message":   "Bark đã chấp nhận thông báo thử — hãy kiểm tra iPhone",
			})
			return
		}
		writeJSON(w, http.StatusBadGateway, map[string]any{
			"status": "FAILED",
			"code":   res.ProviderErrorCode,
			"error":  res.SanitizedError,
		})
		return
	}

	// WEBHOOK provider test
	if s.notifRegistry != nil {
		if whSender, ok := s.notifRegistry.Get("WEBHOOK"); ok {
			testReq := notification.SendRequest{
				DeliveryID:   "del_test_" + id,
				EventID:      "evt_test_ping",
				EventType:    "bank.transaction.credit",
				Target:       target,
				EventPayload: []byte(`{"bank":"ACB","credit":"0","debit":"0","description":"Test Webhook Ping","source":"TEST"}`),
			}
			res := whSender.Send(r.Context(), testReq)
			audit(s.store, r, "notification_channel.test", id)
			if res.Outcome == notification.OutcomeSuccess {
				writeJSON(w, http.StatusOK, map[string]any{
					"status":    "DELIVERED",
					"latencyMs": res.LatencyMs,
					"message":   "Webhook endpoint đã nhận thông báo thử thành công",
				})
				return
			}
			writeJSON(w, http.StatusBadGateway, map[string]any{
				"status": "FAILED",
				"code":   res.ProviderErrorCode,
				"error":  res.SanitizedError,
			})
			return
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"status": "DELIVERED", "message": "Thông báo thử đã được gửi"})
}

func (s *Server) replayDelivery(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	err := s.store.ReplayDelivery(r.Context(), id)
	if errors.Is(err, storage.ErrDeliveryNotDeadLetter) {
		writeError(w, http.StatusConflict, "chỉ có thể gửi lại lượt phân phối ở trạng thái DEAD_LETTER")
		return
	}
	if errors.Is(err, storage.ErrEndpointNotActive) {
		writeError(w, http.StatusBadRequest, "kênh thông báo đang bị tắt, hãy bật kênh trước khi gửi lại")
		return
	}
	if errors.Is(err, storage.ErrNotFound) {
		writeError(w, http.StatusNotFound, "delivery_not_found")
		return
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	audit(s.store, r, "delivery.replay", id)
	s.publishStateEvent("delivery.changed", id, map[string]any{"id": id, "status": "PENDING"})

	resp := map[string]string{"status": "PENDING"}
	if s.wakeFn != nil {
		if err := s.wakeFn(r.Context()); err != nil {
			slog.Warn("delivery replay wake dispatcher degraded", "error", err)
			resp["warning"] = "wake degraded: " + err.Error()
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
