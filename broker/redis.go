package broker

import (
	"errors"
	"flag"
	"log"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/garyburd/redigo/redis"
	"github.com/heroku/busl/util"
)

const (
	msgBuf = 10
)

var (
	redisUrl           = flag.String("redisUrl", os.Getenv("REDIS_URL"), "URL of the redis server")
	redisServer        *url.URL
	redisPool          *redis.Pool
	redisKeyExpire     = 60 // redis uses seconds for EXPIRE
	redisChannelExpire = redisKeyExpire * 5
)

func init() {
	flag.Parse()
	redisServer, _ = url.Parse(*redisUrl)
	redisPool = newPool(redisServer)
}

func newPool(server *url.URL) *redis.Pool {
	cleanServerURL := *server
	cleanServerURL.User = nil
	log.Printf("connecting to redis: %s", cleanServerURL)
	return &redis.Pool{
		MaxIdle:     3,
		IdleTimeout: 4 * time.Minute,
		Dial: func() (c redis.Conn, err error) {
			c, err = redis.Dial("tcp", server.Host)
			if err != nil {
				return
			}

			if server.User == nil {
				return
			}

			pw, pwset := server.User.Password()
			if !pwset {
				return
			}

			if _, err = c.Do("AUTH", pw); err != nil {
				c.Close()
				return
			}
			return
		},
		TestOnBorrow: func(c redis.Conn, t time.Time) error {
			_, err := c.Do("PING")
			return err
		},
	}
}

type channel string

func (c channel) id() string {
	return string(c)
}

func (c channel) wildcardId() string {
	return string(c) + "*"
}

func (c channel) uuid() util.UUID {
	return util.UUID(c)
}

func (c channel) doneId() string {
	return string(c) + "done"
}

func (c channel) killId() string {
	return string(c) + "kill"
}

type RedisBroker struct {
	channel     channel
	subscribers map[chan []byte]bool
	psc         redis.PubSubConn
	position    int64
	mutex       *sync.Mutex
}

func NewRedisBroker(uuid util.UUID) *RedisBroker {
	broker := &RedisBroker{
		channel(uuid),
		make(map[chan []byte]bool),
		redis.PubSubConn{},
		0,
		&sync.Mutex{},
	}

	return broker
}

func (b *RedisBroker) Subscribe() (ch chan []byte, err error) {
	if !NewRedisRegistrar().IsRegistered(b.channel.uuid()) {
		return nil, errors.New("Channel is not registered.")
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()
	ch = make(chan []byte, msgBuf)
	b.subscribers[ch] = true
	go b.redisSubscribe(ch)
	return
}

func (b *RedisBroker) redisSubscribe(ch chan []byte) {
	conn := redisPool.Get()
	defer conn.Close()

	if err := b.replay(ch); err != nil {
		b.Unsubscribe(ch)
		return
	}

	b.psc = redis.PubSubConn{conn}
	b.psc.PSubscribe(b.channel.wildcardId())

	for {
		switch msg := b.psc.Receive().(type) {
		case redis.PMessage:
			switch msg.Channel {
			case b.channel.killId():
				util.Count("RedisBroker.redisSubscribe.Channel.kill")
				b.psc.PUnsubscribe(b.channel.wildcardId())
			case b.channel.id():
				if b.subscribers[ch] {
					data := b.getRange(msg.Data)
					ch <- data
				} else {
					return
				}
			}
		case redis.Subscription:
			if msg.Kind == "punsubscribe" || msg.Kind == "unsubscribe" {
				subscSlice := b.getRange([]byte("-1"))
				ch <- subscSlice

				util.Count("RedisBroker.redisSubscribe.Channel.unsubscribe")
				b.Unsubscribe(ch)
				return
			}
		case error:
			util.CountWithData("RedisBroker.redisSubscribe.RecieveError", 1, "err=%s", msg)
			return
		}
	}
}

func (b *RedisBroker) Unsubscribe(ch chan []byte) {
	if b.subscribers[ch] {
		close(ch)
	}

	b.mutex.Lock()
	defer b.mutex.Unlock()
	delete(b.subscribers, ch)
}

func (b *RedisBroker) UnsubscribeAll() {
	util.CountMany("RedisBroker.UnsubscribeAll", int64(len(b.subscribers)))

	conn := redisPool.Get()
	defer conn.Close()

	_, err := conn.Do("PUBLISH", b.channel.killId(), []byte{1})
	if err != nil {
		util.CountWithData("RedisBroker.publishOn.error", 1, "error=%s", err)
	}

	conn.Do("SETEX", b.channel.doneId(), redisChannelExpire, []byte{1})
}

func (b *RedisBroker) Publish(msg []byte) {
	b.publishOn(msg)
}

func (b *RedisBroker) publishOn(msg []byte) {
	conn := redisPool.Get()
	defer conn.Close()

	conn.Send("MULTI")
	conn.Send("APPEND", b.channel.id(), msg)
	conn.Send("EXPIRE", b.channel.id(), redisKeyExpire)
	conn.Send("DEL", b.channel.doneId())

	appendedVals, err := redis.Values(conn.Do("EXEC"))
	if err != nil {
		util.CountWithData("RedisBroker.publishOn.error", 1, "error=%s", err)
	}
	appendedLen := appendedVals[0].(int64)

	conn.Send("PUBLISH", b.channel.id(), appendedLen)
}

func (b *RedisBroker) replay(ch chan []byte) (err error) {
	conn := redisPool.Get()
	defer conn.Close()

	if _, channelExists := b.subscribers[ch]; !channelExists {
		return errors.New("Channel already closed.")
	}

	result, err := redis.Values(conn.Do("MGET", b.channel.id(), b.channel.doneId()))
	if err != nil {
		util.CountWithData("RedisBroker.publishOn.error", 1, "error=%s", err)
		return
	}

	buffer, channelDone := getRedisByteArray(result[0]), getRedisByteArray(result[1])

	if buffer != nil {
		b.position = int64(len(buffer))
		ch <- buffer
	}

	if channelDone != nil && channelDone[0] == 1 {
		util.Count("RedisBroker.replay.channelDone")
		return errors.New("Channel is done.")
	}

	return
}

func (b *RedisBroker) getRange(newRange []byte) []byte {
	conn := redisPool.Get()
	defer conn.Close()
	subscSlice, err := redis.Bytes(conn.Do("GETRANGE", b.channel.id(), b.position, newRange))
	if err != nil {
		log.Println(err)
		return []byte{}
	} else {
		// TODO: Add mutex
		b.position = b.position + int64(len(subscSlice))
		return subscSlice
	}
}

func getRedisByteArray(v interface{}) []byte {
	if v != nil {
		return v.([]byte)
	}
	return nil
}

type RedisRegistrar struct{}

func NewRedisRegistrar() *RedisRegistrar {
	registrar := &RedisRegistrar{}

	return registrar
}

func (rr *RedisRegistrar) Register(channel util.UUID) (err error) {
	conn := redisPool.Get()
	defer conn.Close()

	_, err = conn.Do("SETEX", channel, redisChannelExpire, make([]byte, 0))
	if err != nil {
		util.CountWithData("RedisRegistrar.Register.error", 1, "error=%s", err)
		return
	}
	return
}

func (rr *RedisRegistrar) IsRegistered(channel util.UUID) (registered bool) {
	conn := redisPool.Get()
	defer conn.Close()

	result, err := redis.Int64(conn.Do("EXISTS", channel))
	if err != nil {
		util.CountWithData("RedisRegistrar.IsRegistered.error", 1, "error=%s", err)
		return false
	}

	return result == 1
}
