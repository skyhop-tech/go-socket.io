package socketio

import (
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"sync"

	"github.com/skyhop-tech/go-socket.io/engineio"
	"github.com/skyhop-tech/go-socket.io/parser"
)

// Conn is a connection in go-socket.io
type Conn interface {
	io.Closer
	Namespace

	// ID returns session id
	ID() string
	UUID() string
	URL() url.URL
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
	RemoteHeader() http.Header
}

// writeQueueSize is the number of outgoing packets buffered per connection.
// A client that falls this far behind is disconnected so it can't stall
// broadcasts to everyone else.
const writeQueueSize = 256

type conn struct {
	engineio.Conn

	id         uint64
	uuid       string
	handlers   *namespaceHandlers
	namespaces *namespaces

	encoder *parser.Encoder
	decoder *parser.Decoder

	writeChan chan parser.Payload
	errorChan chan error
	quitChan  chan struct{}

	closeOnce sync.Once
}

func newConn(engineConn engineio.Conn, handlers *namespaceHandlers) *conn {
	return &conn{
		Conn:       engineConn,
		uuid:       newV4UUID(),
		encoder:    parser.NewEncoder(engineConn),
		decoder:    parser.NewDecoder(engineConn),
		errorChan:  make(chan error),
		writeChan:  make(chan parser.Payload, writeQueueSize),
		quitChan:   make(chan struct{}),
		handlers:   handlers,
		namespaces: newNamespaces(),
	}
}

func (c *conn) Close() error {
	var err error

	c.closeOnce.Do(func() {
		// for each namespace, leave all rooms, and call the disconnect handler.
		c.namespaces.Range(func(ns string, nc *namespaceConn) {
			nc.LeaveAll()

			if nh, _ := c.handlers.Get(ns); nh != nil && nh.onDisconnect != nil {
				nh.onDisconnect(nc, clientDisconnectMsg)
				// remove the room for this connection
				nh.broadcast.Clear(nc.ID())
				nh.broadcast.Clear(nc.UUID())
			}
		})
		err = c.Conn.Close()

		close(c.quitChan)
	})

	return err
}

func (c *conn) connect() error {
	rootHandler, ok := c.handlers.Get(rootNamespace)
	if !ok {
		return errUnavailableRootHandler
	}

	root := newNamespaceConn(c, aliasRootNamespace, rootHandler.broadcast)
	c.namespaces.Set(rootNamespace, root)

	root.Join([]string{root.UUID()})

	c.namespaces.Range(func(ns string, nc *namespaceConn) {
		nc.SetContext(c.Conn.Context())
	})

	header := parser.Header{
		Type: parser.Connect,
	}

	if err := c.encoder.Encode(header); err != nil {
		return err
	}

	handler, ok := c.handlers.Get(header.Namespace)
	if ok {
		_, err := handler.dispatch(root, header)
		return err
	}

	return nil
}

func (c *conn) nextID() uint64 {
	c.id++

	return c.id
}

func (c *conn) write(header parser.Header, args ...reflect.Value) {
	data := make([]interface{}, len(args))

	for i := range data {
		data[i] = args[i].Interface()
	}

	pkg := parser.Payload{
		Header: header,
		Data:   data,
	}

	// Never block: write is called from broadcasts that fan out to many
	// connections, so one slow client must not hold up the rest.
	select {
	case <-c.quitChan:
		return
	default:
	}

	select {
	case c.writeChan <- pkg:
	case <-c.quitChan:
	default:
		// Close asynchronously: the caller may hold the broadcast lock,
		// which Close needs in order to leave rooms.
		go func() {
			c.onError(rootNamespace, errWriteQueueFull)
			_ = c.Close()
		}()
	}
}

func (c *conn) onError(namespace string, err error) {
	select {
	case c.errorChan <- newErrorMessage(namespace, err):
	case <-c.quitChan:
		return
	}
}

func (c *conn) namespace(nsp string) *namespaceHandler {
	handler, _ := c.handlers.Get(nsp)
	return handler
}

func (c *conn) parseArgs(types []reflect.Type) ([]reflect.Value, error) {
	return c.decoder.DecodeArgs(types)
}

func (c *conn) UUID() string {
	return c.uuid
}
