// Licensed to the Apache Software Foundation (ASF) under one
// or more contributor license agreements.  See the NOTICE file
// distributed with this work for additional information
// regarding copyright ownership.  The ASF licenses this file
// to you under the Apache License, Version 2.0 (the
// "License"); you may not use this file except in compliance
// with the License.  You may obtain a copy of the License at
//
//   http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing,
// software distributed under the License is distributed on an
// "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
// KIND, either express or implied.  See the License for the
// specific language governing permissions and limitations
// under the License.

package kafsar

import (
	"container/list"
	"context"
	"fmt"
	"github.com/apache/pulsar-client-go/pulsar"
	"github.com/paashzj/kafka_go/pkg/service"
	"github.com/paashzj/kafka_go_pulsar/pkg/constant"
	"github.com/paashzj/kafka_go_pulsar/pkg/utils"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"net"
	"sync"
	"time"
)

type KafkaImpl struct {
	server           Server
	pulsarConfig     PulsarConfig
	pulsarClient     pulsar.Client
	groupCoordinator GroupCoordinator
	kafsarConfig     KafsarConfig
	readerManager    map[string]*ReaderMetadata
	mutex            sync.RWMutex
	userInfoManager  map[string]*userInfo
	offsetManager    OffsetManager
	memberManager    map[string]*MemberInfo
}

type userInfo struct {
	username string
	clientId string
}

type MessageIdPair struct {
	MessageId pulsar.MessageID
	Offset    int64
}

type MemberInfo struct {
	memberId        string
	groupId         string
	groupInstanceId *string
	clientId        string
}

func NewKafsar(impl Server, config *Config) (*KafkaImpl, error) {
	kafka := KafkaImpl{server: impl, pulsarConfig: config.PulsarConfig, kafsarConfig: config.KafsarConfig}
	pulsarUrl := fmt.Sprintf("pulsar://%s:%d", kafka.pulsarConfig.Host, kafka.pulsarConfig.TcpPort)
	var err error
	kafka.pulsarClient, err = pulsar.NewClient(pulsar.ClientOptions{URL: pulsarUrl})
	if err != nil {
		return nil, err
	}
	pulsarAddr := kafka.getPulsarHttpUrl()
	kafka.offsetManager, err = NewOffsetManager(kafka.pulsarClient, config.KafsarConfig, pulsarAddr)
	if err != nil {
		kafka.pulsarClient.Close()
	}

	offsetChannel := kafka.offsetManager.Start()
	for {
		if <-offsetChannel {
			break
		}
	}
	if kafka.kafsarConfig.GroupCoordinatorType == Cluster {
		kafka.groupCoordinator = NewGroupCoordinatorCluster()
	} else if kafka.kafsarConfig.GroupCoordinatorType == Standalone {
		kafka.groupCoordinator = NewGroupCoordinatorStandalone(kafka.pulsarConfig, kafka.kafsarConfig, kafka.pulsarClient)
	} else {
		return nil, errors.Errorf("unexpect GroupCoordinatorType: %v", kafka.kafsarConfig.GroupCoordinatorType)
	}
	kafka.readerManager = make(map[string]*ReaderMetadata)
	kafka.userInfoManager = make(map[string]*userInfo)
	kafka.memberManager = make(map[string]*MemberInfo)
	return &kafka, nil
}

func (k *KafkaImpl) Produce(addr net.Addr, kafkaTopic string, partition int, req *service.ProducePartitionReq) (*service.ProducePartitionResp, error) {
	panic("implement me")
}

func (k *KafkaImpl) Fetch(addr net.Addr, req *service.FetchReq) ([]*service.FetchTopicResp, error) {
	var maxWaitTime int
	if req.MaxWaitTime < k.kafsarConfig.MaxFetchWaitMs {
		maxWaitTime = req.MaxWaitTime
	} else {
		maxWaitTime = k.kafsarConfig.MaxFetchWaitMs
	}
	fetchStart := time.Now()
	reqList := req.FetchTopicReqList
	result := make([]*service.FetchTopicResp, len(reqList))
	for i, topicReq := range reqList {
		f := &service.FetchTopicResp{}
		f.Topic = topicReq.Topic
		f.FetchPartitionRespList = make([]*service.FetchPartitionResp, len(topicReq.FetchPartitionReqList))
		for j, partitionReq := range topicReq.FetchPartitionReqList {
			f.FetchPartitionRespList[j] = k.FetchPartition(addr, topicReq.Topic, partitionReq, req.MaxBytes, req.MinBytes, maxWaitTime, fetchStart)
		}
		result[i] = f
	}
	return result, nil
}

