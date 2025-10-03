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

// ================= Wire protocol =================

// Candidate представляет WebRTC ICE candidate.
type Candidate struct {
	SDPMid        *string `json:"sdpMid,omitempty"`
	SDPMLineIndex *int    `json:"sdpMLineIndex,omitempty"`
	Candidate     *string `json:"candidate,omitempty"`
}

// Envelope - это обертка для всех сообщений WebSocket.
type Envelope struct {
	Type      string     `json:"type"`                // offer | answer | ice | peer-joined | peer-left | error
	Room      string     `json:"room,omitempty"`      // Сервер подставляет ID комнаты
	PeerID    string     `json:"peerId,omitempty"`    // Для 'peer-joined', 'peer-left'
	From      string     `json:"from,omitempty"`      // Сервер подставляет ID отправителя
	SDP       string     `json:"sdp,omitempty"`       // Для 'offer' и 'answer'
	Candidate *Candidate `json:"candidate,omitempty"` // Для 'ice'
	Payload   string     `json:"payload,omitempty"`   // Для 'error'
}

// ================= Hub / Rooms =================

// Client представляет одно WebSocket соединение.
type Client struct {
	id   string
	room *Room
	conn *websocket.Conn
	send chan []byte // Буферизированный канал для исходящих сообщений
	ctx  context.Context
}

// Room управляет набором клиентов в одной комнате.
type Room struct {
	id      string
	clients map[string]*Client
	mu      sync.RWMutex
}

func (r *Room) add(c *Client) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.clients[c.id] = c
}

func (r *Room) remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.clients[id]; ok {
		delete(r.clients, id)
		log.Printf("Client %s removed from room %s", id, r.id)
	}
}

// broadcast отправляет сообщение всем клиентам в комнате, кроме отправителя.
func (r *Room) broadcast(from string, payload []byte) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for id, c := range r.clients {
		if id == from {
			continue // Не отправляем сообщение самому себе
		}

		// Проверяем, не закрылся ли контекст клиента, перед отправкой
		if c.ctx.Err() != nil {
			continue
		}

		select {
		case c.send <- payload:
		default:
			// Если канал переполнен, значит клиент "тормозит".
			log.Printf("Slow consumer detected for client %s. Closing connection.", c.id)
			go c.conn.Close(websocket.StatusPolicyViolation, "slow consumer")
		}
	}
}

// Hub управляет всеми комнатами.
type Hub struct {
	rooms map[string]*Room
	mu    sync.RWMutex
}

func NewHub() *Hub { return &Hub{rooms: make(map[string]*Room)} }

// getOrCreate возвращает существующую комнату или создает новую.
func (h *Hub) getOrCreate(roomID string) *Room {
	h.mu.Lock()
	defer h.mu.Unlock()
	r, ok := h.rooms[roomID]
	if !ok {
		r = &Room{id: roomID, clients: make(map[string]*Client)}
		h.rooms[roomID] = r
		log.Printf("Room '%s' created", roomID)
	}
	return r
}

// maybeDelete удаляет комнату, если она пуста.
func (h *Hub) maybeDelete(room *Room) {
	if room == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	room.mu.RLock()
	isEmpty := len(room.clients) == 0
	room.mu.RUnlock()

	if isEmpty {
		delete(h.rooms, room.id)
		log.Printf("Room '%s' deleted as it is empty", room.id)
	}
}

// ================= WS handler =================

var hub = NewHub()

const (
	writeTimeout   = 10 * time.Second
	pingInterval   = 30 * time.Second
	readLimitBytes = 1 << 20 // 1 MiB
)

