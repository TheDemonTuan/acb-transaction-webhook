package vietqr

import (
	"errors"
	"fmt"
	"strconv"
)

// TLV represents a Tag-Length-Value entry in EMVCo QR code.
type TLV struct {
	Tag    string `json:"tag"`
	Length int    `json:"length"`
	Value  string `json:"value"`
}

// EncodeTLV formats a tag and string value into an EMVCo TLV string ("TTLLVV...").
func EncodeTLV(tag string, value string) string {
	length := len([]byte(value))
	return fmt.Sprintf("%02s%02d%s", tag, length, value)
}

// EncodeNestedTLV formats an outer tag enclosing pre-formatted inner TLV strings.
func EncodeNestedTLV(tag string, innerTLVs ...string) string {
	var combined string
	for _, s := range innerTLVs {
		combined += s
	}
	return EncodeTLV(tag, combined)
}

// ParseTLVs parses a raw EMVCo payload or nested TLV content into a slice of TLVs.
func ParseTLVs(data string) ([]TLV, error) {
	var items []TLV
	idx := 0
	dataBytes := []byte(data)
	n := len(dataBytes)

	for idx < n {
		if idx+4 > n {
			return nil, fmt.Errorf("truncated TLV header at position %d", idx)
		}
		tag := string(dataBytes[idx : idx+2])
		lenStr := string(dataBytes[idx+2 : idx+4])
		valLen, err := strconv.Atoi(lenStr)
		if err != nil || valLen < 0 {
			return nil, fmt.Errorf("invalid length '%s' for tag '%s' at position %d", lenStr, tag, idx)
		}
		idx += 4
		if idx+valLen > n {
			return nil, fmt.Errorf("value length %d for tag '%s' exceeds remaining data length (%d)", valLen, tag, n-idx)
		}
		val := string(dataBytes[idx : idx+valLen])
		idx += valLen

		items = append(items, TLV{
			Tag:    tag,
			Length: valLen,
			Value:  val,
		})
	}

	return items, nil
}

// FindTLV finds the first TLV entry with the given tag.
func FindTLV(items []TLV, tag string) *TLV {
	for i := range items {
		if items[i].Tag == tag {
			return &items[i]
		}
	}
	return nil
}

// ParseNestedTLVs parses the value of a specific tag as inner TLVs.
func ParseNestedTLVs(items []TLV, tag string) ([]TLV, error) {
	item := FindTLV(items, tag)
	if item == nil {
		return nil, errors.New("tag not found")
	}
	return ParseTLVs(item.Value)
}
