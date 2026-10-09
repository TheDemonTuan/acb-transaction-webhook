package storage

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExpiredProviderOperationCannotCommitAfterChannelReplacement(t *testing.T) {
	ctx := context.Background()
	store, _ := setupTestStoreWithKeyring(t)
	defer store.Close()
	keys := providerTestKeys()
	if _, err := store.SavePaymentProviderConfig(ctx, keys, true, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	_, token, err := store.BeginPaymentProviderOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	pinned := WithPaymentProviderOperation(ctx, token)
	if _, err := store.DB().ExecContext(ctx, `UPDATE payment_provider_operations SET lease_until=? WHERE token=?`, time.Now().UTC().Add(-time.Minute).Format(time.RFC3339Nano), token); err != nil {
		t.Fatal(err)
	}
	changed := keys
	changed.ClientID = "replacement-before-any-orders"
	if _, err := store.SavePaymentProviderConfig(ctx, changed, true, PaymentProviderActor{}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.ReservePaymentOrder(pinned, PaymentOrderIntent{ChannelID: keys.ClientID, IdempotencyKey: "stale-operation", RequestHash: "hash", AmountVnd: 50000, Origin: "STATIC_URL"}); !errors.Is(err, ErrProviderConfigUnavailable) {
		t.Fatalf("stale operation wrote old-channel order: %v", err)
	}
	var count int
	if err := store.DB().QueryRow(`SELECT count(*) FROM payment_orders`).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired operation committed money state")
	}
	if err := store.RenewPaymentProviderOperation(ctx, token); !errors.Is(err, ErrProviderConfigUnavailable) {
		t.Fatal("expired operation resurrected its lease")
	}
}
