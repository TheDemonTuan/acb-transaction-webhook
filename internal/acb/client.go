package acb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/thedemontuan/acb-transaction-webhook/internal/authbrowser"
)

const (
	OfficialHost         = "online.acb.com.vn"
	DefaultClientTimeout = 30 * time.Second
	DefaultUserAgent     = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/133.0.0.0 Safari/537.36"
)

var ErrAuthenticatedFormStateUnavailable = errors.New("ACB authenticated form state is unavailable")

type Client struct {
	baseURL         *url.URL
	bootstrap       *url.URL
	bootstrapFields map[string]string
	http            *http.Client
	mu              sync.Mutex
	now             func() time.Time
	location        *time.Location
	historyDiagnostics map[string]int
}

type Response struct {
	URL              string
	StatusCode       int
	Body             string
	Kind             PageKind
	ClassifierReason string
}

func NewClient(base string, transport http.RoundTripper) (*Client, error) {
	parsed, err := url.Parse(base)
	if err != nil || parsed.Scheme != "https" || !strings.EqualFold(parsed.Hostname(), OfficialHost) {
		return nil, errors.New("ACB base URL must be https://online.acb.com.vn")
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, err
	}
	if transport == nil {
		if defaultTr, ok := http.DefaultTransport.(*http.Transport); ok {
			cloned := defaultTr.Clone()
			cloned.IdleConnTimeout = 30 * time.Second
			cloned.MaxIdleConns = 16
			cloned.MaxIdleConnsPerHost = 4
			cloned.ResponseHeaderTimeout = 25 * time.Second
			cloned.TLSHandshakeTimeout = 10 * time.Second
			transport = cloned
		} else {
			transport = http.DefaultTransport
		}
	}
	location := time.FixedZone("Asia/Ho_Chi_Minh", 7*60*60)
	return &Client{baseURL: parsed, now: time.Now, location: location, http: &http.Client{Jar: jar, Timeout: DefaultClientTimeout, Transport: transport, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		if !strings.EqualFold(req.URL.Hostname(), OfficialHost) {
			return errors.New("ACB redirect leaves official host")
		}
		if req.URL.Scheme == "https" && req.URL.Port() == "443" {
			req.URL.Host = req.URL.Hostname()
		}
		return nil
	}}}, nil
}

func isAllowedACBCookieDomain(domain string) bool {
	d := strings.ToLower(strings.TrimPrefix(domain, "."))
	return d == "" || d == OfficialHost || d == "acb.com.vn"
}

// RestoreCookies accepts only cookies bound to the official ACB host. The
// caller supplies encrypted storage; no cookie ever crosses the dashboard API.
func (c *Client) RestoreSession(handoff authbrowser.Handoff) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.restoreCookies(handoff.Cookies); err != nil {
		return err
	}
	c.bootstrap = nil
	c.bootstrapFields = nil
	if handoff.URL != "" {
		bootstrap, err := c.endpoint(handoff.URL)
		if err != nil {
			return errors.New("invalid ACB session bootstrap URL")
		}
		c.bootstrap = bootstrap
	}
	if handoff.Action != "" {
		action, err := c.endpoint(handoff.Action)
		if err != nil {
			return errors.New("invalid ACB session form action")
		}
		if handoff.Fields["dse_sessionId"] == "" || handoff.Fields["dse_processorState"] == "" {
			return errors.New("ACB session form state is incomplete")
		}
		c.bootstrap = action
		c.bootstrapFields = cloneFields(handoff.Fields)
	}
	return nil
}

func (c *Client) RestoreCookies(cookies []authbrowser.Cookie) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.restoreCookies(cookies)
}

func (c *Client) restoreCookies(cookies []authbrowser.Cookie) error {
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Value == "" || !isAllowedACBCookieDomain(cookie.Domain) {
			return errors.New("invalid ACB session cookie")
		}
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	base := *c.baseURL
	for _, cookie := range cookies {
		jar.SetCookies(&base, []*http.Cookie{{Name: cookie.Name, Value: cookie.Value, Domain: cookie.Domain, Path: cookie.Path, Expires: cookie.Expires, Secure: cookie.Secure, HttpOnly: cookie.HTTPOnly}})
	}
	c.http.Jar = jar
	return nil
}

