package riftclient

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
)

// Event is a gateway dispatch frame: {"t","v","s","c","n","ts","d","seq"}.
type Event struct {
	Type      string          `json:"t"`
	Version   int             `json:"v"`
	ShardID   *string         `json:"s"`
	ChannelID *string         `json:"c"`
	Nonce     string          `json:"n"`
	Timestamp time.Time       `json:"ts"`
	Data      json.RawMessage `json:"d"`
	Seq       int64           `json:"seq"`
}

type frame struct {
	Op   string          `json:"op,omitempty"`
	Type string          `json:"t,omitempty"`
	Data json.RawMessage `json:"d,omitempty"`
	Seq  *int64          `json:"seq,omitempty"`
}

type helloData struct {
	HeartbeatInterval int    `json:"heartbeat_interval"`
	ResumeToken       string `json:"resume_token"`
	ResumeWindowMS    int    `json:"resume_window_ms"`
	SessionID         string `json:"session_id"`
}

// Gateway is a single WebSocket session. Run blocks until the connection drops; callers reconnect.
type Gateway struct {
	client  *Client
	conn    *websocket.Conn
	seq     atomic.Int64
	OnEvent func(*Event)
}

func (c *Client) NewGateway(onEvent func(*Event)) *Gateway {
	return &Gateway{client: c, OnEvent: onEvent}
}

func (g *Gateway) send(ctx context.Context, op string, d any) error {
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(frame{Op: op, Data: data})
	return g.conn.Write(ctx, websocket.MessageText, b)
}

// SubscribeShard asks for a Shard's full event stream. "passive" mode (what the web client uses for
// unfocused Shards) only delivers SHARD_ACTIVITY summaries, not MESSAGE_CREATE.
func (g *Gateway) SubscribeShard(ctx context.Context, shardID string) error {
	return g.send(ctx, "SUBSCRIBE_SHARD", map[string]string{"shard_id": shardID, "mode": "active"})
}

func (g *Gateway) Run(ctx context.Context, onReady func(context.Context, *Gateway)) error {
	ticket, err := g.client.GatewayTicket(ctx)
	if err != nil {
		return fmt.Errorf("getting gateway ticket: %w", err)
	}
	wsURL := strings.Replace(g.client.APIBase, "http", "ws", 1) + "/ws?ticket=" + url.QueryEscape(ticket)
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {DefaultOrigin}, "User-Agent": {g.client.UserAgent}},
	})
	if err != nil {
		return fmt.Errorf("dialing gateway: %w", err)
	}
	conn.SetReadLimit(16 << 20)
	g.conn = conn
	defer conn.CloseNow()

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	var hello helloData
	if err := g.readHello(ctx, &hello); err != nil {
		return err
	}
	if err := g.send(ctx, "SET_CLIENT_CAPABILITIES", map[string]bool{
		"reaction_debounce": true, "presence_bulk": true, "reaction_remove_many": true,
		"gap_resync": true, "message_delete_bulk": true,
	}); err != nil {
		return err
	}
	go g.heartbeatLoop(ctx, time.Duration(hello.HeartbeatInterval)*time.Millisecond)
	if onReady != nil {
		go onReady(ctx, g)
	}

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return fmt.Errorf("gateway read: %w", err)
		}
		var f frame
		if err := json.Unmarshal(data, &f); err != nil {
			continue
		}
		if f.Op != "" {
			continue // HEARTBEAT_ACK, SHARD_ACTIVATED, ...
		}
		var evt Event
		if err := json.Unmarshal(data, &evt); err != nil {
			continue
		}
		if evt.Seq > 0 {
			g.seq.Store(evt.Seq)
		}
		if g.OnEvent != nil {
			g.OnEvent(&evt)
		}
	}
}

func (g *Gateway) readHello(ctx context.Context, hello *helloData) error {
	readCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	_, data, err := g.conn.Read(readCtx)
	if err != nil {
		return fmt.Errorf("waiting for HELLO: %w", err)
	}
	var f frame
	if err := json.Unmarshal(data, &f); err != nil || f.Op != "HELLO" {
		return errors.New("gateway: expected HELLO as first frame")
	}
	if err := json.Unmarshal(f.Data, hello); err != nil {
		return err
	}
	if hello.HeartbeatInterval <= 0 {
		hello.HeartbeatInterval = 30000
	}
	return nil
}

func (g *Gateway) heartbeatLoop(ctx context.Context, interval time.Duration) {
	beat := func() error {
		return g.send(ctx, "HEARTBEAT", map[string]any{"seq": g.seq.Load(), "desktop_active": true})
	}
	if beat() != nil {
		return
	}
	t := time.NewTicker(interval / 2) // the web client beats roughly twice per interval
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := beat(); err != nil {
				g.conn.CloseNow()
				return
			}
		}
	}
}
