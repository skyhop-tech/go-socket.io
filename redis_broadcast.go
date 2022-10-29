package socketio

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

// RedisAdapterOptions is configuration to create new adapter
type RedisAdapterOptions struct {
	Host     string
	Port     int
	Database int
	Prefix   string
	Network  string
	Password string
	Logger   *logrus.Logger
}

// redisBroadcast gives Join, Leave & BroadcastTO server API support to socket.io along with room management
// map of rooms where each room contains a map of connection id to connections in that room
type redisBroadcast struct {
	uid    string
	client *redis.Client
	// Used to prefix all redis keys / channels
	prefix string
	logger *logrus.Logger
}

func newRedisBroadcast(ctx context.Context, nsp string, opts *RedisAdapterOptions) (*redisBroadcast, error) {
	addr := fmt.Sprintf("%s:%d", opts.Host, opts.Port)

	client := redis.NewClient(&redis.Options{
		Addr:        addr,
		DB:          opts.Database,
		Network:     opts.Network,
		MaxConnAge:  0,
		IdleTimeout: 1 * time.Hour,   // default is 5 mins
		ReadTimeout: 5 * time.Minute, // default is 3 seconds
		MaxRetries:  3,
	})

	_, err := client.Ping().Result()
	if err != nil {
		return nil, errors.Wrap(err, "ping redis")
	}

	pErr := client.Publish(fmt.Sprintf("%s%s", opts.Prefix, "backbone"), "payload").Err()
	if pErr != nil {
		panic(pErr)
	}

	uid := newV4UUID()
	rbc := &redisBroadcast{
		uid:    uid,
		client: client,
		prefix: opts.Prefix,
		logger: opts.Logger,
	}

	// We will use a single channel as the backbone for sending messages
	// between instances of this application. Once a message is received
	// on this channel it is propagated to all clients connected to this
	// application instance.
	lErr := rbc.listen(fmt.Sprintf("%s%s", opts.Prefix, "backbone"))
	if lErr != nil {
		return nil, errors.Wrap(lErr, "listen")
	}

	return rbc, nil
}

// AllRooms gives list of all rooms available for redisBroadcast.
func (bc *redisBroadcast) AllRooms() []string {
	return nil
}

// Join joins the given connection to the redisBroadcast room.
func (bc *redisBroadcast) Join(room string, connection Conn) {
}

// Leave leaves the given connection from given room (if exist)
func (bc *redisBroadcast) Leave(room string, connection Conn) {
}

// LeaveAll leaves the given connection from all rooms.
func (bc *redisBroadcast) LeaveAll(connection Conn) {
}

// Clear clears the room.
func (bc *redisBroadcast) Clear(room string) {
}

// Send sends given event & args to all the connections in the specified room.
func (bc *redisBroadcast) Send(room, event string, args ...interface{}) {
}

// SendAll sends given event & args to all the connections to all the rooms.
func (bc *redisBroadcast) SendAll(event string, args ...interface{}) {
}

// ForEach sends data returned by DataFunc, if room does not exits sends nothing.
func (bc *redisBroadcast) ForEach(room string, f EachFunc) {
}

// Len gives number of connections in the room.
func (bc *redisBroadcast) Len(room string) int {
	return 0
}

// Rooms gives the list of all the rooms available for redisBroadcast in case of
// no connection is given, in case of a connection is given, it gives
// list of all the rooms the connection is joined to.
func (bc *redisBroadcast) Rooms(connection Conn) []string {
	return nil
}

// A message gets passed back and forth between
// instances of this application
type message struct {
	// Content of the message
	Content string `json:"content"`
	// Rooms to whic this message is intended to be
	// sent. When we get a message with rooms that
	// we're not currently tracking in memory we'll
	// add it them.
	Rooms []string `json:"rooms"`
}

// Listen on a goroutine for incoming messages & decode
// each message.
func (bc *redisBroadcast) listen(channel string) error {
	incoming, err := subscribe(bc.client, channel)
	if err != nil {
		return errors.Wrap(err, "subscribe")
	}

	bc.logger.Debugf("Subscribed to channel %s", channel)

	go func() {
		for {
			select {
			case in := <-incoming:
				if in == nil {
					fmt.Printf("INCOMING nil: closing\n", in)
					break
				}
				var m message
				err := json.Unmarshal([]byte(in.Payload), &m)
				if err != nil {
					bc.logger.WithFields(logrus.Fields{
						"channel": channel,
						"error":   err,
						"message": in.Payload,
					}).Error("Channel sent an invalid message format, dropping message...")
					continue
				}
				fmt.Printf("INCOMING: %+v\n", m)
			}
		}
		fmt.Println("Shutting down go routine listening")
	}()

	return nil
}

func subscribe(client *redis.Client, channel string) (<-chan *redis.Message, error) {

	sub := client.Subscribe(channel)

	// Force subscription to wait for redis
	// to reply
	subscription, err := sub.Receive()
	if err != nil {
		return nil, errors.Wrapf(err, "receive on channel %s", channel)
	}

	// Should be *Subscription, but others are possible if other actions have been
	// taken on sub since it was created.
	switch subscription.(type) {
	case *redis.Subscription:
		// Subscribe succeeded
	case *redis.Message:
		// Message came in very early, we'll ignore it
	case *redis.Pong:
		// Healthcheck
	default:
		return nil, errors.Errorf("failed to subscribe to channel %s", channel)
	}

	return sub.Channel(), nil
}