// FetchPartition visible for testing
func (k *KafkaImpl) FetchPartition(addr net.Addr, kafkaTopic string, req *service.FetchPartitionReq, maxBytes int, minBytes int, maxWaitMs int, start time.Time) *service.FetchPartitionResp {
	user, exist := k.userInfoManager[addr.String()]
	var records []*service.Record
	recordBatch := service.RecordBatch{Records: records}
	if !exist {
		logrus.Errorf("fetch partition failed when get userinfo by addr %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &service.FetchPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   service.UNKNOWN_SERVER_ERROR,
			RecordBatch: &recordBatch,
		}
	}
	logrus.Infof("%s fetch topic: %s partition %d", addr.String(), kafkaTopic, req.PartitionId)
	partitionedTopic, err := k.partitionedTopic(user, kafkaTopic, req.PartitionId)
	if err != nil {
		logrus.Errorf("fetch partition failed when get pulsar topic %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &service.FetchPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   service.UNKNOWN_SERVER_ERROR,
			RecordBatch: &recordBatch,
		}
	}
	k.mutex.RLock()
	readerMetadata, exist := k.readerManager[partitionedTopic+req.ClientId]
	k.mutex.RUnlock()
	if !exist {
		logrus.Errorf("can not find reader for topic: %s when fetch partition", partitionedTopic)
		return &service.FetchPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   service.UNKNOWN_SERVER_ERROR,
			RecordBatch: &recordBatch,
		}
	}
	byteLength := 0
	var baseOffset int64
	fistMessage := true
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(maxWaitMs)*time.Millisecond)
	defer cancel()
OUT:
	for {
		if time.Since(start).Milliseconds() >= int64(maxWaitMs) || len(recordBatch.Records) >= k.kafsarConfig.MaxFetchRecord {
			break OUT
		}
		message, err := readerMetadata.reader.Next(ctx)
		if err != nil {
			logrus.Errorf("read msg failed. err: %s", err)
			continue
		}
		byteLength = byteLength + utils.CalculateMsgLength(message)
		logrus.Infof("receive msg: %s from %s", message.ID(), message.Topic())
		offset := convOffset(message, k.kafsarConfig.ContinuousOffset)
		if fistMessage {
			fistMessage = false
			baseOffset = offset
		}
		relativeOffset := offset - baseOffset
		record := service.Record{
			Value:          message.Payload(),
			RelativeOffset: int(relativeOffset),
		}
		recordBatch.Records = append(recordBatch.Records, &record)
		readerMetadata.messageIds.PushBack(MessageIdPair{
			MessageId: message.ID(),
			Offset:    offset,
		})
		if byteLength > minBytes && time.Since(start).Milliseconds() >= int64(k.kafsarConfig.MinFetchWaitMs) {
			break
		}
		if byteLength > maxBytes {
			break
		}
	}
	recordBatch.Offset = baseOffset
	return &service.FetchPartitionResp{
		ErrorCode:        service.NONE,
		PartitionId:      req.PartitionId,
		LastStableOffset: 0,
		LogStartOffset:   0,
		RecordBatch:      &recordBatch,
	}
}

func (k *KafkaImpl) GroupJoin(addr net.Addr, req *service.JoinGroupReq) (*service.JoinGroupResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("username not found in join group: %s", req.GroupId)
		return &service.JoinGroupResp{
			ErrorCode:    service.UNKNOWN_SERVER_ERROR,
			MemberId:     req.MemberId,
			GenerationId: -1,
		}, nil
	}
	logrus.Infof("%s joining to group: %s, memberId: %s", addr.String(), req.GroupId, req.MemberId)
	joinGroupResp, err := k.groupCoordinator.HandleJoinGroup(user.username, req.GroupId, req.MemberId, req.ClientId, req.ProtocolType,
		req.SessionTimeout, req.GroupProtocols)
	if err != nil {
		logrus.Errorf("unexpected exception in join group: %s, error: %s", req.GroupId, err)
		return &service.JoinGroupResp{
			ErrorCode:    service.UNKNOWN_SERVER_ERROR,
			MemberId:     req.MemberId,
			GenerationId: -1,
		}, nil
	}
	memberInfo := MemberInfo{
		memberId:        joinGroupResp.MemberId,
		groupId:         req.GroupId,
		groupInstanceId: req.GroupInstanceId,
		clientId:        req.ClientId,
	}
	k.mutex.Lock()
	k.memberManager[addr.String()] = &memberInfo
	k.mutex.Unlock()
	return joinGroupResp, nil
}

