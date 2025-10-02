package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"nhooyr.io/websocket"
)

// ================= Wire protocol =================

type Candidate struct {
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *int    `json:"sdpMLineIndex,omitempty"`
	Candidate     *string `json:"candidate,omitempty"`
}

type Envelope struct {
	Type      string     `json:"type"` // join | peer-joined | offer | answer | ice | leave
	Room      string     `json:"room,omitempty"`
	PeerID    string     `json:"peerId,omitempty"` // исходящие "peer-joined"
	From      string     `json:"from,omitempty"`   // сервер подставляет id отправителя
	SDP       string     `json:"sdp,omitempty"`
	Candidate *Candidate `json:"candidate,omitempty"`
}

// ================= Hub / Rooms =================

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
			// медленный потребитель — закрываем
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

// ================= WS handler =================

var hub = NewHub()

const (
	readLimitBytes = 1 << 20 // 1 MiB
	joinTimeout    = 15 * time.Second
	writeTimeout   = 10 * time.Second
	pingInterval   = 30 * time.Second
	pongTimeout    = 60 * time.Second // сколько ждём PONG на наш PING
)

func wsHandler(w http.ResponseWriter, r *http.Request) {
	// Разрешаем Origin:
	// PROD: укажи точный домен; DEV: "*" (ниже переключается env-ом).
	originPatterns := []string{"webrtc.mediarise.org"}
	if getenv("WS_ALLOW_ANY_ORIGIN", "") != "" {
		originPatterns = []string{"*"}
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		OriginPatterns: originPatterns,
	})
	if err != nil {
		http.Error(w, "upgrade failed", http.StatusBadRequest)
		return
	}
	defer func() { _ = conn.Close(websocket.StatusNormalClosure, "") }()

	// Установим лимит на входящие сообщения
	conn.SetReadLimit(readLimitBytes)

	// Собственный контекст соединения, чтобы можно было его отменить
	connCtx, cancelConn := context.WithCancel(r.Context())
	defer cancelConn()

	// 1) Ждём первый пакет "join" с таймаутом
	var join Envelope
	{
		ctx, cancel := context.WithTimeout(connCtx, joinTimeout)
		err = readJSONSkippingEmpty(ctx, conn, &join)
		cancel()
		if err != nil {
			log.Printf("join read error: %v", err)
			_ = conn.Close(websocket.StatusPolicyViolation, "first message must be join")
			return
		}
		if join.Type != "join" || join.Room == "" || join.PeerID == "" {
			_ = conn.Close(websocket.StatusPolicyViolation, "first message must be join with room+peerId")
			return
		}
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

	// Уведомляем остальных в комнате
	notify := Envelope{Type: "peer-joined", Room: room.id, PeerID: client.id}
	if b, _ := json.Marshal(notify); b != nil {
		room.broadcast(client.id, b)
	}

	// 2) Writer goroutine: серверные PING'и + отправка сообщений
	go writer(connCtx, client)

	// 3) Reader loop
	for {
		var env Envelope
		if err := readJSONSkippingEmpty(connCtx, conn, &env); err != nil {
			// клиент закрылся или сеть/таймаут
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
			if b, _ := json.Marshal(out); b != nil {
				room.broadcast(client.id, b)
			}

		case "ice":
			out := Envelope{
				Type: env.Type, Room: room.id, From: client.id, Candidate: env.Candidate,
			}
			if b, _ := json.Marshal(out); b != nil {
				room.broadcast(client.id, b)
			}

		case "leave":
			log.Printf("peer %s leave", client.id)
			goto EXIT

		default:
			// игнорируем неизвестные типы
		}
	}

EXIT:
	// cleanup
	room.remove(client.id)
	hub.maybeDelete(room.id)
	close(client.send)
}

// writer: периодический PING + отправка накопленных сообщений
func writer(ctx context.Context, c *Client) {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done():
			return

		case msg, ok := <-c.send:
			if !ok {
				return
			}
			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				return
			}

		case <-pingTicker.C:
			// Ping блокирует до получения PONG (или до таймаута контекста)
			pctx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				// Нет PONG — разрываем
				_ = c.conn.Close(websocket.StatusPolicyViolation, "pong timeout")
				return
			}
		}
	}
}

// readJSONSkippingEmpty читает следующее непустое текстовое/binary сообщение и парсит JSON.
// Пустые и не текстовые — игнорирует.
func readJSONSkippingEmpty(ctx context.Context, c *websocket.Conn, v any) error {
	for {
		typ, data, err := c.Read(ctx)
		if err != nil {
			return err
		}
		if len(data) == 0 {
			// пустая строка — бывает от клиентов вроде wscat при лишнем Enter
			continue
		}
		if typ != websocket.MessageText && typ != websocket.MessageBinary {
			// не текст — игнор
			continue
		}
		if err := json.Unmarshal(data, v); err != nil {
			// логируем и продолжаем ждать валидный JSON (не рвём соединение)
			log.Printf("bad json: %v, raw=%q", err, truncateForLog(data, 256))
			continue
		}
		return nil
	}
}

func truncateForLog(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return fmt.Sprintf("%s...(+%d bytes)", string(b[:n]), len(b)-n)
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

	// http server
	go func() {
		log.Printf("signaling server listening on %s", addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("http server error: %v", err)
		}
	}()

	// graceful shutdown
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
