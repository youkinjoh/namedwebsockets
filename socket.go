package main

import (
	"log"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

const (
	// Time allowed to write a message to the peer.
	writeWait = 10 * time.Second

	// Time allowed to read the next pong message from the peer.
	pongWait = 60 * time.Second

	// Send pings to peer with this period. Must be less than pongWait.
	pingPeriod = (pongWait * 9) / 10

	// Maximum message size allowed from peer.
	maxMessageSize = 1024
)

type NamedWebSocket struct {
	serviceName string

	// The current websocket connection instances to this named websocket
	connections []*Connection

	// Buffered channel of outbound service messages.
	broadcastBuffer chan *WSMessage

	// Buffered channel of outbound connect/disconnect messages
	controlBuffer chan *WSMessage

	// Attached DNS-SD discovery registration and browser for this Named Web Socket
	discoveryClient *DiscoveryClient
}

type Connection struct {
	ws *websocket.Conn
	isProxy bool
}

type WSMessage struct {
	source *Connection
	payload []byte
}

var upgrader = websocket.Upgrader{
	ReadBufferSize:  2048,
	WriteBufferSize: 2048,
	CheckOrigin: func(r *http.Request) bool {
		return true // allow all origins
	},
}

// Create a new NamedWebSocket instance (local or broadcast-based) with a given service type
func NewNamedWebSocket(serviceName string, isBroadcast bool) *NamedWebSocket {
	scope := "broadcast"
	if isBroadcast == false {
		scope = "local"
	}

	sock := &NamedWebSocket{
		serviceName: serviceName,
		connections: make([]*Connection, 0),
		broadcastBuffer: make(chan *WSMessage, 512),
		controlBuffer: make(chan *WSMessage, 512),
	}

	go sock.messageDispatcher()

	log.Print("New " + scope + " web socket '" + serviceName + "' created.")

	if isBroadcast {
		go sock.advertise()
	}

	return sock
}

func (sock *NamedWebSocket) advertise() {
	if sock.discoveryClient == nil {
		// Advertise new socket type on the local network
		sock.discoveryClient = NewDiscoveryClient(sock.serviceName)
	}
}

// Set up a new web socket connection
func (sock *NamedWebSocket) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != "GET" {
		http.Error(w, "Method not allowed", 405)
		return
	}

	isProxy := false
	proxyHeader := r.Header.Get("X-BroadcastWebSocket-Proxy")
	if proxyHeader == "true" {
		isProxy = true
	}

	ws, err := upgrader.Upgrade(w, r, map[string][]string{
		"Access-Control-Allow-Origin": []string{"*"},
		"Access-Control-Allow-Credentials": []string{"true"},
		"Access-Control-Allow-Headers": []string{"content-type"},
	})
	if err != nil {
		if _, ok := err.(websocket.HandshakeError); !ok {
			log.Println(err)
		}
		return
	}

	conn := &Connection{
		ws: ws,
		isProxy: isProxy,
	}

	sock.addConnection(conn)

	go sock.writeConnectionPump(conn)
	sock.readConnectionPump(conn)

}

// readConnectionPump pumps messages from an individual websocket connection to the dispatcher
func (sock *NamedWebSocket) readConnectionPump(conn *Connection) {
	defer func() {
		conn.ws.Close()
		sock.removeConnection(conn)
	}()
	conn.ws.SetReadLimit(maxMessageSize)
	conn.ws.SetReadDeadline(time.Now().Add(pongWait))
	conn.ws.SetPongHandler(func(string) error { conn.ws.SetReadDeadline(time.Now().Add(pongWait)); return nil })
	for {
		_, message, err := conn.ws.ReadMessage()
		if err != nil {
			break
		}
		wsBroadcast := &WSMessage{
			source: conn,
			payload: message,
		}
		sock.broadcastBuffer <- wsBroadcast
	}
}

// writeConnectionPump keeps an individual websocket connection alive
func (sock *NamedWebSocket) writeConnectionPump(conn *Connection) {
	ticker := time.NewTicker(pingPeriod)
	defer func() {
		ticker.Stop()
		conn.ws.Close()
		sock.removeConnection(conn)
	}()
	for {
		select {
			case <-ticker.C:
				sock.write(conn, websocket.PingMessage, []byte{})
		}
	}
}

// Handle broadcast of control and service messaging on NamedWebSocket connections
func (sock *NamedWebSocket) messageDispatcher() {
	for {
		select {
		case wsConnect, ok := <-sock.controlBuffer:
			if !ok {
				sock.write(wsConnect.source, websocket.CloseMessage, []byte{})
				return
			}
			sock.broadcast(wsConnect)
		case wsBroadcast, ok := <-sock.broadcastBuffer:
			if !ok {
				sock.write(wsBroadcast.source, websocket.CloseMessage, []byte{})
				return
			}
			sock.broadcast(wsBroadcast)
		}
	}
}

// Set up a new NamedWebSocket connection instance
func (sock *NamedWebSocket) addConnection(conn *Connection) {

	connectPayload := []byte("____connect")

	// Notify new websocket connection of existing websocket connections
	for _, oConn := range sock.connections {
		if !conn.isProxy || (conn.isProxy && !oConn.isProxy) {
			sock.write(conn, websocket.TextMessage, connectPayload)
		}
	}

	if !conn.isProxy {
		// Connect message
		wsConnect := &WSMessage{
			source: conn,
			payload: []byte("____connect"),
		}

		// Broadcast new connect event to all existing named websocket connections
		sock.controlBuffer <- wsConnect
	}

	// Add this websocket instance to connections
	sock.connections = append( sock.connections, conn )
}

// Send a message to the target websocket connection
func (sock *NamedWebSocket) write(conn *Connection, mt int, payload []byte) {
	conn.ws.SetWriteDeadline(time.Now().Add(writeWait))
	conn.ws.WriteMessage(mt, payload)
}

// Broadcast a message to all websocket connections for this NamedWebSocket
// instance (except to the src websocket connection)
func (sock *NamedWebSocket) broadcast(broadcast *WSMessage) {
	for _, conn := range sock.connections {
		if conn.ws != broadcast.source.ws {
			// don't relay messages infinitely between proxy connections
			if (conn.isProxy && broadcast.source.isProxy) {
				continue
			}
			sock.write(conn, websocket.TextMessage, broadcast.payload)
		}
	}
}

// Tear down an existing NamedWebSocket connection instance
func (sock *NamedWebSocket) removeConnection(conn *Connection) {
	for i, oConn := range sock.connections {
		if oConn.ws == conn.ws {
			sock.connections = append(sock.connections[:i], sock.connections[i+1:]...)
			break
		}
	}

	if !conn.isProxy {
		// Broadcast new disconnect event to all existing named websocket connections
		wsDisconnect := &WSMessage{
			source: conn,
			payload: []byte("____disconnect"),
		}

		sock.controlBuffer <- wsDisconnect
	}
}