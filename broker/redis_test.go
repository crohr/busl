package broker_test

import (
	"testing"

	. "github.com/heroku/busl/broker"
	u "github.com/heroku/busl/util"
	. "gopkg.in/check.v1"
)

func Test(t *testing.T) { TestingT(t) }

type RegistrarSuite struct {
	registrar Registrar
	uuid      u.UUID
	broker    Broker
}

type BrokerSuite struct {
	registrar Registrar
	uuid      u.UUID
	broker    Broker
}

var _ = Suite(&RegistrarSuite{})
var _ = Suite(&BrokerSuite{})

/*
 * Registrar Suite
 */

func (s *RegistrarSuite) SetUpTest(c *C) {
	s.registrar = NewRedisRegistrar()
	s.uuid, _ = u.NewUUID()
	s.broker = NewRedisBroker(s.uuid)
}

func (s *RegistrarSuite) TestRegisteredIsRegistered(c *C) {
	s.registrar.Register(s.uuid)
	c.Assert(s.registrar.IsRegistered(s.uuid), u.IsTrue)
}

func (s *RegistrarSuite) TestUnregisteredIsNotRegistered(c *C) {
	c.Assert(s.registrar.IsRegistered(s.uuid), u.IsFalse)
}

func (s *RegistrarSuite) TestUnregisteredRedisSubscribe(c *C) {
	_, err := s.broker.Subscribe()
	c.Assert(err.Error(), Equals, "Channel is not registered.")
}

func (s *RegistrarSuite) TestRegisteredRedisSubscribe(c *C) {
	s.registrar.Register(s.uuid)
	ch, err := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)
	c.Assert(err, IsNil)
}

/*
 * Broker Suite
 */

func (s *BrokerSuite) SetUpTest(c *C) {
	s.registrar = NewRedisRegistrar()
	s.uuid, _ = u.NewUUID()
	s.registrar.Register(s.uuid)
	s.broker = NewRedisBroker(s.uuid)
}

func (s *BrokerSuite) TestRedisSubscribe(c *C) {
	ch, _ := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)
	s.broker.Publish([]byte("busl"))
	c.Assert(string(<-ch), Equals, "busl")
}

func (s *BrokerSuite) TestRedisSubscribeReplay(c *C) {
	s.broker.Publish([]byte("busl"))
	ch, _ := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)
	c.Assert(string(<-ch), Equals, "busl")
}

func (s *BrokerSuite) TestRedisSubscribeChannelDone(c *C) {
	redisBroker := NewRedisBroker(s.uuid)
	redisBroker.Publish([]byte("busl"))
	redisBroker.UnsubscribeAll()

	ch, _ := s.broker.Subscribe()
	defer s.broker.Unsubscribe(ch)
	c.Assert(string(<-ch), Equals, "busl")
}
