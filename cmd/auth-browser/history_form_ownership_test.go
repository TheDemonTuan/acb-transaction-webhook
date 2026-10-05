package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/acb"
)

// Synthetic table/form ownership witness corresponding to the value-free live
// candidate: table-opened, balanced, nonnested DETAIL date form. No bank JS.
const nativeTableHistoryForm = `<!doctype html><table><form id="synthetic-date" action="/acbib/Request" method="post">
<input type="hidden" name="dse_operationName" value="ibkacctDetailProc">
<input type="hidden" name="dse_sessionId" value="synthetic-session">
<input type="hidden" name="dse_processorState" value="synthetic-state">
<input type="hidden" name="dse_nextEventName" value="byDate">
<input type="hidden" name="AccountNbr" value="12345678">
<tr><td><input name="FromDate" value="05/10/2026"><input name="ToDate" value="05/10/2026">
<input type="radio" name="activeDatetimeYN" value="N" checked><input type="radio" name="activeDatetimeYN" value="Y">
<input type="checkbox" name="CheckRef" value="true"><select name="decoy" disabled><option selected>synthetic-decoy</option></select>
</td></tr></form></table>
<table><form id="synthetic-month" action="/acbib/Request"><input type="hidden" name="dse_operationName" value="ibkacctDetailProc"><input type="hidden" name="dse_sessionId" value="month-session"><input type="hidden" name="dse_processorState" value="month-state"><tr><td><input name="MonthCurr" value="10"></td></tr></form></table>`

func TestHistoryFormNativeOwnershipParity(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("opt-in local native ownership oracle")
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'none'; connect-src 'none'; form-action 'none'")
		fmt.Fprint(w, nativeTableHistoryForm)
	}))
	defer server.Close()
	binary := os.Getenv("BROWSER_BIN")
	if binary == "" {
		binary = findDefaultBrowser()
	}
	opts := append(chromedp.DefaultExecAllocatorOptions[:], chromedp.ExecPath(binary), chromedp.Headless, chromedp.NoSandbox, chromedp.DisableGPU, chromedp.Flag("proxy-server", "http://127.0.0.1:1"), chromedp.Flag("proxy-bypass-list", "127.0.0.1;localhost"))
	alloc, cancel := chromedp.NewExecAllocator(context.Background(), opts...)
	defer cancel()
	browserCtx, closeBrowser := chromedp.NewContext(alloc)
	defer closeBrowser()
	ctx, deadline := context.WithTimeout(browserCtx, 30*time.Second)
	defer deadline()
	var got struct {
		Action string            `json:"action"`
		Fields map[string]string `json:"fields"`
		Owners bool              `json:"owners"`
	}
	script := `(() => { const f = document.getElementById('synthetic-date'); const elements = [...f.elements]; return {action: new URL(f.action).pathname, fields:Object.fromEntries(new FormData(f)), owners:elements.every(e => e.form === f) && elements.some(e => e.name === 'FromDate' && !f.contains(e))}; })()`
	if err := chromedp.Run(ctx, chromedp.Navigate(server.URL+"/acbib/Request"), chromedp.Evaluate(script, &got)); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"dse_operationName": "ibkacctDetailProc", "dse_sessionId": "synthetic-session", "dse_processorState": "synthetic-state", "dse_nextEventName": "byDate", "AccountNbr": "12345678", "FromDate": "05/10/2026", "ToDate": "05/10/2026", "activeDatetimeYN": "N"}
	if got.Action != "/acbib/Request" || !got.Owners || !reflect.DeepEqual(got.Fields, want) {
		t.Fatal("native source/table ownership or successful controls differ from synthetic contract")
	}
	t.Log("native Chromium proves same form owner despite non-descendant date controls")
	extracted, err := acb.ExtractHistoryForm(nativeTableHistoryForm)
	if err != nil {
		t.Fatalf("Go extraction disagrees with proven native owner: %v", err)
	}
	if extracted.Action != got.Action || !reflect.DeepEqual(extracted.Fields, want) {
		t.Fatal("Go extraction differs from native successful controls or selected date form")
	}
}
