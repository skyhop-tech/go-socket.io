package socketio

import (
	"context"
	"fmt"
	"time"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
)

// redisBroadcast gives Join, Leave & BroadcastTO server API support to socket.io along with room management
// map of rooms where each room contains a map of connection id to connections in that room
type redisBroadcast struct {
	uid string
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

	pErr := client.Publish("channel1", "payload").Err()
	if pErr != nil {
		panic(pErr)
	}

	uid := newV4UUID()
	rbc := &redisBroadcast{
		uid: uid,
	}

	_, err = rbc.subscribe(client, "channel1")
	if err != nil {
		panic(err)
	}

	//for m := range sub.Channel() {
	//fmt.Println("got a message", m)
	//}

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

func (bc *redisBroadcast) subscribe(client *redis.Client, channel string) (<-chan *redis.Message, error) {

	sub := client.Subscribe("channel1")

	// Force subscription to wait
	subscription, err := sub.Receive()
	if err != nil {
		return nil, err
	}

	// Should be *Subscription, but others are possible if other actions have been
	// taken on sub since it was created.
	switch subscription.(type) {
	case *redis.Subscription:
		fmt.Println("subscribe succeeded")
	case *redis.Message:
		fmt.Println("received first message")
	case *redis.Pong:
		fmt.Println("pong received")
	default:
		fmt.Println("handle error")
	}

	return sub.Channel(), err
}
