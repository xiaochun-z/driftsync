package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/xiaochun-z/driftsync/internal/store"
)

// DeviceCodeClient authenticates with Microsoft identity via the device code flow
// and injects Bearer tokens into every outgoing HTTP request.
type DeviceCodeClient struct {
	Tenant     string
	ClientID   string
	Store      *store.TokenStore
	httpClient *http.Client

	mu     sync.RWMutex
	cached *store.Tokens
}

func NewDeviceCodeClient(tenant, clientID string, st *store.TokenStore) *DeviceCodeClient {
	c := &DeviceCodeClient{
		Tenant:     tenant,
		ClientID:   clientID,
		Store:      st,
		httpClient: &http.Client{Timeout: 30 * time.Second},
	}
	if t, err := st.Load(context.Background()); err == nil {
		c.setCached(t)
	}
	return c
}

func (c *DeviceCodeClient) setCached(t *store.Tokens) { c.mu.Lock(); c.cached = t; c.mu.Unlock() }
func (c *DeviceCodeClient) getCached() *store.Tokens {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.cached
}

func (c *DeviceCodeClient) tokenEndpoint() string {
	return fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/token", c.Tenant)
}
func (c *DeviceCodeClient) deviceEndpoint() string {
	return fmt.Sprintf("https://login.microsoftonline.com/%s/oauth2/v2.0/devicecode", c.Tenant)
}

// authRoundTripper injects an up-to-date Bearer token before each request.
type authRoundTripper struct {
	base http.RoundTripper
	c    *DeviceCodeClient
}

func (rt *authRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	ctx := req.Context()
	tok, err := rt.c.getValidToken(ctx)
	if err != nil {
		return nil, err
	}
	req2 := req.Clone(ctx)
	req2.Header.Set("Authorization", "Bearer "+tok.AccessToken)
	return rt.base.RoundTrip(req2)
}

// AuthorizedClient returns an *http.Client whose every request carries a valid Bearer token.
func (c *DeviceCodeClient) AuthorizedClient(ctx context.Context) *http.Client {
	base := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		MaxIdleConns:        256,
		MaxIdleConnsPerHost: 64,
		IdleConnTimeout:     90 * time.Second,
	}
	return &http.Client{Transport: &authRoundTripper{base: base, c: c}}
}

// EnsureLogin verifies that a valid token is available, starting an interactive
// device-code flow if not.
func (c *DeviceCodeClient) EnsureLogin(ctx context.Context) error {
	if _, err := c.getValidToken(ctx); err == nil {
		return nil
	}
	log.Println("Starting device code flow...")
	return c.deviceCodeFlow(ctx)
}

func (c *DeviceCodeClient) getValidToken(ctx context.Context) (*store.Tokens, error) {
	if t := c.getCached(); t != nil && time.Until(t.ExpiresAt) > 2*time.Minute {
		return t, nil
	}
	t, err := c.Store.Load(ctx)
	if err != nil {
		return nil, errors.New("no valid token")
	}
	if time.Until(t.ExpiresAt) > 2*time.Minute {
		c.setCached(t)
		return t, nil
	}
	// Token exists but is near expiry — attempt silent refresh.
	if nt, err := c.refresh(ctx, t.RefreshToken); err == nil {
		return nt, nil
	}
	return nil, errors.New("no valid token")
}

func (c *DeviceCodeClient) deviceCodeFlow(ctx context.Context) error {
	vals := url.Values{}
	vals.Set("client_id", c.ClientID)
	vals.Set("scope", "offline_access Files.ReadWrite User.Read")

	resp, err := c.httpClient.PostForm(c.deviceEndpoint(), vals)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("device code http %d: %s", resp.StatusCode, string(b))
	}

	var d struct {
		DeviceCode              string `json:"device_code"`
		UserCode                string `json:"user_code"`
		VerificationURI         string `json:"verification_uri"`
		VerificationURIComplete string `json:"verification_uri_complete"`
		ExpiresIn               int    `json:"expires_in"`
		Interval                int    `json:"interval"`
		Message                 string `json:"message"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return err
	}

	if d.VerificationURIComplete != "" {
		log.Printf("Visit: %s", d.VerificationURIComplete)
	} else {
		log.Printf("Go to: %s and enter code: %s", d.VerificationURI, d.UserCode)
	}
	if d.Message != "" {
		fmt.Println(d.Message)
	}

	interval := d.Interval
	if interval <= 0 {
		interval = 5
	}
	deadline := time.Now().Add(time.Duration(d.ExpiresIn) * time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(time.Duration(interval) * time.Second)
		nt, done, err := c.pollToken(ctx, d.DeviceCode)
		if err != nil {
			return err
		}
		if done {
			if err := c.Store.Save(ctx, nt); err != nil {
				return err
			}
			c.setCached(nt)
			return nil
		}
	}
	return errors.New("device code expired before authorization")
}

// tokenResponse is the JSON shape returned by both the poll and refresh token endpoints.
type tokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
	TokenType    string `json:"token_type"`
}

func (t tokenResponse) toTokens() (*store.Tokens, error) {
	if t.TokenType != "Bearer" {
		return nil, fmt.Errorf("unexpected token_type %q (want Bearer)", t.TokenType)
	}
	return &store.Tokens{
		AccessToken:  t.AccessToken,
		RefreshToken: t.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(t.ExpiresIn) * time.Second),
	}, nil
}

func (c *DeviceCodeClient) pollToken(ctx context.Context, deviceCode string) (*store.Tokens, bool, error) {
	vals := url.Values{}
	vals.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")
	vals.Set("client_id", c.ClientID)
	vals.Set("device_code", deviceCode)

	req, err := http.NewRequestWithContext(ctx, "POST", c.tokenEndpoint(), bytes.NewBufferString(vals.Encode()))
	if err != nil {
		return nil, false, fmt.Errorf("build poll request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == 200 {
		var t tokenResponse
		if err := json.Unmarshal(b, &t); err != nil {
			return nil, false, err
		}
		tok, err := t.toTokens()
		return tok, err == nil, err
	}

	var e struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(b, &e)
	if e.Error == "authorization_pending" || e.Error == "slow_down" {
		return nil, false, nil // still waiting
	}
	return nil, false, fmt.Errorf("token poll http %d: %s", resp.StatusCode, string(b))
}

func (c *DeviceCodeClient) refresh(ctx context.Context, refreshToken string) (*store.Tokens, error) {
	vals := url.Values{}
	vals.Set("grant_type", "refresh_token")
	vals.Set("client_id", c.ClientID)
	vals.Set("refresh_token", refreshToken)
	vals.Set("scope", "offline_access Files.ReadWrite User.Read")

	req, err := http.NewRequestWithContext(ctx, "POST", c.tokenEndpoint(), bytes.NewBufferString(vals.Encode()))
	if err != nil {
		return nil, fmt.Errorf("build refresh request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("refresh http %d: %s", resp.StatusCode, string(b))
	}

	var t tokenResponse
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	nt, err := t.toTokens()
	if err != nil {
		return nil, err
	}
	if err := c.Store.Save(ctx, nt); err != nil {
		return nil, err
	}
	c.setCached(nt)
	return nt, nil
}
