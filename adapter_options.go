package socketio

import (
	"fmt"

	"github.com/gomodule/redigo/redis"
)

// RedisAdapterOptions is configuration to create new adapter
type RedisAdapterOptions struct {
	// deprecated. Usage Addr options
	Host string
	// deprecated. Usage Addr options
	Port     string
	Addr     string
	Database int
	Prefix   string
	Network  string
	Password string
}

func (ro *RedisAdapterOptions) getAddr() string {
	if ro.Addr == "" {
		ro.Addr = fmt.Sprintf("%s:%s", ro.Host, ro.Port)
	}
	return ro.Addr
}

func defaultOptions() *RedisAdapterOptions {
	return &RedisAdapterOptions{
		Addr:    "127.0.0.1:6379",
		Prefix:  "socket.io",
		Network: "tcp",
	}
}

func (ro *RedisAdapterOptions) getDialOptions() []redis.DialOption {
	var redisOpts []redis.DialOption
	if len(ro.Password) > 0 {
		redisOpts = append(redisOpts, redis.DialPassword(ro.Password))
	}
	if ro.Database != 0 {
		redisOpts = append(redisOpts, redis.DialDatabase(ro.Database))
	}

	return redisOpts
}

func getOptions(opts *RedisAdapterOptions) *RedisAdapterOptions {
	options := defaultOptions()

	if opts != nil {
		if opts.Host != "" {
			options.Host = opts.Host
		}

		if opts.Port != "" {
			options.Port = opts.Port
		}

		if opts.Database != 0 {
			options.Database = opts.Database
		}

		if opts.Addr != "" {
			options.Addr = opts.Addr
		}

		if opts.Prefix != "" {
			options.Prefix = opts.Prefix
		}

		if opts.Network != "" {
			options.Network = opts.Network
		}

		if len(opts.Password) > 0 {
			options.Password = opts.Password
		}
	}

	return options
}
