package httpapi

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/bankdate"
	"github.com/thedemontuan/acb-transaction-webhook/internal/config"
	"github.com/thedemontuan/acb-transaction-webhook/internal/sepay"
	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

const sepayNativeWebhookPath = "/api/integrations/sepay/webhook"

func nativeSign(secret string, timestamp int64, body []byte) (string, string) {
	ts := strconv.FormatInt(timestamp, 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts+".")); mac.Write(body)
	return ts, "sha256="+hex.EncodeToString(mac.Sum(nil))
}

func nativeHTTPFixture(t *testing.T,cfg sepay.Config) (*storage.Store,http.Handler,*atomic.Int64) {
	t.Helper()
	store,err:=storage.Open(context.Background(),filepath.Join(t.TempDir(),"sepay_native.db")); if err!=nil { t.Fatal(err) }
	t.Cleanup(func(){ _=store.Close() }); sepayWebhookKeyring(t,store)
	ep,err:=store.CreateEndpointWithSecret(context.Background(),"native","https://example.com/webhook"); if err!=nil { t.Fatal(err) }
	if err:=store.SetEndpointStatus(context.Background(),ep.ID,"ACTIVE"); err!=nil { t.Fatal(err) }
	commits:=&atomic.Int64{}
	service:=sepay.NewService(cfg,store,func(event storage.EventNotification){
		var receipts int
		if err:=store.DB().QueryRow(`SELECT COUNT(*) FROM sepay_native_inbox WHERE transaction_id=? AND reason='ACCEPTED'`,event.TransactionID).Scan(&receipts); err!=nil || receipts!=1 { t.Errorf("hint before native commit %d %v",receipts,err) }
		commits.Add(1)
	})
	return store,New(config.Config{Production:true},store).WithSePay(service).Handler(),commits
}

func nativeHTTPPayload(t *testing.T,cfg sepay.Config) []byte {
	t.Helper()
	payload:=map[string]any{"id":int64(9007199254740993),"gateway":cfg.BankName,"transactionDate":time.Now().In(bankdate.DefaultLocation).Format("2006-01-02 15:04:05"),"accountNumber":cfg.AccountNumber,"subAccount":"","transferType":"in","transferAmount":int64(200000),"accumulated":200000,"code":nil,"content":"Thanh toán thử","referenceCode":"","description":""}
	raw,err:=json.Marshal(payload); if err!=nil { t.Fatal(err) }; return raw
}

func nativeHTTPPost(handler http.Handler,secret string,timestamp int64,raw []byte,media string) *httptest.ResponseRecorder {
	req:=httptest.NewRequest(http.MethodPost,sepayNativeWebhookPath,bytes.NewReader(raw))
	ts,sig:=nativeSign(secret,timestamp,raw)
	req.Header.Set("Content-Type",media); req.Header.Set("X-SePay-Timestamp",ts); req.Header.Set("X-SePay-Signature",sig)
	rec:=httptest.NewRecorder(); handler.ServeHTTP(rec,req); return rec
}

func TestNativeWebhookBehavior(t *testing.T) {
	cfg:=sepayWebhookConfig(); cfg.ActiveTransport="native"; cfg.NativeMode=sepay.ModeActive; cfg.NativeWebhookSecret="01234567890123456789012345678901"
	store,handler,commits:=nativeHTTPFixture(t,cfg)
	raw:=nativeHTTPPayload(t,cfg)
	for range 2 {
		rec:=nativeHTTPPost(handler,cfg.NativeWebhookSecret,time.Now().Unix(),raw,"application/json")
		if rec.Code!=http.StatusOK || rec.Body.String()!="{\"success\":true}\n" { t.Fatalf("durable ack %d %s",rec.Code,rec.Body.String()) }
	}
	if commits.Load()!=1 { t.Fatalf("duplicate commit %d",commits.Load()) }
	for _,table:=range []string{"transactions","events","event_journal","deliveries","sepay_native_inbox"} { if n:=webhookTestCount(t,store,table); n!=1 { t.Fatalf("%s count %d",table,n) } }
	if n:=webhookTestCount(t,store,"sepay_receipts"); n!=0 { t.Fatal("native faked Telegram receipt") }
	for _,tc:=range []struct{ name string; raw []byte; secret string; timestamp int64; media string; status int }{
		{"stale",raw,cfg.NativeWebhookSecret,time.Now().Add(-301*time.Second).Unix(),"application/json",401},
		{"unauthenticated",raw,strings.Repeat("x",32),time.Now().Unix(),"application/json",401},
		{"media",raw,cfg.NativeWebhookSecret,time.Now().Unix(),"text/plain",415},
		{"duplicate-key",[]byte(strings.Replace(string(raw),`"id":9007199254740993`,`"id":1,"id":2`,1)),cfg.NativeWebhookSecret,time.Now().Unix(),"application/json",400},
		{"oversized",bytes.Repeat([]byte("x"),65537),cfg.NativeWebhookSecret,time.Now().Unix(),"application/json",413},
	} {
		t.Run(tc.name,func(t *testing.T){ rec:=nativeHTTPPost(handler,tc.secret,tc.timestamp,tc.raw,tc.media); if rec.Code!=tc.status { t.Fatalf("status %d want %d %s",rec.Code,tc.status,rec.Body.String()) } })
	}
	gate,err:=store.AcquireMutationGate(context.Background(),"native-test",time.Minute,"test"); if err!=nil { t.Fatal(err) }
	if gate.ID=="" { t.Fatal("missing gate") }
	rec:=nativeHTTPPost(handler,cfg.NativeWebhookSecret,time.Now().Unix(),bytes.Replace(raw,[]byte("9007199254740993"),[]byte("9007199254740994"),1),"application/json")
	if rec.Code!=503 || webhookTestCount(t,store,"sepay_native_inbox")!=1 { t.Fatal("failed mutation acknowledged/persisted") }
}

func TestNativeWebhookExactRouteAndDisabled(t *testing.T) {
	cfg:=sepayWebhookConfig(); cfg.NativeMode=sepay.ModeDisabled
	_,handler,_:=nativeHTTPFixture(t,cfg)
	raw:=nativeHTTPPayload(t,cfg)
	if rec:=nativeHTTPPost(handler,strings.Repeat("x",32),time.Now().Unix(),raw,"application/json"); rec.Code!=503 { t.Fatal("disabled native accepted",rec.Code) }
	for _,tc:=range []struct{method,path string}{{"GET",sepayNativeWebhookPath},{"POST",sepayNativeWebhookPath+"/"},{"POST",sepayNativeWebhookPath+"/child"}} {
		req:=httptest.NewRequest(tc.method,tc.path,bytes.NewReader(raw)); rec:=httptest.NewRecorder(); handler.ServeHTTP(rec,req)
		if rec.Code==http.StatusOK { t.Fatal("nonexact native route bypass") }
	}
}
