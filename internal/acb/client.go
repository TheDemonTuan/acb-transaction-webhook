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
	baseURL            *url.URL
	bootstrap          *url.URL
	bootstrapFields    map[string]string
	http               *http.Client
	mu                 sync.Mutex
	operationMu        sync.Mutex
	now                func() time.Time
	location           *time.Location
	historyDiagnostics map[string]int
}

type Response struct {
	URL              string
	StatusCode       int
	Body             string
	Kind             PageKind
	ClassifierReason string
	// RequestedAccount is the outbound AccountNbr for a direct POST response.
	// It is not bank-returned identity and must not be logged.
	RequestedAccount string
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

// WithClock sets the clock used to fence transaction-day requests.
func (c *Client) WithClock(now func() time.Time) *Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = now
	return c
}

func (c *Client) checkTodayLocked(date string) error {
	if date != "" && c.now().In(c.location).Format(historyDateLayout) != date {
		return ErrRealtimeDateRollover
	}
	return nil
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

// SessionOperationMutex serializes fenced operations across loaders sharing this client.
func (c *Client) SessionOperationMutex() *sync.Mutex { return &c.operationMu }

// ClearSession removes all authentication state after any in-flight request finishes.
func (c *Client) ClearSession() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	jar, err := cookiejar.New(nil)
	if err != nil {
		return err
	}
	c.http.Jar = jar
	c.bootstrap = nil
	c.bootstrapFields = nil
	c.historyDiagnostics = nil
	c.http.CloseIdleConnections()
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

// SessionAccountNumber returns the exact account captured in the current form,
// without copying cookies or the complete session handoff.
func (c *Client) SessionAccountNumber() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bootstrapFields["AccountNbr"]
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
	if response.Kind == HistoryPage && form.Fields["AccountNbr"] == "" && c.bootstrapFields["AccountNbr"] != "" {
		// History pages may omit the selection while rotating conversational tokens.
		// Keep it only in request state; the bank response remains unchanged.
		form.Fields["AccountNbr"] = c.bootstrapFields["AccountNbr"]
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

// BootstrapToday submits transaction-day history for the current local day.
func (c *Client) BootstrapToday(ctx context.Context, date string) (Response, error) {
	if err := validateHistoryDateRange(date, date); err != nil {
		return Response{}, err
	}
	return c.bootstrapForDate(ctx, date)
}

func isAuthChallengeKind(kind PageKind) bool {
	return kind == LoginPage || kind == OTPChallenge || kind == CaptchaPage
}

func (c *Client) probeAuthLocked(ctx context.Context) (Response, bool, error) {
	if err := ctx.Err(); err != nil {
		return Response{}, false, err
	}
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
		_, actErr := c.endpoint(form.Action)
		if actErr != nil {
			return probeResp, false, ErrInconclusiveAuth
		}
		// getLocked already installed the validated fresh form via updateFormState.
		return probeResp, true, nil
	}
	return probeResp, false, ErrInconclusiveAuth
}

func (c *Client) bootstrapForDate(ctx context.Context, date string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	requestTime := c.now()
	probed := false
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := c.checkTodayLocked(date); err != nil {
		return Response{}, err
	}
	if c.bootstrap == nil || len(c.bootstrapFields) == 0 {
		probed = true
		probeResp, resynced, err := c.probeAuthLocked(ctx)
		if rollover := c.checkTodayLocked(date); rollover != nil {
			return probeResp, rollover
		}
		if err != nil {
			if ctx.Err() != nil {
				return probeResp, ctx.Err()
			}
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
		fields, err = PrepareTodayHistoryFields(c.bootstrapFields, date)
	} else {
		fields, err = PrepareHistoryFields(c.bootstrapFields, requestTime, c.location)
	}
	if err != nil {
		return Response{}, err
	}
	if err := ctx.Err(); err != nil {
		return Response{}, err
	}
	if err := c.checkTodayLocked(date); err != nil {
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
	resp, err := c.do(req, fields["AccountNbr"])
	if err != nil {
		return Response{}, err
	}
	c.logHistoryContract("bootstrap", fields, resp)
	if err := c.checkTodayLocked(date); err != nil {
		return resp, err
	}
	if isAuthChallengeKind(resp.Kind) {
		if probed {
			return resp, ErrInconclusiveAuth
		}
		probeResp, resynced, probeErr := c.probeAuthLocked(ctx)
		if err := c.checkTodayLocked(date); err != nil {
			return probeResp, err
		}
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
	if resp.StatusCode == http.StatusOK && resp.Kind == AccountDetailPage {
		if _, extractErr := ExtractHistoryForm(resp.Body); extractErr != nil {
			if probed {
				return resp, ErrHistoryUnavailable
			}
			return c.resyncHistoryLocked(ctx, resp, fields, false)
		}
	}
	return resp, nil
}

func (c *Client) History(ctx context.Context, endpoint string, fields map[string]string) (Response, error) {
	return c.historyForDate(ctx, endpoint, fields, "")
}

// HistoryToday retains returned navigation for today's transaction-day query.
func (c *Client) HistoryToday(ctx context.Context, endpoint string, fields map[string]string, date string) (Response, error) {
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
		hFields, err = PrepareTodayHistoryFields(fields, date)
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
	if err := c.checkTodayLocked(date); err != nil {
		return Response{}, err
	}
	resp, err := c.do(req, hFields["AccountNbr"])
	if err != nil {
		return Response{}, err
	}
	c.logHistoryContract("history", hFields, resp)
	if err := c.checkTodayLocked(date); err != nil {
		return resp, err
	}
	if isAuthChallengeKind(resp.Kind) || (resp.StatusCode == http.StatusOK && resp.Kind == AccountDetailPage) {
		return c.resyncHistoryLocked(ctx, resp, hFields, isContinuation)
	}
	return resp, nil
}

// resyncHistoryLocked establishes a fresh conversation once, then replays only
// page one with the original account and range. The caller must hold c.mu.
func (c *Client) resyncHistoryLocked(ctx context.Context, failed Response, prepared map[string]string, isContinuation bool) (Response, error) {
	targetAccount, fromDate, toDate := prepared["AccountNbr"], prepared["FromDate"], prepared["ToDate"]
	today := ""
	if prepared["activeDatetimeYN"] == "Y" {
		today = fromDate
		if err := c.checkTodayLocked(today); err != nil {
			return failed, err
		}
	}
	defer func() {
		if c.bootstrapFields != nil {
			c.bootstrapFields["AccountNbr"] = targetAccount
		}
	}()
	if err := ctx.Err(); err != nil {
		return failed, err
	}
	probeResp, resynced, probeErr := c.probeAuthLocked(ctx)
	c.logHistoryContract("probe", prepared, probeResp)
	if err := c.checkTodayLocked(today); err != nil {
		return probeResp, err
	}
	if probeErr != nil {
		// Authenticated but unusable state is a protocol failure, not expiration.
		if probeResp.StatusCode == http.StatusOK && (probeResp.Kind == AccountDetailPage || probeResp.Kind == HistoryPage) {
			return probeResp, ErrHistoryUnavailable
		}
		return probeResp, probeErr
	}
	if !resynced {
		return probeResp, ErrInconclusiveAuth
	}
	fresh, extractErr := ExtractHistoryForm(probeResp.Body)
	if extractErr != nil || fresh.Fields["dse_operationName"] != "ibkacctDetailProc" || fresh.Fields["dse_sessionId"] == "" || fresh.Fields["dse_processorState"] == "" || c.bootstrap == nil {
		return probeResp, ErrHistoryUnavailable
	}
	slog.Info("ACB history form resynchronized", "kind", probeResp.Kind, "classifier_reason", probeResp.ClassifierReason)
	if isContinuation {
		return probeResp, ErrConversationReset
	}
	if err := ctx.Err(); err != nil {
		return probeResp, err
	}
	if targetAccount == "" {
		return probeResp, ErrHistoryUnavailable
	}
	var replayFields map[string]string
	var err error
	if today != "" {
		replayFields, err = PrepareTodayHistoryFields(fresh.Fields, today)
	} else {
		replayFields, err = PrepareHistoryFieldsWithRange(fresh.Fields, fromDate, toDate)
	}
	if err != nil {
		return probeResp, ErrHistoryUnavailable
	}
	replayFields["AccountNbr"] = targetAccount
	replayVals := url.Values{}
	for k, v := range replayFields {
		replayVals.Set(k, v)
	}
	replayReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bootstrap.String(), strings.NewReader(replayVals.Encode()))
	if err != nil {
		return probeResp, fmt.Errorf("prepare history replay request after conversation resync: %w", err)
	}
	replayReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if err := c.checkTodayLocked(today); err != nil {
		return probeResp, err
	}
	replayResp, replayErr := c.do(replayReq, replayFields["AccountNbr"])
	if replayErr != nil {
		return Response{}, replayErr
	}
	c.logHistoryContract("replay", replayFields, replayResp)
	if err := c.checkTodayLocked(today); err != nil {
		return replayResp, err
	}
	if isAuthChallengeKind(replayResp.Kind) {
		return replayResp, ErrInconclusiveAuth
	}
	if replayResp.StatusCode == http.StatusOK && replayResp.Kind != MaintenancePage {
		if replayResp.Kind != HistoryPage {
			return replayResp, ErrHistoryUnavailable
		}
		form, formErr := ExtractHistoryForm(replayResp.Body)
		if formErr != nil {
			return replayResp, ErrHistoryUnavailable
		}
		account := form.Fields["AccountNbr"]
		if (account != "" && account != targetAccount) || (account == "" && replayResp.RequestedAccount != targetAccount) {
			return replayResp, ErrHistoryUnavailable
		}
	}
	return replayResp, nil
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
	return c.do(req, "")
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

func (c *Client) do(req *http.Request, requestedAccount string) (Response, error) {
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
	requestURL := req.URL.String()
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
	// Redirect requests carry the prior response, even when they return to the original URL.
	if resp.Request.Response == nil && resp.Request.Method == http.MethodPost && resp.Request.URL.String() == requestURL {
		result.RequestedAccount = requestedAccount
	}
	c.updateFormState(result)
	return result, nil
}