func (k *KafkaImpl) GroupLeave(addr net.Addr, req *service.LeaveGroupReq) (*service.LeaveGroupResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("username not found in join group: %s", req.GroupId)
		return &service.LeaveGroupResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s leaving group: %s, members: %+v", addr.String(), req.GroupId, req.Members)
	leaveGroupResp, err := k.groupCoordinator.HandleLeaveGroup(user.username, req.GroupId, req.Members)
	if err != nil {
		logrus.Errorf("unexpected exception in leaving group: %s, error: %s", req.GroupId, err)
		return &service.LeaveGroupResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	group, err := k.groupCoordinator.GetGroup(user.username, req.GroupId)
	if err != nil {
		logrus.Errorf("get group %s failed, error: %s", req.GroupId, err)
		return &service.LeaveGroupResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	readerMetadata, exist := k.readerManager[group.partitionedTopic+req.ClientId]
	if exist {
		readerMetadata.reader.Close()
		logrus.Infof("success close reader topic: %s", group.partitionedTopic)
		delete(k.readerManager, group.partitionedTopic+req.ClientId)
		readerMetadata = nil
	}
	return leaveGroupResp, nil
}

func (k *KafkaImpl) GroupSync(addr net.Addr, req *service.SyncGroupReq) (*service.SyncGroupResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("username not found in join group: %s", req.GroupId)
		return &service.SyncGroupResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s syncing group: %s, memberId: %s", addr.String(), req.GroupId, req.MemberId)
	syncGroupResp, err := k.groupCoordinator.HandleSyncGroup(user.username, req.GroupId, req.MemberId, req.GenerationId, req.GroupAssignments)
	if err != nil {
		logrus.Errorf("unexpected exception in sync group: %s, error: %s", req.GroupId, err)
		return &service.SyncGroupResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	return syncGroupResp, nil
}

func (k *KafkaImpl) OffsetListPartition(addr net.Addr, kafkaTopic string, req *service.ListOffsetsPartitionReq) (*service.ListOffsetsPartitionResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("offset list failed when get username by addr %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &service.ListOffsetsPartitionResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s offset list topic: %s, partition: %d", addr.String(), kafkaTopic, req.PartitionId)
	partitionedTopic, err := k.partitionedTopic(user, kafkaTopic, req.PartitionId)
	if err != nil {
		logrus.Errorf("get topic failed. err: %s", err)
		return &service.ListOffsetsPartitionResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	k.mutex.RLock()
	readerMessages, exist := k.readerManager[partitionedTopic+req.ClientId]
	k.mutex.RUnlock()
	if !exist {
		logrus.Errorf("offset list failed, topic: %s, does not exist", partitionedTopic)
		return &service.ListOffsetsPartitionResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	offset := constant.DefaultOffset
	if req.Time == constant.TimeLasted {
		msg, err := utils.GetLatestMsgId(partitionedTopic, k.getPulsarHttpUrl())
		if err != nil {
			logrus.Errorf("get topic %s latest offset failed %s\n", kafkaTopic, err)
			return &service.ListOffsetsPartitionResp{
				ErrorCode: service.UNKNOWN_SERVER_ERROR,
			}, nil
		}
		lastedMsg, err := utils.ReadLastedMsg(partitionedTopic, k.kafsarConfig.MaxFetchWaitMs, msg, k.pulsarClient)
		if err != nil {
			logrus.Errorf("read lasted msg failed. topic: %s, err: %s", kafkaTopic, err)
			return &service.ListOffsetsPartitionResp{
				ErrorCode: service.UNKNOWN_SERVER_ERROR,
			}, nil
		}
		if lastedMsg != nil {
			err := readerMessages.reader.Seek(lastedMsg.ID())
			if err != nil {
				logrus.Errorf("offset list failed, topic: %s, err: %s", partitionedTopic, err)
				return &service.ListOffsetsPartitionResp{
					ErrorCode: service.UNKNOWN_SERVER_ERROR,
				}, nil
			}
			offset = convOffset(lastedMsg, k.kafsarConfig.ContinuousOffset)
		}
	}
	if req.Time == constant.TimeEarliest {
		message, err := utils.ReadEarliestMsg(partitionedTopic, k.kafsarConfig.MaxFetchWaitMs, k.pulsarClient)
		if err != nil {
			logrus.Errorf("read earliest msg failed. topic: %s, err: %s", kafkaTopic, err)
			return &service.ListOffsetsPartitionResp{
				ErrorCode: service.UNKNOWN_SERVER_ERROR,
			}, nil
		}
		if message != nil {
			err := readerMessages.reader.Seek(message.ID())
			if err != nil {
				logrus.Errorf("offset list failed, topic: %s, err: %s", partitionedTopic, err)
				return &service.ListOffsetsPartitionResp{
					ErrorCode: service.UNKNOWN_SERVER_ERROR,
				}, nil
			}
			offset = convOffset(message, k.kafsarConfig.ContinuousOffset)
		}
	}
	return &service.ListOffsetsPartitionResp{
		PartitionId: req.PartitionId,
		Offset:      offset,
		Time:        constant.TimeEarliest,
	}, nil
}

func (k *KafkaImpl) OffsetCommitPartition(addr net.Addr, kafkaTopic string, req *service.OffsetCommitPartitionReq) (*service.OffsetCommitPartitionResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("offset commit failed when get userinfo by addr %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &service.OffsetCommitPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	logrus.Infof("%s topic: %s, partition: %d, commit offset: %d", addr.String(), kafkaTopic, req.PartitionId, req.OffsetCommitOffset)
	partitionedTopic, err := k.partitionedTopic(user, kafkaTopic, req.PartitionId)
	if err != nil {
		logrus.Errorf("offset commit failed when get pulsar topic %s, kafka topic: %s", addr.String(), kafkaTopic)
		return &service.OffsetCommitPartitionResp{
			PartitionId: req.PartitionId,
			ErrorCode:   service.UNKNOWN_SERVER_ERROR,
		}, nil
	}
	k.mutex.RLock()
	readerMessages, exist := k.readerManager[partitionedTopic+req.ClientId]
	k.mutex.RUnlock()
	if !exist {
		logrus.Errorf("commit offset failed, topic: %s, does not exist", partitionedTopic)
		return &service.OffsetCommitPartitionResp{ErrorCode: service.UNKNOWN_TOPIC_ID}, nil
	}
	length := readerMessages.messageIds.Len()
	for i := 0; i < length; i++ {
		front := readerMessages.messageIds.Front()
		if front == nil {
			break
		}
		messageIdPair := front.Value.(MessageIdPair)
		// kafka commit offset maybe greater than current offset
		if messageIdPair.Offset == req.OffsetCommitOffset || ((messageIdPair.Offset < req.OffsetCommitOffset) && (i == length-1)) {
			err := k.offsetManager.CommitOffset(user.username, kafkaTopic, readerMessages.groupId, req.PartitionId, messageIdPair)
			if err != nil {
				logrus.Errorf("commit offset failed. topic: %s, err: %s", kafkaTopic, err)
				return &service.OffsetCommitPartitionResp{
					PartitionId: req.PartitionId,
					ErrorCode:   service.UNKNOWN_SERVER_ERROR,
				}, nil
			}
		}
		if messageIdPair.Offset > req.OffsetCommitOffset {
			break
		}
		logrus.Infof("ack pulsar %s for %s", partitionedTopic, messageIdPair.MessageId)
		readerMessages.messageIds.Remove(front)
	}
	return &service.OffsetCommitPartitionResp{
		PartitionId: req.PartitionId,
		ErrorCode:   service.NONE,
	}, nil
}

func (k *KafkaImpl) OffsetFetch(addr net.Addr, topic string, req *service.OffsetFetchPartitionReq) (*service.OffsetFetchPartitionResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("offset fetch failed when get userinfo by addr %s, kafka topic: %s", addr.String(), topic)
		return &service.OffsetFetchPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	logrus.Infof("%s fetch topic: %s offset, partition: %d", addr.String(), topic, req.PartitionId)
	partitionedTopic, err := k.partitionedTopic(user, topic, req.PartitionId)
	if err != nil {
		logrus.Errorf("offset fetch failed when get pulsar topic %s, kafka topic: %s", addr.String(), topic)
		return &service.OffsetFetchPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	subscriptionName, err := k.server.SubscriptionName(req.GroupId)
	if err != nil {
		logrus.Errorf("sync group %s failed when offset fetch, error: %s", req.GroupId, err)
	}
	messagePair, flag := k.offsetManager.AcquireOffset(user.username, topic, req.GroupId, req.PartitionId)
	messageId := pulsar.EarliestMessageID()
	kafkaOffset := constant.UnknownOffset
	if flag {
		kafkaOffset = messagePair.Offset
		messageId = messagePair.MessageId
	}
	k.mutex.RLock()
	consumerMetadata, exist := k.readerManager[partitionedTopic+req.ClientId]
	k.mutex.RUnlock()
	if !exist {
		k.mutex.Lock()
		metadata := ReaderMetadata{groupId: req.GroupId, messageIds: list.New()}
		channel, reader, err := k.createReader(partitionedTopic, subscriptionName, messageId)
		if err != nil {
			logrus.Errorf("%s, create channel failed, error: %s", topic, err)
			return &service.OffsetFetchPartitionResp{
				ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
			}, nil
		}
		metadata.reader = reader
		metadata.channel = channel
		k.readerManager[partitionedTopic+req.ClientId] = &metadata
		consumerMetadata = &metadata
		k.mutex.Unlock()
	}
	group, err := k.groupCoordinator.GetGroup(user.username, req.GroupId)
	if err != nil {
		logrus.Errorf("get group %s failed, error: %s", req.GroupId, err)
		return &service.OffsetFetchPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	group.partitionedTopic = partitionedTopic
	group.consumerMetadata = consumerMetadata

	return &service.OffsetFetchPartitionResp{
		PartitionId: req.PartitionId,
		Offset:      kafkaOffset,
		LeaderEpoch: -1,
		Metadata:    nil,
		ErrorCode:   int16(service.NONE),
	}, nil
}

func (k *KafkaImpl) partitionedTopic(user *userInfo, kafkaTopic string, partitionId int) (string, error) {
	pulsarTopic, err := k.server.PulsarTopic(user.username, kafkaTopic)
	if err != nil {
		return "", err
	}
	return pulsarTopic + fmt.Sprintf(constant.PartitionSuffixFormat, partitionId), nil
}

func (k *KafkaImpl) OffsetLeaderEpoch(addr net.Addr, topic string, req *service.OffsetLeaderEpochPartitionReq) (*service.OffsetLeaderEpochPartitionResp, error) {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("offset fetch failed when get userinfo by addr %s, kafka topic: %s", addr.String(), topic)
		return &service.OffsetLeaderEpochPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	logrus.Infof("%s offset leader epoch topic: %s, partition: %d", addr.String(), topic, req.PartitionId)
	partitionedTopic, err := k.partitionedTopic(user, topic, req.PartitionId)
	if err != nil {
		logrus.Errorf("get partitioned topic failed. topic: %s", topic)
		return &service.OffsetLeaderEpochPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	msgByte, err := utils.GetLatestMsgId(partitionedTopic, k.getPulsarHttpUrl())
	if err != nil {
		logrus.Errorf("get last msgId failed. topic: %s", topic)
		return &service.OffsetLeaderEpochPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	msg, err := utils.ReadLastedMsg(partitionedTopic, k.kafsarConfig.MaxFetchWaitMs, msgByte, k.pulsarClient)
	if err != nil {
		logrus.Errorf("get last msgId failed. topic: %s", topic)
		return &service.OffsetLeaderEpochPartitionResp{
			ErrorCode: int16(service.UNKNOWN_SERVER_ERROR),
		}, nil
	}
	offset := convOffset(msg, k.kafsarConfig.ContinuousOffset)
	return &service.OffsetLeaderEpochPartitionResp{
		ErrorCode:   int16(service.NONE),
		PartitionId: req.PartitionId,
		LeaderEpoch: req.LeaderEpoch,
		Offset:      offset,
	}, nil
}

func (k *KafkaImpl) SaslAuth(addr net.Addr, req service.SaslReq) (bool, service.ErrorCode) {
	auth, err := k.server.Auth(req.Username, req.Password, req.ClientId)
	if err != nil || !auth {
		return false, service.SASL_AUTHENTICATION_FAILED
	}
	_, exist := k.userInfoManager[addr.String()]
	if !exist {
		k.userInfoManager[addr.String()] = &userInfo{
			username: req.Username,
			clientId: req.ClientId,
		}
	}
	return true, service.NONE
}

func (k *KafkaImpl) SaslAuthTopic(addr net.Addr, req service.SaslReq, topic, permissionType string) (bool, service.ErrorCode) {
	auth, err := k.server.AuthTopic(req.Username, req.Password, req.ClientId, topic, permissionType)
	if err != nil || !auth {
		return false, service.SASL_AUTHENTICATION_FAILED
	}
	return true, service.NONE
}

func (k *KafkaImpl) SaslAuthConsumerGroup(addr net.Addr, req service.SaslReq, consumerGroup string) (bool, service.ErrorCode) {
	auth, err := k.server.AuthTopicGroup(req.Username, req.Password, req.ClientId, consumerGroup)
	if err != nil || !auth {
		return false, service.SASL_AUTHENTICATION_FAILED
	}
	return true, service.NONE
}

func (k *KafkaImpl) Disconnect(addr net.Addr) {
	logrus.Infof("lost connection: %s", addr)
	if addr == nil {
		return
	}
	delete(k.userInfoManager, addr.String())
	k.mutex.Lock()
	defer k.mutex.Unlock()
	memberInfo, exist := k.memberManager[addr.String()]
	if !exist {
		return
	}
	memberList := []*service.LeaveGroupMember{
		{
			MemberId:        memberInfo.memberId,
			GroupInstanceId: memberInfo.groupInstanceId,
		},
	}
	req := service.LeaveGroupReq{
		ClientId: memberInfo.clientId,
		GroupId:  memberInfo.groupId,
		Members:  memberList,
	}
	_, err := k.GroupLeave(addr, &req)
	if err != nil {
		logrus.Errorf("leave group failed. err: %s", err)
	}
}

func (k *KafkaImpl) Close() {
	k.offsetManager.Close()
	k.pulsarClient.Close()
}

func (k *KafkaImpl) GetOffsetManager() OffsetManager {
	return k.offsetManager
}

func (k *KafkaImpl) createReader(partitionedTopic string, subscriptionName string, messageId pulsar.MessageID) (chan pulsar.ReaderMessage, pulsar.Reader, error) {
	channel := make(chan pulsar.ReaderMessage, k.kafsarConfig.ConsumerReceiveQueueSize)
	options := pulsar.ReaderOptions{
		Topic:             partitionedTopic,
		Name:              subscriptionName,
		SubscriptionName:  subscriptionName,
		StartMessageID:    messageId,
		MessageChannel:    channel,
		ReceiverQueueSize: k.kafsarConfig.ConsumerReceiveQueueSize,
	}
	reader, err := k.pulsarClient.CreateReader(options)
	if err != nil {
		return nil, nil, err
	}
	return channel, reader, nil
}

func (k *KafkaImpl) HeartBeat(addr net.Addr, req service.HeartBeatReq) *service.HeartBeatResp {
	user, exist := k.userInfoManager[addr.String()]
	if !exist {
		logrus.Errorf("offset fetch failed when get userinfo by addr %s", addr.String())
		return &service.HeartBeatResp{
			ErrorCode: service.UNKNOWN_SERVER_ERROR,
		}
	}
	return k.groupCoordinator.HandleHeartBeat(user.username, req.GroupId)
}

func (k *KafkaImpl) PartitionNum(kafkaTopic string) (int, error) {
	return 1, nil
}

func (k *KafkaImpl) getPulsarHttpUrl() string {
	return fmt.Sprintf("http://%s:%d", k.pulsarConfig.Host, k.pulsarConfig.HttpPort)
}
