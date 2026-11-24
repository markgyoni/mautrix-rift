package riftclient

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// mediaSvcPrefix is how profile and member avatars are given out ("https://api.therift.chat/media-svc/uploads/...").
// Fetching that URL directly 404s; the part after the prefix is a normal R2 key that needs a token.
const mediaSvcPrefix = DefaultAPIBase + "/media-svc/"

// MediaKey normalises an avatar/media reference to its R2 key where possible (drops the media-svc
// prefix and any query string). Other absolute URLs are returned unchanged.
func MediaKey(ref string) string {
	if k, ok := strings.CutPrefix(ref, mediaSvcPrefix); ok {
		k, _, _ = strings.Cut(k, "?")
		return k
	}
	return ref
}

// MediaURLs resolves R2 keys to short-lived signed download URLs (POST /media/tokens/batch).
// Values that are already absolute URLs are returned unchanged.
func (c *Client) MediaURLs(ctx context.Context, keys ...string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	var toMint []string
	for _, k := range keys {
		if key := MediaKey(k); key != k {
			toMint = append(toMint, key)
			continue
		}
		if strings.HasPrefix(k, "https://") || strings.HasPrefix(k, "http://") {
			out[k] = k
		} else if k != "" {
			toMint = append(toMint, k)
		}
	}
	if len(toMint) == 0 {
		return out, nil
	}
	var resp struct {
		Results []struct {
			Key string `json:"key"`
			URL string `json:"url"`
		} `json:"results"`
	}
	if err := c.do(ctx, http.MethodPost, "/media/tokens/batch", map[string][]string{"keys": toMint}, &resp, reqOpts{}); err != nil {
		return nil, err
	}
	for _, r := range resp.Results {
		out[r.Key] = r.URL
	}
	return out, nil
}

// DownloadMedia fetches an R2 key (or URL) and returns its bytes and content type.
func (c *Client) DownloadMedia(ctx context.Context, key string) ([]byte, string, error) {
	urls, err := c.MediaURLs(ctx, key)
	if err != nil {
		return nil, "", err
	}
	u, ok := urls[MediaKey(key)]
	if !ok {
		return nil, "", fmt.Errorf("no download URL for %s", key)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Origin", DefaultOrigin)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("media download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 100<<20))
	return data, resp.Header.Get("Content-Type"), err
}

type uploadTokenResponse struct {
	UploadURL      string `json:"upload_url"`
	UploadToken    string `json:"upload_token"`
	MaxUploadBytes int64  `json:"max_upload_bytes"`
	R2Key          string `json:"r2_key"`
	Deduped        bool   `json:"deduped"`
}

// UploadMedia uploads a file and returns the attachment to put in a message.
// shardID is set for Shard channels and empty for DMs (the web client just omits it).
func (c *Client) UploadMedia(ctx context.Context, shardID string, data []byte, filename, mimeType string) (*Attachment, error) {
	sum := sha256.Sum256(data)
	tokReq := map[string]any{
		"filename":     filename,
		"content_type": mimeType,
		"file_size":    len(data),
		"content_hash": hex.EncodeToString(sum[:]),
	}
	if shardID != "" {
		tokReq["shard_id"] = shardID
	}
	var tok uploadTokenResponse
	err := c.do(ctx, http.MethodPost, "/media/upload-token", tokReq, &tok, reqOpts{})
	if err != nil {
		return nil, err
	}
	if tok.MaxUploadBytes > 0 && int64(len(data)) > tok.MaxUploadBytes {
		return nil, fmt.Errorf("file is too large for Rift (%d > %d bytes)", len(data), tok.MaxUploadBytes)
	}
	att := &Attachment{R2Key: tok.R2Key, Filename: filename, Size: int64(len(data)), MimeType: mimeType}
	if tok.Deduped {
		return att, nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, tok.UploadURL, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+tok.UploadToken)
	req.Header.Set("X-Rift-Filename", filename)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("User-Agent", c.UserAgent)
	req.Header.Set("Origin", DefaultOrigin)
	start := time.Now()
	resp, err := c.HTTP.Do(req)
	if c.OnRequest != nil {
		status := 0
		if resp != nil {
			status = resp.StatusCode
		}
		c.OnRequest(http.MethodPut, "media:/upload", status, time.Since(start), err)
	}
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, &HTTPError{Status: resp.StatusCode, Method: http.MethodPut, Path: "media:/upload", Body: truncate(string(body), 500)}
	}
	var up struct {
		R2Key    string `json:"r2_key"`
		MimeType string `json:"mime_type"`
	}
	if json.Unmarshal(body, &up) == nil && up.R2Key != "" {
		att.R2Key = up.R2Key
	}
	return att, nil
}

// DownloadURL fetches an external file, such as a GIF from Rift's GIF picker (static.klipy.com).
func (c *Client) DownloadURL(ctx context.Context, u string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.UserAgent)
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 50<<20))
	return data, resp.Header.Get("Content-Type"), err
}
