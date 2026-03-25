package ws

import (
	"encoding/json"
	"log"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jukebox/backend/internal/models"
)

const (
	writeWait      = 10 * time.Second
	pongWait       = 60 * time.Second
	pingPeriod     = (pongWait * 9) / 10
	maxMessageSize = 4096
)

// Client represents a single WebSocket connection to a room.
type Client struct {
	Hub      *Hub
	Conn     *websocket.Conn
	Send     chan []byte
	Session  *models.Session
	UserID   string       // set if authenticated user
	User     *models.User // full user record if authenticated
	IsDJ     bool
	LastChat time.Time    // rate limit chat messages
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
		c.Hub.Unregister <- c
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

		c.Hub.Inbound <- &ClientMessage{Client: c, Message: inbound}
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
		case message, ok := <-c.Send:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				c.Conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}
			if err := c.Conn.WriteMessage(websocket.TextMessage, message); err != nil {
				return
			}

		case <-ticker.C:
			c.Conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.Conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

// SendJSON marshals and sends a WSMessage to this client.
func (c *Client) SendJSON(msg WSMessage) {
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	select {
	case c.Send <- data:
	default:
		// Client buffer full, drop message
	}
}

func (c *Client) sendError(msg string) {
	c.SendJSON(WSMessage{Event: EventError, Payload: map[string]string{"message": msg}})
}

// ClientMessage pairs an inbound message with the client that sent it.
type ClientMessage struct {
	Client  *Client
	Message InboundMessage
}