func (c *Client) SnapshotSession() (authbrowser.Handoff, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bootstrap == nil || len(c.bootstrapFields) == 0 {
		return authbrowser.Handoff{}, ErrAuthenticatedFormStateUnavailable
	}
	cookies := c.http.Jar.Cookies(c.bootstrap)
	snapshot := authbrowser.Handoff{
		Version: 1,
		URL:     c.bootstrap.String(),
		Action:  c.bootstrap.String(),
		Fields:  cloneFields(c.bootstrapFields),
		Cookies: make([]authbrowser.Cookie, 0, len(cookies)),
	}
	for _, cookie := range cookies {
		if cookie.Name == "" || cookie.Value == "" {
			continue
		}
		snapshot.Cookies = append(snapshot.Cookies, authbrowser.Cookie{Name: cookie.Name, Value: cookie.Value, Domain: OfficialHost, Path: "/", Expires: cookie.Expires, Secure: true, HTTPOnly: cookie.HttpOnly})
	}
	if len(snapshot.Cookies) == 0 {
		return authbrowser.Handoff{}, errors.New("ACB session has no cookies")
	}
	return snapshot, nil
}

func (c *Client) updateFormState(response Response) {
	if response.Kind != AccountDetailPage && response.Kind != HistoryPage {
		return
	}
	form, err := ExtractHistoryForm(response.Body)
	if err != nil {
		return
	}
	action, err := c.endpoint(form.Action)
	if err != nil || form.Fields["dse_sessionId"] == "" || form.Fields["dse_processorState"] == "" {
		return
	}
	c.bootstrap = action
	c.bootstrapFields = cloneFields(form.Fields)
}

func cloneFields(fields map[string]string) map[string]string {
	cloned := make(map[string]string, len(fields))
	for key, value := range fields {
		cloned[key] = value
	}
	return cloned
}

func (c *Client) Bootstrap(ctx context.Context) (Response, error) {
	return c.bootstrapForDate(ctx, "")
}

// BootstrapForDate keeps a realtime poll on one immutable local day.
func (c *Client) BootstrapForDate(ctx context.Context, date string) (Response, error) {
	if err := validateHistoryDateRange(date, date); err != nil {
		return Response{}, err
	}
	return c.bootstrapForDate(ctx, date)
}

func isAuthChallengeKind(kind PageKind) bool {
	return kind == LoginPage || kind == OTPChallenge || kind == CaptchaPage
}

func (c *Client) probeAuthLocked(ctx context.Context) (Response, bool, error) {
	probeResp, getErr := c.getLocked(ctx, "/acbib/Request")
	if getErr != nil {
		return probeResp, false, getErr
	}
	if probeResp.StatusCode == http.StatusUnauthorized || probeResp.StatusCode == http.StatusForbidden || isAuthChallengeKind(probeResp.Kind) {
		return probeResp, false, &AuthFailure{Kind: probeResp.Kind, Reason: probeResp.ClassifierReason}
	}
	if probeResp.StatusCode >= 500 || probeResp.StatusCode == http.StatusTooManyRequests || probeResp.Kind == MaintenancePage || probeResp.Kind == UnknownPage {
		return probeResp, false, ErrInconclusiveAuth
	}
	if probeResp.Kind == AccountDetailPage || probeResp.Kind == HistoryPage {
		form, extractErr := ExtractHistoryForm(probeResp.Body)
		if extractErr != nil || form.Fields["dse_sessionId"] == "" || form.Fields["dse_processorState"] == "" {
			return probeResp, false, ErrInconclusiveAuth
		}
		action, actErr := c.endpoint(form.Action)
		if actErr != nil {
			return probeResp, false, ErrInconclusiveAuth
		}
		c.bootstrap = action
		c.bootstrapFields = cloneFields(form.Fields)
		return probeResp, true, nil
	}
	return probeResp, false, ErrInconclusiveAuth
}

func (c *Client) bootstrapForDate(ctx context.Context, date string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bootstrap == nil || len(c.bootstrapFields) == 0 {
		probeResp, resynced, err := c.probeAuthLocked(ctx)
		if err != nil {
			var authFail *AuthFailure
			if errors.As(err, &authFail) {
				return probeResp, err
			}
			return Response{}, ErrAuthenticatedFormStateUnavailable
		}
		if !resynced {
			if date != "" && probeResp.Kind == HistoryPage {
				return Response{}, ErrAuthenticatedFormStateUnavailable
			}
			return probeResp, nil
		}
	}
	var fields map[string]string
	var err error
	if date != "" {
		fields, err = PrepareHistoryFieldsForDate(c.bootstrapFields, date)
	} else {
		fields, err = PrepareHistoryFields(c.bootstrapFields, c.now(), c.location)
	}
	if err != nil {
		return Response{}, err
	}
	values := url.Values{}
	for key, value := range fields {
		values.Set(key, value)
	}
	slog.Debug("requesting ACB history", "request_path", SafePath(c.bootstrap.String()), "from_date", fields["FromDate"], "to_date", fields["ToDate"])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bootstrap.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return Response{}, err
	}
	c.logHistoryContract(fields, resp)
	if isAuthChallengeKind(resp.Kind) {
		probeResp, resynced, probeErr := c.probeAuthLocked(ctx)
		if probeErr != nil {
			var authFail *AuthFailure
			if errors.As(probeErr, &authFail) {
				return probeResp, probeErr
			}
			return resp, probeErr
		}
		if !resynced {
			return resp, ErrInconclusiveAuth
		}
		slog.Info("ACB session state resynchronized after conversational token rejected", "classifier_reason", probeResp.ClassifierReason)
		return probeResp, nil
	}
	return resp, nil
}

