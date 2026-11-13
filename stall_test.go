//go:build stall

// Simulates a single unresponsive client (e.g. a phone that lost signal:
// TCP stays half-open, client stops reading and stops pinging) and measures
// the effect on healthy clients in *other* rooms, on the same pod and on a
// second pod sharing the same Redis stream.
//
// Requires a Redis server. Start one with:
//
//	redis-server --port 6390 --save '' --appendonly no
//
// Run with:
//
//	go test -tags stall -run TestSlowClientStall -v -count=1 .
//
// Env:
//
//	STALL_REDIS_ADDR    redis host:port            (default 127.0.0.1:6390)
//	STALL_PING_TIMEOUT  engine.io ping timeout     (default 15s; prod uses 60s)
package socketio_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-redis/redis"
	gws "github.com/gorilla/websocket"
	"github.com/sirupsen/logrus"

	socketio "github.com/skyhop-tech/go-socket.io"
	"github.com/skyhop-tech/go-socket.io/engineio"
	"github.com/skyhop-tech/go-socket.io/engineio/transport"
	"github.com/skyhop-tech/go-socket.io/engineio/transport/websocket"
)

const (
	healthyRoom = "healthy"
	slowRoom    = "slow"
	tickEvent   = "tick"

	// Acceptable worst-case delivery latency for a healthy client.
	maxHealthyLatency = time.Second
)

// sample is one delivered (or one published) message.
type sample struct {
	at      time.Duration // offset from test start
	latency time.Duration
}

type recorder struct {
	mu      sync.Mutex
	samples map[string][]sample
}

func (r *recorder) add(key string, s sample) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.samples[key] = append(r.samples[key], s)
}

// smallBufListener shrinks the server-side socket send buffer so a client that
// stops reading blocks the server's writes after a few KB rather than a few MB.
// In production the same thing happens, just after more data.
type smallBufListener struct{ net.Listener }

func (l smallBufListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.SetWriteBuffer(4096)
	}
	return c, err
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func newPod(t *testing.T, prefix string, pingTimeout time.Duration) (*socketio.Server, string) {
	t.Helper()

	host, portStr, err := net.SplitHostPort(envOr("STALL_REDIS_ADDR", "127.0.0.1:6390"))
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(portStr)

	logger := logrus.New()
	logger.SetLevel(logrus.WarnLevel)

	srv := socketio.NewServer(&engineio.Options{
		PingTimeout:  pingTimeout,
		PingInterval: 5 * time.Second,
		Transports: []transport.Transport{
			&websocket.Transport{CheckOrigin: func(*http.Request) bool { return true }},
		},
	})
	// Same streaming settings as values.prod.yaml
	srv.Adapter(&socketio.RedisAdapterOptions{
		Host:                    host,
		Port:                    port,
		Prefix:                  prefix,
		Network:                 "tcp",
		Logger:                  logger,
		UseStreaming:            true,
		StreamMaxLength:         10000,
		ConsumerBatchSize:       10,
		ConsumerThreadCount:     10,
		ConsumerBlockTtlSeconds: 5,
		JoinViaCallbackFunc: func(interface{}, string, string) (bool, error) {
			return false, nil
		},
	})

	ctx := context.Background()
	srv.OnConnect(ctx, "/", func(c socketio.Conn) error {
		u := c.URL()
		srv.JoinRooms("/", []string{u.Query().Get("room")}, c)
		return nil
	})
	srv.OnDisconnect(ctx, "/", func(socketio.Conn, string) {})
	srv.OnError(ctx, "/", func(socketio.Conn, error) {})

	go srv.Serve()

	ts := httptest.NewUnstartedServer(srv)
	ts.Listener = smallBufListener{ts.Listener}
	ts.Start()

	t.Cleanup(func() {
		ts.Close()
		_ = srv.Close()
	})

	return srv, "ws" + strings.TrimPrefix(ts.URL, "http")
}

// dial opens an EIO=3 websocket session and waits for the socket.io connect
// packet ("40").
func dial(t *testing.T, base, room string, rcvBuf int) *gws.Conn {
	t.Helper()

	d := gws.Dialer{
		HandshakeTimeout: 5 * time.Second,
		NetDial: func(network, addr string) (net.Conn, error) {
			c, err := net.Dial(network, addr)
			if err != nil {
				return nil, err
			}
			if rcvBuf > 0 {
				_ = c.(*net.TCPConn).SetReadBuffer(rcvBuf)
			}
			return c, nil
		},
	}
	ws, _, err := d.Dial(fmt.Sprintf("%s/socket.io/?EIO=3&transport=websocket&room=%s", base, room), nil)
	if err != nil {
		t.Fatalf("dial %s: %v", room, err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("handshake %s: %v", room, err)
		}
		if string(msg) == "40" {
			break
		}
	}
	_ = ws.SetReadDeadline(time.Time{})
	return ws
}

