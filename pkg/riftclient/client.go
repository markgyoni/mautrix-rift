// Package riftclient is a standalone client for Rift's (undocumented) web API.
// It must not import anything from mautrix so it can be tested on its own.
package riftclient

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	DefaultAPIBase = "https://api.therift.chat"
	DefaultOrigin  = "https://app.therift.chat"
	// Mimic the web client; the API checks Origin for CORS and X-Rift-Client for platform.
	DefaultUserAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/157.0.0.0 Safari/537.36"
	ClientVersion    = "web 2026-11-20 17:35"
)

var ErrUnauthorized = errors.New("rift: unauthorized")

type HTTPError struct {
	Status int
	Method string
	Path   string
	Body   string
}

// RetryAfter returns retry_after_seconds from a 429 body, or 1.
func (e *HTTPError) RetryAfter() int {
	var body struct {
		Error struct {
			RetryAfterSeconds int `json:"retry_after_seconds"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(e.Body), &body) == nil && body.Error.RetryAfterSeconds > 0 {
		return min(body.Error.RetryAfterSeconds, 60)
	}
	return 1
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("rift: %s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Body)
}

func (e *HTTPError) Unwrap() error {
	if e.Status == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	return nil
}

type Client struct {
	APIBase   string
	HTTP      *http.Client
	UserAgent string

	// OnTokensRefreshed is called after a successful refresh so the caller can persist the new tokens.
	OnTokensRefreshed func(access, refresh string)
	// OnRequest, if set, is called after every HTTP request (for logging; path has no query string).
	OnRequest func(method, path string, status int, dur time.Duration, err error)

	// pace spaces out API requests (see wait); Rift answers bursts with 429 RATE_LIMITED.
	paceLock sync.Mutex
	nextSlot time.Time

	lock         sync.Mutex
	accessToken  string
	refreshToken string
	accessExpiry time.Time
}

func NewClient(accessToken, refreshToken string) *Client {
	c := &Client{APIBase: DefaultAPIBase, HTTP: &http.Client{Timeout: 60 * time.Second}, UserAgent: DefaultUserAgent}
	c.SetTokens(accessToken, refreshToken)
	return c
}

func (c *Client) SetTokens(access, refresh string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	c.accessToken, c.refreshToken = access, refresh
	c.accessExpiry = jwtExpiry(access)
}

func (c *Client) Tokens() (access, refresh string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	return c.accessToken, c.refreshToken
}

type reqOpts struct {
	noAuth  bool
	headers map[string]string
}

func (c *Client) do(ctx context.Context, method, path string, body, out any, opts reqOpts) error {
	if !opts.noAuth {
		if err := c.ensureFresh(ctx); err != nil {
			return err
		}
	}
	refreshed := false
	for attempt := 0; ; attempt++ {
		if err := c.pace(ctx); err != nil {
			return err
		}
		err := c.doOnce(ctx, method, path, body, out, opts)
		if errors.Is(err, ErrUnauthorized) && !opts.noAuth && !refreshed {
			refreshed = true
			if rerr := c.Refresh(ctx); rerr != nil {
				return errors.Join(err, rerr)
			}
			continue
		}
		var httpErr *HTTPError
		if errors.As(err, &httpErr) && httpErr.Status == http.StatusTooManyRequests && attempt < maxRateLimitRetries {
			// Body: {"error":{"code":"RATE_LIMITED","retry_after_seconds":N}} (confirmed).
			wait := time.Duration(httpErr.RetryAfter()+1) * time.Second
			c.backOff(wait)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		return err
	}
}

const (
	maxRateLimitRetries = 5
	// minRequestGap keeps the bridge well under the web client's own request rate.
	minRequestGap = 100 * time.Millisecond
)

// pace blocks until this request's slot, so a burst (e.g. syncing many ghosts) is spread out.
func (c *Client) pace(ctx context.Context) error {
	c.paceLock.Lock()
	now := time.Now()
	slot := c.nextSlot
	if slot.Before(now) {
		slot = now
	}
	c.nextSlot = slot.Add(minRequestGap)
	c.paceLock.Unlock()
	if d := time.Until(slot); d > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(d):
		}
	}
	return nil
}

// backOff pushes every later request past a 429's retry-after window.
func (c *Client) backOff(d time.Duration) {
	c.paceLock.Lock()
	if until := time.Now().Add(d); c.nextSlot.Before(until) {
		c.nextSlot = until
	}
	c.paceLock.Unlock()
}

func (c *Client) doOnce(ctx context.Context, method, path string, body, out any, opts reqOpts) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.APIBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Origin", DefaultOrigin)
	req.Header.Set("X-Rift-Client", "web")
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet {
		req.Header.Set("Idempotency-Key", uuid.NewString())
	}
	if !opts.noAuth {
		access, _ := c.Tokens()
		req.Header.Set("Authorization", "Bearer "+access)
	}
	for k, v := range opts.headers {
		req.Header.Set(k, v)
	}
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if c.OnRequest != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		c.OnRequest(method, req.URL.Path, status, time.Since(start), err)
	}
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		return &HTTPError{Status: resp.StatusCode, Method: method, Path: path, Body: truncate(string(data), 500)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("rift: decoding %s %s: %w", method, path, err)
		}
	}
	return nil
}

func (c *Client) ensureFresh(ctx context.Context) error {
	c.lock.Lock()
	expiry := c.accessExpiry
	c.lock.Unlock()
	if !expiry.IsZero() && time.Until(expiry) < time.Minute {
		return c.Refresh(ctx)
	}
	return nil
}

// Login does a plain password login (POST /auth/login). This gives tokens but no keys;
// combine with DeriveKeysFromMnemonic for DMs.
func (c *Client) Login(ctx context.Context, username, password string) (*AuthResponse, error) {
	var resp AuthResponse
	err := c.doOnce(ctx, http.MethodPost, "/auth/login", map[string]string{"username": username, "password": password}, &resp, reqOpts{noAuth: true})
	if err != nil {
		return nil, err
	}
	c.SetTokens(resp.AccessToken, resp.RefreshToken)
	return &resp, nil
}

// ErrorCode returns error.code from an API error body, or "".
func ErrorCode(err error) string {
	code, _ := errorBody(err)
	return code
}

func errorBody(err error) (code, loginTicket string) {
	var httpErr *HTTPError
	if !errors.As(err, &httpErr) {
		return "", ""
	}
	var body struct {
		Error struct {
			Code        string `json:"code"`
			LoginTicket string `json:"login_ticket"`
		} `json:"error"`
	}
	_ = json.Unmarshal([]byte(httpErr.Body), &body)
	return body.Error.Code, body.Error.LoginTicket
}

// TOTPLoginTicket returns the login ticket when a login failed with TOTP_REQUIRED (2FA is on).
// Shape from the web bundle: {"error":{"code":"TOTP_REQUIRED","login_ticket":"..."}}.
func TOTPLoginTicket(err error) string {
	if code, ticket := errorBody(err); code == "TOTP_REQUIRED" {
		return ticket
	}
	return ""
}

// Error codes of POST /auth/2fa/login (web bundle).
const (
	ErrCodeInvalidTOTP       = "INVALID_TOTP_CODE"   // wrong code; the ticket can be reused
	ErrCodeTOTPTicketExpired = "TOTP_TICKET_EXPIRED" // start over with /auth/login
)

// TOTPLogin finishes a 2FA login with the ticket from TOTPLoginTicket and a 6-digit code.
func (c *Client) TOTPLogin(ctx context.Context, loginTicket, code string) (*AuthResponse, error) {
	var resp AuthResponse
	err := c.doOnce(ctx, http.MethodPost, "/auth/2fa/login", map[string]string{"login_ticket": loginTicket, "code": code}, &resp, reqOpts{noAuth: true})
	if err != nil {
		return nil, err
	}
	c.SetTokens(resp.AccessToken, resp.RefreshToken)
	return &resp, nil
}

type TrustedLoginSession struct {
	SessionID         string `json:"session_id"`
	RequestSecret     string `json:"request_secret"`
	ExpiresAt         string `json:"expires_at"`
	SafetyCodeVersion int    `json:"safety_code_version"`
}

type TrustedLoginStatus struct {
	Status          string `json:"status"` // pending, approved, denied, expired
	EncryptedBundle string `json:"encrypted_bundle"`
	BundleIV        string `json:"bundle_iv"`
	ApproverPubkey  string `json:"approver_pubkey"`
	AccessToken     string `json:"access_token"`
	RefreshToken    string `json:"refresh_token"`
	User            *User  `json:"user"`
}

const trustedLoginSafetyCodeVersion = 2

// InitiateTrustedLogin starts a device-approval login. The user then types req.Code on a signed-in device.
func (c *Client) InitiateTrustedLogin(ctx context.Context, username, password, deviceLabel string, req *PairingRequest) (*TrustedLoginSession, error) {
	var resp TrustedLoginSession
	err := c.doOnce(ctx, http.MethodPost, "/pairing/trusted-login/initiate", map[string]any{
		"username":            username,
		"password":            password,
		"device_label":        deviceLabel,
		"ephemeral_pubkey":    hex.EncodeToString(req.Share),
		"safety_code_version": trustedLoginSafetyCodeVersion,
	}, &resp, reqOpts{noAuth: true})
	if err != nil {
		return nil, err
	}
	if resp.SafetyCodeVersion != trustedLoginSafetyCodeVersion {
		_ = c.CancelTrustedLogin(ctx, &resp)
		return nil, fmt.Errorf("rift: unsupported pairing version %d", resp.SafetyCodeVersion)
	}
	return &resp, nil
}

func (c *Client) TrustedLoginStatus(ctx context.Context, sess *TrustedLoginSession) (*TrustedLoginStatus, error) {
	var resp TrustedLoginStatus
	err := c.doOnce(ctx, http.MethodGet, "/pairing/trusted-login/"+url.PathEscape(sess.SessionID)+"/status", nil, &resp,
		reqOpts{noAuth: true, headers: map[string]string{"X-Rift-Trusted-Login-Secret": sess.RequestSecret}})
	return &resp, err
}

func (c *Client) CancelTrustedLogin(ctx context.Context, sess *TrustedLoginSession) error {
	return c.doOnce(ctx, http.MethodPost, "/pairing/trusted-login/"+url.PathEscape(sess.SessionID)+"/cancel", nil, nil,
		reqOpts{noAuth: true, headers: map[string]string{"X-Rift-Trusted-Login-Secret": sess.RequestSecret}})
}

// Refresh exchanges the refresh token for new tokens. Refresh tokens rotate.
func (c *Client) Refresh(ctx context.Context) error {
	_, refresh := c.Tokens()
	if refresh == "" {
		return ErrUnauthorized
	}
	var resp AuthResponse
	err := c.doOnce(ctx, http.MethodPost, "/auth/refresh", map[string]any{
		"refresh_token":      refresh,
		"include_user":       true,
		"refresh_request_id": uuid.NewString(),
		"client":             map[string]string{"version": ClientVersion},
	}, &resp, reqOpts{noAuth: true})
	if err != nil {
		return fmt.Errorf("rift: refreshing token: %w", err)
	}
	c.SetTokens(resp.AccessToken, resp.RefreshToken)
	if c.OnTokensRefreshed != nil {
		c.OnTokensRefreshed(resp.AccessToken, resp.RefreshToken)
	}
	return nil
}

func (c *Client) Logout(ctx context.Context) error {
	return c.do(ctx, http.MethodDelete, "/users/me/sessions/current", nil, nil, reqOpts{})
}

func (c *Client) GatewayTicket(ctx context.Context) (string, error) {
	var resp struct {
		Ticket string `json:"ticket"`
	}
	err := c.do(ctx, http.MethodPost, "/auth/ws-ticket", nil, &resp, reqOpts{})
	return resp.Ticket, err
}

func (c *Client) ListShards(ctx context.Context) (out []*Shard, err error) {
	return out, c.do(ctx, http.MethodGet, "/shards", nil, &out, reqOpts{})
}

func (c *Client) ShardBootstrap(ctx context.Context, shardID string) (out *ShardBootstrap, err error) {
	return out, c.do(ctx, http.MethodGet, "/shards/"+url.PathEscape(shardID)+"/bootstrap", nil, &out, reqOpts{})
}

func (c *Client) ListDMs(ctx context.Context) (out []*DMChannel, err error) {
	return out, c.do(ctx, http.MethodGet, "/dms", nil, &out, reqOpts{})
}

func (c *Client) ListSplinters(ctx context.Context) (out []*Splinter, err error) {
	return out, c.do(ctx, http.MethodGet, "/splinters", nil, &out, reqOpts{})
}

func (c *Client) GetProfiles(ctx context.Context, ids ...string) (out []*Profile, err error) {
	return out, c.do(ctx, http.MethodPost, "/users/profiles/batch", map[string][]string{"ids": ids}, &out, reqOpts{})
}

// ListShardMembers pages GET /shards/{id}/members/window?segment=all (offset paging, as the web
// client's member picker does) up to max members. complete is false if the list was cut short.
func (c *Client) ListShardMembers(ctx context.Context, shardID string, max int) (members []*ShardMember, complete bool, err error) {
	const pageSize = 500
	offset := 0
	for len(members) < max {
		var page membersWindow
		path := fmt.Sprintf("/shards/%s/members/window?segment=all&limit=%d&offset=%d", url.PathEscape(shardID), pageSize, offset)
		if err = c.do(ctx, http.MethodGet, path, nil, &page, reqOpts{}); err != nil {
			return nil, false, err
		}
		members = append(members, page.Members...)
		if page.NextOffset == nil || len(page.Members) == 0 {
			return members, true, nil
		}
		offset = *page.NextOffset
	}
	return members, false, nil
}

func (c *Client) GetPeerIdentity(ctx context.Context, userID string) (out *PeerIdentity, err error) {
	return out, c.do(ctx, http.MethodGet, "/users/"+url.PathEscape(userID)+"/prekeys/identity", nil, &out, reqOpts{})
}

// HistoryParams pages through message history. Before/BeforeID take the created_at and ID of the
// oldest message already seen (from the web client bundle; both are sent together).
type HistoryParams struct {
	Limit    int
	Before   time.Time
	BeforeID string
}

func (p HistoryParams) query() string {
	q := url.Values{}
	if p.Limit > 0 {
		q.Set("limit", strconv.Itoa(p.Limit))
	}
	if !p.Before.IsZero() {
		q.Set("before", p.Before.UTC().Format(time.RFC3339Nano))
		if p.BeforeID != "" {
			q.Set("before_id", p.BeforeID)
		}
	}
	return q.Encode()
}

// GetMessages fetches a page of a Shard channel's history, newest first.
func (c *Client) GetMessages(ctx context.Context, channelID string, p HistoryParams) (out []*Message, err error) {
	return out, c.do(ctx, http.MethodGet, "/messages/"+url.PathEscape(channelID)+"?"+p.query(), nil, &out, reqOpts{})
}

// GetDMMessages fetches a page of a DM's history, newest first.
func (c *Client) GetDMMessages(ctx context.Context, dmID string, p HistoryParams) (out []*Message, err error) {
	return out, c.do(ctx, http.MethodGet, "/dms/"+url.PathEscape(dmID)+"/messages?"+p.query(), nil, &out, reqOpts{})
}

func (c *Client) SendMessage(ctx context.Context, channelID string, req *SendMessageRequest) (out *Message, err error) {
	if req.ClientMessageID == "" {
		req.ClientMessageID = uuid.NewString()
	}
	return out, c.do(ctx, http.MethodPost, "/messages/"+url.PathEscape(channelID), req, &out, reqOpts{})
}

func (c *Client) EditMessage(ctx context.Context, channelID, messageID, content string, ast *Node) (out *Message, err error) {
	body := map[string]any{"content": content, "content_ast": ast}
	return out, c.do(ctx, http.MethodPatch, "/messages/"+url.PathEscape(channelID)+"/"+url.PathEscape(messageID), body, &out, reqOpts{})
}

func (c *Client) DeleteMessage(ctx context.Context, channelID, messageID string) error {
	return c.do(ctx, http.MethodDelete, "/messages/"+url.PathEscape(channelID)+"/"+url.PathEscape(messageID), nil, nil, reqOpts{})
}

func (c *Client) AddReaction(ctx context.Context, messageID, emoji string) error {
	return c.do(ctx, http.MethodPut, "/reactions/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(emoji), nil, nil, reqOpts{})
}

// RemoveReaction: DELETE on the same path (from the web client bundle).
func (c *Client) RemoveReaction(ctx context.Context, messageID, emoji string) error {
	return c.do(ctx, http.MethodDelete, "/reactions/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(emoji), nil, nil, reqOpts{})
}

// AckChannel / AckDM mark a conversation read up to messageID: POST {message_id} (confirmed).
func (c *Client) AckChannel(ctx context.Context, channelID, messageID string) error {
	return c.do(ctx, http.MethodPost, "/channels/"+url.PathEscape(channelID)+"/ack", map[string]string{"message_id": messageID}, nil, reqOpts{})
}

func (c *Client) AckDM(ctx context.Context, dmID, messageID string) error {
	return c.do(ctx, http.MethodPost, "/dms/"+url.PathEscape(dmID)+"/ack", map[string]string{"message_id": messageID}, nil, reqOpts{})
}

func (c *Client) SendTyping(ctx context.Context, channelID string) error {
	return c.do(ctx, http.MethodPost, "/messages/"+url.PathEscape(channelID)+"/typing", nil, nil, reqOpts{})
}

// SendDM encrypts plaintext (see EncodeDMPlaintext) with rift-dm-v4 and sends it to a DM channel. Attachments travel unencrypted
// next to the payload (the text may be empty, which still gets encrypted). cid is the client_message_id
// (bound into the AAD); a new one is generated if empty.
func (c *Client) SendDM(ctx context.Context, keys *AccountKeys, dmID, recipientIdentityHex, cid, text, replyToID string, attachments []*Attachment) (*Message, error) {
	if cid == "" {
		cid = uuid.NewString()
	}
	payload, err := EncryptDMv4(keys.DMPrivate, recipientIdentityHex, cid, nil, text, nil, nil)
	if err != nil {
		return nil, err
	}
	req := &SendMessageRequest{MessageKind: "e2ee", ClientMessageID: cid, E2EEPayload: payload, ReplyToID: replyToID, Attachments: attachments}
	var out *Message
	return out, c.do(ctx, http.MethodPost, "/dms/"+url.PathEscape(dmID)+"/messages", req, &out, reqOpts{})
}

func (c *Client) GetDMMessage(ctx context.Context, dmID, messageID string) (out *Message, err error) {
	return out, c.do(ctx, http.MethodGet, "/dms/"+url.PathEscape(dmID)+"/messages/"+url.PathEscape(messageID), nil, &out, reqOpts{})
}

// EditDM re-encrypts plaintext (see EncodeDMPlaintext) under the message's original client_message_id (the AAD binds it).
func (c *Client) EditDM(ctx context.Context, keys *AccountKeys, dmID, messageID, clientMessageID, recipientIdentityHex, text string) error {
	payload, err := EncryptDMv4(keys.DMPrivate, recipientIdentityHex, clientMessageID, nil, text, nil, nil)
	if err != nil {
		return err
	}
	body := map[string]any{"content": "", "e2ee_payload": payload, "content_ast": nil}
	return c.do(ctx, http.MethodPatch, "/dms/"+url.PathEscape(dmID)+"/messages/"+url.PathEscape(messageID), body, nil, reqOpts{})
}

// SendSplinterMessage posts a plaintext message to a Splinter. Splinters share the DM endpoints but
// aren't E2EE; the web client sends them with message_kind "legacy_plaintext" (from the bundle; received
// Splinter messages carry message_kind "envelope").
func (c *Client) SendSplinterMessage(ctx context.Context, splinterID string, req *SendMessageRequest) (out *Message, err error) {
	if req.ClientMessageID == "" {
		req.ClientMessageID = uuid.NewString()
	}
	req.MessageKind = "legacy_plaintext"
	return out, c.do(ctx, http.MethodPost, "/dms/"+url.PathEscape(splinterID)+"/messages", req, &out, reqOpts{})
}

// EditSplinterMessage edits a plaintext Splinter message (PATCH {content, content_ast}, inferred from the
// channel edit body and the web client's editDmMessage; not captured yet).
func (c *Client) EditSplinterMessage(ctx context.Context, splinterID, messageID, content string, ast *Node) error {
	body := map[string]any{"content": content, "content_ast": ast}
	return c.do(ctx, http.MethodPatch, "/dms/"+url.PathEscape(splinterID)+"/messages/"+url.PathEscape(messageID), body, nil, reqOpts{})
}

func (c *Client) DeleteDM(ctx context.Context, dmID, messageID string) error {
	return c.do(ctx, http.MethodDelete, "/dms/"+url.PathEscape(dmID)+"/messages/"+url.PathEscape(messageID), nil, nil, reqOpts{})
}

func (c *Client) AddDMReaction(ctx context.Context, dmID, messageID, emoji string) error {
	return c.do(ctx, http.MethodPut, "/dms/"+url.PathEscape(dmID)+"/messages/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(emoji), nil, nil, reqOpts{})
}

func (c *Client) RemoveDMReaction(ctx context.Context, dmID, messageID, emoji string) error {
	return c.do(ctx, http.MethodDelete, "/dms/"+url.PathEscape(dmID)+"/messages/"+url.PathEscape(messageID)+"/reactions/"+url.PathEscape(emoji), nil, nil, reqOpts{})
}

// UploadIdentityKeys publishes the DM identity key and signed prekey, as the web client does after login.
func (c *Client) UploadIdentityKeys(ctx context.Context, keys *AccountKeys) error {
	err := c.do(ctx, http.MethodPost, "/users/me/prekeys/identity", map[string]string{
		"public_key": keys.DMPublicHex(),
		"signature":  hex.EncodeToString(keys.Sign(keys.DMPublic)),
	}, nil, reqOpts{})
	if err != nil {
		return err
	}
	return c.do(ctx, http.MethodPost, "/users/me/prekeys/signed", map[string]any{
		"key_id":     1,
		"public_key": keys.SignedPrekeyPublicHex(),
		"signature":  hex.EncodeToString(keys.Sign(keys.SignedPrekeyPublic)),
	}, nil, reqOpts{})
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
