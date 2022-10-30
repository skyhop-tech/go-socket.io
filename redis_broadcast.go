package socketio

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/go-redis/redis"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type messageType string

const (
	// Represents a normal chat message
	// intended to be broadcasted to all
	// connections in the given chat room.
	ChatType messageType = "chat"

	// Broadcased when a connection joins
	// a room
	JoinType messageType = "join"

	// Publish this type of message to
	// tell other instances to clear
	// the given rooms
	ClearType messageType = "clear"
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

type redisBroadcast struct {
	// We maintain a single connection to redis
	// for this namespace.
	client *redis.Client

	logger *logrus.Logger

	// Any thread unsafe values should be keept
	// in here
	unsafe unsafe
}

// We keep any thread unsafe variables here
// just to be explicitly about the need to
// user locks.
type unsafe struct {
	// Use this lock when necessary to make the following
	// fields safe for concurrent use.
	lock sync.RWMutex

	// Unique ID to identify this instance
	instanceId string

	// Used to prefix all redis keys / channels
	prefix  string
	channel string

	// Maps of all rooms that this instance knows about.
	// When other instances broadcast message, they include
	// the target rooms. We use that information to constantly
	// update this map. This means we may have rooms that
	// dont necessarily map to connections on this instance
	// but does on others.
	rooms map[string]description
}

// Describes the rooms including which clients
// this application instance maintains connections
// for.
type description struct {
	// Name of the room
	name string

	// Map of connectionIDs to connections
	// maintained by this instance in the
	// room. The len of this map represents
	// the number of members in the room
	// held by this instance.
	connections map[string]Conn

	// Map of instanceIDs to number of members
	// reported by that instance. Represents
	// the number of members in the room
	// held by each instance including this
	// one.
	reportedMembers map[string]int
}

// A message gets passed back and forth between
// instances of this application
type message struct {
	// Instance ID of the sender of this message.
	// We need this information to maintain connection
	// counts between all instances of this application.
	//
	// If instance 1 has 2 connections in room A
	// and instance 2 has 1 connection. We can conclude
	// that there are 3 connections in room A.
	InstanceId string `json:"instance_id"`

	// The ID of the client connection related to this
	// message.
	ClientId *string `json:"client_id"`

	// The type of message determines what we should do
	// with it e.g. a "chat" message should have its
	// message emitted to all connections in the target
	// rooms.
	Type messageType `json:"type"`

	// Message Event
	Event string `json:"event"`

	// Content of the message in array form so we can send
	// multiple messages in a single envelope.
	Content []any `json:"content"`

	// Rooms to which this message is intended to be
	// sent. When we get a message with rooms that
	// we're not currently tracking in memory we'll
	// add it them.
	Rooms []metadata `json:"rooms"`
}

// Instances send metadata about rooms they're
// aware of to other instances to keep each
// other in sync.
type metadata struct {
	// Room name
	Name string `json:"name"`
	// Number of connections that the instance
	// broadcasting this message maintains for
	// this room.
	Members int `json:"members"`
}

func newRedisBroadcast(ctx context.Context, nsp string, opts *RedisAdapterOptions) (*redisBroadcast, error) {
	client := redis.NewClient(&redis.Options{
		Addr:        fmt.Sprintf("%s:%d", opts.Host, opts.Port),
		DB:          opts.Database,
		Network:     opts.Network,
		MaxConnAge:  0,
		IdleTimeout: 5 * time.Minute, // default is 5 mins
		ReadTimeout: 3 * time.Second, // default is 3 seconds
		MaxRetries:  3,
	})

	// Make sure we are connected
	_, err := client.Ping().Result()
	if err != nil {
		return nil, errors.Wrap(err, "ping redis")
	}

	channel := fmt.Sprintf("%s%s", opts.Prefix, "backbone")

	id := newV4UUID()
	rbc := &redisBroadcast{
		client: client,
		logger: opts.Logger,
		unsafe: unsafe{
			instanceId: id,
			prefix:     opts.Prefix,
			channel:    fmt.Sprintf("%s%s", opts.Prefix, "backbone"),
			rooms:      make(map[string]description),
		},
	}

	// We will use a single channel as the backbone for sending messages
	// between instances of this application. Once a message is received
	// on this channel it is propagated to all clients connected to this
	// application instance.
	lErr := rbc.listen(channel)
	if lErr != nil {
		return nil, errors.Wrap(lErr, "listen")
	}

	return rbc, nil
}

// AllRooms gives list of all rooms that this instance is currently
// aware of. Instance become aware of more rooms when clients join
// and send chat messages to them.
func (bc *redisBroadcast) AllRooms() []string {
	bc.unsafe.lock.RLock()
	defer bc.unsafe.lock.RUnlock()
	var rooms []string
	for _, desc := range bc.unsafe.rooms {
		rooms = append(rooms, desc.name)
	}
	return rooms
}

// Join joins the given connection to the redisBroadcast room.
func (bc *redisBroadcast) Join(room string, conn Conn) {
	bc.unsafe.lock.Lock()
	defer bc.unsafe.lock.Unlock()

	if _, ok := bc.unsafe.rooms[room]; !ok {
		bc.unsafe.rooms[room] = description{
			name:            room,
			connections:     make(map[string]Conn),
			reportedMembers: make(map[string]int),
		}
	}

	client := conn.ID()
	bc.unsafe.rooms[room].connections[client] = conn

	rooms := []metadata{
		{
			Name:    room,
			Members: len(bc.unsafe.rooms[room].connections),
		},
	}

	// Let all instances know that a client joined this room.
	bc.publish(&client, bc.unsafe.instanceId, bc.unsafe.channel, JoinType, rooms, "")

	//pretty, err := json.MarshalIndent(bc.unsafe.rooms, "", "    ")
	//fmt.Printf("%s joining room=%s\nROOMS AFTER\n%s\n%+v\n%+v\n", conn.ID(), room, pretty, err, bc.unsafe.rooms)
}

// Leave removes the given connection from the given room
func (bc *redisBroadcast) Leave(room string, conn Conn) {
	bc.unsafe.lock.Lock()
	defer bc.unsafe.lock.Unlock()

	if _, ok := bc.unsafe.rooms[room]; ok {
		delete(bc.unsafe.rooms[room].connections, conn.ID())
	}
}

// LeaveAll leaves the given connection from all rooms.
func (bc *redisBroadcast) LeaveAll(conn Conn) {
	bc.unsafe.lock.Lock()
	defer bc.unsafe.lock.Unlock()

	for room, _ := range bc.unsafe.rooms {
		delete(bc.unsafe.rooms[room].connections, conn.ID())
	}
}

// Clear clears the room.
func (bc *redisBroadcast) Clear(room string) {
	bc.unsafe.lock.Lock()
	defer bc.unsafe.lock.Unlock()

	delete(bc.unsafe.rooms, room)

	// Everytime we publish a message we send what we know
	// about the rooms we're sending the message to.
	rooms := []metadata{
		{
			Name:    room,
			Members: 0,
		},
	}

	bc.publish(nil, bc.unsafe.instanceId, bc.unsafe.channel, ClearType, rooms, "")
}

// Send sends given event & args to all the connections in the specified room.
func (bc *redisBroadcast) Send(room, event string, args ...interface{}) {
	bc.unsafe.lock.RLock()
	defer bc.unsafe.lock.RUnlock()

	if _, ok := bc.unsafe.rooms[room]; !ok {
		bc.logger.Debugf("Tried to send to room=%s that doesnt exist", room)
		return
	}
	desc := bc.unsafe.rooms[room]
	for _, conn := range desc.connections {
		conn.Emit(event, args...)
	}

	// Everytime we publish a message we send what we know
	// about the rooms we're sending the message to.
	rooms := []metadata{
		{
			Name:    room,
			Members: len(desc.connections),
		},
	}

	bc.publish(nil, bc.unsafe.instanceId, bc.unsafe.channel, ChatType, rooms, event, args)
}

// SendAll sends given event & args to all the connections to all the rooms.
func (bc *redisBroadcast) SendAll(event string, args ...interface{}) {
	bc.unsafe.lock.RLock()
	defer bc.unsafe.lock.RUnlock()

	for _, desc := range bc.unsafe.rooms {
		for _, conn := range desc.connections {
			conn.Emit(event, args...)
		}
	}

	// Everytime we publish a message we send what we know
	// about the rooms we're sending the message to.
	var rooms []metadata
	for name, desc := range bc.unsafe.rooms {
		rooms = append(rooms, metadata{
			Name:    name,
			Members: len(desc.connections),
		})
	}

	bc.publish(nil, bc.unsafe.instanceId, bc.unsafe.channel, ChatType, rooms, event, args)
}

// ForEach sends data returned by DataFunc, if room does not exits sends nothing.
func (bc *redisBroadcast) ForEach(room string, f EachFunc) {
	bc.unsafe.lock.RLock()
	defer bc.unsafe.lock.RUnlock()
	bc.logger.Tracef("ForEach %s", room)
	desc, ok := bc.unsafe.rooms[room]
	if !ok {
		return
	}
	for _, conn := range desc.connections {
		f(conn)
	}
}

// Len gives number of connections in the room.
func (bc *redisBroadcast) Len(room string) int {
	bc.unsafe.lock.RLock()
	defer bc.unsafe.lock.RUnlock()

	if _, ok := bc.unsafe.rooms[room]; !ok {
		return 0
	}
	desc := bc.unsafe.rooms[room]
	// Add up the number of connections reported by
	// all instances including this one
	var count int
	for id, c := range desc.reportedMembers {
		bc.logger.Tracef("%s reported %d", id, c)
		count += c
	}
	bc.logger.Tracef("Total Len %d", count)
	return count
}

// Rooms returns a list of all rooms that the given connection has
// joined. If connection is nil, returns a list of all rooms.
func (bc *redisBroadcast) Rooms(conn Conn) []string {
	// Socketio will ask for all rooms in this namespace
	// by passing in a nil connection.
	if conn == nil {
		return bc.AllRooms()
	}

	bc.unsafe.lock.RLock()
	defer bc.unsafe.lock.RUnlock()

	var rooms []string
	for _, desc := range bc.unsafe.rooms {
		if _, ok := desc.connections[conn.ID()]; ok {
			rooms = append(rooms, desc.name)
		}
	}

	return rooms
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

// publish is the way we talk with other instances of this application.
func (bc *redisBroadcast) publish(client *string, id, channel string, typ messageType, rooms []metadata, event string, args ...any) {
	m := message{
		InstanceId: id,
		ClientId:   client, // only happens when a client joins
		Type:       typ,
		Event:      event,
		Content:    args,
		Rooms:      rooms,
	}

	payload, err := json.Marshal(m)
	if err != nil {
		bc.logger.WithFields(logrus.Fields{
			"error":   err,
			"message": m,
		}).Error("SendAll() Failed to encode message. Dropping")
		return
	}

	pErr := bc.client.Publish(channel, payload).Err()
	if pErr != nil {
		bc.logger.WithFields(logrus.Fields{
			"error":   pErr,
			"message": m,
		}).Error("SendAll() Failed to publish message. Dropping")
		return
	}
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
				bc.handleMessage(&m)
			}
		}
		fmt.Println("Shutting down go routine listening")
	}()

	return nil
}

