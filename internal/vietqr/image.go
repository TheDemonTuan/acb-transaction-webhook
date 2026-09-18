package vietqr

import (
	"encoding/base64"
	"fmt"

	"github.com/skip2/go-qrcode"
)

const (
	DefaultQRSize = 400
)

// GeneratePNG renders an EMVCo payload into a PNG QR code byte array.
func GeneratePNG(payload string, size int) ([]byte, error) {
	if size <= 0 {
		size = DefaultQRSize
	}
	pngBytes, err := qrcode.Encode(payload, qrcode.Medium, size)
	if err != nil {
		return nil, fmt.Errorf("failed to encode QR code: %w", err)
	}
	return pngBytes, nil
}

// GenerateBase64PNG renders an EMVCo payload into a data URI PNG string ("data:image/png;base64,...").
func GenerateBase64PNG(payload string, size int) (string, error) {
	pngBytes, err := GeneratePNG(payload, size)
	if err != nil {
		return "", err
	}
	encoded := base64.StdEncoding.EncodeToString(pngBytes)
	return "data:image/png;base64," + encoded, nil
}
