package sepay

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bankdate"
)

var ErrNativeAuthentication = errors.New("invalid SePay webhook authentication")
var ErrInvalidNativeWebhook = errors.New("invalid SePay webhook")

type NativeWebhook struct {
	ID int64
	TransferAmount int64
	BalanceVND *int64
	Gateway, AccountNumber, SubAccount, ReferenceCode, Content, Description, TransferType string
	Code *string
	TransactionAt time.Time
	RawPayload []byte
}

// VerifyNativeWebhook authenticates the exact bytes before interpreting money.
func VerifyNativeWebhook(rawBody []byte, headers http.Header, secret string, now time.Time) (NativeWebhook, error) {
	fail := NativeWebhook{}
	timestamps, signatures := headers.Values("X-SePay-Timestamp"), headers.Values("X-SePay-Signature")
	if len(timestamps) != 1 || len(signatures) != 1 || len(secret) < 32 || len(secret) > 256 || !utf8.ValidString(secret) {
		return fail, ErrNativeAuthentication
	}
	timestamp := timestamps[0]
	seconds, err := strconv.ParseInt(timestamp, 10, 64)
	if err != nil || strconv.FormatInt(seconds, 10) != timestamp {
		return fail, ErrNativeAuthentication
	}
	signedAt := time.Unix(seconds, 0)
	if signedAt.Before(now.Add(-300*time.Second)) || signedAt.After(now.Add(300*time.Second)) {
		return fail, ErrNativeAuthentication
	}
	signature := signatures[0]
	if !strings.HasPrefix(signature, "sha256=") || len(signature) != 71 {
		return fail, ErrNativeAuthentication
	}
	digest, err := hex.DecodeString(signature[7:])
	if err != nil { return fail, ErrNativeAuthentication }
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	if !hmac.Equal(digest, mac.Sum(nil)) { return fail, ErrNativeAuthentication }
	if len(rawBody) == 0 || len(rawBody) > 64<<10 || !utf8.Valid(rawBody) { return fail, ErrInvalidNativeWebhook }

	// The native contract is flat. Token decoding rejects duplicate keys and
	// nested objects/arrays rather than accepting json.Unmarshal's last key wins.
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.UseNumber()
	token, err := dec.Token()
	if err != nil || token != json.Delim('{') { return fail, ErrInvalidNativeWebhook }
	fields := make(map[string]any, 13)
	for dec.More() {
		key, err := dec.Token()
		if err != nil { return fail, ErrInvalidNativeWebhook }
		name, ok := key.(string)
		if !ok { return fail, ErrInvalidNativeWebhook }
		if _, exists := fields[name]; exists { return fail, ErrInvalidNativeWebhook }
		value, err := dec.Token()
		if err != nil { return fail, ErrInvalidNativeWebhook }
		if _, nested := value.(json.Delim); nested || value == nil && name != "code" && name != "accumulated" { return fail, ErrInvalidNativeWebhook }
		fields[name] = value
	}
	if token, err = dec.Token(); err != nil || token != json.Delim('}') { return fail, ErrInvalidNativeWebhook }
	if _, err = dec.Token(); err != io.EOF { return fail, ErrInvalidNativeWebhook }

	out := NativeWebhook{}
	for _, field := range []struct { name string; target *int64 }{{"id", &out.ID}, {"transferAmount", &out.TransferAmount}} {
		number, ok := fields[field.name].(json.Number)
		if !ok { return fail, ErrInvalidNativeWebhook }
		value, err := strconv.ParseInt(string(number), 10, 64)
		if err != nil || value <= 0 { return fail, ErrInvalidNativeWebhook }
		*field.target = value
	}
	if out.TransferAmount > 9007199254740991 { return fail, ErrInvalidNativeWebhook }
	if number, ok := fields["accumulated"].(json.Number); ok {
		if value, err := strconv.ParseInt(string(number), 10, 64); err == nil && value != 0 && value >= -maxAmountVND && value <= maxAmountVND {
			out.BalanceVND = &value
		}
	}
	for _, field := range []struct { name string; target *string }{
		{"gateway", &out.Gateway}, {"accountNumber", &out.AccountNumber}, {"subAccount", &out.SubAccount},
		{"referenceCode", &out.ReferenceCode}, {"content", &out.Content}, {"description", &out.Description}, {"transferType", &out.TransferType},
	} {
		value, ok := fields[field.name].(string)
		if !ok { return fail, ErrInvalidNativeWebhook }
		*field.target = value
	}
	if out.Gateway == "" || out.AccountNumber == "" || out.TransferType != "in" && out.TransferType != "out" { return fail, ErrInvalidNativeWebhook }
	code, ok := fields["code"]
	if !ok { return fail, ErrInvalidNativeWebhook }
	if code != nil {
		value, ok := code.(string)
		if !ok { return fail, ErrInvalidNativeWebhook }
		out.Code = &value
	}
	date, ok := fields["transactionDate"].(string)
	if !ok { return fail, ErrInvalidNativeWebhook }
	out.TransactionAt, err = time.ParseInLocation("2006-01-02 15:04:05", date, bankdate.DefaultLocation)
	if err != nil || out.TransactionAt.Format("2006-01-02 15:04:05") != date { return fail, ErrInvalidNativeWebhook }
	out.RawPayload = append([]byte(nil), rawBody...)
	return out, nil
}