func (c *Client) History(ctx context.Context, endpoint string, fields map[string]string) (Response, error) {
	return c.historyForDate(ctx, endpoint, fields, "")
}

// HistoryForDate preserves the realtime poll day while retaining server navigation state.
func (c *Client) HistoryForDate(ctx context.Context, endpoint string, fields map[string]string, date string) (Response, error) {
	if err := validateHistoryDateRange(date, date); err != nil {
		return Response{}, err
	}
	return c.historyForDate(ctx, endpoint, fields, date)
}

func (c *Client) historyForDate(ctx context.Context, endpoint string, fields map[string]string, date string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	isContinuation := fields != nil && fields["_raw"] == "true"
	var hFields map[string]string
	var err error
	if date != "" {
		hFields, err = PrepareHistoryFieldsForDate(fields, date)
	} else {
		hFields, err = PrepareHistoryFields(fields, c.now(), c.location)
	}
	if err != nil {
		return Response{}, err
	}
	if fields != nil && fields["AccountNbr"] != "" {
		if c.bootstrapFields != nil {
			c.bootstrapFields["AccountNbr"] = fields["AccountNbr"]
		}
	} else if c.bootstrapFields != nil && c.bootstrapFields["AccountNbr"] != "" {
		if hFields["AccountNbr"] == "" {
			hFields["AccountNbr"] = c.bootstrapFields["AccountNbr"]
		}
	}
	if !isContinuation || hFields["dse_nextEventName"] == "" {
		hFields["dse_nextEventName"] = "byDate"
	}
	hFields["activeDatetimeYN"] = "N"
	delete(hFields, "activeDatetimeByMonth")
	delete(hFields, "MonthCurr")
	delete(hFields, "YearCurr")
	requestURL, err := c.endpoint(endpoint)
	if err != nil {
		return Response{}, err
	}
	values := url.Values{}
	for key, value := range hFields {
		values.Set(key, value)
	}
	slog.Debug("requesting ACB history", "request_path", SafePath(requestURL.String()), "from_date", hFields["FromDate"], "to_date", hFields["ToDate"])
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return Response{}, err
	}
	c.logHistoryContract(hFields, resp)
	if isAuthChallengeKind(resp.Kind) {
		probeResp, resynced, probeErr := c.probeAuthLocked(ctx)
		if probeErr != nil {
			var authFail *AuthFailure
			if errors.As(probeErr, &authFail) {
				return probeResp, probeErr
			}
			return resp, probeErr
		}
		if !resynced {
			return resp, ErrInconclusiveAuth
		}
		slog.Info("ACB session state resynchronized after conversational token rejected in history", "classifier_reason", probeResp.ClassifierReason)
		if isContinuation {
			return probeResp, ErrConversationReset
		}
		// Page 1 replay: reuse target account and dates from fields
		if fields != nil && fields["AccountNbr"] != "" && c.bootstrapFields != nil {
			c.bootstrapFields["AccountNbr"] = fields["AccountNbr"]
		}
		var replayFields map[string]string
		if date != "" {
			replayFields, err = PrepareHistoryFieldsForDate(c.bootstrapFields, date)
		} else if fields != nil && fields["_explicitRange"] == "true" && fields["FromDate"] != "" && fields["ToDate"] != "" {
			replayFields, err = PrepareHistoryFieldsWithRange(c.bootstrapFields, fields["FromDate"], fields["ToDate"])
		} else {
			replayFields, err = PrepareHistoryFields(c.bootstrapFields, c.now(), c.location)
		}
				if err != nil {
					return probeResp, fmt.Errorf("prepare history replay after conversation resync: %w", err)
				}
				if c.bootstrapFields != nil && c.bootstrapFields["AccountNbr"] != "" && replayFields["AccountNbr"] == "" {
					replayFields["AccountNbr"] = c.bootstrapFields["AccountNbr"]
				}
				replayFields["dse_nextEventName"] = "byDate"
				replayFields["activeDatetimeYN"] = "N"
				delete(replayFields, "activeDatetimeByMonth")
				delete(replayFields, "MonthCurr")
				delete(replayFields, "YearCurr")
				replayVals := url.Values{}
			for k, v := range replayFields {
				replayVals.Set(k, v)
			}
			replayURL := c.bootstrap
			if replayURL == nil {
				replayURL = requestURL
			}
			replayReq, err := http.NewRequestWithContext(ctx, http.MethodPost, replayURL.String(), strings.NewReader(replayVals.Encode()))
			if err != nil {
				return probeResp, fmt.Errorf("prepare history replay request after conversation resync: %w", err)
			}
			replayReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			replayResp, replayErr := c.do(replayReq)
			if replayErr != nil {
				return Response{}, replayErr
			}
			c.logHistoryContract(replayFields, replayResp)
			if isAuthChallengeKind(replayResp.Kind) {
				return replayResp, &AuthFailure{Kind: replayResp.Kind, Reason: replayResp.ClassifierReason}
			}
			return replayResp, nil
	}
	return resp, nil
}

