package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

func (s *server) registerAutomation(mux *http.ServeMux) {
	mux.Handle("GET /sessions/{attemptID}/observation", s.requireInternal(http.HandlerFunc(s.automationObservation)))
	mux.Handle("GET /sessions/{attemptID}/captcha", s.requireInternal(http.HandlerFunc(s.automationCapture)))
	for _, action := range []string{"login", "captcha", "otp"} {
		mux.Handle("POST /sessions/{attemptID}/"+action, s.requireInternal(http.HandlerFunc(s.automationAction)))
	}
}

func automationError(w http.ResponseWriter, status int) {
	writeJSON(w, status, map[string]string{"error": "browser automation request could not be completed"})
}

// Caller holds opMu. Session-map mutex is never held while attaching to Chromium.
func (s *server) automationSession(w http.ResponseWriter, r *http.Request) *browserSession {
	w.Header().Set("Cache-Control", "no-store")
	s.mu.Lock()
	defer s.mu.Unlock()
	item := s.session
	if item == nil || item.AttemptID != r.PathValue("attemptID") {
		automationError(w, 404)
		return nil
	}
	if !time.Now().Before(item.ExpiresAt) || item.Status == "EXPIRED" {
		automationError(w, 410)
		return nil
	}
	if terminalStatus(item.Status) {
		automationError(w, 409)
		return nil
	}
	item.automated = true
	// A manual observer may have visited another account before automation took ownership.
	if item.accountNumber == "" {
		item.verified = false
		item.handoff = ""
	}
	return item
}

func (s *server) automationObservation(w http.ResponseWriter, r *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	item := s.automationSession(w, r)
	if item == nil {
		return
	}
	observation, _, _, err := s.observeAutomation(r.Context(), item)
	if err != nil {
		automationError(w, 503)
		return
	}
	writeJSON(w, 200, observation)
}

