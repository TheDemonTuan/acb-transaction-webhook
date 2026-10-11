package storage

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func nativeStorageInput() SePayWebhookInput {
	at := time.Now().UTC().Truncate(time.Second)
	return SePayWebhookInput{StoreKey:"test-store",BankCode:"VCB",AccountNumber:"TEST123",Mode:"active",Gateway:"Fixture Bank",NotificationAccountNumber:"TEST123",TransferType:"in",ProviderID:9007199254740993,AmountVND:200000,ActivationAt:at.Add(-time.Hour),TransactionAt:at,RawPayload:[]byte(`{"private":"native payer evidence"}`)}
}

func nativeFinancialCounts(t *testing.T,s *Store,want int) {
	t.Helper()
	for _,table := range []string{"transactions","events","event_journal","deliveries"} { sepayTestCount(t,s,table,want) }
}

func TestNativeAtomicCreditConcurrentReplayAndConflict(t *testing.T) {
	ctx := context.Background()
	s,keyring := setupTestStoreWithKeyring(t)
	defer s.Close()
	settlementTestEndpoint(t,s)
	var path string
	if err := s.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name='main'`).Scan(&path); err != nil { t.Fatal(err) }
	second,err := Open(ctx,path); if err != nil { t.Fatal(err) }; defer second.Close(); second.WithKeyring(keyring)
	in := nativeStorageInput()
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func(i int) { defer wg.Done(); store:=s; if i%2!=0 { store=second }; if _,err:=store.IngestSePayWebhook(ctx,in); err!=nil { t.Error(err) } }(i)
	}
	wg.Wait()
	nativeFinancialCounts(t,s,1)
	sepayTestCount(t,s,"sepay_native_inbox",1)
	sepayTestCount(t,s,"sepay_receipts",0)
	var encrypted []byte
	var received,transactionID string
	if err:=s.DB().QueryRow(`SELECT payload_envelope,received_at,transaction_id FROM sepay_native_inbox`).Scan(&encrypted,&received,&transactionID); err!=nil { t.Fatal(err) }
	if bytes.Contains(encrypted,[]byte("native payer evidence")) { t.Fatal("plaintext native evidence") }
	plain,err:=s.sepayDecrypt(encrypted,[]byte(fmt.Sprintf("sepay-native:%s:%d",in.StoreKey,in.ProviderID)))
	if err!=nil || !bytes.Equal(plain,in.RawPayload) { t.Fatal("evidence not preserved",err) }
	in.AmountVND++
	for range 2 {
		r,err:=s.IngestSePayWebhook(ctx,in)
		if err!=nil || r.ReviewReason!="REFERENCE_CONFLICT" || r.Event!=nil { t.Fatalf("altered id not quarantined: %+v %v",r,err) }
	}
	sepayTestCount(t,s,"transaction_quarantine",1)
	nativeFinancialCounts(t,s,1)
	var after string
	if err:=s.DB().QueryRow(`SELECT received_at FROM sepay_native_inbox`).Scan(&after); err!=nil || after!=received { t.Fatal("first observation replaced",err) }
	var provider string
	if err:=s.DB().QueryRow(`SELECT CASE WHEN EXISTS(SELECT 1 FROM sepay_native_inbox WHERE transaction_id=t.id) THEN 'SEPAY' ELSE '' END FROM transactions t WHERE id=?`,transactionID).Scan(&provider); err!=nil || provider!="SEPAY" { t.Fatal("native projection missing",err) }
}

func TestNativeObserveActivationAndReferenceIdentity(t *testing.T) {
	for _,scenario:=range []string{"observe-replay","preactivation","new-reference-conflict","telegram-link","telegram-empty-reference","telegram-unverified"} {
		t.Run(scenario,func(t *testing.T) {
			s:=sepayTestStore(t); settlementTestEndpoint(t,s); ctx:=context.Background(); in:=nativeStorageInput(); expected:=0
			if scenario=="observe-replay" {
				in.Mode="observe"; if r,err:=s.IngestSePayWebhook(ctx,in); err!=nil || r.ReviewReason!="OBSERVATION" { t.Fatal(r,err) }; in.Mode="active"
			} else if scenario=="preactivation" { in.ActivationAt=in.TransactionAt.Add(time.Second)
			} else if scenario=="new-reference-conflict" {
				in.Reference="bank-ref"; if _,err:=s.IngestSePayWebhook(ctx,in); err!=nil { t.Fatal(err) }; in.ProviderID++; expected=1
			} else {
				telegram:=sepayTestInput(1,1,"bank-ref"); telegram.Credit.AmountVND=in.AmountVND; telegram.Credit.TransactionAt=in.TransactionAt
				if _,err:=s.IngestSePayNotification(ctx,telegram); err!=nil { t.Fatal(err) }; expected=1; in.NativeReferenceVerified=true
				if scenario=="telegram-link" { in.Reference="bank-ref" } else if scenario=="telegram-unverified" { in.Reference="another-ref"; in.NativeReferenceVerified=false }
			}
			r,err:=s.IngestSePayWebhook(ctx,in); if err!=nil || r.Event!=nil { t.Fatal(r,err) }
			if scenario=="telegram-link" && !r.Duplicate { t.Fatal("verified reference not linked") }
			if scenario=="telegram-empty-reference" && r.ReviewReason!="MISSING_REFERENCE" { t.Fatal("missing cross-transport identity accepted") }
			nativeFinancialCounts(t,s,expected)
		})
	}
}

func TestNativeConfigNamespaceAndCutoverGates(t *testing.T) {
	ctx:=context.Background()
	t.Run("accepted-namespace",func(t *testing.T) {
		s:=sepayTestStore(t); in:=nativeStorageInput()
		if _,err:=s.IngestSePayWebhook(ctx,in); err!=nil { t.Fatal(err) }
		cfg:=SePayManagedConfig{ConfigJSON:json.RawMessage(`{"fields":{"mode":"disabled"}}`)}
		if _,err:=s.SaveSePayManagedConfig(ctx,cfg,"renamed",in.BankCode,in.AccountNumber,PaymentProviderActor{}); !errors.Is(err,ErrSePayStoreKeyImmutable) { t.Fatal("accepted namespace reset",err) }
		if r,err:=s.IngestSePayWebhook(ctx,in); err!=nil || !r.Duplicate { t.Fatal("replay failed",err) }
		sepayTestCount(t,s,"transactions",1)
	})
	t.Run("receipt-history",func(t *testing.T) {
		s:=sepayTestStore(t); telegram:=sepayTestInput(1,1,"reference")
		if _,err:=s.IngestSePayNotification(ctx,telegram); err!=nil { t.Fatal(err) }
		cfg:=SePayManagedConfig{ConfigJSON:json.RawMessage(`{"fields":{"mode":"active","activeTransport":"native","nativeMode":"active","nativeReferenceVerified":false}}`)}
		if _,err:=s.SaveSePayManagedConfig(ctx,cfg,telegram.StoreKey,telegram.BankCode,telegram.AccountNumber,PaymentProviderActor{}); !errors.Is(err,ErrSePayReferenceUnverified) { t.Fatal("unverified cutover accepted",err) }
		cfg.ConfigJSON=json.RawMessage(`{"fields":{"mode":"active","activeTransport":"native","nativeMode":"active","nativeReferenceVerified":true}}`)
		if _,err:=s.SaveSePayManagedConfig(ctx,cfg,telegram.StoreKey,telegram.BankCode,telegram.AccountNumber,PaymentProviderActor{}); err!=nil { t.Fatal("verified cutover rejected",err) }
	})
}

func TestNativeReviewUnionAndInvalidCursor(t *testing.T) {
	s:=sepayTestStore(t); ctx:=context.Background(); in:=nativeStorageInput(); in.ReviewReason="ACCOUNT_MISMATCH"
	if _,err:=s.IngestSePayWebhook(ctx,in); err!=nil { t.Fatal(err) }
	telegram:=sepayTestInput(1,1,"ref"); telegram.ReviewReason="INVALID_AMOUNT"; telegram.Credit=nil
	if _,err:=s.IngestSePayNotification(ctx,telegram); err!=nil { t.Fatal(err) }
	at:="2026-10-10T12:00:00Z"
	for _,table:=range []string{"sepay_telegram_inbox","sepay_native_inbox"} { if _,err:=s.DB().Exec(`UPDATE `+table+` SET received_at=?`,at); err!=nil { t.Fatal(err) } }
	first,err:=s.ListSePayReviews(ctx,"",1)
	if err!=nil || len(first.Items)!=1 || first.Items[0].Transport!="telegram" || first.NextCursor=="" { t.Fatal(first,err) }
	second,err:=s.ListSePayReviews(ctx,first.NextCursor,1)
	if err!=nil || len(second.Items)!=1 || second.Items[0].Transport!="native" || second.NextCursor!="" { t.Fatal(second,err) }
	for _,tuple:=range []any{[]any{2.5,at,"native","test-store:1"},[]any{2,at,"native",""},[]any{2,at,"native","test-store:0"},[]any{2,at,"telegram","0:1"},[]any{2,at,"telegram","900001:01"},[]any{2,at,"native","bad store:1"}} {
		raw,_:=json.Marshal(tuple); cursor:=base64.RawURLEncoding.EncodeToString(raw)
		if _,err:=s.ListSePayReviews(ctx,cursor,1); !errors.Is(err,ErrInvalidSePayReviewCursor) { t.Fatalf("invalid cursor accepted %s: %v",raw,err) }
	}
}
