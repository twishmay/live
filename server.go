package live

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/sessions"
	"github.com/jfyne/live/internal/embed"
	"golang.org/x/net/html"
	"golang.org/x/time/rate"
	"nhooyr.io/websocket"
)

type viewSockets struct {
	view    *View
	sockets map[*Socket]struct{}
}

// Server enables broadcasting to a set of subscribers.
type Server struct {
	// subscriberMessageBuffer controls the max number
	// of messages that can be queued for a subscriber
	// before it is kicked.
	//
	// Defaults to 16.
	subscriberMessageBuffer int

	// publishLimiter controls the rate limit applied to the publish endpoint.
	//
	// Defaults to one publish every 100ms with a burst of 8.
	publishLimiter *rate.Limiter

	// logf controls where logs are sent.
	// Defaults to log.Printf.
	logf func(f string, v ...interface{})

	// serveMux routes the various endpoints to the appropriate handler.
	serveMux mux.Router

	// session store
	store      sessions.Store
	sessionKey string

	// All of our current sockets.
	socketsMu sync.Mutex
	sockets   map[*Socket]struct{}

	// All of our current views and their sockets.
	viewsMu sync.Mutex
	views   map[string]*viewSockets
}

// NewServer constructs a Server with the defaults.
func NewServer(sessionKey string, secret []byte) *Server {
	// Get live javascript template.
	liveJS := template.Must(template.New("live.js").Parse(string(embed.Get("/live.js"))))

	s := &Server{
		subscriberMessageBuffer: 16,
		logf:                    log.Printf,
		sockets:                 make(map[*Socket]struct{}),
		publishLimiter:          rate.NewLimiter(rate.Every(time.Millisecond*100), 8),
		store:                   sessions.NewCookieStore(secret),
		sessionKey:              sessionKey,
		views:                   make(map[string]*viewSockets),
	}
	s.serveMux.HandleFunc("/live.js", func(w http.ResponseWriter, r *http.Request) {
		if err := liveJS.Execute(w, nil); err != nil {
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
		}
	})

	return s
}

// Add registers a view with the server.
func (s *Server) Add(view *View) {
	// Register the view
	s.views[view.path] = &viewSockets{
		view:    view,
		sockets: make(map[*Socket]struct{}),
	}

	// Handle regular http requests.
	s.serveMux.HandleFunc(view.path, s.viewHTTP(view))

	// Handle socket connections for the view.
	s.serveMux.HandleFunc("/"+path.Join("socket", view.path), s.viewWS(view))

	go func(v *View) {
		for {
			select {
			case m := <-v.emitter:
				log.Println(m)
			}
		}
	}(view)
}

// ServeHTTP.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.serveMux.ServeHTTP(w, r)
}

func (s *Server) getSession(r *http.Request) (Session, error) {
	var sess Session
	session, err := s.store.Get(r, s.sessionKey)
	if err != nil {
		return NewSession(), err
	}

	v, ok := session.Values[SessionKey]
	if !ok {
		log.Println("failed to find existing session")
		// Create new connection.
		ns := NewSession()
		sess = ns
	}
	sess, ok = v.(Session)
	if !ok {
		log.Println("failed to assert session type")
		// Create new connection and set.
		ns := NewSession()
		sess = ns
	}
	return sess, nil
}

func (s *Server) saveSession(w http.ResponseWriter, r *http.Request, session Session) error {
	c, err := s.store.Get(r, s.sessionKey)
	if err != nil {
		return err
	}
	c.Values[SessionKey] = session
	return c.Save(r, w)
}

func (s *Server) viewHTTP(view *View) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := mux.Vars(r)

		// Get session.
		session, err := s.getSession(r)
		if err != nil {
			s.logf("session get err: %w", err)
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		// Get socket.
		sock := NewSocket(session)

		if err := sock.HandleView(r.Context(), view, params); err != nil {
			s.logf("socket handle view err: %w", err)
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		var rendered bytes.Buffer
		html.Render(&rendered, sock.currentRender)

		if err := s.saveSession(w, r, session); err != nil {
			s.logf("session save err: %w", err)
			w.WriteHeader(500)
			w.Write([]byte(err.Error()))
			return
		}

		w.WriteHeader(200)
		io.Copy(w, &rendered)
	}
}

