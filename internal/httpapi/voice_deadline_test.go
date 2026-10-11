package httpapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/storage"
	"github.com/thedemontuan/acb-transaction-webhook/internal/ttsclient"
)

func TestVoiceAssetLookupCannotRenewSynthesisBudget(t *testing.T) {
	server, store, originalTTS, _, _ := setupTestVoiceServer(t)
	defer store.Close()
	originalTTS.Close()
	manifestEntered := make(chan struct{}, 1)
	manifestCanceled := make(chan struct{}, 1)
	synthEntered := make(chan struct{}, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/assets/profiles/"+storage.DefaultVoiceProfile.SynthesisKey+"/manifest" {
			manifestEntered <- struct{}{}
			<-r.Context().Done()
			manifestCanceled <- struct{}{}
			return
		}
		synthEntered <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	server.WithTTSClient(ttsclient.New(upstream.URL, "fixture-token"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	request := httptest.NewRequest(http.MethodPost, "/api/public/v1/voice/test", nil).WithContext(ctx)
	response := httptest.NewRecorder()
	completed := make(chan struct{})
	go func() { server.Handler().ServeHTTP(response, request); close(completed) }()
	select {
	case <-manifestEntered:
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("asset lookup not exercised")
	}
	cancel()
	select {
	case <-completed:
	case <-time.After(5 * time.Second):
		t.Fatal("request retained canceled budget")
	}
	if response.Code != http.StatusBadGateway {
		t.Fatalf("cancellation response=%d: %s", response.Code, response.Body.String())
	}
	select {
	case <-manifestCanceled:
	case <-time.After(time.Second):
		t.Fatal("asset lookup retained canceled request")
	}
	select {
	case <-synthEntered:
		t.Fatal("expired asset miss renewed synthesis budget")
	default:
	}
}
