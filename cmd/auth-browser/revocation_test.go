package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/fetch"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

func TestBrowserSessionRevocation(t *testing.T) {
	if os.Getenv("ACB_BROWSER_INTEGRATION") != "1" {
		t.Skip("opt in with ACB_BROWSER_INTEGRATION=1")
	}
	browser := findDefaultBrowser()
	if _, err := exec.LookPath(browser); err != nil {
		t.Fatal("opt-in browser unavailable: set BROWSER_BIN to installed Chromium")
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "recovery", "revocation-authenticated.html"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"confirmed", "active", "already-expired", "local-cookie-deletion", "unsupported", "ambiguous", "off-origin", "iframe", "confirmation", "background-post", "form-background-post"} {
		t.Run(mode, func(t *testing.T) {
			var mu sync.Mutex
			valid := mode != "already-expired"
			clicks, forbiddenPosts, outside := 0, 0, 0
			var protectedTokens []string
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { mu.Lock(); outside++; mu.Unlock() }))
			defer foreign.Close()
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				w.Header().Set("Cache-Control", "no-store")
				if r.Method != "GET" {
					body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
					if mode == "form-background-post" && r.Method == "POST" && r.URL.Path == "/acbib/fixture-revoke" && string(body) == "operation=fixture-revoke&confirm=yes" {
						clicks++
						valid = false
						_, _ = w.Write([]byte(`<html><body><input type="password"><button>Đăng nhập</button></body></html>`))
						return
					}
					forbiddenPosts++
					w.WriteHeader(405)
					return
				}
				cookie, _ := r.Cookie("fixture_session")
				token := ""
				if cookie != nil {
					token = cookie.Value
				}
				if r.URL.Path == "/acbib/logout" {
					clicks++
					if mode != "local-cookie-deletion" {
						valid = false
					}
					http.SetCookie(w, &http.Cookie{Name: "fixture_session", Value: "", Path: "/", MaxAge: -1})
					_, _ = w.Write([]byte(`<html><body><input type="password"><button>Đăng nhập</button></body></html>`))
					return
				}
				protectedTokens = append(protectedTokens, token)
				if !valid || (token != "synthetic-seed" && token != "synthetic-rotated") {
					_, _ = w.Write([]byte(`<html><body><input type="password"><button>Đăng nhập</button></body></html>`))
					return
				}
				// Prove the snapshot is taken after the read-only probe rotates cookies.
				if token == "synthetic-seed" {
					http.SetCookie(w, &http.Cookie{Name: "fixture_session", Value: "synthetic-rotated", Path: "/", HttpOnly: true})
				}
				html := string(fixture)
				switch mode {
				case "unsupported":
					html = strings.ReplaceAll(html, `<a href="/acbib/logout">Đăng xuất</a>`, `<span>Đăng xuất</span><button onclick="fetch('/arbitrary')">Transfer</button>`)
				case "ambiguous":
					html = strings.ReplaceAll(html, `</body>`, `<button>Logout</button></body>`)
				case "off-origin":
					html = strings.ReplaceAll(html, `/acbib/logout`, foreign.URL+`/logout`)
				case "iframe":
					html = strings.ReplaceAll(html, `</body>`, `<iframe src="about:blank"></iframe></body>`)
				case "confirmation":
					html = strings.ReplaceAll(html, `<a href="/acbib/logout">Đăng xuất</a>`, `<button onclick="if(confirm('Xác nhận đăng xuất?'))location.href='/acbib/logout'">Đăng xuất</button>`)
				case "background-post":
					html = strings.ReplaceAll(html, `<a href="/acbib/logout">Đăng xuất</a>`, `<a href="/acbib/logout" onclick="navigator.sendBeacon('/acbib/unrelated','operation=transfer');fetch('/acbib/logout',{method:'POST',body:'operation=transfer',keepalive:true}).catch(()=>{})">Đăng xuất</a>`)
				case "form-background-post":
					html = strings.ReplaceAll(html, `<a href="/acbib/logout">Đăng xuất</a>`, `<form method="post" action="/acbib/fixture-revoke" onsubmit="navigator.sendBeacon('/acbib/unrelated','operation=transfer');fetch('/acbib/fixture-revoke',{method:'POST',body:'operation=transfer',keepalive:true}).catch(()=>{})"><input type="hidden" name="operation" value="fixture-revoke"><button type="submit" name="confirm" value="yes">Đăng xuất</button></form>`)
				}
				_, _ = w.Write([]byte(html))
			}))
			defer upstream.Close()
			s := &server{profiles: t.TempDir(), browserExec: browser, extraFlags: []string{"--headless=new"}, revokeOrigin: upstream.URL, internalToken: "fixture-internal", internalAuthRequired: true}
			mux := http.NewServeMux()
			s.registerRevocation(mux)
			api := httptest.NewServer(mux)
			defer api.Close()
			defer s.stopCurrent()
			u, _ := url.Parse(upstream.URL)
			h := authbrowser.Handoff{Version: 1, URL: upstream.URL + "/acbib/protected", Action: upstream.URL + "/acbib/history", Cookies: []authbrowser.Cookie{{Name: "fixture_session", Value: "synthetic-seed", Domain: u.Hostname(), Path: "/", HTTPOnly: true}}}
			encoded, err := authbrowser.EncodeHandoff(h, []byte("fixture-nonce"))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			attempt := ""
			if mode == "active" {
				port, err := allocateFreePort()
				if err != nil {
					t.Fatal(err)
				}
				profile, err := newProfileDirName()
				if err != nil {
					t.Fatal(err)
				}
				launchCtx, launchCancel := context.WithCancel(ctx)
				item := &browserSession{AttemptID: "active-fixture", Status: "AWAITING_USER_LOGIN", ExpiresAt: time.Now().Add(time.Minute), profile: profile, cancel: launchCancel, debugURL: fmt.Sprintf("http://127.0.0.1:%d", port), done: make(chan struct{})}
				s.session = item
				ready := make(chan error, 1)
				go s.launch(launchCtx, item, port, ready, browserLaunchOptions{InitialURL: "about:blank", ObserveLogin: false})
				if err := <-ready; err != nil {
					t.Fatal(err)
				}
				alloc, allocCancel := chromedp.NewRemoteAllocator(ctx, item.debugURL)
				tab, tabCancel := chromedp.NewContext(alloc)
				if err := chromedp.Run(tab, network.SetCookies(revokeCookieParams(h.Cookies)), chromedp.Navigate(h.URL)); err != nil {
					t.Fatal(err)
				}
				// Detach without closing the active page: revoke must discover it itself.
				chromedp.FromContext(tab).Target.TargetID = ""
				tabCancel()
				allocCancel()
				attempt, encoded = "active-fixture", ""
			}
			result, err := authbrowser.NewClient(api.URL, "fixture-internal").RevokeSession(ctx, "revoke-fixture", attempt, encoded)
			if err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			defer mu.Unlock()
			want, wantClicks := "UNCONFIRMED", 0
			if mode == "confirmed" || mode == "active" || mode == "confirmation" || mode == "background-post" || mode == "form-background-post" {
				want, wantClicks = "CONFIRMED", 1
			}
			if mode == "already-expired" {
				want = "ALREADY_EXPIRED"
			}
			if mode == "local-cookie-deletion" {
				wantClicks = 1
			}
			if result.Status != want || clicks != wantClicks || forbiddenPosts != 0 || outside != 0 {
				t.Fatalf("result=%+v clicks=%d forbiddenPosts=%d offOrigin=%d; want status=%s clicks=%d", result, clicks, forbiddenPosts, outside, want, wantClicks)
			}
			if wantClicks == 1 && (len(protectedTokens) < 2 || protectedTokens[len(protectedTokens)-1] != "synthetic-rotated") {
				t.Fatal("isolated post-probe did not use the frozen, rotated pre-click cookie")
			}
			payload, _ := json.Marshal(result)
			if bytes.Contains(payload, []byte("synthetic-")) || bytes.Contains(payload, []byte("handoff")) {
				t.Fatal("revocation response leaked snapshot")
			}
			if s.session != nil {
				t.Fatal("revocation left active browser reachable")
			}
		})
	}
}