func (s *Server) viewWS(view *View) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		params := mux.Vars(r)
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			s.logf("%v", err)
			return
		}
		defer c.Close(websocket.StatusInternalError, "")

		// Get the session from the http request.
		session, err := s.getSession(r)
		err = s.viewSocket(r.Context(), view, params, session, c)
		if errors.Is(err, context.Canceled) {
			return
		}
		if websocket.CloseStatus(err) == websocket.StatusNormalClosure ||
			websocket.CloseStatus(err) == websocket.StatusGoingAway {
			return
		}
		if err != nil {
			s.logf("%v", err)
			return
		}
	}
}

func (s *Server) viewSocket(ctx context.Context, view *View, params map[string]string, session Session, c *websocket.Conn) error {
	// Get the sessions socket and register it with the server.
	sock := NewSocket(session)
	sock.AssignWS(c)
	s.addSocket(sock)
	defer s.deleteSocket(sock)
	s.addViewSocket(view.path, sock)
	defer s.deleteViewSocket(view.path, sock)

	s.logf("%s connected to %s", session.ID, view.path)

	if err := sock.HandleView(ctx, view, params); err != nil {
		return fmt.Errorf("socket handle error: %w", err)
	}

	// Handle events coming from the websocket connection.
	readError := make(chan error)
	go func() {
		for {
			t, d, err := c.Read(ctx)
			if err != nil {
				readError <- err
				break
			}
			switch t {
			case websocket.MessageText:
				var m SocketMessage
				if err := json.Unmarshal(d, &m); err != nil {
					readError <- err
					break
				}
				if err := view.handleEvent(m.T, sock, m); err != nil {
					if !errors.Is(err, ErrNoEventHandler) {
						readError <- err
						break
					} else {
						s.logf("%s", err)
					}
				}
			case websocket.MessageBinary:
				log.Println("binary messages unhandled")
			}
		}
		close(readError)
	}()

	// Send events to the websocket connection.
	for {
		select {
		case err := <-readError:
			if err != nil {
				writeTimeout(ctx, time.Second*5, c, SocketMessage{T: EventError, Data: err.Error()})
				return fmt.Errorf("read error: %w", err)
			}
		case msg := <-sock.msgs:
			if err := writeTimeout(ctx, time.Second*5, c, msg); err != nil {
				return err
			}
		case <-ctx.Done():
		}
	}
}

func (s *Server) broadcast(msg SocketMessage) {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()

	s.publishLimiter.Wait(context.Background())

	for c := range s.sockets {
		select {
		case c.msgs <- msg:
		default:
			go c.closeSlow()
		}
	}
}

// addSocket registers a socket with the server.
func (s *Server) addSocket(c *Socket) {
	s.socketsMu.Lock()
	defer s.socketsMu.Unlock()

	s.sockets[c] = struct{}{}
}

// addViewSocket  registers a socket with a view on the server.
func (s *Server) addViewSocket(view string, c *Socket) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()

	_, ok := s.views[view]
	if !ok {
		s.logf("no such view to add socket: %s", view)
		return
	}
	s.views[view].sockets[c] = struct{}{}
}

// deleteSocket deletes the given socket from the server.
func (s *Server) deleteSocket(c *Socket) {
	s.socketsMu.Lock()
	delete(s.sockets, c)
	s.socketsMu.Unlock()
}

// deleteViewSocket deletes the given socket from the view.
func (s *Server) deleteViewSocket(view string, c *Socket) {
	s.viewsMu.Lock()
	defer s.viewsMu.Unlock()

	_, ok := s.views[view]
	if !ok {
		s.logf("no such view to delete socket: %s", view)
		return
	}
	delete(s.views[view].sockets, c)
}

func writeTimeout(ctx context.Context, timeout time.Duration, c *websocket.Conn, msg SocketMessage) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	data, err := json.Marshal(&msg)
	if err != nil {
		return err
	}

	return c.Write(ctx, websocket.MessageText, data)
}

// RunServer run a live server.
func RunServer(ls *Server) error {
	l, err := net.Listen("tcp", ":8080")
	if err != nil {
		return err
	}
	log.Printf("listening on http://%v", l.Addr())

	s := &http.Server{
		Handler:      ls,
		ReadTimeout:  time.Second * 10,
		WriteTimeout: time.Second * 10,
	}
	errc := make(chan error, 1)
	go func() {
		errc <- s.Serve(l)
	}()

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	select {
	case err := <-errc:
		log.Printf("failed to serve: %v", err)
	case sig := <-sigs:
		log.Printf("terminating: %v", sig)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second*10)
	defer cancel()

	return s.Shutdown(ctx)
}