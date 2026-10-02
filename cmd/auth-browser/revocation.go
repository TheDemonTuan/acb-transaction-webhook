package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

// A single native form navigation is authorized by the inspected logout DOM.
// AJAX, background submissions, changed fields and redirects cannot reuse it.
type revokeFormPermit struct {
	URL       string `json:"url"`
	Method    string `json:"method"`
	Body      string `json:"body"`
	Supported bool   `json:"supported"`
	used      atomic.Bool
}

func (p *revokeFormPermit) allows(e *fetch.EventRequestPaused) bool {
	if p == nil || p.Method != "POST" || e.ResourceType != network.ResourceTypeDocument || e.Request.Method != p.Method || e.Request.URL != p.URL {
		return false
	}
	var body strings.Builder
	for _, entry := range e.Request.PostDataEntries {
		if entry == nil {
			return false
		}
		decoded, err := base64.StdEncoding.DecodeString(entry.Bytes)
		if err != nil || body.Len()+len(decoded) > 64<<10 {
			return false
		}
		body.Write(decoded)
	}
	return body.String() == p.Body && p.used.CompareAndSwap(false, true)
}

func (s *server) registerRevocation(mux *http.ServeMux) {
	// Unlike development login endpoints, revocation always requires a token.
	mux.Handle("POST /session-revocations", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if s.internalToken == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get(authbrowser.InternalTokenHeader)), []byte(s.internalToken)) != 1 {
			writeJSON(w, 401, map[string]string{"error": "unauthorized"})
			return
		}
		s.revokeSession(w, r)
	}))
}

func (s *server) revocationOrigin() string {
	if s.revokeOrigin != "" {
		return s.revokeOrigin
	}
	return "https://online.acb.com.vn"
}

func (s *server) allowedRevokeURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.User != nil || u.Fragment != "" {
		return false
	}
	o, err := url.Parse(s.revocationOrigin())
	return err == nil && u.Scheme == o.Scheme && strings.EqualFold(u.Host, o.Host)
}

func (s *server) validateRevokeHandoff(h authbrowser.Handoff) bool {
	if !s.allowedRevokeURL(h.URL) || (h.Action != "" && !s.allowedRevokeURL(h.Action)) || len(h.Cookies) == 0 {
		return false
	}
	for _, c := range h.Cookies {
		if !s.allowedRevokeCookie(c.Domain) || c.Name == "" || c.Value == "" || strings.ContainsAny(c.Name+c.Value, "\r\n\x00") {
			return false
		}
	}
	return true
}

func (s *server) allowedRevokeCookie(domain string) bool {
	domain = strings.ToLower(strings.TrimPrefix(domain, "."))
	if s.revokeOrigin != "" {
		u, _ := url.Parse(s.revokeOrigin)
		return domain == u.Hostname()
	}
	return domain == "online.acb.com.vn" || domain == "acb.com.vn"
}

