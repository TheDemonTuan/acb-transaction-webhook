package vietqr

import (
	"errors"
	"strings"
)

const (
	// DefaultAcbBIN is ACB acquiring bank code in VietQR / NAPAS.
	DefaultAcbBIN = "970416"
	// VietQRAID is the NAPAS / VietQR globally unique identifier.
	VietQRAID = "A000000727"
	// DefaultServiceCode is Quick Response Interbank Fast Transfer to Account.
	DefaultServiceCode = "QRIBFTTA"
	// DefaultCurrencyVND is ISO 4217 code for Vietnamese Dong.
	DefaultCurrencyVND = "704"
	// DefaultCountryVN is ISO 3166-1 alpha 2 code for Vietnam.
	DefaultCountryVN = "VN"
	// DefaultHybridHost is the fallback domain for custom tag 80 metadata.
	DefaultHybridHost = "transactions.tuannguyenviet.site"

	ModeStandard  = "standard"
	ModeReference = "reference"
	ModeHybrid    = "hybrid"
)

// BuilderConfig specifies parameters for building an EMVCo VietQR payload.
type BuilderConfig struct {
	Mode          string // "standard", "reference", or "hybrid"
	BIN           string // Bank BIN (e.g. "970416")
	AccountNumber string // Beneficiary account number
	AccountName   string // Optional beneficiary name
	ServiceCode   string // Service code (default "QRIBFTTA")
	Currency      string // Currency code (default "704")
	Amount        string // Optional amount (e.g. "1000")
	CountryCode   string // Country code (default "VN")
	Reference     string // Reference / Test ID (e.g. "QRTEST01")
	CustomHost    string // Host for tag 80 (default "transactions.tuannguyenviet.site")
	CustomType    string // Type for tag 80 (default "1")
	CustomToken   string // Token for tag 80 (defaults to Reference)
}

// BuildPayload generates a standard-compliant VietQR payload string with CRC16.
func BuildPayload(cfg BuilderConfig) (string, error) {
	accNum := strings.TrimSpace(cfg.AccountNumber)
	if accNum == "" {
		return "", errors.New("accountNumber is required")
	}

	bin := strings.TrimSpace(cfg.BIN)
	if bin == "" {
		bin = DefaultAcbBIN
	}

	serviceCode := strings.TrimSpace(cfg.ServiceCode)
	if serviceCode == "" {
		serviceCode = DefaultServiceCode
	}

	currency := strings.TrimSpace(cfg.Currency)
	if currency == "" {
		currency = DefaultCurrencyVND
	}

	country := strings.TrimSpace(cfg.CountryCode)
	if country == "" {
		country = DefaultCountryVN
	}

	// 1. Tag 00: Payload Format Indicator ("01")
	payload := EncodeTLV("00", "01")

	// 2. Tag 01: Point of Initiation Method ("11" = Static)
	payload += EncodeTLV("01", "11")

	// 3. Tag 38: Merchant Account Information (NAPAS VietQR)
	// Subtag 00: GUID "A000000727"
	tag38Sub00 := EncodeTLV("00", VietQRAID)
	// Subtag 01: Beneficiary Bank Organization
	//   Sub-subtag 00: BIN
	//   Sub-subtag 01: Account Number
	sub01Sub00 := EncodeTLV("00", bin)
	sub01Sub01 := EncodeTLV("01", accNum)
	tag38Sub01 := EncodeNestedTLV("01", sub01Sub00, sub01Sub01)
	// Subtag 02: Service Code
	tag38Sub02 := EncodeTLV("02", serviceCode)
	payload += EncodeNestedTLV("38", tag38Sub00, tag38Sub01, tag38Sub02)

	// 4. Tag 53: Transaction Currency ("704")
	payload += EncodeTLV("53", currency)

	// 5. Tag 54: Transaction Amount (optional)
	amount := strings.TrimSpace(cfg.Amount)
	if amount != "" {
		payload += EncodeTLV("54", amount)
	}

	// 6. Tag 58: Country Code ("VN")
	payload += EncodeTLV("58", country)

	// Determine if Tag 62 / Tag 80 should be included based on Mode
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	ref := strings.TrimSpace(cfg.Reference)

	switch mode {
	case ModeReference:
		if ref != "" {
			tag62Sub05 := EncodeTLV("05", ref)
			payload += EncodeNestedTLV("62", tag62Sub05)
		}
	case ModeHybrid:
		if ref != "" {
			tag62Sub05 := EncodeTLV("05", ref)
			payload += EncodeNestedTLV("62", tag62Sub05)
		}

		host := strings.TrimSpace(cfg.CustomHost)
		if host == "" {
			host = DefaultHybridHost
		}
		cType := strings.TrimSpace(cfg.CustomType)
		if cType == "" {
			cType = "1"
		}
		token := strings.TrimSpace(cfg.CustomToken)
		if token == "" {
			token = ref
		}
		if token != "" || host != "" {
			tag80Sub00 := EncodeTLV("00", host)
			tag80Sub01 := EncodeTLV("01", cType)
			tag80Sub02 := EncodeTLV("02", token)
			payload += EncodeNestedTLV("80", tag80Sub00, tag80Sub01, tag80Sub02)
		}
	case ModeStandard:
		// Standard VietQR does not include tag 62 or tag 80
	default:
		// Default to standard unless reference or custom tag specified
		if ref != "" {
			tag62Sub05 := EncodeTLV("05", ref)
			payload += EncodeNestedTLV("62", tag62Sub05)
		}
	}

	// 7. Tag 63: CRC16-CCITT
	prefix := payload + "6304"
	crc := CalculateCRC(prefix)
	fullPayload := prefix + crc

	return fullPayload, nil
}
