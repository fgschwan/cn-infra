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

package kafka

import (
	"github.com/Shopify/sarama"
	"github.com/ligato/cn-infra/db/keyval"
	"github.com/ligato/cn-infra/flavors/local"
	"github.com/ligato/cn-infra/health/statuscheck"
	"github.com/ligato/cn-infra/logging"
	"github.com/ligato/cn-infra/messaging"
	"github.com/ligato/cn-infra/messaging/kafka/client"
	"github.com/ligato/cn-infra/messaging/kafka/mux"
	"github.com/ligato/cn-infra/utils/safeclose"
)

const topic = "status-check"

// Plugin provides API for interaction with kafka brokers.
type Plugin struct {
	Deps         // inject
	subscription chan (*client.ConsumerMessage)

	// Kafka plugin is using two multiplexers. The first one is using 'hash' (default) partitioner. The second mux
	// uses manual partitioner which allows to send a message to specified partition and watching to desired partition/offset
	muxHash   *mux.Multiplexer
	muxManual *mux.Multiplexer // todo only one mux will be kept

	consumer *client.Consumer
	hsClient sarama.Client
	manClient sarama.Client
	disabled bool
}

// Deps is here to group injected dependencies of plugin
// to not mix with other plugin fields.
type Deps struct {
	local.PluginInfraDeps //inject
}

// FromExistingMux is used mainly for testing purposes.
func FromExistingMux(mux *mux.Multiplexer) *Plugin {
	return &Plugin{muxHash: mux}
}

// Init is called at plugin initialization.
func (plugin *Plugin) Init() (err error) {
	// Prepare topic and  subscription for status check client
	plugin.subscription = make(chan *client.ConsumerMessage)

	// Get muxCfg data (contains kafka brokers ip addresses)
	muxCfg := &mux.Config{}
	found, err := plugin.PluginConfig.GetValue(muxCfg)
	if !found {
		plugin.Log.Info("kafka muxCfg not found ", plugin.PluginConfig.GetConfigName(), " - skip loading this plugin")
		plugin.disabled = true
		return nil //skip loading the plugin
	}
	if err != nil {
		return err
	}
	// retrieve clientCfg
	clientCfg := plugin.getClientConfig(muxCfg, plugin.Log, topic)

	// init 'hash' sarama client
	plugin.hsClient, err = client.NewClient(clientCfg, client.Hash)
	if err != nil {
		return err
	}

	// init 'manual' sarama client
	plugin.manClient, err = client.NewClient(clientCfg, client.Manual)
	if err != nil {
		return err
	}

	// init bsm/sarama consumer
	plugin.consumer, err = client.NewConsumer(clientCfg, nil)
	if err != nil {
		return err
	}

	// Initialize both multiplexers to allow both, dynamic and manual mode
	if plugin.muxHash == nil {
		name := plugin.ServiceLabel.GetAgentLabel() + "-hash"
		plugin.muxHash, err = mux.InitMultiplexerWithConfig(clientCfg, plugin.hsClient, plugin.manClient, name, plugin.Log)
		if err != nil {
			return err
		}
		plugin.Log.Debug("Default multiplexer initialized")
	}
	if plugin.muxManual == nil {
		name := plugin.ServiceLabel.GetAgentLabel() + "-manual"
		plugin.muxManual, err = mux.InitMultiplexerWithConfig(clientCfg, plugin.hsClient, plugin.manClient, name, plugin.Log)
		if err != nil {
			return err
		}
		plugin.Log.Debug("Manual multiplexer initialized")
	}

	return err
}

// AfterInit is called in the second phase of the initialization. The kafka multiplexerNewWatcher
// is started, all consumers have to be subscribed until this phase.
func (plugin *Plugin) AfterInit() error {
	if plugin.muxHash != nil {
		err := plugin.muxHash.Start()
		if err != nil {
			return  err
		}
	}
	if plugin.muxManual != nil {
		err := plugin.muxManual.Start()
		if err != nil {
			return err
		}
	}

	// Register for providing status reports (polling mode)
	if plugin.StatusCheck != nil {
		plugin.StatusCheck.Register(plugin.PluginName, func() (statuscheck.PluginState, error) {
			// Method 'RefreshMetadata()' returns error if kafka server is unavailable
			err := plugin.consumer.Client.RefreshMetadata(topic)
			if err == nil {
				return statuscheck.OK, nil
			}
			plugin.Log.Errorf("Kafka server unavailable")
			return statuscheck.Error, err
		})
	} else {
		plugin.Log.Warnf("Unable to start status check for kafka")
	}

	return wasError
}

// Close is called at plugin cleanup phase.
func (plugin *Plugin) Close() error {
	_, err := safeclose.CloseAll(plugin.consumer, plugin.hsClient, plugin.muxHash, plugin.muxManual)
	return err
}