func (s *server) automationCapture(w http.ResponseWriter, r *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	item := s.automationSession(w, r)
	if item == nil {
		return
	}
	observation, _, png, err := s.observeAutomation(r.Context(), item)
	if err != nil {
		automationError(w, 503)
		return
	}
	if r.URL.Query().Get("revision") != observation.Revision || item.consumed {
		automationError(w, 409)
		return
	}
	if !observation.CaptchaRequired || len(png) == 0 {
		automationError(w, 422)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.WriteHeader(200)
	_, _ = w.Write(png)
}

func (s *server) automationAction(w http.ResponseWriter, r *http.Request) {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	item := s.automationSession(w, r)
	if item == nil {
		return
	}
	action := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
	var login authbrowser.LoginInput
	var challenge authbrowser.ChallengeInput
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10))
	decoder.DisallowUnknownFields()
	var input any = &challenge
	if action == "login" {
		input = &login
	}
	if decoder.Decode(input) != nil || decoder.Decode(&struct{}{}) != io.EOF {
		automationError(w, 400)
		return
	}
	revision, value := challenge.Revision, challenge.Value
	if action == "login" {
		revision, value = login.Revision, login.Captcha
	}
	observation, dom, _, err := s.observeAutomation(r.Context(), item)
	if err != nil {
		automationError(w, 503)
		return
	}
	if revision == "" || revision != observation.Revision || item.consumed {
		automationError(w, 409)
		return
	}
	allowed := (action == "login" && observation.State == authbrowser.LoginForm) || (action == "captcha" && observation.State == authbrowser.CaptchaRequired) || (action == "otp" && observation.State == authbrowser.OTPRequired)
	if !allowed {
		automationError(w, 422)
		return
	}
	if action == "login" && (login.Username == "" || login.Password == "" || login.AccountNumber == "") {
		automationError(w, 400)
		return
	}
	if (action == "otp" && !validAutomationAnswer(value, 4, 10, true)) || ((action == "captcha" || observation.CaptchaRequired) && !validAutomationAnswer(value, 1, 16, false)) {
		automationError(w, 400)
		return
	}
	if !time.Now().Before(item.ExpiresAt) {
		automationError(w, 410)
		return
	}
	// Consume before any side effect. Neither timeout nor ambiguous CDP result permits replay.
	item.consumed = true
	if action == "login" {
		item.accountNumber = login.AccountNumber
	}
	payload, _ := json.Marshal(map[string]string{"action": action, "username": login.Username, "password": login.Password, "value": value, "fingerprint": dom.ActionFingerprint})
	script := automationDOMLibrary + "\n(() => { const input=" + string(payload) + "; const d=inspectRecovery(" + fixtureBool(s.fixtureDOM) + "); if(d.fingerprint!==input.fingerprint || !d.form || !d.submit) return false; const fields=input.action==='login'?[[d.username,input.username],[d.password,input.password],...(d.captcha?[[d.captcha,input.value]]:[])]:[[input.action==='otp'?d.otp:d.captcha,input.value]]; for(const [el,val] of fields){if(!el || (el.maxLength>0 && val.length>el.maxLength)) return false; if(el.pattern){try{if(!(new RegExp('^(?:'+el.pattern+')$')).test(val)) return false;}catch(e){return false;}}} for(const [el,val] of fields){Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(el,val); el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true}));} d.submit.click(); return true; })()"
	var submitted bool
	err = s.withAutomationTab(r.Context(), item, func(ctx context.Context, _ target.ID) error {
		return chromedp.Run(ctx, chromedp.Evaluate(script, &submitted))
	})
	// Drop owned references. Go strings and Chromium memory cannot be reliably zeroized.
	login = authbrowser.LoginInput{}
	challenge = authbrowser.ChallengeInput{}
	value = ""
	payload = nil
	script = ""
	if err != nil || !submitted {
		automationError(w, 409)
		return
	}
	// Allow a navigation/validation cycle, but do not repeat the submit on failure.
	deadline := time.Now().Add(3 * time.Second)
	for {
		next, _, _, observeErr := s.observeAutomation(r.Context(), item)
		if observeErr == nil && (!item.consumed || time.Now().After(deadline)) {
			writeJSON(w, 200, next)
			return
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			automationError(w, 503)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func validAutomationAnswer(value string, min, max int, digits bool) bool {
	if len(value) < min || len(value) > max {
		return false
	}
	for _, c := range []byte(value) {
		if c >= '0' && c <= '9' {
			continue
		}
		if !digits && ((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')) {
			continue
		}
		return false
	}
	return true
}
func fixtureBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

type recoveryDOM struct {
	State             authbrowser.AuthPageState `json:"state"`
	Reason            string                    `json:"reason"`
	Fingerprint       string                    `json:"fingerprint"`
	ActionFingerprint string                    `json:"-"`
	Captcha           bool                      `json:"captchaRequired"`
	X                 float64                   `json:"x"`
	Y                 float64                   `json:"y"`
	Width             float64                   `json:"width"`
	Height            float64                   `json:"height"`
}

func (s *server) matchesAutomationURL(raw string) bool {
	expected := acbLoginURL()
	if s.loginURL != "" {
		expected = s.loginURL
	}
	actual, err := url.Parse(raw)
	if err != nil {
		return false
	}
	allow, err := url.Parse(expected)
	if err != nil {
		return false
	}
	// Unlike authenticated page matching, the initial obkloginop query is valid here.
	return actual.Scheme == allow.Scheme && strings.EqualFold(actual.Host, allow.Host) && actual.User == nil && (strings.HasPrefix(actual.Path, "/acbib/") || actual.Path == allow.Path)
}

func (s *server) withAutomationTab(ctx context.Context, item *browserSession, fn func(context.Context, target.ID) error) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	alloc, allocCancel := chromedp.NewRemoteAllocator(ctx, item.debugURL)
	defer allocCancel()
	browserCtx, browserCancel := chromedp.NewContext(alloc)
	defer browserCancel()
	browser, err := chromedp.FromContext(browserCtx).Allocator.Allocate(browserCtx)
	if err != nil {
		return err
	}
	targets, err := target.GetTargets().Do(cdp.WithExecutor(browserCtx, browser))
	if err != nil {
		return err
	}
	var selected *target.Info
	for _, info := range targets {
		if info.Type == "page" && s.matchesAutomationURL(info.URL) {
			if selected != nil {
				return errors.New("ambiguous page")
			}
			selected = info
		}
	}
	if selected == nil {
		return errors.New("no allowed page")
	}
	tab, tabCancel := chromedp.NewContext(browserCtx, chromedp.WithTargetID(selected.TargetID))
	defer func() {
		if c := chromedp.FromContext(tab); c != nil && c.Target != nil {
			c.Target.TargetID = ""
		}
		tabCancel()
	}()
	return fn(tab, selected.TargetID)
}

func (s *server) observeAutomation(ctx context.Context, item *browserSession) (authbrowser.AuthObservation, recoveryDOM, []byte, error) {
	var dom recoveryDOM
	var png []byte
	err := s.withAutomationTab(ctx, item, func(tab context.Context, id target.ID) error {
		script := automationDOMLibrary + "\n(() => { const d=inspectRecovery(" + fixtureBool(s.fixtureDOM) + "); return {state:d.state,reason:d.reason,fingerprint:d.fingerprint,captchaRequired:!!d.captcha,x:d.rect?.x||0,y:d.rect?.y||0,width:d.rect?.width||0,height:d.rect?.height||0}; })()"
		if err := chromedp.Run(tab, chromedp.Evaluate(script, &dom)); err != nil {
			return err
		}
		dom.ActionFingerprint = dom.Fingerprint
		if dom.Captcha {
			if dom.Width <= 0 || dom.Height <= 0 || dom.Width > 1024 || dom.Height > 512 {
				dom.State = authbrowser.Unknown
				dom.Reason = "UNSAFE_CAPTCHA_CROP"
				dom.Captcha = false
				return nil
			}
			if err := chromedp.Run(tab, chromedp.ActionFunc(func(ctx context.Context) error {
				var err error
				png, err = page.CaptureScreenshot().WithFormat(page.CaptureScreenshotFormatPng).WithClip(&page.Viewport{X: dom.X, Y: dom.Y, Width: dom.Width, Height: dom.Height, Scale: 1}).WithCaptureBeyondViewport(false).Do(ctx)
				return err
			})); err != nil {
				return err
			}
			if len(png) > authbrowser.MaxCaptchaBytes || !bytes.HasPrefix(png, []byte("\x89PNG\r\n\x1a\n")) {
				return errors.New("invalid crop")
			}
			digest := sha256.Sum256(png)
			dom.Fingerprint += ":" + hex.EncodeToString(digest[:])
		}
		// Use existing authenticated signal/cookie contract; automatic history extraction never clicks the first account.
		var signals domSignals
		var currentURL string
		if err := chromedp.Run(tab, chromedp.Evaluate(acbDOMCheckScript, &signals), chromedp.Location(&currentURL)); err != nil {
			return err
		}
		cookies, err := storage.GetCookies().Do(cdp.WithExecutor(tab, chromedp.FromContext(tab).Browser))
		if err != nil {
			return err
		}
		cookiesPresent := len(filterACBCookies(cookies)) > 0
		if s.loginURL != "" {
			cookiesPresent = len(cookies) > 0
		}
		if signals.isAuthenticated() && cookiesPresent && dom.Reason != "FRAME_UNSUPPORTED" {
			if item.accountNumber == "" {
				dom.State = authbrowser.Unknown
				dom.Reason = "ACCOUNT_SELECTION_REQUIRED"
				return nil
			}
			account, _ := json.Marshal(item.accountNumber)
			var selection struct {
				browserFormState
				Pending bool `json:"pending"`
			}
			if err := chromedp.Run(tab, chromedp.Evaluate(automaticHistoryScript+"("+string(account)+")", &selection)); err != nil {
				return err
			}
			form := selection.browserFormState
			if selection.Pending {
				dom.State = authbrowser.Unknown
				dom.Reason = "ACCOUNT_SELECTION_PENDING"
				return nil
			}
			if !validHistoryForm(form) || form.Fields["AccountNbr"] != item.accountNumber || !s.matchesAutomationURL(form.Action) {
				dom.State = authbrowser.Unknown
				dom.Reason = "ACCOUNT_SELECTION_REQUIRED"
				return nil
			}
			// Production retains the existing authenticatedACB predicate. Fixture URL matching is injected only in Go tests.
			if !authenticatedACB(currentURL, signals, cookies) && s.loginURL == "" {
				return nil
			}
			handoff, err := encodeHandoff(currentURL, cookies, form)
			if s.loginURL != "" {
				nonce := make([]byte, 24)
				if _, err = rand.Read(nonce); err != nil {
					return err
				}
				handoff, err = authbrowser.EncodeHandoff(authbrowser.Handoff{Version: 1, URL: currentURL, Action: form.Action, Fields: form.Fields, Cookies: fixtureCookies(cookies)}, nonce)
			}
			if err != nil {
				return err
			}
			s.mu.Lock()
			if s.session == item && !terminalStatus(item.Status) && time.Now().Before(item.ExpiresAt) {
				item.handoff = handoff
				item.verified = true
				item.Status = "VERIFIED"
			}
			s.mu.Unlock()
			dom.State = authbrowser.Authenticated
			dom.Reason = ""
		}
		return nil
	})
	if err != nil {
		return authbrowser.AuthObservation{}, dom, nil, err
	}
	if dom.State != authbrowser.Authenticated {
		s.mu.Lock()
		if s.session == item {
			item.verified = false
			item.handoff = ""
			if item.Status == "VERIFIED" {
				item.Status = "AWAITING_USER_LOGIN"
			}
		}
		s.mu.Unlock()
	}
	if !time.Now().Before(item.ExpiresAt) {
		return authbrowser.AuthObservation{}, dom, nil, errors.New("expired session")
	}
	fingerprint := dom.Fingerprint
	if fingerprint == "" {
		fingerprint = string(dom.State) + ":" + dom.Reason
	}
	if fingerprint != item.fingerprint || item.revision == "" {
		nonce := make([]byte, 18)
		if _, err := rand.Read(nonce); err != nil {
			return authbrowser.AuthObservation{}, dom, nil, err
		}
		item.revision = base64.RawURLEncoding.EncodeToString(nonce)
		item.fingerprint = fingerprint
		item.consumed = false
	}
	if item.consumed && dom.State != authbrowser.Authenticated {
		dom.State = authbrowser.Unknown
		dom.Reason = "ACTION_OUTCOME_UNKNOWN"
	}
	return authbrowser.AuthObservation{State: dom.State, Revision: item.revision, CaptchaRequired: dom.Captcha, ReasonCode: dom.Reason, ExpiresAt: item.ExpiresAt}, dom, png, nil
}

// Only used by the unexported fixture URL injection. No production host/TLS bypass exists.
func fixtureCookies(cookies []*network.Cookie) []authbrowser.Cookie {
	result := make([]authbrowser.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		result = append(result, authbrowser.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly})
	}
	return result
}

// Selector provenance: main.go's DOM signals and main_test.go's login/OTP fixtures.
// These are structural fixture evidence, NOT a verified live ACB login contract.
// No SafeKey push/QR, arbitrary selector, frame access or raw bank error classification.
const automationDOMLibrary = `
function inspectRecovery(fixture) {
 const visible=e=>!!e && !e.disabled && getComputedStyle(e).visibility==='visible' && getComputedStyle(e).display!=='none' && e.getClientRects().length>0 && e.getBoundingClientRect().width>0 && e.getBoundingClientRect().height>0;
 const result={state:'UNKNOWN',reason:'UNRECOGNIZED_PAGE',fingerprint:''};
 if(window.top!==window || [...document.querySelectorAll('iframe,frame')].some(visible)){result.reason='FRAME_UNSUPPORTED';return result;}
 if(!window.__acbRecoveryNodes){window.__acbRecoveryNodes={ids:new WeakMap(),next:1,document:crypto.randomUUID()};}
 const n=window.__acbRecoveryNodes; const id=e=>{if(!e)return 0;if(!n.ids.has(e))n.ids.set(e,n.next++);return n.ids.get(e)};
 const all=sel=>[...document.querySelectorAll(sel)];
 const pw=all('input[type="password"]'), users=all('input[name="username" i],input[name="user" i],input[id="username" i],input[id="user" i]'), caps=all('input[name*="captcha" i],input[id*="captcha" i]'), otps=all('input[name*="otp" i],input[id*="otp" i],input[name*="authcode" i],input[id*="authcode" i]');
 const safe=all('input[name*="safekey" i],input[id*="safekey" i]');
 if(safe.length){result.state='UNSUPPORTED_CHALLENGE';result.reason='UNSUPPORTED_CHALLENGE';return result;}
 if(fixture){const code=document.body.dataset.recoveryCode; if(['CREDENTIALS_REJECTED','ACCOUNT_LOCKED','MAINTENANCE','UNSUPPORTED_CHALLENGE'].includes(code)){result.state=code==='MAINTENANCE'?'MAINTENANCE':code==='UNSUPPORTED_CHALLENGE'?'UNSUPPORTED_CHALLENGE':'LOGIN_REJECTED';result.reason=code;result.fingerprint=n.document+':'+code;return result;}}
 let state,controls=[];
 if(pw.length){state='LOGIN_FORM'; if(pw.length!==1 || users.length!==1 || caps.length>1 || otps.length){result.reason='AMBIGUOUS_CONTROLS';return result;} result.password=pw[0];result.username=users[0];result.captcha=caps[0];controls=[...pw,...users,...caps];}
 else if(otps.length){state='OTP_REQUIRED';if(otps.length!==1 || caps.length){result.reason='AMBIGUOUS_CONTROLS';return result;} result.otp=otps[0];controls=otps;}
 else if(caps.length){state='CAPTCHA_REQUIRED';if(caps.length!==1){result.reason='AMBIGUOUS_CONTROLS';return result;}result.captcha=caps[0];controls=caps;}
 else return result;
 const form=controls[0].form;
 if(!form || controls.some(e=>e.form!==form || !visible(e) || e.readOnly)){result.reason='UNSAFE_CONTROLS';return result;}
 if(new URL(form.action,location.href).origin!==location.origin){result.reason='WRONG_FORM_ORIGIN';return result;}
 const submits=[...form.querySelectorAll('button:not([type]),button[type="submit"],input[type="submit"]')];
 if(submits.length!==1 || !visible(submits[0])){result.reason='AMBIGUOUS_SUBMIT';return result;}
 result.form=form;result.submit=submits[0];
 let image=null;
 if(result.captcha){const images=[...form.querySelectorAll('img[src*="captcha" i],img[id*="captcha" i],canvas[id*="captcha" i],canvas[class*="captcha" i]')];if(images.length!==1 || !visible(images[0])){result.reason='UNSAFE_CAPTCHA_CROP';return result;}image=images[0];if(image.tagName==='IMG' && (!image.complete || !image.naturalWidth)){result.reason='CAPTCHA_LOADING';return result;}const b=image.getBoundingClientRect();if(b.x<0 || b.y<0 || b.right>innerWidth || b.bottom>innerHeight){result.reason='UNSAFE_CAPTCHA_CROP';return result;}for(const [x,y] of [[b.x+1,b.y+1],[b.right-1,b.bottom-1],[b.x+b.width/2,b.y+b.height/2]]){if(document.elementFromPoint(x,y)!==image){result.reason='UNSAFE_CAPTCHA_CROP';return result;}}result.rect={x:b.x+scrollX,y:b.y+scrollY,width:b.width,height:b.height};}
 // No control values in the fingerprint. DOM replacement, navigation, image changes and validation text advance it.
 const hash=text=>{let h=2166136261;for(const c of text){h=Math.imul(h^c.charCodeAt(0),16777619)}return h>>>0};
 const validation=[...form.querySelectorAll('[role="alert"],[aria-invalid="true"]')].map(e=>[id(e),hash(e.textContent||''),e.getAttribute('aria-invalid')]);
 result.fingerprint=JSON.stringify([n.document,location.origin,location.pathname,state,id(form),controls.map(e=>[id(e),e.name,e.type,e.maxLength,e.pattern]),id(result.submit),id(image),image?.getAttribute('src'),validation]);
 if(validation.length && state!=='CAPTCHA_REQUIRED'){result.state='UNKNOWN';result.reason='UNRECOGNIZED_REJECTION';return result;}
 result.state=state;result.reason='';return result;
}
`

// Exact AccountNbr only. AccountMasked and arbitrary first links are intentionally excluded.
const automaticHistoryScript = `(account => {
 const allowed=new Set(['dse_applicationId','dse_operationName','dse_pageId','dse_processorState','dse_errorPage','dse_nextEventName','dse_sessionId','dse_processorId','dse_processorIdForGenMenu','AccountNbr','virtualAccount','storeName','CheckRef','EdtRef','CheckDoiUng','activeDatetimeYN','FromDate','ToDate']);
 const matches=[];
 for(const form of document.forms){const accountFields=[...form.elements].filter(e=>e.name==='AccountNbr' && !e.disabled);if(accountFields.length!==1)continue;const el=accountFields[0];if(el.tagName==='SELECT'){const choices=[...el.options].filter(o=>o.value===account && !o.disabled);if(choices.length!==1)continue;if(el.value!==account){el.value=account;el.dispatchEvent(new Event('change',{bubbles:true}));return {action:'',fields:{},pending:true};}}if(el.value!==account)continue;const fields={};let duplicate=false;for(const e of form.elements){if(!e.name || !allowed.has(e.name) || e.disabled)continue;if(e.name in fields){duplicate=true;break}fields[e.name]=e.value||'';}if(!duplicate && fields.dse_operationName==='ibkacctDetailProc')matches.push({action:form.action,fields});}
 if(matches.length===1)return matches[0];
 if(matches.length===0 && !window.__acbRecoveryAccountNavigation){const links=[...document.querySelectorAll('a[href]')].filter(e=>{const u=new URL(e.href,location.href);return u.origin===location.origin && u.searchParams.get('AccountNbr')===account && /ibkacctDetailProc/i.test(u.search)});if(links.length===1 && links[0].getClientRects().length){window.__acbRecoveryAccountNavigation=true;links[0].click();return {action:'',fields:{},pending:true};}}
 return {action:'',fields:{}};
})`
