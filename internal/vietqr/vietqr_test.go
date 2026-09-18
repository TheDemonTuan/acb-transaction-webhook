package vietqr

import (
	"bytes"
	"strings"
	"testing"
)

func TestComputeCRC16CCITT_StandardVectors(t *testing.T) {
	// Standard CCITT false test vector: "123456789" -> 0x29B1
	crc := ComputeCRC16CCITT([]byte("123456789"))
	if crc != 0x29B1 {
		t.Fatalf("expected CRC 0x29B1, got 0x%04X", crc)
	}

	calc := CalculateCRC("123456789")
	if calc != "29B1" {
		t.Fatalf("expected string CRC 29B1, got %s", calc)
	}
}

func TestTLVEncodeAndParse(t *testing.T) {
	tlvStr := EncodeTLV("00", "01")
	if tlvStr != "000201" {
		t.Fatalf("expected '000201', got '%s'", tlvStr)
	}

	nested := EncodeNestedTLV("38", EncodeTLV("00", "A000000727"), EncodeTLV("01", "123"))
	tlvs, err := ParseTLVs(nested)
	if err != nil {
		t.Fatalf("unexpected parse error: %v", err)
	}
	if len(tlvs) != 1 {
		t.Fatalf("expected 1 outer TLV, got %d", len(tlvs))
	}
	if tlvs[0].Tag != "38" {
		t.Fatalf("expected tag 38, got %s", tlvs[0].Tag)
	}

	subTLVs, err := ParseTLVs(tlvs[0].Value)
	if err != nil {
		t.Fatalf("failed to parse sub TLVs: %v", err)
	}
	if len(subTLVs) != 2 {
		t.Fatalf("expected 2 sub TLVs, got %d", len(subTLVs))
	}
	if subTLVs[0].Tag != "00" || subTLVs[0].Value != "A000000727" {
		t.Fatalf("unexpected subTLV 00: %+v", subTLVs[0])
	}
	if subTLVs[1].Tag != "01" || subTLVs[1].Value != "123" {
		t.Fatalf("unexpected subTLV 01: %+v", subTLVs[1])
	}
}

func TestBuildStandardVietQR(t *testing.T) {
	cfg := BuilderConfig{
		Mode:          ModeStandard,
		BIN:           "970416",
		AccountNumber: "123456789",
	}
	payload, err := BuildPayload(cfg)
	if err != nil {
		t.Fatalf("BuildPayload failed: %v", err)
	}

	// Verify CRC validity
	valid, expectedCRC, actualCRC := ValidateCRC(payload)
	if !valid {
		t.Fatalf("CRC validation failed: expected %s, actual %s", expectedCRC, actualCRC)
	}

	// Parse payload and check all required fields
	parsed, err := ParsePayload(payload)
	if err != nil {
		t.Fatalf("ParsePayload failed: %v", err)
	}

	if parsed.FormatIndicator != "01" {
		t.Errorf("Tag 00: expected '01', got '%s'", parsed.FormatIndicator)
	}
	if parsed.InitiationMethod != "11" {
		t.Errorf("Tag 01: expected '11', got '%s'", parsed.InitiationMethod)
	}
	if parsed.AID != VietQRAID {
		t.Errorf("AID: expected '%s', got '%s'", VietQRAID, parsed.AID)
	}
	if parsed.BIN != "970416" {
		t.Errorf("BIN: expected '970416', got '%s'", parsed.BIN)
	}
	if parsed.AccountNumber != "123456789" {
		t.Errorf("AccountNumber: expected '123456789', got '%s'", parsed.AccountNumber)
	}
	if parsed.ServiceCode != DefaultServiceCode {
		t.Errorf("ServiceCode: expected '%s', got '%s'", DefaultServiceCode, parsed.ServiceCode)
	}
	if parsed.Currency != "704" {
		t.Errorf("Currency: expected '704', got '%s'", parsed.Currency)
	}
	if parsed.CountryCode != "VN" {
		t.Errorf("CountryCode: expected 'VN', got '%s'", parsed.CountryCode)
	}
	if parsed.Reference != "" {
		t.Errorf("Standard QR should not have reference, got '%s'", parsed.Reference)
	}
	if parsed.CustomTemplate {
		t.Errorf("Standard QR should not have tag 80")
	}
	if !parsed.CRCValid {
		t.Errorf("Parsed QR reports invalid CRC")
	}
}

