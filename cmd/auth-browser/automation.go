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
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/storage"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

func (s *server) registerAutomation(mux *http.ServeMux) {
	mux.Handle("GET /sessions/{attemptID}/observation", s.requireInternal(http.HandlerFunc(s.automationObservation)))
	mux.Handle("GET /sessions/{attemptID}/captcha", s.requireInternal(http.HandlerFunc(s.automationCapture)))
	for _, action := range []string{"login", "captcha", "request-otp", "otp"} {
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
		slog.Warn("ACB automation observation failed", "failure", automationFailureClass(err))
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
		logAutomationActionFailure(action, "before_action_observation", 503, err)
		automationError(w, 503)
		return
	}
	if revision == "" || revision != observation.Revision || item.consumed {
		logAutomationActionFailure(action, "revision_validation", 409, nil)
		automationError(w, 409)
		return
	}
	allowed := (action == "login" && observation.State == authbrowser.LoginForm) || (action == "captcha" && observation.State == authbrowser.CaptchaRequired) || (action == "request-otp" && observation.State == authbrowser.OTPRequestRequired) || (action == "otp" && observation.State == authbrowser.OTPRequired)
	if !allowed {
		logAutomationActionFailure(action, "state_validation", 422, nil)
		automationError(w, 422)
		return
	}
	if action == "login" && (login.Username == "" || login.Password == "" || login.AccountNumber == "") {
		automationError(w, 400)
		return
	}
	if (action == "request-otp" && value != "") || (action == "otp" && !validAutomationAnswer(value, 4, 10, true)) || ((action == "captcha" || observation.CaptchaRequired) && !validAutomationAnswer(value, 1, 16, false)) {
		automationError(w, 400)
		return
	}
	if action == "otp" && dom.OTPLength > 0 && !validAutomationAnswer(value, dom.OTPLength, dom.OTPLength, true) {
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
	script := automationDOMLibrary + "\n(() => { const input=" + string(payload) + "; const d=inspectRecovery(" + fixtureBool(s.fixtureDOM) + `);
 if(d.fingerprint!==input.fingerprint || !d.form || !d.submit) return false;
 if(input.action==='request-otp'){
  if(input.value!=='' || d.state!=='OTP_REQUEST_REQUIRED' || !d.choice) return false;
  if(!d.choice.checked) d.choice.click();
  const current=inspectRecovery(` + fixtureBool(s.fixtureDOM) + `);
  if(current.state!=='OTP_REQUEST_REQUIRED' || current.fingerprint!==input.fingerprint || current.form!==d.form || current.choice!==d.choice || current.submit!==d.submit || !current.choice.checked) return false;
  current.submit.click(); return true;
 }
 if(input.action==='otp' && d.otpLength===6){
  if(d.state!=='OTP_REQUIRED' || !/^[0-9]{6}$/.test(input.value) || d.digits?.length!==6) return false;
  for(let i=0;i<6;i++){
   const el=d.digits[i], key=input.value[i];
   el.focus();
   el.dispatchEvent(new KeyboardEvent('keydown',{key,code:'Digit'+key,keyCode:key.charCodeAt(0),which:key.charCodeAt(0),bubbles:true}));
   el.dispatchEvent(new KeyboardEvent('keypress',{key,code:'Digit'+key,keyCode:key.charCodeAt(0),which:key.charCodeAt(0),charCode:key.charCodeAt(0),bubbles:true}));
   Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(el,key);
   el.dispatchEvent(new InputEvent('input',{bubbles:true,inputType:'insertText',data:key}));
   el.dispatchEvent(new Event('change',{bubbles:true}));
   el.dispatchEvent(new KeyboardEvent('keyup',{key,code:'Digit'+key,keyCode:key.charCodeAt(0),which:key.charCodeAt(0),bubbles:true}));
  }
  const current=inspectRecovery(` + fixtureBool(s.fixtureDOM) + `);
  if(current.state!=='OTP_REQUIRED' || current.otpLength!==6 || current.fingerprint!==input.fingerprint || current.form!==d.form || current.submit!==d.submit || current.cancel!==d.cancel || current.digits.some((el,i)=>el!==d.digits[i])) return false;
  current.submit.click(); return true;
 }
 const fields=input.action==='login'?[[d.username,input.username],[d.password,input.password],...(d.captcha?[[d.captcha,input.value]]:[])]:[[input.action==='otp'?d.otp:d.captcha,input.value]];
 for(const [el,val] of fields){if(!el || (el.maxLength>0 && val.length>el.maxLength)) return false; if(el.pattern){try{if(!(new RegExp('^(?:'+el.pattern+')$')).test(val)) return false;}catch(e){return false;}}}
 for(const [el,val] of fields){Object.getOwnPropertyDescriptor(HTMLInputElement.prototype,'value').set.call(el,val); el.dispatchEvent(new Event('input',{bubbles:true})); el.dispatchEvent(new Event('change',{bubbles:true}));}
 d.submit.click(); return true; })()`
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
		phase := "action_guard"
		if err != nil {
			phase = "action_evaluation"
		}
		logAutomationActionFailure(action, phase, 409, err)
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
			logAutomationActionFailure(action, "after_action_observation", 503, observeErr)
			automationError(w, 503)
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Never log CDP/JavaScript text, URLs, revisions, form values or submitted input.
// A navigation exception may contain the evaluated script and its credentials.
func automationFailureClass(err error) string {
	if err == nil {
		return "GUARD_REJECTED"
	}
	if errors.Is(err, context.Canceled) {
		return "CONTEXT_CANCELLED"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "DEADLINE_EXCEEDED"
	}
	var exception *runtime.ExceptionDetails
	if errors.As(err, &exception) {
		return "JAVASCRIPT_EXCEPTION"
	}
	return "BROWSER_PROTOCOL_FAILURE"
}

func logAutomationActionFailure(action, phase string, status int, err error) {
	slog.Warn("ACB automation action failed", "action", action, "phase", phase, "status", status, "failure", automationFailureClass(err))
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
	OTPLength         int                       `json:"otpLength,omitempty"`
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
		script := automationDOMLibrary + "\n(() => { const d=inspectRecovery(" + fixtureBool(s.fixtureDOM) + "); return {state:d.state,reason:d.reason,fingerprint:d.fingerprint,captchaRequired:!!d.captcha,otpLength:d.otpLength||0,x:d.rect?.x||0,y:d.rect?.y||0,width:d.rect?.width||0,height:d.rect?.height||0}; })()"
		if err := chromedp.Run(tab, chromedp.Evaluate(script, &dom)); err != nil {
			return err
		}
		dom.ActionFingerprint = dom.Fingerprint
		// A CAPTCHA control is not an admitted crop on a rejected/unfinished form.
		// Keep the recognition failure instead of replacing it with crop validation.
		if dom.Captcha && dom.State == authbrowser.Unknown {
			dom.Captcha = false
			return nil
		}
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
	return authbrowser.AuthObservation{State: dom.State, Revision: item.revision, CaptchaRequired: dom.Captcha, OTPLength: dom.OTPLength, ReasonCode: dom.Reason, ExpiresAt: item.ExpiresAt}, dom, png, nil
}

// Only used by the unexported fixture URL injection. No production host/TLS bypass exists.
func fixtureCookies(cookies []*network.Cookie) []authbrowser.Cookie {
	result := make([]authbrowser.Cookie, 0, len(cookies))
	for _, cookie := range cookies {
		result = append(result, authbrowser.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Secure: cookie.Secure, HTTPOnly: cookie.HTTPOnly})
	}
	return result
}

// The loginOp/UserName/PassWord/SecurityCode login, detectLoginNewDeviceProc /
// confirmPage AuthTyp radio, and otpPage six digit controls/native buttons were
// observed on 2026-10-03. Other generic OTP selectors remain fixture-derived.
// No frame access or raw bank error classification is permitted.
const automationDOMLibrary = `
function inspectRecovery(fixture) {
 const visible=e=>!!e && !e.disabled && getComputedStyle(e).visibility==='visible' && getComputedStyle(e).display!=='none' && e.getClientRects().length>0 && e.getBoundingClientRect().width>0 && e.getBoundingClientRect().height>0;
 const result={state:'UNKNOWN',reason:'UNRECOGNIZED_PAGE',fingerprint:''};
 if(window.top!==window){result.reason='FRAME_UNSUPPORTED';return result;}
 const bankForms=[...document.forms].filter(form=>{const action=new URL(form.action,location.href);return form.name==='loginOp' && form.method.toLowerCase()==='post' && action.origin===location.origin && action.pathname==='/acbib/Request';});
 const bankForm=bankForms.length===1?bankForms[0]:null;
 const bankField=(name,id,type)=>bankForm && [...bankForm.elements].filter(e=>e.tagName==='INPUT' && e.name===name && e.id===id && e.type===type);
 const bankUser=bankField('UserName','user-name','text'), bankPassword=bankField('PassWord','password','password'), bankCaptcha=bankField('SecurityCode','security-code','text');
 const bankLogin=bankUser?.length===1 && bankPassword?.length===1 && bankCaptcha?.length===1;
 // ACB embeds a promotional frame outside the login form. Never inspect it or
 // treat any other frame as a supported authentication surface.
 const promotionalFrame=e=>{if(!bankLogin || e.tagName!=='IFRAME' || e.id!=='iframe-banner' || bankForm.contains(e))return false;const u=new URL(e.src,location.href);return u.origin==='https://acb.com.vn' && u.pathname==='/acbo-tin-tuc-acbonline';};
 if([...document.querySelectorAll('iframe,frame')].some(e=>visible(e) && !promotionalFrame(e))){result.reason='FRAME_UNSUPPORTED';return result;}
 if(!window.__acbRecoveryNodes){window.__acbRecoveryNodes={ids:new WeakMap(),next:1,document:crypto.randomUUID()};}
 const n=window.__acbRecoveryNodes; const id=e=>{if(!e)return 0;if(!n.ids.has(e))n.ids.set(e,n.next++);return n.ids.get(e)};
 const all=sel=>[...document.querySelectorAll(sel)];
 const pw=all('input[type="password"]'), users=all('input[name="username" i],input[name="user" i],input[id="username" i],input[id="user" i]'), caps=[...all('input[name*="captcha" i],input[id*="captcha" i]'),...(bankLogin?bankCaptcha:[])], otps=all('input[name*="otp" i],input[id*="otp" i],input[name*="authcode" i],input[id*="authcode" i]');
 // Hidden EdtOtp/resend-otp are not editable code controls. Admit the complete
 // observed split-digit contract before generic OTP or SafeKey heuristics.
 const splitCandidate=all('[id],[name]').some(e=>/^digit-/.test(e.id) || e.id==='EdtOtp' || e.name==='EdtOtp') || all('input[name="dse_processorState"]').some(e=>e.type==='hidden' && e.value==='otpPage');
 if(splitCandidate){
  if(document.querySelector('iframe,frame')){result.reason='FRAME_UNSUPPORTED';return result;}
  const forms=[...document.forms].filter(e=>e.name==='form');
  if(forms.length!==1){result.reason='AMBIGUOUS_CONTROLS';return result;}
  const form=forms[0], action=new URL(form.action,location.href);
  if(action.origin!==location.origin || action.username || action.password){result.reason='WRONG_FORM_ORIGIN';return result;}
  if(form.method.toLowerCase()!=='post' || action.pathname!=='/acbib/Request' || action.search || action.hash){result.reason='UNRECOGNIZED_PAGE';return result;}
  const hidden=(name,value)=>{const fields=[...form.elements].filter(e=>e.name===name);return fields.length===1 && fields[0].tagName==='INPUT' && fields[0].type==='hidden' && !fields[0].disabled && fields[0].value===value?fields[0]:null;};
  const operation=hidden('dse_operationName','detectLoginNewDeviceProc'), processorState=hidden('dse_processorState','otpPage');
  if(!operation || !processorState){result.reason='UNRECOGNIZED_PAGE';return result;}
  const digits=[];
  for(let i=1;i<=6;i++){
   const nodes=all('[id]').filter(e=>e.id==='digit-'+i);
   if(nodes.length!==1 || nodes[0].tagName!=='INPUT' || nodes[0].type!=='text' || nodes[0].name!=='' || nodes[0].form!==form || !visible(nodes[0]) || nodes[0].readOnly || nodes[0].maxLength!==1 || nodes[0].pattern!==''){result.reason='AMBIGUOUS_CONTROLS';return result;}
   digits.push(nodes[0]);
  }
  const codes=all('[id],[name]').filter(e=>e.id==='EdtOtp' || e.name==='EdtOtp');
  if(codes.length!==1 || codes[0].tagName!=='INPUT' || codes[0].type!=='hidden' || codes[0].id!=='EdtOtp' || codes[0].name!=='EdtOtp' || codes[0].form!==form || codes[0].disabled){result.reason='AMBIGUOUS_CONTROLS';return result;}
  const nativeButton=(key,label,handler)=>{const nodes=all('[id],[name]').filter(e=>e.id===key || e.name===key);return nodes.length===1 && nodes[0].tagName==='INPUT' && nodes[0].type==='button' && nodes[0].id===key && nodes[0].name===key && nodes[0].form===form && visible(nodes[0]) && nodes[0].value===label && nodes[0].getAttribute('onclick')===handler?nodes[0]:null;};
  const submit=nativeButton('button','Xác nhận',"submitForm('ok');"), cancel=nativeButton('button2','Hủy',"submitForm('close');");
  if(!submit || !cancel){result.reason='AMBIGUOUS_SUBMIT';return result;}
  const resends=all('[id="resend-otp"]');
  if(resends.length!==1 || resends[0].tagName!=='INPUT' || resends[0].type!=='hidden' || resends[0].name!=='' || resends[0].form!==form || resends[0].disabled){result.reason='AMBIGUOUS_CONTROLS';return result;}
  const resend=resends[0];
  const hiddenNames=new Set(['EdtOtp','dse_sessionId','dse_applicationId','dse_operationName','dse_pageId','dse_processorState','dse_processorId','dse_errorPage','dse_nextEventName','countDownTimeLeft','Certificate','Thumprint']);
  const rendered=e=>getComputedStyle(e).visibility==='visible' && getComputedStyle(e).display!=='none' && e.getClientRects().length>0 && e.getBoundingClientRect().width>0 && e.getBoundingClientRect().height>0;
  const ignored=e=>e===resend || (e.tagName==='INPUT' && e.type==='hidden' && hiddenNames.has(e.name)) || (e.tagName==='TEXTAREA' && ['Signature','PlainText'].includes(e.name) && !rendered(e));
  const controls=[...new Set([...form.elements].filter(e=>['INPUT','SELECT','TEXTAREA','BUTTON'].includes(e.tagName)).concat([...form.querySelectorAll('[role="button"],[onclick]')]))];
  const unexpectedHandler=[form,...form.querySelectorAll('*'),...form.elements].some(e=>[...e.attributes].some(a=>/^on/i.test(a.name) && !((e===submit || e===cancel) && a.name==='onclick')));
  if(controls.some(e=>!digits.includes(e) && e!==submit && e!==cancel && !ignored(e)) || controls.some(e=>ignored(e) && e!==resend && [...form.elements].filter(other=>other.name===e.name).length!==1) || unexpectedHandler || form.querySelector('[role="alert"],[aria-invalid="true"]')){result.reason='AMBIGUOUS_CONTROLS';return result;}
  // Only unrelated navigation/language controls may coexist outside this form.
  const competing=[...pw,...caps,...otps,...all('input[name*="safekey" i],input[id*="safekey" i],input[name*="securitycode" i],input[id*="securitycode" i],input[id^="digit-"]')];
  if(competing.some(e=>visible(e) && !digits.includes(e))){result.reason='AMBIGUOUS_CONTROLS';return result;}
  result.form=form;result.digits=digits;result.submit=submit;result.cancel=cancel;result.otpLength=6;
  // Structural identities only. Never read code, hidden token, or textarea values.
  result.fingerprint=JSON.stringify([n.document,location.origin,location.pathname,'OTP_REQUIRED',id(form),form.name,form.method,action.pathname,id(operation),operation.name,operation.type,id(processorState),processorState.name,processorState.type,id(codes[0]),codes[0].name,codes[0].type,id(resend),resend.name,resend.id,resend.type,digits.map(e=>[id(e),e.id,e.name,e.type,e.maxLength,e.pattern]),[submit,cancel].map(e=>[id(e),e.id,e.name,e.type])]);
  result.state='OTP_REQUIRED';result.reason='';return result;
 }
 const safe=all('input[name*="safekey" i],input[id*="safekey" i]');
 // SafeKey on the observed confirmation is a method radio, not an OTP field.
 // Admit only that complete contract before rejecting other SafeKey challenges.
 const confirmationForms=[...document.forms].filter(form=>form.name==='form');
 const confirmationChoice=safe.filter(e=>e.type==='radio');
 if(confirmationChoice.length){
  if(document.querySelector('iframe,frame')){result.reason='FRAME_UNSUPPORTED';return result;}
  if(confirmationForms.length!==1 || pw.some(visible) || otps.some(visible)){result.reason='AMBIGUOUS_CONTROLS';return result;}
  const form=confirmationForms[0], action=new URL(form.action,location.href);
  if(action.origin!==location.origin || action.username || action.password){result.reason='WRONG_FORM_ORIGIN';return result;}
  if(form.method.toLowerCase()!=='post' || action.pathname!=='/acbib/Request' || action.search || action.hash){result.reason='UNRECOGNIZED_PAGE';return result;}
  const hidden=(name,value)=>{const fields=[...form.elements].filter(e=>e.name===name);return fields.length===1 && fields[0].tagName==='INPUT' && fields[0].type==='hidden' && !fields[0].disabled && fields[0].value===value?fields[0]:null;};
  const operation=hidden('dse_operationName','detectLoginNewDeviceProc'), processorState=hidden('dse_processorState','confirmPage');
  if(!operation || !processorState){result.reason='UNRECOGNIZED_PAGE';return result;}
  const choices=all('input').filter(e=>e.name==='AuthTyp' || e.id==='safekey');
  if(choices.length!==1 || safe.length!==1 || choices[0].name!=='AuthTyp' || choices[0].id!=='safekey' || choices[0].type!=='radio' || choices[0].form!==form || !visible(choices[0])){result.reason='AMBIGUOUS_CONTROLS';return result;}
  const buttons=all('[id],[name]').filter(e=>e.id==='button' || e.name==='button');
  if(buttons.length!==1 || buttons[0].tagName!=='INPUT' || buttons[0].type!=='button' || buttons[0].id!=='button' || buttons[0].name!=='button' || buttons[0].form!==form || !visible(buttons[0]) || buttons[0].value!=='Tiếp tục' || buttons[0].getAttribute('onclick')!=="submitForm('ok');"){result.reason='AMBIGUOUS_SUBMIT';return result;}
  const choice=choices[0], submit=buttons[0];
  // Navigation/language controls outside this form cannot submit its challenge.
  const controls=[...new Set([...form.elements].filter(e=>['INPUT','SELECT','TEXTAREA','BUTTON'].includes(e.tagName)).concat([...form.querySelectorAll('[role="button"],[onclick]')]))].filter(e=>e.tagName!=='INPUT' || e.type!=='hidden');
  if(controls.some(e=>e!==choice && e!==submit) || form.querySelector('[role="alert"],[aria-invalid="true"]')){result.reason='AMBIGUOUS_CONTROLS';return result;}
  result.form=form;result.choice=choice;result.submit=submit;
  // Structural identities only: never include hidden-field or radio/button values,
  // cookie data, session tokens, account data, or validation text.
  result.fingerprint=JSON.stringify([n.document,location.origin,location.pathname,'OTP_REQUEST_REQUIRED',id(form),form.name,form.method,action.pathname,id(operation),operation.name,operation.type,id(processorState),processorState.name,processorState.type,id(choice),choice.name,choice.id,choice.type,id(submit),submit.name,submit.id,submit.type]);
  result.state='OTP_REQUEST_REQUIRED';result.reason='';return result;
 }
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
 const bankSubmit=bankLogin && form===bankForm?[...form.querySelectorAll('a.button-blue.acbone-submit-button[href="#"]')].filter(e=>/^\s*submitFormLogin\(\);\s*sendInsider\('ins_login_start'\);?\s*$/.test(e.getAttribute('onclick')||'')):[];
 const submits=[...form.querySelectorAll('button:not([type]),button[type="submit"],input[type="submit"]'),...bankSubmit];
 if(submits.length!==1 || !visible(submits[0])){result.reason='AMBIGUOUS_SUBMIT';return result;}
 result.form=form;result.submit=submits[0];
 let image=null;
 if(result.captcha){const images=[...form.querySelectorAll('img[src*="captcha" i],img[id*="captcha" i],canvas[id*="captcha" i],canvas[class*="captcha" i]')];if(images.length!==1 || !visible(images[0])){result.reason='UNSAFE_CAPTCHA_CROP';return result;}image=images[0];if(image.tagName==='IMG' && (!image.complete || !image.naturalWidth)){result.reason='CAPTCHA_LOADING';return result;}const b=image.getBoundingClientRect();if(b.x<0 || b.y<0 || b.right>innerWidth || b.bottom>innerHeight){result.reason='UNSAFE_CAPTCHA_CROP';return result;}for(const [x,y] of [[b.x+1,b.y+1],[b.right-1,b.bottom-1],[b.x+b.width/2,b.y+b.height/2]]){if(document.elementFromPoint(x,y)!==image){result.reason='UNSAFE_CAPTCHA_CROP';return result;}}result.rect={x:b.x+scrollX,y:b.y+scrollY,width:b.width,height:b.height};}
 // No control values in the fingerprint. DOM replacement, navigation, image changes and validation text advance it.
 const hash=text=>{let h=2166136261;for(const c of text){h=Math.imul(h^c.charCodeAt(0),16777619)}return h>>>0};
 const validation=[...form.querySelectorAll('[role="alert"],[aria-invalid="true"]')].map(e=>[id(e),hash(e.textContent||''),e.getAttribute('aria-invalid')]);
 result.fingerprint=JSON.stringify([n.document,location.origin,location.pathname,state,id(form),controls.map(e=>[id(e),e.name,e.type,e.maxLength,e.pattern]),id(result.submit),id(image),image?.getAttribute('src'),validation]);
	const captchaCode=fixture && state==='LOGIN_FORM' && result.captcha && ['CAPTCHA_REJECTED','INVALID_CAPTCHA'].includes(document.body.dataset.recoveryCode) ? document.body.dataset.recoveryCode : '';
	if(validation.length && state!=='CAPTCHA_REQUIRED' && !captchaCode){result.state='UNKNOWN';result.reason='UNRECOGNIZED_REJECTION';return result;}
	result.state=state;result.reason=captchaCode;return result;
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
