package acb

import (
	"context"
	"errors"
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

func (c *Client) historyRange() (string, string) {
	today := c.now().In(c.location)
	return today.Format(historyDateLayout), today.Format(historyDateLayout)
}

func cloneFields(fields map[string]string) map[string]string {
	cloned := make(map[string]string, len(fields))
	for key, value := range fields {
		cloned[key] = value
	}
	return cloned
}

func (c *Client) Bootstrap(ctx context.Context) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bootstrap == nil || len(c.bootstrapFields) == 0 {
		refreshResp, err := c.getLocked(ctx, "/acbib/Request")
		if err != nil {
			return Response{}, ErrAuthenticatedFormStateUnavailable
		}
		if c.bootstrap == nil || len(c.bootstrapFields) == 0 {
			return refreshResp, nil
		}
	}
	fields, err := PrepareHistoryFields(c.bootstrapFields, c.now(), c.location)
	if err != nil {
		return Response{}, err
	}
	fromDate, toDate := c.historyRange()
	slog.Debug("requesting ACB history", "from_date", fromDate, "to_date", toDate)
	values := url.Values{}
	for key, value := range fields {
		values.Set(key, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.bootstrap.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := c.do(req)
	if err != nil {
		return Response{}, err
	}
	if resp.Kind == LoginPage {
		refreshResp, refreshErr := c.getLocked(ctx, "/acbib/Request")
		if refreshErr == nil && (refreshResp.Kind == AccountDetailPage || refreshResp.Kind == HistoryPage) {
			slog.Info("ACB session state resynchronized after conversational token rejected", "classifier_reason", refreshResp.ClassifierReason)
			return refreshResp, nil
		}
	}
	return resp, nil
}

func (c *Client) History(ctx context.Context, endpoint string, fields map[string]string) (Response, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	hFields, err := PrepareHistoryFields(fields, c.now(), c.location)
	if err != nil {
		return Response{}, err
	}
	fromDate, toDate := c.historyRange()
	slog.Debug("requesting ACB history", "from_date", fromDate, "to_date", toDate)
	requestURL, err := c.endpoint(endpoint)
	if err != nil {
		return Response{}, err
	}
	values := url.Values{}
	for key, value := range hFields {
		values.Set(key, value)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), strings.NewReader(values.Encode()))
	if err != nil {
		return Response{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return c.do(req)
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
	result := Response{URL: resp.Request.URL.String(), StatusCode: resp.StatusCode, Body: string(body), Kind: kind, ClassifierReason: reason}
	c.updateFormState(result)
	return result, nil
}
