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
	Type      string     `json:"type"`                // offer | answer | ice | peer-joined | error
	Room      string     `json:"room,omitempty"`      // Сервер подставляет ID комнаты
	PeerID    string     `json:"peerId,omitempty"`    // Для 'peer-joined'
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
		select {
		case c.send <- payload:
		default:
			// Если канал переполнен, значит клиент "тормозит".
			// Закрываем его соединение в отдельной горутине.
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
		log.Printf("Room %s created", roomID)
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
		log.Printf("Room %s deleted as it is empty", room.id)
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
	// --- ИСПРАВЛЕНИЕ 1: Получаем параметры из URL, а не ждем 'join' ---
	roomID := r.URL.Query().Get("room")
	peerID := r.URL.Query().Get("peer")

	if roomID == "" || peerID == "" {
		http.Error(w, "Missing 'room' or 'peer' query parameter", http.StatusBadRequest)
		return
	}

	// Обновляем соединение до WebSocket
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{
		// Разрешаем только доверенные источники (Origin).
		// Для разработки можно установить переменную окружения WS_DEV_MODE=1
		InsecureSkipVerify: getenv("WS_DEV_MODE", "") != "",
		OriginPatterns:     []string{"webrtc.mediarise.org"},
	})
	if err != nil {
		log.Printf("WebSocket upgrade failed: %v", err)
		// http.Error уже отправлен функцией Accept
		return
	}
	// Гарантируем закрытие соединения при выходе из функции.
	defer conn.Close(websocket.StatusNormalClosure, "handler finished")
	conn.SetReadLimit(readLimitBytes)

	// Создаем клиента и добавляем его в комнату
	room := hub.getOrCreate(roomID)
	client := &Client{
		id:   peerID,
		room: room,
		conn: conn,
		send: make(chan []byte, 256), // Увеличенный буфер для защиты от кратковременных всплесков
	}
	room.add(client)
	log.Printf("Client %s connected to room %s", client.id, room.id)

	// Оповещаем остальных участников о новом клиенте
	notifyPayload, _ := json.Marshal(&Envelope{Type: "peer-joined", PeerID: client.id})
	room.broadcast(client.id, notifyPayload)

	// Запускаем горутину для записи сообщений этому клиенту
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	go writer(ctx, client)

	// При выходе из функции (включая панику), убираем клиента из комнаты.
	defer func() {
		room.remove(client.id)
		hub.maybeDelete(room)
		leavePayload, _ := json.Marshal(&Envelope{Type: "peer-left", PeerID: client.id}) // Оповещаем, что клиент ушел
		room.broadcast(client.id, leavePayload)
	}()

	// --- ИСПРАВЛЕНИЕ 2: Упрощенный цикл чтения ---
	// Теперь мы не ждем 'join', а сразу начинаем обрабатывать сообщения.
	for {
		msgType, data, err := conn.Read(ctx)
		if err != nil {
			if websocket.CloseStatus(err) != -1 {
				log.Printf("Read error from %s: %v", client.id, err)
			}
			// Ошибка чтения означает, что клиент отсоединился. Выходим из цикла.
			break
		}

		if msgType != websocket.MessageText {
			continue // Игнорируем не-текстовые сообщения
		}

		var envelope Envelope
		if err := json.Unmarshal(data, &envelope); err != nil {
			log.Printf("Invalid JSON from %s: %v", client.id, err)
			continue // Игнорируем невалидный JSON
		}

		// --- ИСПРАВЛЕНИЕ 3: Сервер сам подставляет `from` и `room` ---
		envelope.From = client.id
		envelope.Room = room.id

		// Ре-маршаллинг для рассылки другим клиентам
		payload, err := json.Marshal(envelope)
		if err != nil {
			log.Printf("JSON marshal error: %v", err)
			continue
		}

		switch envelope.Type {
		case "offer", "answer", "ice":
			room.broadcast(client.id, payload)
		default:
			log.Printf("Unknown message type '%s' from client %s", envelope.Type, client.id)
		}
	}
}

// writer обрабатывает отправку сообщений и пинги.
func writer(ctx context.Context, c *Client) {
	pingTicker := time.NewTicker(pingInterval)
	defer pingTicker.Stop()

	for {
		select {
		case <-ctx.Done(): // Контекст соединения был отменен
			return
		case msg, ok := <-c.send:
			if !ok { // Канал `send` был закрыт
				c.conn.Close(websocket.StatusNormalClosure, "channel closed")
				return
			}

			wctx, cancel := context.WithTimeout(ctx, writeTimeout)
			err := c.conn.Write(wctx, websocket.MessageText, msg)
			cancel()
			if err != nil {
				log.Printf("Write error to %s: %v", c.id, err)
				return // Завершаем горутину при ошибке записи
			}
		case <-pingTicker.C:
			// Используем Ping, который сам ждет Pong.
			// Если Pong не придет, вернется ошибка и мы выйдем из горутины.
			if err := c.conn.Ping(ctx); err != nil {
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
		IdleTimeout:       120 * time.Second, // Добавлено для лучшего управления ресурсами
	}

	// Запуск сервера в горутине, чтобы не блокировать main.
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("HTTP server ListenAndServe error: %v", err)
		}
	}()

	// Настройка Graceful Shutdown.
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