// runHealthy reads continuously and pings like a real client, recording the
// delivery latency of each tick.
func runHealthy(ws *gws.Conn, key string, start time.Time, rec *recorder, done <-chan struct{}) {
	go func() {
		t := time.NewTicker(3 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := ws.WriteMessage(gws.TextMessage, []byte("2")); err != nil {
					return
				}
			}
		}
	}()
	go func() {
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if !strings.HasPrefix(string(msg), "42") {
				continue
			}
			var args []string
			if err := json.Unmarshal(msg[2:], &args); err != nil || len(args) != 2 || args[0] != tickEvent {
				continue
			}
			sent, err := strconv.ParseInt(args[1], 10, 64)
			if err != nil {
				continue
			}
			now := time.Now()
			rec.add(key, sample{at: now.Sub(start), latency: now.Sub(time.Unix(0, sent))})
		}
	}()
}

// publish broadcasts a tick every interval from the given pod, recording how
// long the BroadcastToRoom call itself blocks (in chat-api this is the
// sendMessage handler of the user who is typing).
func publish(srv *socketio.Server, key string, interval time.Duration, start time.Time, rec *recorder, done <-chan struct{}) {
	go func() {
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				before := time.Now()
				srv.BroadcastToRoom("/", healthyRoom, tickEvent, strconv.FormatInt(before.UnixNano(), 10))
				rec.add(key, sample{at: before.Sub(start), latency: time.Since(before)})
			}
		}
	}()
}

// pendingPerGroup reads XINFO GROUPS for the backbone stream.
func pendingPerGroup(rdb *redis.Client, stream string) []int64 {
	res, err := rdb.Do("XINFO", "GROUPS", stream).Result()
	if err != nil {
		return nil
	}
	var out []int64
	groups, _ := res.([]interface{})
	for _, g := range groups {
		kv, _ := g.([]interface{})
		for i := 0; i+1 < len(kv); i += 2 {
			if k, _ := kv[i].(string); k == "pending" {
				n, _ := kv[i+1].(int64)
				out = append(out, n)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] > out[j] })
	return out
}

