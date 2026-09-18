package vietqr

import (
	"errors"
	"fmt"
)

// ParsedQR represents the decoded and structured components of an EMVCo VietQR payload.
type ParsedQR struct {
	RawPayload       string            `json:"rawPayload"`
	FormatIndicator  string            `json:"formatIndicator"`  // Tag 00
	InitiationMethod string            `json:"initiationMethod"` // Tag 01
	AID              string            `json:"aid"`              // Tag 38.00
	BIN              string            `json:"bin"`              // Tag 38.01.00
	AccountNumber    string            `json:"accountNumber"`    // Tag 38.01.01
	ServiceCode      string            `json:"serviceCode"`      // Tag 38.02
	Currency         string            `json:"currency"`         // Tag 53
	Amount           string            `json:"amount,omitempty"` // Tag 54
	CountryCode      string            `json:"countryCode"`      // Tag 58
	Reference        string            `json:"reference,omitempty"` // Tag 62.05
	CustomHost       string            `json:"customHost,omitempty"` // Tag 80.00
	CustomType       string            `json:"customType,omitempty"` // Tag 80.01
	CustomToken      string            `json:"customToken,omitempty"` // Tag 80.02
	CustomTemplate   bool              `json:"customTemplate"`       // true if tag 80 is present
	CRC              string            `json:"crc"`              // Tag 63
	CRCValid         bool              `json:"crcValid"`
	ExpectedCRC      string            `json:"expectedCrc"`
	Tags             map[string]string `json:"tags"`
}

// ParsePayload decodes an EMVCo QR payload and extracts structured VietQR fields.
func ParsePayload(payload string) (*ParsedQR, error) {
	if payload == "" {
		return nil, errors.New("empty payload")
	}

	tlvs, err := ParseTLVs(payload)
	if err != nil {
		return nil, fmt.Errorf("failed to parse TLVs: %w", err)
	}

	validCRC, expectedCRC, actualCRC := ValidateCRC(payload)

	parsed := &ParsedQR{
		RawPayload:  payload,
		CRC:         actualCRC,
		CRCValid:    validCRC,
		ExpectedCRC: expectedCRC,
		Tags:        make(map[string]string),
	}

	for _, item := range tlvs {
		parsed.Tags[item.Tag] = item.Value
	}

	// Tag 00
	if item := FindTLV(tlvs, "00"); item != nil {
		parsed.FormatIndicator = item.Value
	}
	// Tag 01
	if item := FindTLV(tlvs, "01"); item != nil {
		parsed.InitiationMethod = item.Value
	}
	// Tag 53
	if item := FindTLV(tlvs, "53"); item != nil {
		parsed.Currency = item.Value
	}
	// Tag 54
	if item := FindTLV(tlvs, "54"); item != nil {
		parsed.Amount = item.Value
	}
	// Tag 58
	if item := FindTLV(tlvs, "58"); item != nil {
		parsed.CountryCode = item.Value
	}

	// Tag 38: Merchant Account Information
	if item := FindTLV(tlvs, "38"); item != nil {
		subTLVs, err := ParseTLVs(item.Value)
		if err == nil {
			if sub00 := FindTLV(subTLVs, "00"); sub00 != nil {
				parsed.AID = sub00.Value
			}
			if sub01 := FindTLV(subTLVs, "01"); sub01 != nil {
				bankTLVs, err := ParseTLVs(sub01.Value)
				if err == nil {
					if binItem := FindTLV(bankTLVs, "00"); binItem != nil {
						parsed.BIN = binItem.Value
					}
					if accItem := FindTLV(bankTLVs, "01"); accItem != nil {
						parsed.AccountNumber = accItem.Value
					}
				}
			}
			if sub02 := FindTLV(subTLVs, "02"); sub02 != nil {
				parsed.ServiceCode = sub02.Value
			}
		}
	}

	// Tag 62: Additional Data Field Template
	if item := FindTLV(tlvs, "62"); item != nil {
		subTLVs, err := ParseTLVs(item.Value)
		if err == nil {
			if refItem := FindTLV(subTLVs, "05"); refItem != nil {
				parsed.Reference = refItem.Value
			}
		}
	}

	// Tag 80: Custom Hybrid Template
	if item := FindTLV(tlvs, "80"); item != nil {
		parsed.CustomTemplate = true
		subTLVs, err := ParseTLVs(item.Value)
		if err == nil {
			if hostItem := FindTLV(subTLVs, "00"); hostItem != nil {
				parsed.CustomHost = hostItem.Value
			}
			if typeItem := FindTLV(subTLVs, "01"); typeItem != nil {
				parsed.CustomType = typeItem.Value
			}
			if tokenItem := FindTLV(subTLVs, "02"); tokenItem != nil {
				parsed.CustomToken = tokenItem.Value
			}
		}
	}

	return parsed, nil
}
