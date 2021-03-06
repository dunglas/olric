// Copyright 2018-2020 Burak Sezer
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

/*Package client implements a Golang client to access an Olric cluster from outside. */
package client // import "github.com/buraksezer/olric/client"

import (
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/buraksezer/olric"
	"github.com/buraksezer/olric/internal/bufpool"
	"github.com/buraksezer/olric/internal/protocol"
	"github.com/buraksezer/olric/internal/transport"
	"github.com/buraksezer/olric/serializer"
	"github.com/buraksezer/olric/stats"
	"github.com/pkg/errors"
	"github.com/vmihailenco/msgpack"
)

var (
	logger     = log.New(os.Stderr, "olric: ", log.Lshortfile)
	bufferPool = bufpool.New()
)

// Client implements Go client of Olric Binary Protocol and its methods.
type Client struct {
	config     *Config
	client     *transport.Client
	serializer serializer.Serializer
	streams    *streams
	wg         sync.WaitGroup
}

// Config includes configuration parameters for the Client.
type Config struct {
	Addrs                 []string
	Serializer            serializer.Serializer
	DialTimeout           time.Duration
	KeepAlive             time.Duration
	MaxConn               int
	MaxListenersPerStream int
}

// New returns a new Client instance. The second parameter is serializer, it can be nil.
func New(c *Config) (*Client, error) {
	if c == nil {
		return nil, fmt.Errorf("config cannot be nil")
	}
	if len(c.Addrs) == 0 {
		return nil, fmt.Errorf("addrs list cannot be empty")
	}
	if c.Serializer == nil {
		c.Serializer = serializer.NewGobSerializer()
	}
	if c.MaxConn == 0 {
		c.MaxConn = 1
	}
	if c.MaxListenersPerStream <= 0 {
		c.MaxListenersPerStream = maxListenersPerStream
	}
	cc := &transport.ClientConfig{
		Addrs:       c.Addrs,
		DialTimeout: c.DialTimeout,
		KeepAlive:   c.KeepAlive,
		MaxConn:     c.MaxConn,
	}
	client := transport.NewClient(cc)
	// About the hack: This looks weird, but I need to mock client.CreateStream function to test streams
	// independently. I don't want to use a mocking library for this. So I created a function named
	// createStreamFunction and I overwrite that function in test.
	createStreamFunction = client.CreateStream
	return &Client{
		config:     c,
		client:     client,
		serializer: c.Serializer,
		streams:    &streams{m: make(map[uint64]*stream)},
	}, nil
}

// Ping sends a dummy protocol messsage to the given host. This is useful to
// measure RTT between hosts. It also can be used as aliveness check.
func (c *Client) Ping(addr string) error {
	req := protocol.NewSystemMessage(protocol.OpPing)
	_, err := c.client.RequestTo(addr, req)
	return err
}

// Stats exposes some useful metrics to monitor an Olric node.
func (c *Client) Stats(addr string) (stats.Stats, error) {
	s := stats.Stats{}
	req := protocol.NewSystemMessage(protocol.OpStats)
	resp, err := c.client.RequestTo(addr, req)
	if err != nil {
		return s, err
	}
	err = checkStatusCode(resp)
	if err != nil {
		return s, err
	}

	err = msgpack.Unmarshal(resp.Value(), &s)
	if err != nil {
		return s, err
	}
	return s, nil
}

// Close cancels underlying context and cancels ongoing requests.
func (c *Client) Close() {
	c.streams.mu.RLock()
	defer c.streams.mu.RUnlock()

	for _, s := range c.streams.m {
		s.cancel()
	}
	c.client.Close()
	c.wg.Wait()
}

// NewDMap creates and returns a new dmap instance to access DMaps on the cluster.
func (c *Client) NewDMap(name string) *DMap {
	return &DMap{
		Client: c,
		name:   name,
	}
}

func checkStatusCode(resp protocol.EncodeDecoder) error {
	status := resp.Status()
	switch {
	case status == protocol.StatusOK:
		return nil
	case status == protocol.StatusInternalServerError:
		return errors.Wrap(olric.ErrInternalServerError, string(resp.Value()))
	case status == protocol.StatusErrNoSuchLock:
		return olric.ErrNoSuchLock
	case status == protocol.StatusErrLockNotAcquired:
		return olric.ErrLockNotAcquired
	case status == protocol.StatusErrKeyNotFound:
		return olric.ErrKeyNotFound
	case status == protocol.StatusErrWriteQuorum:
		return olric.ErrWriteQuorum
	case status == protocol.StatusErrReadQuorum:
		return olric.ErrReadQuorum
	case status == protocol.StatusErrOperationTimeout:
		return olric.ErrOperationTimeout
	case status == protocol.StatusErrKeyFound:
		return olric.ErrKeyFound
	case status == protocol.StatusErrClusterQuorum:
		return olric.ErrClusterQuorum
	case status == protocol.StatusErrEndOfQuery:
		return olric.ErrEndOfQuery
	case status == protocol.StatusErrUnknownOperation:
		return olric.ErrUnknownOperation
	case status == protocol.StatusErrInvalidArgument:
		return olric.ErrInvalidArgument
	case status == protocol.StatusErrServerGone:
		return olric.ErrServerGone
	case status == protocol.StatusErrKeyTooLarge:
		return olric.ErrKeyTooLarge
	case status == protocol.StatusErrNotImplemented:
		return olric.ErrNotImplemented
	default:
		return fmt.Errorf("unknown status: %v", resp.Status())
	}
}

func (c *Client) unmarshalValue(rawval interface{}) (interface{}, error) {
	var value interface{}
	err := c.serializer.Unmarshal(rawval.([]byte), &value)
	if err != nil {
		return nil, err
	}
	if _, ok := value.(struct{}); ok {
		return nil, nil
	}
	return value, nil
}
