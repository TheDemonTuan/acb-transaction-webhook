package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/vietqr"
)

// PaymentQRPreviewRequest is the payload for testing VietQR generation.
type PaymentQRPreviewRequest struct {
	AccountNumber string `json:"accountNumber"`
	AccountName   string `json:"accountName"`
	Mode          string `json:"mode"`
	TestID        string `json:"testId"`
	Host          string `json:"host"`
	Amount        string `json:"amount"`
}

// previewPaymentQR handles POST /api/v1/payment-qr/preview.
// It generates EMVCo VietQR payloads and renders base64 PNGs purely in-memory
// without writing to database or touching production payment QR assets.
func (s *Server) previewPaymentQR(w http.ResponseWriter, r *http.Request) {
	var input PaymentQRPreviewRequest
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	accNum := strings.TrimSpace(input.AccountNumber)
	accName := strings.TrimSpace(input.AccountName)

	if accNum == "" {
		// Fallback to configured payment QR if available
		existing, err := s.store.GetPaymentQR(r.Context(), "")
		if err == nil && existing != nil {
			accNum = existing.AccountNumber
			if accName == "" {
				accName = existing.AccountName
			}
		}
	}

	if accNum == "" {
		writeError(w, http.StatusBadRequest, "accountNumber is required")
		return
	}

	mode := strings.ToLower(strings.TrimSpace(input.Mode))
	if mode == "" {
		mode = vietqr.ModeStandard
	}
	if mode != vietqr.ModeStandard && mode != vietqr.ModeReference && mode != vietqr.ModeHybrid {
		writeError(w, http.StatusBadRequest, "invalid mode: must be 'standard', 'reference', or 'hybrid'")
		return
	}

	testID := strings.TrimSpace(input.TestID)
	if testID == "" {
		testID = fmt.Sprintf("QRTEST%02d", time.Now().Unix()%100)
	}

	host := strings.TrimSpace(input.Host)
	if host == "" {
		host = vietqr.DefaultHybridHost
	}

	// Register test token with CanaryTracker for scan detection
	if s.canaryTracker != nil {
		s.canaryTracker.RegisterToken(testID)
	}

	cfg := vietqr.BuilderConfig{
		Mode:          mode,
		BIN:           vietqr.DefaultAcbBIN,
		AccountNumber: accNum,
		AccountName:   accName,
		Amount:        strings.TrimSpace(input.Amount),
		Reference:     testID,
		CustomHost:    host,
		CustomType:    "1",
		CustomToken:   testID,
	}

	payload, err := vietqr.BuildPayload(cfg)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to build VietQR payload: "+err.Error())
		return
	}

	parsed, err := vietqr.ParsePayload(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to parse VietQR payload: "+err.Error())
		return
	}

	base64PNG, err := vietqr.GenerateBase64PNG(payload, 360)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to render QR image: "+err.Error())
		return
	}

	resp := map[string]any{
		"mode":    mode,
		"payload": payload,
		"image":   base64PNG,
		"parsed": map[string]any{
			"bin":              parsed.BIN,
			"accountNumber":    parsed.AccountNumber,
			"service":          parsed.ServiceCode,
			"reference":        parsed.Reference,
			"customHost":       parsed.CustomHost,
			"customType":       parsed.CustomType,
			"customToken":      parsed.CustomToken,
			"customTemplate":   parsed.CustomTemplate,
			"formatIndicator":  parsed.FormatIndicator,
			"initiationMethod": parsed.InitiationMethod,
			"currency":         parsed.Currency,
			"countryCode":      parsed.CountryCode,
		},
		"crc":         parsed.CRC,
		"crcValid":    parsed.CRCValid,
		"expectedCrc": parsed.ExpectedCRC,
		"testId":      testID,
	}

	writeJSON(w, http.StatusOK, resp)
}
