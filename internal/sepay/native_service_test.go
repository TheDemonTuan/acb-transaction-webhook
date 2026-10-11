package sepay

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
)

func nativeServiceHook(t *testing.T,cfg Config,id int64,reference string,at time.Time) NativeWebhook {
	t.Helper()
	return NativeWebhook{ID:id,Gateway:cfg.BankName,AccountNumber:cfg.effectiveNotificationAccountNumber(),SubAccount:cfg.NativeSubAccount,ReferenceCode:reference,TransferType:"in",TransferAmount:50000,TransactionAt:at,RawPayload:[]byte(`{"private":"native evidence"}`)}
}

func TestNativeAuthorityPreventsTelegramCreditBothOrders(t *testing.T) {
	for _,nativeFirst:=range []bool{true,false} {
		t.Run(map[bool]string{true:"native-first",false:"telegram-first"}[nativeFirst],func(t *testing.T) {
			ctx:=context.Background(); store:=protocolStore(t); cfg:=protocolConfig(t)
			cfg.ActiveTransport="native"; cfg.NativeMode=ModeActive; cfg.NativeWebhookSecret=nativeTestSecret
			service:=NewService(cfg,store,nil)
			notification,err:=ParseNotification(fixtureNotification); if err!=nil { t.Fatal(err) }
			hook:=nativeServiceHook(t,cfg,1,notification.Reference,notification.TransactionAt)
			update:=decodeFixture(t,telegramFixture(t,cfg,1,1,fixtureNotification))
			ep,err := store.CreateEndpointWithSecret(ctx,"native-authority","https://example.com/hook"); if err != nil { t.Fatal(err) }
			if err := store.SetEndpointStatus(ctx,ep.ID,"ACTIVE"); err != nil { t.Fatal(err) }
			if nativeFirst { if err:=service.HandleNativeWebhook(ctx,hook); err!=nil { t.Fatal(err) } }
			if err:=service.HandleUpdate(ctx,update); err!=nil { t.Fatal(err) }
			if !nativeFirst { if err:=service.HandleNativeWebhook(ctx,hook); err!=nil { t.Fatal(err) } }
			for _,table:=range []string{"transactions","events","event_journal","deliveries"} { protocolCount(t,store,table,1) }
			protocolCount(t,store,"sepay_receipts",0)
			var reason string
			if err:=store.DB().QueryRow(`SELECT reason FROM sepay_telegram_inbox`).Scan(&reason); err!=nil || reason!="OBSERVATION" { t.Fatal("Telegram authority bypass",reason,err) }
		})
	}
}

func TestNativeExactReceiverAndMasterObserve(t *testing.T) {
	for _,scenario:=range []string{"nonempty-va","master-observe","native-only-telegram-disabled"} {
		t.Run(scenario,func(t *testing.T) {
			store:=protocolStore(t); cfg:=protocolConfig(t); cfg.ActiveTransport="native"; cfg.NativeMode=ModeActive; cfg.NativeWebhookSecret=nativeTestSecret
			hook:=nativeServiceHook(t,cfg,1,"ref",time.Now().UTC())
			if scenario=="nonempty-va" { hook.SubAccount="another-va" }
			if scenario=="master-observe" { cfg.Mode=ModeObserve }
			if scenario=="native-only-telegram-disabled" { cfg.BotID,cfg.ChatID,cfg.SenderBotID=0,0,0 }
			service:=NewService(cfg,store,nil)
			if scenario=="native-only-telegram-disabled" {
				if service.TelegramEnabled() || service.Authenticate(cfg.WebhookSecret) || !errors.Is(service.HandleUpdate(context.Background(),TelegramUpdate{}),ErrUnavailable) { t.Fatal("native-only invented Telegram source") }
				return
			}
			if err:=service.HandleNativeWebhook(context.Background(),hook); err!=nil { t.Fatal(err) }
			protocolCount(t,store,"transactions",0)
			var reason string
			if err:=store.DB().QueryRow(`SELECT reason FROM sepay_native_inbox`).Scan(&reason); err!=nil { t.Fatal(err) }
			want:="ACCOUNT_MISMATCH"; if scenario=="master-observe" { want="OBSERVATION" }
			if reason!=want { t.Fatalf("reason %s want %s",reason,want) }
		})
	}
}

