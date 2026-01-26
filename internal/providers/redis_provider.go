package providers

import "github.com/go-redis/redis/v8"

func NewRedisProvider(addr string) *redis.Client {
	return redis.NewClient(&redis.Options{Addr: addr})
}