// Anytime we get a message we parse as much info as we can to
// keep our own state updated.
func (bc *redisBroadcast) handleMessage(m *message) {
	bc.unsafe.lock.Lock()
	defer bc.unsafe.lock.Unlock()

	var client string
	if m.ClientId != nil {
		client = *m.ClientId
	}
	pretty, err := json.MarshalIndent(m, "", "    ")
	fmt.Printf("INCOMING %+v %s\n%s\n%+v\n", client, m.Type, pretty, err)

	// Each message contains metadata about the rooms
	// the message applies to, make use of it
	for _, meta := range m.Rooms {
		// Add the room to the list of rooms we are currently
		// aware of if necessary
		if _, ok := bc.unsafe.rooms[meta.Name]; !ok {
			bc.unsafe.rooms[meta.Name] = description{
				name:            meta.Name,
				connections:     make(map[string]Conn),
				reportedMembers: make(map[string]int),
			}
		}
		// Update the number of memebers reported by the instance that sent
		// this message
		bc.unsafe.rooms[meta.Name].reportedMembers[m.InstanceId] = meta.Members
	}

	switch m.Type {
	// If this is a chat message then emit to all rooms
	// passed into the chat message
	case ChatType:
		for _, meta := range m.Rooms {
			if _, ok := bc.unsafe.rooms[meta.Name]; !ok {
				continue
			}
			for _, conn := range bc.unsafe.rooms[meta.Name].connections {
				conn.Emit(m.Event, m.Content...)
			}
		}
	// Clear the entire room
	case ClearType:
		for _, meta := range m.Rooms {
			if _, ok := bc.unsafe.rooms[meta.Name]; !ok {
				continue
			}
			delete(bc.unsafe.rooms, meta.Name)
		}
	}
}