func (c *Client) CloseIdleConnections() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeIdleConnectionsLocked()
}

func (c *Client) closeIdleConnectionsLocked() {
	if tr, ok := c.http.Transport.(interface{ CloseIdleConnections() }); ok {
		tr.CloseIdleConnections()
	}
}

func (c *Client) Get(ctx context.Context, endpoint string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.getLocked(ctx, endpoint)
}

func (c *Client) getLocked(ctx context.Context, endpoint string) (Response, error) {
	var requestURL *url.URL
	if endpoint == "" && c.bootstrap != nil {
		copy := *c.bootstrap
		requestURL = &copy
	} else {
		var err error
		requestURL, err = c.endpoint(endpoint)
		if err != nil {
			return Response{}, err
		}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, requestURL.String(), nil)
	if err != nil {
		return Response{}, err
	}
	return c.do(req)
}

func (c *Client) endpoint(endpoint string) (*url.URL, error) {
	if !strings.HasPrefix(endpoint, "https://") && !strings.HasPrefix(endpoint, "http://") {
		if !strings.HasPrefix(endpoint, "/") {
			endpoint = "/" + endpoint
		}
		if !strings.HasPrefix(endpoint, "/acbib") {
			endpoint = "/acbib" + endpoint
		}
	}
	requestURL, err := c.baseURL.Parse(endpoint)
	if err != nil || !strings.EqualFold(requestURL.Hostname(), OfficialHost) {
		return nil, errors.New("invalid ACB endpoint")
	}
	return requestURL, nil
}

func (c *Client) do(req *http.Request) (Response, error) {
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", DefaultUserAgent)
	}
	if req.Header.Get("Accept") == "" {
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8")
	}
	if req.Header.Get("Accept-Language") == "" {
		req.Header.Set("Accept-Language", "vi-VN,vi;q=0.9,en-US;q=0.8,en;q=0.7")
	}
	if req.Header.Get("Referer") == "" {
		req.Header.Set("Referer", "https://online.acb.com.vn/acbib/Request")
	}
	if req.Header.Get("Origin") == "" && req.Method == http.MethodPost {
		req.Header.Set("Origin", "https://online.acb.com.vn")
	}
	if req.Header.Get("Sec-Ch-Ua") == "" {
		req.Header.Set("Sec-Ch-Ua", `"Not(A:Brand";v="99", "Chromium";v="133", "Google Chrome";v="133"`)
	}
	if req.Header.Get("Sec-Ch-Ua-Mobile") == "" {
		req.Header.Set("Sec-Ch-Ua-Mobile", "?0")
	}
	if req.Header.Get("Sec-Ch-Ua-Platform") == "" {
		req.Header.Set("Sec-Ch-Ua-Platform", `"Linux"`)
	}
	if req.Header.Get("Sec-Fetch-Dest") == "" {
		req.Header.Set("Sec-Fetch-Dest", "document")
	}
	if req.Header.Get("Sec-Fetch-Mode") == "" {
		req.Header.Set("Sec-Fetch-Mode", "navigate")
	}
	if req.Header.Get("Sec-Fetch-Site") == "" {
		req.Header.Set("Sec-Fetch-Site", "same-origin")
	}
	if req.Header.Get("Sec-Fetch-User") == "" {
		req.Header.Set("Sec-Fetch-User", "?1")
	}
	if req.Header.Get("Upgrade-Insecure-Requests") == "" {
		req.Header.Set("Upgrade-Insecure-Requests", "1")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		c.closeIdleConnectionsLocked()
		return Response{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		c.closeIdleConnectionsLocked()
		return Response{}, err
	}
	kind, reason := ClassifyPageWithReason(resp.Request.URL.String(), string(body))
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		kind = LoginPage
		reason = "AUTH_HTTP_STATUS"
	}
	result := Response{URL: resp.Request.URL.String(), StatusCode: resp.StatusCode, Body: string(body), Kind: kind, ClassifierReason: reason}
	c.updateFormState(result)
	return result, nil
}