func TestManagedNativeCutoverFreshBoundaryAndSecretPreservation(t *testing.T) {
	for _,transition:=range []string{"telegram-to-native","native-observe-to-active"} {
		t.Run(transition,func(t *testing.T) {
			ctx:=context.Background(); store:=protocolStore(t); old:=protocolConfig(t)
			old.ActiveTransport="telegram"; old.NativeMode=ModeObserve; old.NativeWebhookSecret=nativeTestSecret
			if transition=="native-observe-to-active" { old.ActiveTransport="native" }
			service:=NewService(old,store,nil); f:=fieldsFromConfig(old); f.ReceiverVerified,f.SourceSeparated=true,true
			f.ActiveTransport="native"; f.NativeMode=ModeActive; f.ActivationAt=old.ActivationAt.Format(time.RFC3339)
			before:=time.Now().UTC()
			saved,err:=service.SaveAdminConfig(ctx,0,f,"","",storage.PaymentProviderActor{})
			if err!=nil { t.Fatal(err) }
			activation,err:=time.Parse(time.RFC3339Nano,saved.Config.ActivationAt)
			if err!=nil || activation.Before(before) { t.Fatal("new authority inherited old cutoff",activation,err) }
			snap,err:=service.Snapshot(ctx); if err!=nil { t.Fatal(err) }
			if snap.cfg.NativeWebhookSecret!=nativeTestSecret { t.Fatal("secret bytes changed") }
			hook:=nativeServiceHook(t,snap.cfg,1,"delayed-ref",before.Add(-time.Second))
			if err:=snap.HandleNativeWebhook(ctx,hook); err!=nil { t.Fatal(err) }
			protocolCount(t,store,"transactions",0)
			var reason string; if err:=store.DB().QueryRow(`SELECT reason FROM sepay_native_inbox`).Scan(&reason); err!=nil || reason!="PRE_ACTIVATION" { t.Fatal(reason,err) }
			f=saved.Config; f.StoreName="Updated Store"
			next,err:=service.SaveAdminConfig(ctx,saved.Revision,f,"","",storage.PaymentProviderActor{})
			if err!=nil || next.Config.ActivationAt!=saved.Config.ActivationAt { t.Fatal("continuous authority moved cutoff",next,err) }
		})
	}
}

func TestNativeBootstrapAndAdminWithoutTelegramIDs(t *testing.T) {
	fields:=testConfigFields()
	for _,key:=range []string{"botId","chatId","senderBotId","topicId","webhookSecret"} { delete(fields,key) }
	fields["activeTransport"]="native"; fields["nativeMode"]=ModeObserve; fields["nativeWebhookSecret"]=nativeTestSecret
	cfg,err:=BootstrapConfig(context.Background(),protocolStore(t),configJSON(t,fields))
	if err!=nil || cfg.BotID!=0 || cfg.NativeMode!=ModeObserve || cfg.NativeWebhookSecret!=nativeTestSecret { t.Fatal("native bootstrap requires fake Telegram",cfg,err) }
	store:=protocolStore(t); service:=NewService(Config{Mode:ModeDisabled},store,nil)
	f:=fieldsFromConfig(cfg); f.Mode=ModeObserve
	saved,err:=service.SaveAdminConfig(context.Background(),0,f,"",nativeTestSecret,storage.PaymentProviderActor{})
	if err!=nil || saved.Config.BotID!="" || !saved.HasNativeWebhookSecret { t.Fatal("native-only admin rejected",saved,err) }
	data,_:=json.Marshal(saved)
	if strings.Contains(string(data),nativeTestSecret) { t.Fatal("secret disclosed") }
	legacy,err:=ParseConfig(configJSON(t,testConfigFields()))
	if err!=nil || legacy.ActiveTransport!="telegram" || legacy.NativeMode!=ModeDisabled { t.Fatal("legacy defaults changed",legacy,err) }
	for _,raw:=range []string{`{"mode":"disabled"}`,`{"mode":"disabled","activeTransport":"native","nativeMode":"disabled"}`} {
		cfg,err:=ParseConfig(raw); if err!=nil || NewService(cfg,nil,nil).Enabled() { t.Fatal("disabled bootstrap enabled adapter",err) }
	}
}
