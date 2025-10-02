package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"nhooyr.io/websocket"
)

// ====== wire protocol (совпадает с Android) ======
type Candidate struct {
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *int    `json:"sdpMLineIndex,omitempty"`
	Candidate     *string `json:"candidate,omitempty"`
}

type Envelope struct {
	Type      string     `json:"type"` // join | peer-joined | offer | answer | ice | leave
	Room      string     `json:"room,omitempty"`
	PeerID    string     `json:"peerId,omitempty"` // в исходящих "peer-joined"
	From      string     `json:"from,omitempty"`   // сервер подставляет id отправителя
	SDP       string     `json:"sdp,omitempty"`
	Candidate *Candidate `json:"candidate,omitempty"`
}

// ====== room hub ======
type Client struct {
	id   string
	room *Room
	conn *websocket.Conn
	send chan []byte
}

type Room struct {
	id      string
	clients map[string]*Client
	mu      sync.RWMutex
}

func (r *Room) add(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.clients == nil {
		r.clients = make(map[string]*Client)
	}
	r.clients[c.id] = c
}

func (r *Room) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.clients, id)
}

func (r *Room) broadcast(from string, payload []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, c := range r.clients {
		if id == from {
			continue
		}
		select {
		case c.send <- payload:
		default:
			// переполнен канал — отключаем
			go c.conn.Close(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

type Hub struct {
	rooms map[string]*Room
	mu    sync.RWMutex
}

func NewHub() *Hub { return &Hub{rooms: make(map[string]*Room)} }

func (h *Hub) get(roomID string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomID]
	if !ok {
		r = &Room{id: roomID, clients: make(map[string]*Client)}
		h.rooms[roomID] = r
	}
	return r
}

func (h *Hub) maybeDelete(roomID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if r, ok := h.rooms[roomID]; ok {
		r.mu.RLock()
		empty := len(r.clients) == 0
		r.mu.RUnlock()
		if empty {
			delete(h.rooms, roomID)
		}
	}
}

// ====== websocket handler ======
var hub = NewHub()

func wsHandler(w http.ResponseWriter, r *http.Request) {
	// Позволим любые Origin (для локальной разработки). В проде — включи проверку.
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: []string{"*"},
	})
	if err != nil {
		http.Error(w, "upgrade failed", http.StatusBadRequest)
		return
	}
	defer func() {
		_ = conn.Close(websocket.StatusNormalClosure, "")
	}()

	ctx := r.Context()
	conn.SetReadLimit(1 << 20) // 1MB

	// Ждём первый пакет "join"
	var join Envelope
	if err := readJSON(ctx, conn, &join); err != nil {
		log.Printf("join read error: %v", err)
		return
	}
	if join.Type != "join" || join.Room == "" || join.PeerID == "" {
		_ = conn.Close(websocket.StatusPolicyViolation, "first message must be join with room+peerId")
		return
	}

	room := hub.get(join.Room)
	client := &Client{
		id:   join.PeerID,
		room: room,
		conn: conn,
		send: make(chan []byte, 64),
	}
	room.add(client)
	log.Printf("peer %s joined room %s", client.id, room.id)

	// Уведомим остальных
	notify := Envelope{Type: "peer-joined", Room: room.id, PeerID: client.id}
	notifyBytes, _ := json.Marshal(notify)
	room.broadcast(client.id, notifyBytes)

	// Запускаем writer goroutine
	go writer(ctx, client)

	// Reader loop
	for {
		var env Envelope
		if err := readJSON(ctx, conn, &env); err != nil {
			if websocket.CloseStatus(err) != -1 {
				log.Printf("peer %s read close: %v", client.id, err)
			}
			break
		}

		switch env.Type {
		case "offer", "answer":
			out := Envelope{
				Type: env.Type, Room: room.id, From: client.id, SDP: env.SDP,
			}
			b, _ := json.Marshal(out)
			room.broadcast(client.id, b)

		case "ice":
			out := Envelope{
				Type: env.Type, Room: room.id, From: client.id, Candidate: env.Candidate,
			}
			b, _ := json.Marshal(out)
			room.broadcast(client.id, b)

		case "leave":
			// добровольный выход
			log.Printf("peer %s leave", client.id)
			goto EXIT

		default:
			// игнор
		}
	}

EXIT:
	// Очистка
	room.remove(client.id)
	hub.maybeDelete(room.id)
	close(client.send)
	// можно уведомить о выходе при необходимости:
	// left := Envelope{Type: "peer-left", Room: room.id, PeerID: client.id}
	// b, _ := json.Marshal(left)
	// room.broadcast(client.id, b)
}

func writer(ctx context.Context, c *Client) {
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				return
			}
			ctxWrite, cancel := context.WithTimeout(ctx, 10*time.Second)
			err := c.conn.Write(ctxWrite, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}
		case <-ping.C:
			_ = c.conn.Ping(ctx)
		}
	}
}

func readJSON(ctx context.Context, c *websocket.Conn, v any) error {
	typ, data, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText && typ != websocket.MessageBinary {
		return errors.New("unexpected ws message type")
	}
	return json.Unmarshal(data, v)
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	addr := getenv("ADDR", ":8777")

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/healthz", healthz)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// graceful shutdown
	go func() {
		log.Printf("signaling server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
	log.Println("bye")
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
