// (c) 2019-2020, Ava Labs, Inc. All rights reserved.
// See the file LICENSE for licensing terms.

package json

import (
	"errors"
	"net/http"
	"time"

	"github.com/sasha-s/go-deadlock"

	"github.com/gorilla/websocket"

	"github.com/ava-labs/gecko/snow"
)

const (
	// Size of the ws read buffer
	readBufferSize = 1024

	// Size of the ws write buffer
	writeBufferSize = 1024

	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 512 // bytes

	// Maximum number of pending messages to send to a peer.
	maxPendingMessages = 256 // messages
)

var upgrader = websocket.Upgrader{
	ReadBufferSize:  readBufferSize,
	WriteBufferSize: writeBufferSize,
	CheckOrigin:     func(*http.Request) bool { return true },
}

var (
	errDuplicateChannel = errors.New("duplicate channel")
)

// PubSubServer maintains the set of active clients and sends messages to the clients.
type PubSubServer struct {
	ctx *snow.Context

	lock     deadlock.Mutex
	conns    map[*Connection]map[string]struct{}
	channels map[string]map[*Connection]struct{}
}

// NewPubSubServer ...
func NewPubSubServer(ctx *snow.Context) *PubSubServer {
	return &PubSubServer{
		ctx:      ctx,
		conns:    make(map[*Connection]map[string]struct{}),
		channels: make(map[string]map[*Connection]struct{}),
	}
}

func (s *PubSubServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	wsConn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.ctx.Log.Debug("Failed to upgrade %s", err)
		return
	}
	conn := &Connection{s: s, conn: wsConn, send: make(chan interface{}, maxPendingMessages)}
	s.addConnection(conn)
}

// Publish ...
func (s *PubSubServer) Publish(channel string, msg interface{}) {
	s.lock.Lock()
	defer s.lock.Unlock()

	conns, exists := s.channels[channel]
	if !exists {
		s.ctx.Log.Warn("attempted to publush to an unknown channel %s", channel)
		return
	}

	pubMsg := &publish{
		Channel: channel,
		Value:   msg,
	}

	for conn := range conns {
		select {
		case conn.send <- pubMsg:
		default:
			s.ctx.Log.Verbo("dropping message to subscribed connection due to too many pending messages")
		}
	}
}

// Register ...
func (s *PubSubServer) Register(channel string) error {
	s.lock.Lock()
	defer s.lock.Unlock()

	if _, exists := s.channels[channel]; exists {
		return errDuplicateChannel
	}

	s.channels[channel] = make(map[*Connection]struct{})
	return nil
}

func (s *PubSubServer) addConnection(conn *Connection) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.conns[conn] = make(map[string]struct{})

	go conn.writePump()
	go conn.readPump()
}

func (s *PubSubServer) removeConnection(conn *Connection) {
	s.lock.Lock()
	defer s.lock.Unlock()

	channels, exists := s.conns[conn]
	if !exists {
		s.ctx.Log.Warn("attempted to remove an unknown connection")
		return
	}

	for channel := range channels {
		delete(s.channels[channel], conn)
	}
}

func (s *PubSubServer) addChannel(conn *Connection, channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	channels, exists := s.conns[conn]
	if !exists {
		return
	}

	conns, exists := s.channels[channel]
	if !exists {
		return
	}

	channels[channel] = struct{}{}
	conns[conn] = struct{}{}
}

func (s *PubSubServer) removeChannel(conn *Connection, channel string) {
	s.lock.Lock()
	defer s.lock.Unlock()

	channels, exists := s.conns[conn]
	if !exists {
		return
	}

	conns, exists := s.channels[channel]
	if !exists {
		return
	}

	delete(channels, channel)
	delete(conns, conn)
}

type publish struct {
	Channel string      `json:"channel"`
	Value   interface{} `json:"value"`
}

type subscribe struct {
	Channel     string `json:"channel"`
	Unsubscribe bool   `json:"unsubscribe"`
}

// Connection is a representation of the websocket connection.
type Connection struct {
	s *PubSubServer

	// The websocket connection.
	conn *websocket.Conn

	// Buffered channel of outbound messages.
	send chan interface{}
}

// readPump pumps messages from the websocket connection to the hub.
//
// The application runs readPump in a per-connection goroutine. The application
// ensures that there is at most one reader on a connection by executing all
// reads from this goroutine.
func (c *Connection) readPump() {
	defer func() {
		c.s.removeConnection(c)
		c.conn.Close()
	}()

	c.conn.SetReadLimit(maxMessageSize)
	c.conn.SetReadDeadline(time.Now().Add(pongWait))
	c.conn.SetPongHandler(func(string) error { c.conn.SetReadDeadline(time.Now().Add(pongWait)); return nil })

	for {
		msg := subscribe{}
		err := c.conn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				c.s.ctx.Log.Debug("Unexpected close in websockets: %s", err)
			}
			break
		}
		if msg.Unsubscribe {
			c.s.removeChannel(c, msg.Channel)
		} else {
			c.s.addChannel(c, msg.Channel)
		}
	}
}

// writePump pumps messages from the hub to the websocket connection.
//
// A goroutine running writePump is started for each connection. The
// application ensures that there is at most one writer to a connection by
// executing all writes from this goroutine.
func (c *Connection) writePump() {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		c.conn.Close()
	}()
	for {
		select {
		case message, ok := <-c.send:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if !ok {
				// The hub closed the channel.
				c.conn.WriteMessage(websocket.CloseMessage, []byte{})
				return
			}

			if err := c.conn.WriteJSON(message); err != nil {
				return
			}
		case <-ticker.C:
			c.conn.SetWriteDeadline(time.Now().Add(writeWait))
			if err := c.conn.WriteMessage(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}