func (s *server) revokeSession(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	var input struct {
		OperationID string `json:"operationId"`
		AttemptID   string `json:"attemptId"`
		Handoff     string `json:"handoff"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF || !isValidAttemptID(input.OperationID) || (input.AttemptID != "" && !isValidAttemptID(input.AttemptID)) {
		writeJSON(w, 400, map[string]string{"error": "INVALID_REVOCATION_INPUT"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	// Bound even the wait for an in-flight login operation.
	if !s.opMu.TryLock() {
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				writeJSON(w, 200, revokeUnknown("LOGOUT_OUTCOME_UNKNOWN"))
				return
			case <-tick.C:
			}
			if s.opMu.TryLock() {
				break
			}
		}
	}
	defer s.opMu.Unlock()
	result := s.performRevocation(ctx, input.OperationID, input.AttemptID, input.Handoff)
	writeJSON(w, 200, result)
}

func revokeUnknown(reason string) authbrowser.RevocationResult {
	return authbrowser.RevocationResult{Status: "UNCONFIRMED", ReasonCode: reason}
}

// Caller owns opMu. The snapshot is only used here, never restored to monitoring.
func (s *server) performRevocation(ctx context.Context, operationID, attemptID, encoded string) authbrowser.RevocationResult {
	var h authbrowser.Handoff
	if encoded != "" {
		var err error
		h, err = authbrowser.DecodeHandoff(encoded)
		if err != nil || !s.validateRevokeHandoff(h) {
			return revokeUnknown("LOGOUT_SNAPSHOT_INVALID")
		}
	}
	s.mu.Lock()
	item := s.session
	if item == nil || attemptID == "" || item.AttemptID != attemptID || terminalStatus(item.Status) || !time.Now().Before(item.ExpiresAt) {
		item = nil
	}
	if item != nil {
		item.automated = true
	}
	s.mu.Unlock()
	if item == nil {
		if encoded == "" {
			return revokeUnknown("LOGOUT_NO_SESSION")
		}
		profile, err := newProfileDirName()
		if err != nil {
			return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
		}
		portAlloc := s.allocatePort
		if portAlloc == nil {
			portAlloc = allocateFreePort
		}
		port, err := portAlloc()
		if err != nil {
			return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
		}
		launchCtx, launchCancel := context.WithCancel(ctx)
		item = &browserSession{AttemptID: operationID, Status: "REVOKING", ExpiresAt: time.Now().Add(time.Minute), profile: profile, cancel: launchCancel, debugURL: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}
		ready := make(chan error, 1)
		go s.launch(launchCtx, item, port, ready, browserLaunchOptions{InitialURL: "about:blank", ObserveLogin: false})
		defer s.reapSession(item, 3*time.Second)
		select {
		case err := <-ready:
			if err != nil {
				return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
			}
		case <-ctx.Done():
			return revokeUnknown("LOGOUT_OUTCOME_UNKNOWN")
		}
	} else {
		defer func() {
			s.mu.Lock()
			if s.session == item {
				s.session = nil
			}
			s.mu.Unlock()
			s.reapSession(item, 3*time.Second)
		}()
	}
	alloc, allocCancel := chromedp.NewRemoteAllocator(ctx, item.debugURL)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(alloc)
	defer browserCancel()
	browser, err := chromedp.FromContext(browserCtx).Allocator.Allocate(browserCtx)
	if err != nil {
		return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
	}
	chromedp.FromContext(browserCtx).Browser = browser
	infos, err := target.GetTargets().Do(cdp.WithExecutor(browserCtx, browser))
	if err != nil {
		return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
	}
	var selected *target.Info
	for _, info := range infos {
		if info.Type == "page" && s.allowedRevokeURL(info.URL) {
			if selected != nil {
				return revokeUnknown("LOGOUT_CONTROL_UNSUPPORTED")
			}
			selected = info
		}
	}
	if selected == nil {
		for _, info := range infos {
			if info.Type == "page" && info.URL == "about:blank" {
				if selected != nil {
					return revokeUnknown("LOGOUT_CONTROL_UNSUPPORTED")
				}
				selected = info
			}
		}
	}
	if selected == nil {
		return revokeUnknown("LOGOUT_CONTROL_UNSUPPORTED")
	}
	tab, tabCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(selected.TargetID))
	defer tabCancel()
	if err := chromedp.Run(tab); err != nil {
		return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
	}
	allowMutation := &atomic.Bool{}
	responseStatus := &atomic.Int64{}
	permit := &atomic.Pointer[revokeFormPermit]{}
	if s.guardRevocationTab(tab, allowMutation, responseStatus, permit) != nil {
		return revokeUnknown("LOGOUT_BROWSER_UNAVAILABLE")
	}
	protectedURL := h.URL
	if protectedURL == "" {
		protectedURL = selected.URL
	}
	if !s.allowedRevokeURL(protectedURL) || strings.Contains(strings.ToLower(protectedURL), "logout") {
		return revokeUnknown("LOGOUT_PROBE_UNSUPPORTED")
	}
	if selected.URL == "about:blank" {
		if err := chromedp.Run(tab, network.SetCookies(revokeCookieParams(h.Cookies))); err != nil {
			return revokeUnknown("LOGOUT_SNAPSHOT_INVALID")
		}
	}
	// A GET only: do not submit the handoff's history form or construct operations.
	if err := chromedp.Run(tab, chromedp.Navigate(protectedURL)); err != nil {
		return revokeUnknown("LOGOUT_PROBE_FAILED")
	}
	before, err := s.revokeProbeState(tab, responseStatus.Load())
	if err != nil {
		return revokeUnknown("LOGOUT_PROBE_FAILED")
	}
	if before == "DENIED" {
		return authbrowser.RevocationResult{Status: "ALREADY_EXPIRED", ReasonCode: "SESSION_ALREADY_EXPIRED"}
	}
	if before != "AUTHENTICATED" {
		return revokeUnknown("LOGOUT_PROBE_UNSUPPORTED")
	}
	var cookies []*network.Cookie
	if err := chromedp.Run(tab, chromedp.ActionFunc(func(c context.Context) error {
		var err error
		cookies, err = network.GetCookies().WithURLs([]string{protectedURL}).Do(c)
		return err
	})); err != nil {
		return revokeUnknown("LOGOUT_PROBE_FAILED")
	}
	// Freeze the freshest pre-click jar. Post-click local cookie deletion is not evidence.
	frozen := make([]*network.CookieParam, 0, len(cookies))
	for _, cookie := range cookies {
		if !s.allowedRevokeCookie(cookie.Domain) {
			return revokeUnknown("LOGOUT_SNAPSHOT_INVALID")
		}
		frozen = append(frozen, &network.CookieParam{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly})
	}
	if len(frozen) == 0 {
		return revokeUnknown("LOGOUT_PROBE_UNSUPPORTED")
	}
	plan := &revokeFormPermit{}
	originJSON, _ := json.Marshal(s.revocationOrigin())
	if chromedp.Run(tab, chromedp.Evaluate(revokeControlScript+"\ninspectLogout("+string(originJSON)+", false)", plan)) != nil || !plan.Supported {
		return revokeUnknown("LOGOUT_CONTROL_UNSUPPORTED")
	}
	permit.Store(plan)
	allowMutation.Store(true)
	var clicked bool
	clickErr := chromedp.Run(tab, chromedp.Evaluate(revokeControlScript+"\ninspectLogout("+string(originJSON)+", true)", &clicked))
	if clickErr == nil && !clicked {
		return revokeUnknown("LOGOUT_OUTCOME_UNKNOWN")
	}
	// Only an explicit logout-associated dialog is eligible for confirmation.
	settle := time.NewTimer(2 * time.Second)
	defer settle.Stop()
	settleTick := time.NewTicker(100 * time.Millisecond)
	defer settleTick.Stop()
	confirmed := false
	settling := true
	for settling {
		if !confirmed {
			_ = chromedp.Run(tab, chromedp.Evaluate(revokeConfirmationScript, &confirmed))
		}
		select {
		case <-ctx.Done():
			return revokeUnknown("LOGOUT_OUTCOME_UNKNOWN")
		case <-settle.C:
			settling = false
		case <-settleTick.C:
		}
	}
	after, err := s.probeRevokedCookies(ctx, protectedURL, frozen)
	if err != nil {
		return revokeUnknown("LOGOUT_PROBE_FAILED")
	}
	if after == "DENIED" {
		return authbrowser.RevocationResult{Status: "CONFIRMED", ReasonCode: "SESSION_REVOKED"}
	}
	return revokeUnknown("LOGOUT_NOT_CONFIRMED")
}

// Use a separate Chromium profile, not merely a new tab. Cookie injection and
// the protected GET then use their own default browser context, independent
// of both the live page and its post-click cookie jar.
func (s *server) probeRevokedCookies(ctx context.Context, protectedURL string, frozen []*network.CookieParam) (string, error) {
	profile, err := newProfileDirName()
	if err != nil {
		return "", err
	}
	portAlloc := s.allocatePort
	if portAlloc == nil {
		portAlloc = allocateFreePort
	}
	port, err := portAlloc()
	if err != nil {
		return "", err
	}
	launchCtx, launchCancel := context.WithCancel(ctx)
	item := &browserSession{AttemptID: "revocation-proof", Status: "REVOKING", ExpiresAt: time.Now().Add(time.Minute), profile: profile, cancel: launchCancel, debugURL: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}
	ready := make(chan error, 1)
	go s.launch(launchCtx, item, port, ready, browserLaunchOptions{InitialURL: "about:blank", ObserveLogin: false})
	defer s.reapSession(item, 3*time.Second)
	select {
	case err := <-ready:
		if err != nil {
			return "", err
		}
	case <-ctx.Done():
		return "", ctx.Err()
	}
	alloc, allocCancel := chromedp.NewRemoteAllocator(ctx, item.debugURL)
	defer allocCancel()
	tab, tabCancel := chromedp.NewContext(alloc)
	defer tabCancel()
	if err := chromedp.Run(tab); err != nil {
		return "", err
	}
	status := &atomic.Int64{}
	if err := s.guardRevocationTab(tab, &atomic.Bool{}, status, &atomic.Pointer[revokeFormPermit]{}); err != nil {
		return "", err
	}
	if err := chromedp.Run(tab, network.SetCookies(frozen), chromedp.Navigate(protectedURL)); err != nil {
		return "", err
	}
	return s.revokeProbeState(tab, status.Load())
}

func revokeCookieParams(cookies []authbrowser.Cookie) []*network.CookieParam {
	result := make([]*network.CookieParam, 0, len(cookies))
	for _, c := range cookies {
		p := &network.CookieParam{Name: c.Name, Value: c.Value, Domain: c.Domain, Path: c.Path, Secure: c.Secure, HTTPOnly: c.HTTPOnly}
		if !c.Expires.IsZero() {
			expiry := cdp.TimeSinceEpoch(c.Expires)
			p.Expires = &expiry
		}
		result = append(result, p)
	}
	return result
}

func (s *server) guardRevocationTab(ctx context.Context, allowMutation *atomic.Bool, responseStatus *atomic.Int64, permit *atomic.Pointer[revokeFormPermit]) error {
	chromedp.ListenTarget(ctx, func(event any) {
		switch e := event.(type) {
		case *network.EventResponseReceived:
			if e.Type == network.ResourceTypeDocument && s.allowedRevokeURL(e.Response.URL) {
				responseStatus.Store(e.Response.Status)
			}
		case *fetch.EventRequestPaused:
			go func() {
				executor := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target)
				allowed := s.allowedRevokeURL(e.Request.URL) && (e.Request.Method == "GET" || (allowMutation.Load() && permit.Load().allows(e)))
				if !allowed {
					_ = fetch.FailRequest(e.RequestID, network.ErrorReasonBlockedByClient).Do(executor)
				} else {
					_ = fetch.ContinueRequest(e.RequestID).Do(executor)
				}
			}()
		case *page.EventJavascriptDialogOpening:
			go func() {
				message := strings.ToLower(e.Message)
				accept := allowMutation.Load() && e.Type == page.DialogTypeConfirm && (strings.Contains(message, "đăng xuất") || strings.Contains(message, "logout"))
				_ = page.HandleJavaScriptDialog(accept).Do(cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Target))
			}()
		}
	})
	return chromedp.Run(ctx, network.Enable(), network.SetCacheDisabled(true), network.SetBypassServiceWorker(true), fetch.Enable(), cdpbrowser.SetDownloadBehavior(cdpbrowser.SetDownloadBehaviorBehaviorDeny))
}

func (s *server) revokeProbeState(ctx context.Context, responseStatus int64) (string, error) {
	var location string
	var signals domSignals
	if err := chromedp.Run(ctx, chromedp.Location(&location), chromedp.Evaluate(acbDOMCheckScript, &signals)); err != nil {
		return "", err
	}
	if !s.allowedRevokeURL(location) {
		return "", errors.New("unsafe probe")
	}
	if responseStatus == 401 || responseStatus == 403 {
		return "DENIED", nil
	}
	if signals.VisiblePassword || signals.VisibleLogin {
		return "DENIED", nil
	}
	if responseStatus == 200 && signals.isAuthenticated() {
		return "AUTHENTICATED", nil
	}
	return "UNKNOWN", nil
}

// The inspected element is clicked normally. HTML onclick source is never evaluated.
const revokeControlScript = `function inspectLogout(origin, click) {
 const visible = el => {const s=getComputedStyle(el),r=el.getBoundingClientRect();return s.display!=='none'&&s.visibility!=='hidden'&&s.opacity!=='0'&&r.width>0&&r.height>0;};
 const norm = x => (x||'').normalize('NFC').trim().replace(/\s+/g,' ').toLowerCase();
 const signal = x => /logout/i.test(x||'');
	if([...document.querySelectorAll('dialog[open],[role="dialog"]')].some(visible)) return false;
 if(location.origin!==origin || document.querySelector('iframe,frame')) return false;
 const candidates=[...document.querySelectorAll('a,button,input[type="submit"]')].filter(el=>visible(el)&&( ['đăng xuất','logout'].includes(norm(el.tagName==='INPUT'?el.value:el.textContent)) || signal(el.getAttribute('href')) || signal(el.getAttribute('onclick')) ));
 if(candidates.length!==1) return false;
 const el=candidates[0], href=el.getAttribute('href'), form=el.form;
 if(el.target && el.target!=='_self') return false;
 if(href && (!/^https?:|^\/|^[^:]+$/i.test(href) || new URL(href,location.href).origin!==origin)) return false;
 if(form && (new URL(el.formAction||form.action,location.href).origin!==origin || (el.formTarget||form.target||'_self')!=='_self')) return false;
 const handler=el.getAttribute('onclick')||'';
 if(/window\.open|(?:https?:)?\/\//i.test(handler)) return false;
 if(!click) {
   const plan={supported:true,url:'',method:'GET',body:''};
   if(form && el.tagName!=='A' && (el.type==='submit' || (el.tagName==='BUTTON' && !el.type))) {
     plan.method=(el.getAttribute('formmethod')||form.method||'get').toUpperCase();
     if(!['GET','POST'].includes(plan.method)) return false;
     plan.url=new URL(el.getAttribute('formaction')||form.action,location.href).href;
     if(plan.method==='POST') {
       if((el.getAttribute('formenctype')||form.enctype)!=='application/x-www-form-urlencoded' || handler) return false;
       const data=new FormData(form,el);
       if([...data.values()].some(v=>typeof v!=='string')) return false;
       plan.body=new URLSearchParams(data).toString();
       if(plan.body.length>65536) return false;
     }
   }
   return plan;
 }
 el.click(); return true;
}`

const revokeConfirmationScript = `(() => {
 const dialogs=[...document.querySelectorAll('dialog[open],[role="dialog"]')].filter(el=>el.getBoundingClientRect().width>0);
 if(dialogs.length!==1 || !/(đăng xuất|logout)/i.test(dialogs[0].textContent)) return false;
 const buttons=[...dialogs[0].querySelectorAll('button,input[type="button"]')].filter(el=>el.getBoundingClientRect().width>0 && ['đăng xuất','logout','xác nhận','confirm','yes','có'].includes(((el.tagName==='INPUT'?el.value:el.textContent)||'').trim().toLowerCase()));
 if(buttons.length!==1 || buttons[0].form || buttons[0].hasAttribute('onclick')) return false;
 buttons[0].click(); return true;
})()`