func TestSlowClientStall(t *testing.T) {
	pingTimeout, err := time.ParseDuration(envOr("STALL_PING_TIMEOUT", "15s"))
	if err != nil {
		t.Fatal(err)
	}

	const (
		healthyPerPod = 10
		baseline      = 5 * time.Second
		tickInterval  = 200 * time.Millisecond
	)
	recovery := 10 * time.Second

	prefix := fmt.Sprintf("stalltest-%d-", time.Now().UnixNano())
	stream := prefix + "backbone"

	rdb := redis.NewClient(&redis.Options{Addr: envOr("STALL_REDIS_ADDR", "127.0.0.1:6390")})
	if err := rdb.Ping().Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() {
		// The adapter's consumer goroutines never stop, so deleting the stream
		// here makes them spin on NOGROUP errors. Clean up with:
		//   redis-cli -p 6390 --scan --pattern 'stalltest-*' | xargs redis-cli -p 6390 del
		rdb.Close()
	})

	podA, urlA := newPod(t, prefix, pingTimeout)
	podB, urlB := newPod(t, prefix, pingTimeout)

	done := make(chan struct{})
	t.Cleanup(func() { close(done) })

	rec := &recorder{samples: map[string][]sample{}}
	start := time.Now()

	for i := 0; i < healthyPerPod; i++ {
		runHealthy(dial(t, urlA, healthyRoom, 0), "podA healthy", start, rec, done)
		runHealthy(dial(t, urlB, healthyRoom, 0), "podB healthy", start, rec, done)
	}

	// The unresponsive client: tiny receive buffer, never reads, never pings.
	slow := dial(t, urlA, slowRoom, 1024)
	defer slow.Close()

	// Let the Join messages propagate so both pods know every room.
	time.Sleep(time.Second)
	start = time.Now()

	publish(podA, "podA BroadcastToRoom", tickInterval, start, rec, done)
	publish(podB, "podB BroadcastToRoom", tickInterval, start, rec, done)

	// Sample Redis consumer-group backlog once a second.
	var pendingMu sync.Mutex
	pending := map[int][]int64{}
	go func() {
		tk := time.NewTicker(time.Second)
		defer tk.Stop()
		for {
			select {
			case <-done:
				return
			case <-tk.C:
				sec := int(time.Since(start) / time.Second)
				p := pendingPerGroup(rdb, stream)
				pendingMu.Lock()
				pending[sec] = p
				pendingMu.Unlock()
			}
		}
	}()

	time.Sleep(baseline)

	// Something is sent to the slow client's room (a chat message, a typing
	// indicator...). Payload is large only so the tiny socket buffers fill
	// immediately; in production small messages fill them over time.
	t.Logf("t=%s: sending 5 x 64KB messages to the room of the unresponsive client on pod A", time.Since(start).Round(time.Millisecond))
	big := strings.Repeat("x", 64*1024)
	for i := 0; i < 5; i++ {
		podB.BroadcastToRoom("/", slowRoom, "big", big)
	}

	time.Sleep(pingTimeout + recovery)
	total := time.Since(start)

	// Report
	keys := []string{"podA healthy", "podB healthy", "podA BroadcastToRoom", "podB BroadcastToRoom"}
	rec.mu.Lock()
	defer rec.mu.Unlock()

	buckets := int(total/time.Second) + 1
	maxBy := map[string][]time.Duration{}
	countBy := map[string][]int{}
	for _, k := range keys {
		maxBy[k] = make([]time.Duration, buckets)
		countBy[k] = make([]int, buckets)
		for _, s := range rec.samples[k] {
			b := int(s.at / time.Second)
			if b >= buckets {
				continue
			}
			countBy[k][b]++
			if s.latency > maxBy[k][b] {
				maxBy[k][b] = s.latency
			}
		}
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\nping timeout=%s, %d healthy clients per pod, ticks every %s from each pod\n", pingTimeout, healthyPerPod, tickInterval)
	fmt.Fprintf(&sb, "per-second worst latency (deliveries received in that second)\n\n")
	fmt.Fprintf(&sb, "%4s | %-20s | %-20s | %-14s | %-14s | %s\n", "sec", "podA healthy recv", "podB healthy recv", "podA send call", "podB send call", "redis pending/group")
	pendingMu.Lock()
	for b := 0; b < buckets; b++ {
		marker := ""
		if time.Duration(b)*time.Second >= baseline && time.Duration(b)*time.Second < baseline+time.Second {
			marker = "  <- slow client's room gets traffic"
		}
		fmt.Fprintf(&sb, "%4d | %8s (%4d msgs) | %8s (%4d msgs) | %14s | %14s | %v%s\n",
			b,
			maxBy["podA healthy"][b].Round(time.Millisecond), countBy["podA healthy"][b],
			maxBy["podB healthy"][b].Round(time.Millisecond), countBy["podB healthy"][b],
			maxBy["podA BroadcastToRoom"][b].Round(time.Millisecond),
			maxBy["podB BroadcastToRoom"][b].Round(time.Millisecond),
			pending[b], marker)
	}
	pendingMu.Unlock()
	t.Log(sb.String())

	for _, k := range keys {
		var worst time.Duration
		for _, s := range rec.samples[k] {
			if s.latency > worst {
				worst = s.latency
			}
		}
		if worst > maxHealthyLatency {
			t.Errorf("%s: worst latency %s exceeds %s", k, worst.Round(time.Millisecond), maxHealthyLatency)
		} else {
			t.Logf("%s: worst latency %s", k, worst.Round(time.Millisecond))
		}
	}
}

// TestSlowClientIsDropped checks that an unresponsive client receiving
// sustained traffic is disconnected once its write queue overflows, rather
// than holding resources until the ping timeout.
func TestSlowClientIsDropped(t *testing.T) {
	pingTimeout, err := time.ParseDuration(envOr("STALL_PING_TIMEOUT", "15s"))
	if err != nil {
		t.Fatal(err)
	}

	rdb := redis.NewClient(&redis.Options{Addr: envOr("STALL_REDIS_ADDR", "127.0.0.1:6390")})
	if err := rdb.Ping().Err(); err != nil {
		t.Skipf("redis not reachable: %v", err)
	}
	t.Cleanup(func() { rdb.Close() })

	pod, url := newPod(t, fmt.Sprintf("stalltest-%d-", time.Now().UnixNano()), pingTimeout)

	dropped := make(chan error, 1)
	pod.OnError(context.Background(), "/", func(c socketio.Conn, err error) {
		if c == nil {
			return
		}
		if u := c.URL(); u.Query().Get("room") == slowRoom {
			select {
			case dropped <- err:
			default:
			}
		}
	})

	slow := dial(t, url, slowRoom, 1024)
	defer slow.Close()
	time.Sleep(time.Second)

	start := time.Now()
	payload := strings.Repeat("x", 1024)
	for i := 0; i < 2*256; i++ {
		pod.BroadcastToRoom("/", slowRoom, "msg", payload)
	}

	select {
	case err := <-dropped:
		t.Logf("unresponsive client dropped after %s: %v", time.Since(start).Round(time.Millisecond), err)
	case <-time.After(pingTimeout / 2):
		t.Fatalf("unresponsive client not dropped within %s", pingTimeout/2)
	}
}