func TestBuildReferenceVietQR(t *testing.T) {
	cfg := BuilderConfig{
		Mode:          ModeReference,
		BIN:           "970416",
		AccountNumber: "123456789",
		Reference:     "QRTEST01",
	}
	payload, err := BuildPayload(cfg)
	if err != nil {
		t.Fatalf("BuildPayload failed: %v", err)
	}

	valid, expectedCRC, actualCRC := ValidateCRC(payload)
	if !valid {
		t.Fatalf("CRC validation failed: expected %s, actual %s", expectedCRC, actualCRC)
	}

	parsed, err := ParsePayload(payload)
	if err != nil {
		t.Fatalf("ParsePayload failed: %v", err)
	}

	if parsed.Reference != "QRTEST01" {
		t.Errorf("Reference: expected 'QRTEST01', got '%s'", parsed.Reference)
	}
	if parsed.CustomTemplate {
		t.Errorf("Reference mode should not have tag 80")
	}
}

func TestBuildHybridVietQR(t *testing.T) {
	cfg := BuilderConfig{
		Mode:          ModeHybrid,
		BIN:           "970416",
		AccountNumber: "987654321",
		Reference:     "QRTEST01",
		CustomHost:    "transactions.tuannguyenviet.site",
		CustomType:    "1",
		CustomToken:   "QRTEST01",
	}
	payload, err := BuildPayload(cfg)
	if err != nil {
		t.Fatalf("BuildPayload failed: %v", err)
	}

	valid, expectedCRC, actualCRC := ValidateCRC(payload)
	if !valid {
		t.Fatalf("CRC validation failed: expected %s, actual %s", expectedCRC, actualCRC)
	}

	parsed, err := ParsePayload(payload)
	if err != nil {
		t.Fatalf("ParsePayload failed: %v", err)
	}

	if !parsed.CustomTemplate {
		t.Fatalf("Expected CustomTemplate to be true")
	}
	if parsed.CustomHost != "transactions.tuannguyenviet.site" {
		t.Errorf("CustomHost: expected 'transactions.tuannguyenviet.site', got '%s'", parsed.CustomHost)
	}
	if parsed.CustomType != "1" {
		t.Errorf("CustomType: expected '1', got '%s'", parsed.CustomType)
	}
	if parsed.CustomToken != "QRTEST01" {
		t.Errorf("CustomToken: expected 'QRTEST01', got '%s'", parsed.CustomToken)
	}
	if parsed.Reference != "QRTEST01" {
		t.Errorf("Reference: expected 'QRTEST01', got '%s'", parsed.Reference)
	}
}

func TestRoundTripEncodeDecode(t *testing.T) {
	originalCfg := BuilderConfig{
		Mode:          ModeHybrid,
		BIN:           "970416",
		AccountNumber: "01234567890",
		Reference:     "CANARY_TOKEN_42",
		CustomHost:    "gateway.test.site",
		CustomType:    "1",
		CustomToken:   "CANARY_TOKEN_42",
	}
	originalPayload, err := BuildPayload(originalCfg)
	if err != nil {
		t.Fatalf("BuildPayload failed: %v", err)
	}

	parsed, err := ParsePayload(originalPayload)
	if err != nil {
		t.Fatalf("ParsePayload failed: %v", err)
	}

	rebuiltCfg := BuilderConfig{
		Mode:          ModeHybrid,
		BIN:           parsed.BIN,
		AccountNumber: parsed.AccountNumber,
		Reference:     parsed.Reference,
		CustomHost:    parsed.CustomHost,
		CustomType:    parsed.CustomType,
		CustomToken:   parsed.CustomToken,
	}
	rebuiltPayload, err := BuildPayload(rebuiltCfg)
	if err != nil {
		t.Fatalf("Re-BuildPayload failed: %v", err)
	}

	if originalPayload != rebuiltPayload {
		t.Fatalf("Round-trip payload mismatch:\nOriginal: %s\nRebuilt:  %s", originalPayload, rebuiltPayload)
	}
}

func TestGeneratePNGAndBase64(t *testing.T) {
	cfg := BuilderConfig{
		Mode:          ModeStandard,
		AccountNumber: "123456789",
	}
	payload, err := BuildPayload(cfg)
	if err != nil {
		t.Fatalf("BuildPayload failed: %v", err)
	}

	pngBytes, err := GeneratePNG(payload, 256)
	if err != nil {
		t.Fatalf("GeneratePNG failed: %v", err)
	}
	if len(pngBytes) == 0 {
		t.Fatal("PNG bytes is empty")
	}
	// PNG file signature: 0x89 'P' 'N' 'G' '\r' '\n' 0x1A '\n'
	pngHeader := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}
	if !bytes.HasPrefix(pngBytes, pngHeader) {
		t.Fatal("Generated bytes do not have PNG header")
	}

	base64Str, err := GenerateBase64PNG(payload, 256)
	if err != nil {
		t.Fatalf("GenerateBase64PNG failed: %v", err)
	}
	if !strings.HasPrefix(base64Str, "data:image/png;base64,") {
		t.Fatalf("Expected data URI prefix, got: %s", base64Str[:30])
	}
}