func TestRevocationAdmissionRejectsUnauthorizedAndUnsafeSnapshots(t *testing.T) {
	s := &server{internalToken: "fixture-internal"} // Mandatory even when development auth flag is false.
	mux := http.NewServeMux()
	s.registerRevocation(mux)
	for _, input := range []struct {
		token, body string
		status      int
	}{
		{"", `{"operationId":"logout"}`, 401},
		{"wrong", `{"operationId":"logout"}`, 401},
		{"fixture-internal", `{"operationId":"logout","extra":true}`, 400},
		{"fixture-internal", `{"operationId":"logout","handoff":"` + strings.Repeat("x", 64<<10) + `"}`, 400},
	} {
		r := httptest.NewRequest("POST", "/session-revocations", strings.NewReader(input.body))
		r.Header.Set(authbrowser.InternalTokenHeader, input.token)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		if w.Code != input.status {
			t.Fatalf("admission returned %d, want %d", w.Code, input.status)
		}
	}
	for _, h := range []authbrowser.Handoff{
		{URL: "http://online.acb.com.vn/acbib/protected", Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: "acb.com.vn"}}},
		{URL: "https://online.acb.com.vn/acbib/protected", Action: "https://evil.test/logout", Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: "acb.com.vn"}}},
		{URL: "https://online.acb.com.vn/acbib/protected", Cookies: []authbrowser.Cookie{{Name: "session", Value: "synthetic", Domain: "evil-acb.com.vn"}}},
	} {
		if s.validateRevokeHandoff(h) {
			t.Fatal("unsafe handoff was accepted")
		}
	}
}

func TestRevokeFormPermitRejectsUnrelatedAndDuplicateRequests(t *testing.T) {
	permit := &revokeFormPermit{URL: "https://online.acb.com.vn/acbib/dynamic-operation", Method: "POST", Body: "operation=logout&confirm=yes"}
	request := func(rawURL, method, body string, kind network.ResourceType) *fetch.EventRequestPaused {
		return &fetch.EventRequestPaused{ResourceType: kind, Request: &network.Request{URL: rawURL, Method: method, PostDataEntries: []*network.PostDataEntry{{Bytes: base64.StdEncoding.EncodeToString([]byte(body))}}}}
	}
	for _, event := range []*fetch.EventRequestPaused{
		request(permit.URL, "POST", permit.Body, network.ResourceTypeFetch),
		request(permit.URL, "POST", "operation=transfer&confirm=yes", network.ResourceTypeDocument),
		request("https://online.acb.com.vn/acbib/transfer", "POST", permit.Body, network.ResourceTypeDocument),
		request(permit.URL, "PUT", permit.Body, network.ResourceTypeDocument),
	} {
		if permit.allows(event) {
			t.Fatal("unrelated request consumed logout permit")
		}
	}
	matching := request(permit.URL, "POST", permit.Body, network.ResourceTypeDocument)
	if !permit.allows(matching) {
		t.Fatal("validated native logout form was blocked")
	}
	if permit.allows(matching) {
		t.Fatal("logout form permit authorized a duplicate request")
	}
}
