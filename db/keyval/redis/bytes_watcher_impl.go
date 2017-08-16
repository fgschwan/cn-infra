// Copyright (c) 2017 Cisco and/or its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at:
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package redis

import (
	"strings"

	"fmt"

	goredis "github.com/go-redis/redis"
	"github.com/ligato/cn-infra/db"
	"github.com/ligato/cn-infra/db/keyval"
	"github.com/ligato/cn-infra/utils/safeclose"
)

const keySpaceEventPrefix = "__keyspace@*__:"

// BytesWatchPutResp is sent when new key-value pair has been inserted or the value is updated
type BytesWatchPutResp struct {
	key   string
	value []byte
	rev   int64 // TODO Does Redis data have revision?
}

// NewBytesWatchPutResp creates an instance of BytesWatchPutResp
func NewBytesWatchPutResp(key string, value []byte, revision int64) *BytesWatchPutResp {
	return &BytesWatchPutResp{key: key, value: value, rev: revision}
}

// GetChangeType returns "Put" for BytesWatchPutResp
func (resp *BytesWatchPutResp) GetChangeType() db.PutDel {
	return db.Put
}

// GetKey returns the key that has been inserted
func (resp *BytesWatchPutResp) GetKey() string {
	return resp.key
}

// GetValue returns the value that has been inserted
func (resp *BytesWatchPutResp) GetValue() []byte {
	return resp.value
}

// GetRevision returns the revision associated with create action
func (resp *BytesWatchPutResp) GetRevision() int64 {
	return resp.rev
}

// BytesWatchDelResp is sent when a key-value pair has been removed
type BytesWatchDelResp struct {
	key string
	rev int64 // TODO Does Redis data have revision?
}

// NewBytesWatchDelResp creates an instance of BytesWatchDelResp
func NewBytesWatchDelResp(key string, revision int64) *BytesWatchDelResp {
	return &BytesWatchDelResp{key: key, rev: revision}
}

// GetChangeType returns "Delete" for BytesWatchPutResp
func (resp *BytesWatchDelResp) GetChangeType() db.PutDel {
	return db.Delete
}

// GetKey returns the key that has been deleted
func (resp *BytesWatchDelResp) GetKey() string {
	return resp.key
}

// GetValue returns nil for BytesWatchDelResp
func (resp *BytesWatchDelResp) GetValue() []byte {
	return nil
}

// GetRevision returns the revision associated with the delete operation
func (resp *BytesWatchDelResp) GetRevision() int64 {
	return resp.rev
}

// Watch starts subscription for changes associated with the selected key. Watch events will be delivered to respChan.
// Subscription can be canceled by StopWatch call.
func (db *BytesConnectionRedis) Watch(respChan chan keyval.BytesWatchResp, keys ...string) error {
	if db.closed {
		return fmt.Errorf("Watch(%v) called on a closed connection", keys)
	}
	return watch(db, respChan, db.closeCh, nil, nil, keys...)
}

func watch(db *BytesConnectionRedis, respChan chan<- keyval.BytesWatchResp, closeChan <-chan struct{},
	addPrefix func(key string) string, trimPrefix func(key string) string, keys ...string) error {
	patterns := make([]string, len(keys))
	for i, k := range keys {
		if addPrefix != nil {
			k = addPrefix(k)
		}
		patterns[i] = keySpaceEventPrefix + wildcard(k)
	}
	pubSub := db.client.PSubscribe(patterns...)
	startWatch(db, pubSub, respChan, trimPrefix, patterns...)
	go func() {
		_, active := <-closeChan
		if !active {
			db.Debugf("Received signal to close Watch(%v)", patterns)
			if !db.closed {
				err := pubSub.PUnsubscribe(patterns...)
				if err != nil {
					db.Errorf("PUnsubscribe %v failed: %s", patterns, err)
				}
				safeclose.Close(pubSub)
			}
		}
	}()
	return nil
}

func startWatch(db *BytesConnectionRedis, pubSub *goredis.PubSub,
	respChan chan<- keyval.BytesWatchResp, trimPrefix func(key string) string, patterns ...string) {
	go func() {
		defer func() { db.Debugf("Watch(%v) exited", patterns) }()
		db.Debugf("start Watch(%v)", patterns)
		for {
			msg, err := pubSub.ReceiveMessage()
			if db.closed {
				return
			}
			if err != nil {
				db.Errorf("Watch(%v) encountered error: %s", patterns, err)
				continue
			}
			if msg == nil {
				// channel closed?
				db.Debugf("%T.ReceiveMessage() returned nil", pubSub)
				continue
			}
			db.Debugf("Receive %T: %s %s %s", msg, msg.Pattern, msg.Channel, msg.Payload)
			key := msg.Channel[strings.Index(msg.Channel, ":")+1:]
			db.Debugf("key = %s", key)
			switch msg.Payload {
			case "set":
				// keyspace event does not carry value.  Need to retrieve it.
				val, _, rev, err := db.GetValue(key)
				if err != nil {
					db.Errorf("GetValue(%s) failed with error %s", key, err)
				}
				if val == nil {
					db.Debugf("GetValue(%s) returned nil", key)
				}
				if trimPrefix != nil {
					key = trimPrefix(key)
				}
				respChan <- NewBytesWatchPutResp(key, val, rev)
			case "del", "expired":
				if trimPrefix != nil {
					key = trimPrefix(key)
				}
				respChan <- NewBytesWatchDelResp(key, 0)
			default:
				db.Debugf("%T: %s %s %s -- not handled", msg, msg.Pattern, msg.Channel, msg.Payload)
			}
		}
	}()
}

// Watch starts subscription for changes associated with the selected key. Watch events will be delivered to respChan.
func (pdb *BytesBrokerWatcherRedis) Watch(respChan chan keyval.BytesWatchResp, keys ...string) error {
	if pdb.delegate.closed {
		return fmt.Errorf("Watch(%v) called on a closed connection", keys)
	}
	return watch(pdb.delegate, respChan, pdb.closeCh, pdb.addPrefix, pdb.trimPrefix, keys...)
}
