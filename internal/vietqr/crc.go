package vietqr

import (
	"fmt"
	"strings"
)

// ComputeCRC16CCITT computes the CRC16-CCITT (False) checksum.
// Polynomial: 0x1021, Initial value: 0xFFFF, No reflection, No XOR out.
// Standard EMVCo MPM CRC calculation specification.
func ComputeCRC16CCITT(data []byte) uint16 {
	crc := uint16(0xFFFF)
	for _, b := range data {
		crc ^= uint16(b) << 8
		for i := 0; i < 8; i++ {
			if (crc & 0x8000) != 0 {
				crc = (crc << 1) ^ 0x1021
			} else {
				crc = crc << 1
			}
		}
	}
	return crc
}

// CalculateCRC calculates the 4-character uppercase hexadecimal CRC for an EMVCo payload.
// The input payload MUST include "6304" at the end.
func CalculateCRC(payloadWithTag6304 string) string {
	crcVal := ComputeCRC16CCITT([]byte(payloadWithTag6304))
	return fmt.Sprintf("%04X", crcVal)
}

// ValidateCRC checks whether the given full payload ends with a valid CRC16-CCITT tag "6304XXXX".
func ValidateCRC(fullPayload string) (bool, string, string) {
	if len(fullPayload) < 8 {
		return false, "", ""
	}
	idx := strings.LastIndex(fullPayload, "6304")
	if idx < 0 || len(fullPayload) != idx+8 {
		return false, "", ""
	}
	portionForCRC := fullPayload[:idx+4]
	actualCRC := strings.ToUpper(fullPayload[idx+4:])
	expectedCRC := CalculateCRC(portionForCRC)
	return actualCRC == expectedCRC, expectedCRC, actualCRC
}
