// Copyright 2015-2016 trivago GmbH
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

package consumer

import (
	"encoding/json"
	"fmt"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/credentials"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/kinesis"
	"github.com/trivago/gollum/core"
	"github.com/trivago/gollum/core/log"
	"github.com/trivago/gollum/shared"
	"io/ioutil"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	kinesisCredentialEnv    = "environment"
	kinesisCredentialStatic = "static"
	kinesisCredentialShared = "shared"
	kinesisCredentialNone   = "none"
	kinesisOffsetNewest     = "newest"
	kinesisOffsetOldest     = "oldest"
)

// Kinesis consumer plugin
// Configuration example
//
//   - "consumer.Kinesis":
//     Enable: true
//     KinesisStream: "default"
//     DefaultOffset: "Newest"
//     OffsetFile: ""
//     RecordsPerQuery: 100
//     NoRecordsSleepMs: 1000
//     CredentialType: "environment"
//     CredentialId: ""
//     CredentialToken: ""
//     CredentialSecret: ""
//     CredentialFile: ""
//     CredentialProfile: ""
//     Stream:
//       - "kinesis"
//
// DefaultOffset defines the message index to start reading from.
// Valid values are either "Newset", "Oldest", or a number.
// The default value is "Newest".
//
// KinesisStream defines the stream to read from.
// By default this is set to "default"
//
// CredentialType defines the credentials that are to be used when
// connectiong to kensis. This can be one of the following: environment,
// static, shared, none.
// Static enables the parameters CredentialId, CredentialToken and
// CredentialSecretm shared enables the parameters CredentialFile and
// CredentialProfile. None will not use any credentials and environment
// will pull the credentials from environmental settings.
// By default this is set to environmental.
//
// NoRecordsSleepMs defines the number of milliseconds to sleep before
// trying to pull new records from a shard that did not return any records.
// By default this is set to 1000.
//
// RetrySleepSec defines the number of seconds to wait after trying to
// reconnect to a shard. By default this is set to 4.
//
// RecordsPerQuery defines the number of records to pull per query.
// By default this is set to 100.
type Kinesis struct {
	core.ConsumerBase
	client          *kinesis.Kinesis
	config          *aws.Config
	offsets         map[string]int64
	stream          string
	offsetType      string
	offsetFile      string
	defaultOffset   int64
	recordsPerQuery int64
	sleepTime       time.Duration
	retryTime       time.Duration
}

func init() {
	shared.TypeRegistry.Register(Kinesis{})
}

// Configure initializes this consumer with values from a plugin config.
func (cons *Kinesis) Configure(conf core.PluginConfig) error {
	err := cons.ConsumerBase.Configure(conf)
	if err != nil {
		return err
	}

	cons.stream = conf.GetString("KinesisStream", "default")
	cons.offsetFile = conf.GetString("OffsetFile", "")
	cons.recordsPerQuery = int64(conf.GetInt("RecordsPerQuery", 1000))
	cons.sleepTime = time.Duration(conf.GetInt("NoRecordsSleepMs", 1000)) * time.Millisecond
	cons.retryTime = time.Duration(conf.GetInt("RetrySleepSec", 4)) * time.Second

	// Config
	cons.config = aws.NewConfig()
	if endpoint := conf.GetString("Endpoint", ""); endpoint != "" {
		cons.config.WithEndpoint(endpoint)
	}

	if region := conf.GetString("Region", ""); region != "" {
		cons.config.WithRegion(region)
	}

	// Credentials
	credentialType := strings.ToLower(conf.GetString("CredentialType", kinesisCredentialEnv))
	switch credentialType {
	case kinesisCredentialEnv:
		cons.config.WithCredentials(credentials.NewEnvCredentials())

	case kinesisCredentialStatic:
		id := conf.GetString("CredentialId", "")
		token := conf.GetString("CredentialToken", "")
		secret := conf.GetString("CredentialSecret", "")
		cons.config.WithCredentials(credentials.NewStaticCredentials(id, secret, token))

	case kinesisCredentialShared:
		filename := conf.GetString("CredentialFile", "")
		profile := conf.GetString("CredentialProfile", "")
		cons.config.WithCredentials(credentials.NewSharedCredentials(filename, profile))

	case kinesisCredentialNone:
		// Nothing

	default:
		return fmt.Errorf("Unknwon CredentialType: %s", credentialType)
	}

	// Offset
	offsetValue := strings.ToLower(conf.GetString("DefaultOffset", kinesisOffsetNewest))
	switch offsetValue {
	case kinesisOffsetNewest:
		cons.offsetType = kinesis.ShardIteratorTypeLatest
		cons.defaultOffset = 0

	case kinesisOffsetOldest:
		cons.offsetType = kinesis.ShardIteratorTypeAtSequenceNumber
		cons.defaultOffset = 0

	default:
		cons.offsetType = kinesis.ShardIteratorTypeAtSequenceNumber
		cons.defaultOffset, _ = strconv.ParseInt(offsetValue, 10, 64)
	}

	if cons.offsetFile != "" {
		fileContents, err := ioutil.ReadFile(cons.offsetFile)
		if err != nil {
			return err
		}
		cons.offsetType = kinesis.ShardIteratorTypeAfterSequenceNumber
		if err := json.Unmarshal(fileContents, &cons.offsets); err != nil {
			return err
		}
	}

	return nil
}

