package ws

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jukebox/backend/internal/models"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096

	// sendBufferSize is the per-client outbound frame buffer.
	// Slow-client policy: sends never block — a per-client message that
	// overflows the buffer is dropped, and the broadcast fanout
	// (Hub.fanout) evicts a client whose buffer is full. A slow reader
	// gets a clean disconnect/reconnect instead of stalling the room or
	// growing memory without bound.
	sendBufferSize = 256
)

// outbound is a single frame queued for delivery to one client.
// Broadcast frames additionally carry a shared *websocket.PreparedMessage
// so permessage-deflate compresses the payload once per broadcast instead
// of once per recipient; per-client frames carry raw JSON bytes only.
type outbound struct {
	data     []byte
	prepared *websocket.PreparedMessage
}

// Client represents a single WebSocket connection to a room.
type Client struct {
	Hub      *Hub
	Conn     *websocket.Conn
	Send     chan outbound
	Session  *models.Session
	UserID   string       // set if authenticated user
	User     *models.User // full user record if authenticated
	IsDJ     bool
	LastChat time.Time // rate limit chat messages
	LastReaction time.Time // rate limit reactions
	LastSubmit   time.Time // rate limit track submissions
	LastPlaybackReport time.Time // rate limit report_duration / autoplay_track_ended

	// done is closed (via close) when the hub drops the client — a
	// normal unregister or a slow-client eviction. Send itself is never
	// closed, so any goroutine may queue frames without racing a
	// channel close.
	done      chan struct{}
	closeOnce sync.Once

	// registered is closed by Hub.onRegister once all of this client's
	// join-time I/O (AddListener, StartListenEvent, initial state) has
	// finished. Hub.onUnregister waits on it, so on a fast
	// connect-then-disconnect the leave-time I/O can never overtake the
	// join-time I/O — which would strand a ghost session in the Redis
	// listeners set and leave a never-ended listen_event row.
	registered chan struct{}
}

// NewClient builds a client with its outbound buffer and lifecycle channel.
func NewClient(hub *Hub, conn *websocket.Conn, session *models.Session) *Client {
	return &Client{
		Hub:        hub,
		Conn:       conn,
		Send:       make(chan outbound, sendBufferSize),
		Session:    session,
		done:       make(chan struct{}),
		registered: make(chan struct{}),
	}
}

// Done exposes the client's lifecycle channel: closed when the hub drops
// the client. Lets the connection handler tie external bookkeeping (per-IP
// connection slots) to the client's actual lifetime.
func (c *Client) Done() <-chan struct{} {
	return c.done
}

// close marks the client as dropped, waking its WritePump. Idempotent and
// safe from any goroutine.
func (c *Client) close() {
	c.closeOnce.Do(func() { close(c.done) })
}

// send queues one frame without ever blocking the caller. Returns false
// if the frame was dropped because the client is closing or its buffer
// is full (see sendBufferSize for the slow-client policy).
func (c *Client) send(out outbound) bool {
	select {
	case <-c.done:
		return false
	default:
	}
	select {
	case c.Send <- out:
		return true
	default:
		return false
	}
}

// DisplayName returns the best display name: stage name > display name > session name.
func (c *Client) DisplayName() string {
	if c.User != nil && c.User.StageName != "" {
		return c.User.StageName
	}
	if c.User != nil && c.User.DisplayName != "" {
		return c.User.DisplayName
	}
	return c.Session.DisplayName
}

// ReadPump reads messages from the WebSocket connection and forwards them to the hub.
func (c *Client) ReadPump() {
	defer func() {
		c.Hub.unregister(c)
		c.Conn.Close()
	}()

	c.Conn.SetReadLimit(maxMessageSize)
	c.Conn.SetReadDeadline(time.Now().Add(pongWait))
	c.Conn.SetPongHandler(func(string) error {
		c.Conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	for {
		_, message, err := c.Conn.ReadMessage()
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseNormalClosure) {
				log.Printf("ws read error: %v", err)
			}
			break
		}

		var inbound InboundMessage
		if err := json.Unmarshal(message, &inbound); err != nil {
			c.sendError("invalid message format")
			continue
		}

		select {
		case c.Hub.Inbound <- &ClientMessage{Client: c, Message: inbound}:
		case <-c.Hub.quit:
			return
		}
	}
}

// WritePump sends messages from the hub to the WebSocket connection.
func (c *Client) WritePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.Conn.Close()
	}()

	for {
		select {
		case out := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			var err error
			if out.prepared != nil {
				// Broadcast frame: the compressed form is cached in the
				// PreparedMessage, shared across all recipients.
				err = c.Conn.WritePreparedMessage(out.prepared)
			} else {
				err = c.Conn.WriteMessage(websocket.TextMessage, out.data)
			}
			if err != nil {
				return
			}

		case <-c.done:
			// The hub dropped this client. Frames still buffered in
			// Send are discarded by design (the client is reconnecting
			// or gone).
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
			return

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// SendJSON marshals and sends a WSMessage to this client. Never blocks;
// the frame is dropped if the client's buffer is full.
func (c *Client) SendJSON(msg WSMessage) {
	c.sendValue(msg)
}

// sendValue marshals any JSON-serializable value and queues it for this client.
func (c *Client) sendValue(v interface{}) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	c.send(outbound{data: data})
}

func (c *Client) sendError(msg string) {
	c.SendJSON(WSMessage{Event: EventError, Payload: map[string]string{"message": msg}})
}

// sendSubmitResult delivers the direct reply to a submit_track action.
// WS CONTRACT (frozen): {"type":"submit_result","ok":bool,"error":string|null},
// sent only to the submitting client, which waits up to 4s for it (or a
// queue/request update echoing the submitted track) before reporting failure.
func (c *Client) sendSubmitResult(ok bool, errMsg string) {
	var errField *string
	if errMsg != "" {
		errField = &errMsg
	}
	c.sendValue(struct {
		Type  string  `json:"type"`
		OK    bool    `json:"ok"`
		Error *string `json:"error"`
	}{Type: "submit_result", OK: ok, Error: errField})
}

// rejectSubmit reports a failed submit_track to the submitting client.
// The legacy error event goes first — clients that predate submit_result
// surface submit rejections by matching that event's message — then the
// contractual submit_result. Newer clients resolve on the first one they
// understand and treat the other as a no-op.
func (c *Client) rejectSubmit(reason string) {
	c.sendError(reason)
	c.sendSubmitResult(false, reason)
}

// ClientMessage pairs an inbound message with the client that sent it.
type ClientMessage struct {
	Client  *Client
	Message InboundMessage
}
