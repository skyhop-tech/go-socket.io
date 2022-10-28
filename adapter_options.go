package socketio

// RedisAdapterOptions is configuration to create new adapter
type RedisAdapterOptions struct {
	Host     string
	Port     int
	Database int
	Prefix   string
	Network  string
	Password string
}