func (cons *Kinesis) processShard(iteratorConfig *kinesis.GetShardIteratorInput) {
	iterator, err := cons.client.GetShardIterator(iteratorConfig)
	if err != nil || iterator.ShardIterator == nil {
		Log.Error.Printf("Failed to iterate shard %s/%s", *iteratorConfig.StreamName, *iteratorConfig.ShardId)
		time.AfterFunc(cons.retryTime, func() { cons.processShard(iteratorConfig) })
		return // ### return, retry ###
	}

	recordConfig := &kinesis.GetRecordsInput{
		ShardIterator: iterator.ShardIterator,
		Limit:         aws.Int64(cons.recordsPerQuery),
	}

	cons.AddWorker()
	defer cons.WorkerDone()

	for {
		result, err := cons.client.GetRecords(recordConfig)
		if err != nil || iterator.ShardIterator == nil {
			Log.Error.Printf("Failed to get records from shard %s/%s", *iteratorConfig.StreamName, *iteratorConfig.ShardId)
			time.AfterFunc(cons.retryTime, func() { cons.processShard(iteratorConfig) })
			return // ### return, retry ###
		}

		if result.NextShardIterator == nil {
			Log.Warning.Printf("Shard %s/%s has been closed", *iteratorConfig.StreamName, *iteratorConfig.ShardId)
			return // ### return, closed ###
		}

		for _, record := range result.Records {
			if record == nil {
				Log.Debug.Printf("Empty record detected")
				continue // ### continue ###
			}

			seq, _ := strconv.ParseInt(*(*record).SequenceNumber, 10, 64)
			cons.Enqueue((*record).Data, uint64(seq))
			cons.offsets[*iteratorConfig.ShardId] = seq
		}

		recordConfig.ShardIterator = result.NextShardIterator
		if len(result.Records) == 0 {
			time.Sleep(cons.sleepTime)
		}
	}
}

func (cons *Kinesis) connect() error {
	defer cons.WorkerDone() // main worker done
	cons.client = kinesis.New(session.New(cons.config))

	// Get shard ids for stream
	streamQuery := &kinesis.DescribeStreamInput{
		StreamName: aws.String(cons.stream),
	}

	streamInfo, err := cons.client.DescribeStream(streamQuery)
	if err != nil {
		return err
	}

	if streamInfo.StreamDescription == nil {
		return fmt.Errorf("StreamDescription could not be retrieved.")
	}

	for _, shard := range streamInfo.StreamDescription.Shards {
		if shard.ShardId == nil {
			return fmt.Errorf("ShardId could not be retrieved.")
		}

		offset, offsetStored := cons.offsets[*shard.ShardId]
		if !offsetStored {
			offset = cons.defaultOffset
			cons.offsets[*shard.ShardId] = 0
		}

		iteratorConfig := &kinesis.GetShardIteratorInput{
			ShardId:                aws.String(*shard.ShardId),
			ShardIteratorType:      aws.String(cons.offsetType),
			StreamName:             aws.String(cons.stream),
			StartingSequenceNumber: aws.String(fmt.Sprintf("%d", offset)),
		}

		go cons.processShard(iteratorConfig)
	}
	return nil
}

// Consume listens to stdin.
func (cons *Kinesis) Consume(workers *sync.WaitGroup) {
	cons.AddMainWorker(workers)
	if err := cons.connect(); err != nil {
		Log.Error.Print("Kinesis connection error: ", err)
		return
	}
	cons.ControlLoop()
}
