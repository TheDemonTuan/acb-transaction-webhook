package sepay

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"
)

const nativeTestSecret = " 01234567890123456789012345678901 "
const nativeTestBody = `{"id":9007199254740993,"gateway":"Test Bank","transactionDate":"2026-10-10 10:05:00","accountNumber":"VA123456","subAccount":"","transferType":"in","transferAmount":200000,"accumulated":200000,"code":null,"content":"Cảm ơn","referenceCode":"","description":""}`

func nativeHeaders(raw []byte, secret string, timestamp int64) http.Header {
	ts := strconv.FormatInt(timestamp,10)
	mac := hmac.New(sha256.New,[]byte(secret))
	mac.Write([]byte(ts+".")); mac.Write(raw)
	h := make(http.Header)
	h.Set("X-SePay-Timestamp",ts)
	h.Set("X-SePay-Signature","sha256="+hex.EncodeToString(mac.Sum(nil)))
	return h
}

func TestNativeVerificationExactBytesAndTypes(t *testing.T) {
	now := time.Date(2026,10,10,3,5,0,0,time.UTC)
	raw := []byte(" \n"+nativeTestBody+"\n")
	hook,err := VerifyNativeWebhook(raw,nativeHeaders(raw,nativeTestSecret,now.Unix()),nativeTestSecret,now)
	if err != nil || hook.ID != 9007199254740993 || hook.TransferAmount != 200000 || !hook.TransactionAt.Equal(now) || string(hook.RawPayload) != string(raw) || hook.Code != nil { t.Fatalf("exact evidence lost: %+v %v",hook,err) }
	for _, seconds := range []int64{-301,301} {
		if _,err := VerifyNativeWebhook(raw,nativeHeaders(raw,nativeTestSecret,now.Unix()+seconds),nativeTestSecret,now); !errors.Is(err,ErrNativeAuthentication) { t.Fatal("stale signature accepted") }
	}
	for _, seconds := range []int64{-300,300} {
		if _,err := VerifyNativeWebhook(raw,nativeHeaders(raw,nativeTestSecret,now.Unix()+seconds),nativeTestSecret,now); err != nil { t.Fatal("boundary signature rejected",err) }
	}
	h := nativeHeaders(raw,nativeTestSecret,now.Unix())
	if _,err := VerifyNativeWebhook([]byte(nativeTestBody),h,nativeTestSecret,now); !errors.Is(err,ErrNativeAuthentication) { t.Fatal("changed whitespace authenticated") }
	h.Add("X-SePay-Signature",h.Get("X-SePay-Signature"))
	if _,err := VerifyNativeWebhook(raw,h,nativeTestSecret,now); !errors.Is(err,ErrNativeAuthentication) { t.Fatal("duplicate auth header accepted") }
	for _,body := range []string{
		strings.Replace(nativeTestBody,`9007199254740993`,`9223372036854775808`,1),
		strings.Replace(nativeTestBody,`9007199254740993`,`9007199254740993.0`,1),
		strings.Replace(nativeTestBody,`200000,"accumulated"`,`9007199254740992,"accumulated"`,1),
		strings.Replace(nativeTestBody,`"subAccount":""`,`"subAccount":null`,1),
		strings.Replace(nativeTestBody,`"code":null`,`"code":42`,1),
		strings.Replace(nativeTestBody,`"id":9007199254740993`,`"id":1,"id":2`,1),
		strings.Replace(nativeTestBody,`"content":"Cảm ơn"`,`"content":{"x":1,"x":2}`,1),
		nativeTestBody+` {}`, strings.Replace(nativeTestBody,`"transactionDate":"2026-10-10 10:05:00"`,`"transactionDate":"2026-02-30 10:05:00"`,1),
	} {
		raw := []byte(body)
		if _,err := VerifyNativeWebhook(raw,nativeHeaders(raw,nativeTestSecret,now.Unix()),nativeTestSecret,now); !errors.Is(err,ErrInvalidNativeWebhook) { t.Fatalf("invalid body accepted: %s (%v)",body,err) }
	}
}

func TestNativeOptionalBalanceDoesNotRejectCredit(t *testing.T) {
	now := time.Date(2026,10,10,3,5,0,0,time.UTC)
	for _, tc := range []struct { raw string; want int64 }{
		{"2350000", 2350000}, {"-2350000", -2350000}, {"9007199254740991", 9007199254740991}, {"-9007199254740991", -9007199254740991},
		{"", 0}, {"null", 0}, {"0", 0}, {`"2350000"`, 0}, {"true", 0}, {"1.5", 0}, {"1e3", 0}, {"9007199254740992", 0}, {"-9007199254740992", 0}, {"9223372036854775808", 0},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			body := strings.Replace(nativeTestBody, `"accumulated":200000,`, "", 1)
			if tc.raw != "" { body = strings.Replace(nativeTestBody, `"accumulated":200000`, `"accumulated":`+tc.raw, 1) }
			raw := []byte(body)
			hook, err := VerifyNativeWebhook(raw, nativeHeaders(raw,nativeTestSecret,now.Unix()), nativeTestSecret, now)
			if err != nil || hook.TransferAmount != 200000 { t.Fatalf("credit rejected: %+v %v", hook, err) }
			if tc.want == 0 && hook.BalanceVND != nil || tc.want != 0 && (hook.BalanceVND == nil || *hook.BalanceVND != tc.want) { t.Fatalf("balance=%v want=%d", hook.BalanceVND, tc.want) }
		})
	}
	for _, value := range []string{`{}`, `[]`, `1,"accumulated":2`} {
		raw := []byte(strings.Replace(nativeTestBody, `"accumulated":200000`, `"accumulated":`+value, 1))
		if _, err := VerifyNativeWebhook(raw, nativeHeaders(raw,nativeTestSecret,now.Unix()), nativeTestSecret, now); !errors.Is(err, ErrInvalidNativeWebhook) { t.Fatalf("invalid balance structure accepted: %s", value) }
	}
}