// NewBytesConnection returns a new instance of a connection to access kafka brokers. The connection allows to create
// new kafka providers/consumers on multiplexer with hash partitioner.
func (plugin *Plugin) NewBytesConnection(name string) *mux.BytesConnection {
	return plugin.muxHash.NewBytesConnection(name)
}

// NewBytesConnectionToPartition returns a new instance of a connection to access kafka brokers. The connection allows to create
// new kafka providers/consumers on multiplexer with manual partitioner which allows to send messages to specific partition
// in kafka cluster and watch on partition/offset.
func (plugin *Plugin) NewBytesConnectionToPartition(name string) *mux.BytesConnection {
	return plugin.muxHash.NewBytesConnection(name)
}

// NewProtoConnection returns a new instance of a connection to access kafka brokers. The connection allows to create
// new kafka providers/consumers on multiplexer with hash partitioner.The connection uses proto-modelled messages.
func (plugin *Plugin) NewProtoConnection(name string) mux.Connection {
	return plugin.muxHash.NewProtoConnection(name, &keyval.SerializerJSON{})
}

// NewProtoManualConnection returns a new instance of a connection to access kafka brokers. The connection allows to create
// new kafka providers/consumers on multiplexer with manual partitioner which allows to send messages to specific partition
// in kafka cluster and watch on partition/offset. The connection uses proto-modelled messages.
func (plugin *Plugin) NewProtoManualConnection(name string) mux.ManualConnection {
	return plugin.muxHash.NewProtoManualConnection(name, &keyval.SerializerJSON{})
}

// NewSyncPublisher creates a publisher that allows to publish messages using synchronous API. The publisher creates
// new proto connection on multiplexer with default partitioner.
func (plugin *Plugin) NewSyncPublisher(connectionName string, topic string) (messaging.ProtoPublisher, error) {
	return plugin.NewProtoConnection(connectionName).NewSyncPublisher(topic)
}

// NewSyncPublisherToPartition creates a publisher that allows to publish messages to custom partition using synchronous API.
// The publisher creates new proto connection on multiplexer with manual partitioner.
func (plugin *Plugin) NewSyncPublisherToPartition(connectionName string, topic string, partition int32) (messaging.ProtoPublisher, error) {
	return plugin.NewProtoManualConnection(connectionName).NewSyncPublisherToPartition(topic, partition)
}

// NewAsyncPublisher creates a publisher that allows to publish messages using asynchronous API. The publisher creates
// new proto connection on multiplexer with default partitioner.
func (plugin *Plugin) NewAsyncPublisher(connectionName string, topic string, successClb func(messaging.ProtoMessage), errorClb func(messaging.ProtoMessageErr)) (messaging.ProtoPublisher, error) {
	return plugin.NewProtoConnection(connectionName).NewAsyncPublisher(topic, successClb, errorClb)
}

// NewAsyncPublisherToPartition creates a publisher that allows to publish messages to custom partition using asynchronous API.
// The publisher creates new proto connection on multiplexer with manual partitioner.
func (plugin *Plugin) NewAsyncPublisherToPartition(connectionName string, topic string, partition int32, successClb func(messaging.ProtoMessage), errorClb func(messaging.ProtoMessageErr)) (messaging.ProtoPublisher, error) {
	return plugin.NewProtoManualConnection(connectionName).NewAsyncPublisherToPartition(topic, partition, successClb, errorClb)
}

// NewWatcher creates a watcher that allows to start/stop consuming of messaging published to given topics.
func (plugin *Plugin) NewWatcher(name string) messaging.ProtoWatcher {
	return plugin.NewProtoConnection(name)
}

// NewPartitionWatcher creates a watcher that allows to start/stop consuming of messaging published to given topics, offset and partition
func (plugin *Plugin) NewPartitionWatcher(name string) messaging.ProtoPartitionWatcher {
	return plugin.NewProtoManualConnection(name)
}

// Disabled if the plugin config was not found
func (plugin *Plugin) Disabled() (disabled bool) {
	return plugin.disabled
}

// Receive client config according to kafka config data
func (plugin *Plugin) getClientConfig(config *mux.Config, logger logging.Logger, topic string) *client.Config {
	clientCfg := client.NewConfig(logger)
	// Set brokers obtained from kafka config. In case there are none available, use a default one
	if len(config.Addrs) > 0 {
		clientCfg.SetBrokers(config.Addrs...)
	} else {
		clientCfg.SetBrokers(mux.DefAddress)
	}
	clientCfg.SetGroup(plugin.ServiceLabel.GetAgentLabel())
	clientCfg.SetRecvMessageChan(plugin.subscription)
	clientCfg.SetInitialOffset(sarama.OffsetNewest)
	clientCfg.SetTopics(topic)
	return clientCfg
}