func wsHandler(w http.ResponseWriter, r *http.Request) {
	roomID := r.URL.Query().Get("room")
	peerID := r.URL.Query().Get("peer")

	if roomID == "" || peerID == "" {
		http.Error(w, "Query parameters 'room' and 'peer' are required", http.StatusBadRequest)
		return
	}

	// --- ГЛАВНОЕ ИСПРАВЛЕНИЕ: Конфигурация для локальной отладки ---
	// Для Android-эмулятора, который не отправляет заголовок Origin, нам нужно
	// отключить проверку. Для браузеров оставляем проверку.
	// Для локальной отладки с эмулятором запускайте сервер так: `WS_DEV_MODE=1 go run .`
	isDevMode := getenv("WS_DEV_MODE", "") != ""
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// В режиме разработки отключаем проверку Origin. Это РАЗРЕШИТ
		// подключение с Android-эмулятора, который не шлет этот заголовок.
		InsecureSkipVerify: isDevMode,
		// В продакшене (когда WS_DEV_MODE не установлен) будут работать только эти домены.
		OriginPatterns: []string{"jopa-call.com", "webrtc.mediarise.org"},
	})
	if err != nil {
		log.Printf("WebSocket upgrade failed for peer %s: %v", peerID, err)
		return
	}
	defer conn.Close(websocket.StatusNormalClosure, "handler finished")
	conn.SetReadLimit(readLimitBytes)

	// Контекст для этого конкретного соединения
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	room := hub.getOrCreate(roomID)
	client := &Client{
		id:   peerID,
		room: room,
		conn: conn,
		send: make(chan []byte, 256),
		ctx:  ctx,
	}
	room.add(client)
	log.Printf("Client '%s' connected to room '%s'", client.id, room.id)

	// При выходе из функции (включая панику), убираем клиента из комнаты.
	defer func() {
		room.remove(client.id)
		hub.maybeDelete(room)
		// Оповещаем остальных, что клиент ушел
		leavePayload, _ := json.Marshal(&Envelope{Type: "peer-left", PeerID: client.id})
		room.broadcast(client.id, leavePayload)
		log.Printf("Cleanup for client '%s' finished", client.id)
	}()

	// Оповещаем нового клиента обо всех, кто уже есть в комнате
	existingPeers := make([]string, 0, len(room.clients))
	room.mu.RLock()
	for id := range room.clients {
		if id != client.id {
			existingPeers = append(existingPeers, id)
		}
	}
	room.mu.RUnlock()

	for _, peer := range existingPeers {
		payload, _ := json.Marshal(&Envelope{Type: "peer-joined", PeerID: peer})
		client.send <- payload
	}

	// Оповещаем остальных участников о новом клиенте
	notifyPayload, _ := json.Marshal(&Envelope{Type: "peer-joined", PeerID: client.id})
	room.broadcast(client.id, notifyPayload)

	// Запускаем горутину для записи сообщений этому клиенту
	go writer(client)

	// Основной цикл чтения сообщений от клиента
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			if errors.Is(err, context.Canceled) {
				log.Printf("Client %s context cancelled.", client.id)
			} else if websocket.CloseStatus(err) != -1 {
				log.Printf("Connection closed by client %s: %v", client.id, err)
			} else {
				log.Printf("Read error from client %s: %v", client.id, err)
			}
			break // Выходим из цикла при любой ошибке чтения
		}

		if msgType != websocket.MessageText {
			continue // Игнорируем не-текстовые сообщения
		}

		var envelope Envelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			log.Printf("Invalid JSON from %s: %v", client.id, err)
			continue
		}

		envelope.From = client.id
		envelope.Room = room.id
		log.Printf("Received '%s' from '%s', broadcasting to room '%s'", envelope.Type, client.id, room.id)

		payload, err := json.Marshal(envelope)
		if err != nil {
			log.Printf("JSON marshal error: %v", err)
			continue
		}

		switch envelope.Type {
		case "offer", "answer", "ice":
			room.broadcast(client.id, payload)
		default:
			log.Printf("Unknown message type '%s' from client '%s'", envelope.Type, client.id)
		}
	}
}

// writer обрабатывает отправку сообщений и пинги.
func writer(c *Client) {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case msg, ok := <-c.send:
			if !ok {
				c.conn.Close(websocket.StatusNormalClosure, "send channel closed")
				return
			}
			wctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				log.Printf("Write error to %s: %v", c.id, err)
				return
			}
		case <-pingTicker.C:
			pctx, cancel := context.WithTimeout(c.ctx, writeTimeout)
			err := c.conn.Ping(pctx)
			cancel()
			if err != nil {
				log.Printf("Ping failed for %s: %v", c.id, err)
				return
			}
		}
	}
}

func healthz(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func main() {
	addr := getenv("ADDR", ":8777")
	log.Printf("Starting server on %s", addr)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", wsHandler)
	mux.HandleFunc("/healthz", healthz)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server ListenAndServe error: %v", err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop

	log.Println("Shutting down server...")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Shutdown(shutdownCtx); err != nil {
		log.Printf("HTTP server Shutdown error: %v", err)
	} else {
		log.Println("Server gracefully stopped")
	}
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
