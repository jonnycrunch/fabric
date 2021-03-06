/*
Copyright IBM Corp. 2016 All Rights Reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

		 http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package obcpbft

import (
	"fmt"
	gp "google/protobuf"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/protobuf/proto"
	"github.com/op/go-logging"

	pb "github.com/hyperledger/fabric/protos"
)

func init() {
	logging.SetLevel(logging.DEBUG, "")
}

func TestEnvOverride(t *testing.T) {
	config := loadConfig()

	key := "general.mode"               // for a key that exists
	envName := "CORE_PBFT_GENERAL_MODE" // env override name
	overrideValue := "overide_test"     // value to override default value with

	// test key
	if ok := config.IsSet("general.mode"); !ok {
		t.Fatalf("Cannot test env override because \"%s\" does not seem to be set", key)
	}

	os.Setenv(envName, overrideValue)
	// The override config value will cause other calls to fail unless unset.
	defer func() {
		os.Unsetenv(envName)
	}()

	if ok := config.IsSet("general.mode"); !ok {
		t.Fatalf("Env override in place, and key \"%s\" is not set", key)
	}

	// read key
	configVal := config.GetString("general.mode")
	if configVal != overrideValue {
		t.Fatalf("Env override in place, expected key \"%s\" to be \"%s\" but instead got \"%s\"", key, overrideValue, configVal)
	}

}

func TestMaliciousPrePrepare(t *testing.T) {
	mock := &omniProto{
		broadcastImpl: func(msgPayload []byte) {
			t.Fatalf("Expected to ignore malicious pre-prepare")
		},
	}
	instance := newPbftCore(1, loadConfig(), mock)
	defer instance.close()
	instance.replicaCount = 5

	digest1 := "hi there"
	request2 := &Request{Payload: []byte("other"), ReplicaId: uint64(generateBroadcaster(instance.replicaCount))}

	pbftMsg := &Message_PrePrepare{&PrePrepare{
		View:           0,
		SequenceNumber: 1,
		RequestDigest:  digest1,
		Request:        request2,
		ReplicaId:      0,
	}}
	sendEvent(instance, pbftMsg)
}

func TestWrongReplicaID(t *testing.T) {
	mock := &omniProto{
		validateImpl: func(msg []byte) error {
			return nil
		},
	}
	instance := newPbftCore(1, loadConfig(), mock)

	chainTxMsg := createOcMsgWithChainTx(1)
	req := &Request{
		Timestamp: chainTxMsg.Timestamp,
		Payload:   chainTxMsg.Payload,
		ReplicaId: 1,
	}
	pbftMsg := &Message{&Message_Request{req}}
	next, err := instance.recvMsg(pbftMsg, 0)

	if next != nil || err == nil {
		t.Fatalf("Shouldn't have processed message with incorrect replica ID")
	}

	if err != nil {
		rightError := strings.HasPrefix(err.Error(), "Sender ID")
		if !rightError {
			t.Fatalf("Should have returned error about incorrect replica ID on the incoming message")
		}
	}
}

func TestIncompletePayload(t *testing.T) {
	mock := &omniProto{
		validateImpl: func(msg []byte) error {
			return nil
		},
	}
	instance := newPbftCore(1, loadConfig(), mock)
	defer instance.close()
	instance.replicaCount = 5

	broadcaster := uint64(generateBroadcaster(instance.replicaCount))

	checkMsg := func(msg *Message, errMsg string, args ...interface{}) {
		mock.broadcastImpl = func(msgPayload []byte) {
			t.Errorf(errMsg, args...)
		}
		sendEvent(instance, pbftMessageEvent{msg: msg, sender: broadcaster})
	}

	checkMsg(&Message{}, "Expected to reject empty message")
	checkMsg(&Message{&Message_Request{&Request{ReplicaId: broadcaster}}}, "Expected to reject empty request")
	checkMsg(&Message{&Message_PrePrepare{&PrePrepare{ReplicaId: broadcaster}}}, "Expected to reject empty pre-prepare")
}

func TestNetwork(t *testing.T) {
	validatorCount := 7
	net := makePBFTNetwork(validatorCount, nil)

	msg := createPbftRequestWithChainTx(1, uint64(generateBroadcaster(validatorCount)))
	net.pbftEndpoints[0].pbft.manager.queue() <- msg

	err := net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions <= 0 {
			t.Errorf("Instance %d did not execute transaction", pep.id)
			continue
		}
		if pep.sc.executions != 1 {
			t.Errorf("Instance %d executed more than one transaction", pep.id)
			continue
		}
		if !reflect.DeepEqual(pep.sc.lastExecution, msg.Payload) {
			t.Errorf("Instance %d executed wrong transaction, %x should be %x",
				pep.id, pep.sc.lastExecution, msg.Payload)
		}
	}
}

type checkpointConsumer struct {
	simpleConsumer
	execWait *sync.WaitGroup
}

func (cc *checkpointConsumer) execute(seqNo uint64, tx []byte) {
}

func TestCheckpoint(t *testing.T) {
	execWait := &sync.WaitGroup{}
	finishWait := &sync.WaitGroup{}

	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", 2)
	config.Set("general.logmultiplier", 2)
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	execReq := func(iter int64) {
		txTime := &gp.Timestamp{Seconds: iter, Nanos: 0}
		tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
		txPacked, err := proto.Marshal(tx)
		if err != nil {
			t.Fatalf("Failed to marshal TX block: %s", err)
		}
		msg := &Message{&Message_Request{&Request{Payload: txPacked, ReplicaId: uint64(generateBroadcaster(validatorCount))}}}
		net.pbftEndpoints[0].pbft.manager.queue() <- pbftMessageEvent{msg: msg, sender: msg.GetRequest().ReplicaId}

		net.process()
	}

	// execWait is 0, and execute will proceed
	execReq(1)
	execReq(2)
	finishWait.Wait()
	net.process()

	for _, pep := range net.pbftEndpoints {
		if len(pep.pbft.chkpts) != 1 {
			t.Errorf("Expected 1 checkpoint, found %d", len(pep.pbft.chkpts))
			continue
		}

		if _, ok := pep.pbft.chkpts[2]; !ok {
			t.Errorf("Expected checkpoint for seqNo 2")
			continue
		}

		if pep.pbft.h != 2 {
			t.Errorf("Expected low water mark to be 2, got %d", pep.pbft.h)
			continue
		}
	}

	// this will block executes for now
	execWait.Add(1)
	execReq(3)
	execReq(4)
	execReq(5)
	execReq(6)

	// by now the requests should have committed, but not yet executed
	// we also reached the high water mark by now.

	execReq(7)

	// request 7 should not have committed, because no more free seqNo
	// could be assigned.

	// unblock executes.
	execWait.Add(-1)

	net.process()
	finishWait.Wait() // Decoupling the execution thread makes this nastiness necessary
	net.process()

	// by now request 7 should have been confirmed and executed

	for _, pep := range net.pbftEndpoints {
		expectedExecutions := uint64(7)
		if pep.sc.executions != expectedExecutions {
			t.Errorf("Should have executed %d, got %d instead for replica %d", expectedExecutions, pep.sc.executions, pep.id)
		}
	}
}

func TestLostPrePrepare(t *testing.T) {
	validatorCount := 4
	net := makePBFTNetwork(validatorCount, nil)
	defer net.stop()

	txTime := &gp.Timestamp{Seconds: 1, Nanos: 0}
	tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
	txPacked, _ := proto.Marshal(tx)

	req := &Request{
		Timestamp: &gp.Timestamp{Seconds: 1, Nanos: 0},
		Payload:   txPacked,
		ReplicaId: uint64(generateBroadcaster(validatorCount)),
	}

	net.pbftEndpoints[0].pbft.manager.queue() <- (req)

	// clear all messages sent by primary
	msg := <-net.msgs
	prePrep := &Message{}
	err := proto.Unmarshal(msg.msg, prePrep)
	if err != nil {
		t.Fatalf("Error unmarshaling message")
	}
	net.clearMessages()

	// deliver pre-prepare to subset of replicas
	for _, pep := range net.pbftEndpoints[1 : len(net.pbftEndpoints)-1] {
		pep.pbft.manager.queue() <- prePrep.GetPrePrepare()
	}

	err = net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	for _, pep := range net.pbftEndpoints {
		if pep.id != 3 && pep.sc.executions != 1 {
			t.Errorf("Expected execution on replica %d", pep.id)
			continue
		}
		if pep.id == 3 && pep.sc.executions > 0 {
			t.Errorf("Expected no execution")
			continue
		}
	}
}

func TestInconsistentPrePrepare(t *testing.T) {
	validatorCount := 4
	net := makePBFTNetwork(validatorCount, nil)
	defer net.stop()

	txTime := &gp.Timestamp{Seconds: 1, Nanos: 0}
	tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
	txPacked, _ := proto.Marshal(tx)

	makePP := func(iter int64) *PrePrepare {
		req := &Request{
			Timestamp: &gp.Timestamp{Seconds: iter, Nanos: 0},
			Payload:   txPacked,
			ReplicaId: uint64(generateBroadcaster(validatorCount)),
		}
		preprep := &PrePrepare{
			View:           0,
			SequenceNumber: 1,
			RequestDigest:  hashReq(req),
			Request:        req,
			ReplicaId:      0,
		}
		return preprep
	}

	net.pbftEndpoints[0].pbft.manager.queue() <- makePP(1).Request

	// clear all messages sent by primary
	net.clearMessages()

	// replace with fake messages
	net.pbftEndpoints[1].pbft.manager.queue() <- makePP(1)
	net.pbftEndpoints[2].pbft.manager.queue() <- makePP(2)
	net.pbftEndpoints[3].pbft.manager.queue() <- makePP(3)

	net.process()

	for n, pep := range net.pbftEndpoints {
		if pep.sc.executions < 1 || pep.sc.executions > 3 {
			t.Errorf("Replica %d expected [1,3] executions, got %d", n, pep.sc.executions)
			continue
		}
	}
}

// This test is designed to detect a conflation of S and S' from the paper in the view change
func TestViewChangeWatermarksMovement(t *testing.T) {
	instance := newPbftCore(0, loadConfig(), &omniProto{
		viewChangeImpl: func(v uint64) {},
		skipToImpl: func(s uint64, id []byte, replicas []uint64) {
			t.Fatalf("Should not have attempted to initiate state transfer")
		},
		broadcastImpl: func(b []byte) {},
	})
	instance.manager.start()
	instance.activeView = false
	instance.view = 1
	instance.lastExec = 10

	vset := make([]*ViewChange, 3)

	// Replica 0 sent checkpoints for 10
	vset[0] = &ViewChange{
		H: 5,
		Cset: []*ViewChange_C{
			{
				SequenceNumber: 10,
				Id:             "ten",
			},
		},
	}

	// Replica 1 sent checkpoints for 10
	vset[1] = &ViewChange{
		H: 5,
		Cset: []*ViewChange_C{
			{
				SequenceNumber: 10,
				Id:             "ten",
			},
		},
	}

	// Replica 2 sent checkpoints for 10
	vset[2] = &ViewChange{
		H: 5,
		Cset: []*ViewChange_C{
			{
				SequenceNumber: 10,
				Id:             "ten",
			},
		},
	}

	xset := make(map[uint64]string)
	xset[11] = ""

	instance.newViewStore[1] = &NewView{
		View:      1,
		Vset:      vset,
		Xset:      xset,
		ReplicaId: 1,
	}

	if nil != instance.processNewView() {
		t.Fatalf("Failed to successfully process new view")
	}

	expected := uint64(10)
	if instance.h != expected {
		t.Fatalf("Expected to move high watermark to %d, but picked %d", expected, instance.h)
	}
}

// This test is designed to detect a conflation of S and S' from the paper in the view change
func TestViewChangeCheckpointSelection(t *testing.T) {
	instance := &pbftCore{
		f:  1,
		N:  4,
		id: 0,
	}

	vset := make([]*ViewChange, 3)

	// Replica 0 sent checkpoints for 5
	vset[0] = &ViewChange{
		H: 5,
		Cset: []*ViewChange_C{
			{
				SequenceNumber: 10,
				Id:             "ten",
			},
		},
	}

	// Replica 1 sent checkpoints for 5
	vset[1] = &ViewChange{
		H: 5,
		Cset: []*ViewChange_C{
			{
				SequenceNumber: 10,
				Id:             "ten",
			},
		},
	}

	// Replica 2 sent checkpoints for 15
	vset[2] = &ViewChange{
		H: 10,
		Cset: []*ViewChange_C{
			{
				SequenceNumber: 15,
				Id:             "fifteen",
			},
		},
	}

	checkpoint, ok, _ := instance.selectInitialCheckpoint(vset)

	if !ok {
		t.Fatalf("Failed to pick correct a checkpoint for view change")
	}

	expected := uint64(10)
	if checkpoint.SequenceNumber != expected {
		t.Fatalf("Expected to pick checkpoint %d, but picked %d", expected, checkpoint.SequenceNumber)
	}
}

func TestViewChange(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", 2)
	config.Set("general.logmultiplier", 2)
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	execReq := func(iter int64) {
		txTime := &gp.Timestamp{Seconds: iter, Nanos: 0}
		tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
		txPacked, err := proto.Marshal(tx)
		if err != nil {
			t.Fatalf("Failed to marshal TX block: %s", err)
		}
		msg := &Message{&Message_Request{&Request{Payload: txPacked, ReplicaId: uint64(generateBroadcaster(validatorCount))}}}
		net.pbftEndpoints[0].pbft.manager.queue() <- pbftMessageEvent{msg: msg, sender: msg.GetRequest().ReplicaId}
		if err != nil {
			t.Fatalf("Request failed: %s", err)
		}

		err = net.process()
		if err != nil {
			t.Fatalf("Processing failed: %s", err)
		}
	}

	execReq(1)
	execReq(2)
	execReq(3)

	for i := 2; i < len(net.pbftEndpoints); i++ {
		net.pbftEndpoints[i].pbft.sendViewChange()
	}

	err := net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	if net.pbftEndpoints[1].pbft.view != 1 || net.pbftEndpoints[0].pbft.view != 1 {
		t.Fatalf("Replicas did not follow f+1 crowd to trigger view-change")
	}

	cp, ok, _ := net.pbftEndpoints[1].pbft.selectInitialCheckpoint(net.pbftEndpoints[1].pbft.getViewChanges())
	if !ok || cp.SequenceNumber != 2 {
		t.Fatalf("Wrong new initial checkpoint: %+v",
			net.pbftEndpoints[1].pbft.viewChangeStore)
	}

	msgList := net.pbftEndpoints[1].pbft.assignSequenceNumbers(net.pbftEndpoints[1].pbft.getViewChanges(), cp.SequenceNumber)
	if msgList[4] != "" || msgList[5] != "" || msgList[3] == "" {
		t.Fatalf("Wrong message list: %+v", msgList)
	}
}

func TestInconsistentDataViewChange(t *testing.T) {
	validatorCount := 4
	net := makePBFTNetwork(validatorCount, nil)
	defer net.stop()

	txTime := &gp.Timestamp{Seconds: 1, Nanos: 0}
	tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
	txPacked, _ := proto.Marshal(tx)

	makePP := func(iter int64) *PrePrepare {
		req := &Request{
			Timestamp: &gp.Timestamp{Seconds: iter, Nanos: 0},
			Payload:   txPacked,
			ReplicaId: uint64(generateBroadcaster(validatorCount)),
		}
		preprep := &PrePrepare{
			View:           0,
			SequenceNumber: 1,
			RequestDigest:  hashReq(req),
			Request:        req,
			ReplicaId:      0,
		}
		return preprep
	}

	net.pbftEndpoints[0].pbft.manager.queue() <- makePP(0).Request

	// clear all messages sent by primary
	net.clearMessages()

	// replace with fake messages
	net.pbftEndpoints[1].pbft.manager.queue() <- makePP(1)
	net.pbftEndpoints[2].pbft.manager.queue() <- makePP(1)
	net.pbftEndpoints[3].pbft.manager.queue() <- makePP(0)

	err := net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions < 1 {
			t.Errorf("Expected execution")
			continue
		}
	}
}

func TestViewChangeWithStateTransfer(t *testing.T) {
	validatorCount := 4
	net := makePBFTNetwork(validatorCount, nil)
	defer net.stop()

	var err error

	for _, pep := range net.pbftEndpoints {
		pep.pbft.K = 2
		pep.pbft.L = 6
		pep.pbft.requestTimeout = 500 * time.Millisecond
	}

	txTime := &gp.Timestamp{Seconds: 1, Nanos: 0}
	tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
	txPacked, _ := proto.Marshal(tx)

	broadcaster := uint64(generateBroadcaster(validatorCount))

	makePP := func(iter int64) *PrePrepare {
		req := &Request{
			Timestamp: &gp.Timestamp{Seconds: iter, Nanos: 0},
			Payload:   txPacked,
			ReplicaId: broadcaster,
		}
		preprep := &PrePrepare{
			View:           0,
			SequenceNumber: uint64(iter),
			RequestDigest:  hashReq(req),
			Request:        req,
			ReplicaId:      0,
		}
		return preprep
	}

	// Have primary advance the sequence number past a checkpoint for replicas 0,1,2
	for i := int64(1); i <= 3; i++ {
		net.pbftEndpoints[0].pbft.manager.queue() <- makePP(i).Request

		// clear all messages sent by primary
		net.clearMessages()

		net.pbftEndpoints[0].pbft.manager.queue() <- makePP(i)
		net.pbftEndpoints[1].pbft.manager.queue() <- makePP(i)
		net.pbftEndpoints[2].pbft.manager.queue() <- makePP(i)

		err = net.process()
		if err != nil {
			t.Fatalf("Processing failed: %s", err)
		}

	}

	fmt.Println("Done with stage 1")

	// Add to replica 3's complaint, cause a view change
	net.pbftEndpoints[1].pbft.sendViewChange()
	net.pbftEndpoints[2].pbft.sendViewChange()
	err = net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	fmt.Println("Done with stage 3")

	net.pbftEndpoints[1].pbft.manager.queue() <- makePP(5).Request
	err = net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions != 4 {
			t.Errorf("Replica %d expected execution through seqNo 5 with one null exec, got %d executions", pep.pbft.id, pep.sc.executions)
			continue
		}
	}
	fmt.Println("Done with stage 3")
}

func TestNewViewTimeout(t *testing.T) {
	millisUntilTimeout := time.Duration(800)

	validatorCount := 4
	config := loadConfig()
	config.Set("general.timeout.request", "400ms")
	config.Set("general.timeout.viewchange", "800ms")
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	replica1Disabled := false
	net.filterFn = func(src int, dst int, msg []byte) []byte {
		if dst == -1 && src == 1 && replica1Disabled {
			return nil
		}
		return msg
	}

	broadcaster := uint64(generateBroadcaster(validatorCount))

	req := createPbftRequestWithChainTx(1, broadcaster)

	go net.processContinually()

	// This will eventually trigger 1's request timeout
	// We check that one single timed out replica will not keep trying to change views by itself
	net.pbftEndpoints[1].pbft.manager.queue() <- req
	fmt.Println("Debug: Sleeping 1")
	time.Sleep(5 * millisUntilTimeout * time.Millisecond)
	fmt.Println("Debug: Waking 1")

	// This will eventually trigger 3's request timeout, which will lead to a view change to 1.
	// However, we disable 1, which will disable the new-view going through.
	// This checks that replicas will automatically move to view 2 when the view change times out.
	// However, 2 does not know about the missing request, and therefore the request will not be
	// pre-prepared and finally executed.
	replica1Disabled = true
	net.pbftEndpoints[3].pbft.manager.queue() <- req
	fmt.Println("Debug: Sleeping 2")
	time.Sleep(5 * millisUntilTimeout * time.Millisecond)
	fmt.Println("Debug: Waking 2")

	// So far, we are in view 2, and replica 1 and 3 (who got the request) in view change to view 3.
	// Submitting the request to 0 will eventually trigger its view-change timeout, which will make
	// all replicas move to view 3 and finally process the request.
	net.pbftEndpoints[0].pbft.manager.queue() <- req
	fmt.Println("Debug: Sleeping 3")
	time.Sleep(5 * millisUntilTimeout * time.Millisecond)
	fmt.Println("Debug: Waking 3")

	for i, pep := range net.pbftEndpoints {
		if pep.pbft.view < 3 {
			t.Errorf("Should have reached view 3, got %d instead for replica %d", pep.pbft.view, i)
		}
		executionsExpected := uint64(1)
		if pep.sc.executions != executionsExpected {
			t.Errorf("Should have executed %d, got %d instead for replica %d", executionsExpected, pep.sc.executions, i)
		}
	}
}

func TestViewChangeUpdateSeqNo(t *testing.T) {
	millisUntilTimeout := 400 * time.Millisecond

	validatorCount := 4
	config.Set("general.timeout.request", "400ms")
	config.Set("general.timeout.viewchange", "400ms")
	net := makePBFTNetwork(validatorCount, config)
	for _, pe := range net.pbftEndpoints {
		pe.pbft.lastExec = 99
		pe.pbft.h = 99 / pe.pbft.K * pe.pbft.K
	}
	net.pbftEndpoints[0].pbft.seqNo = 99

	go net.processContinually()

	broadcaster := uint64(generateBroadcaster(validatorCount))

	req := createPbftRequestWithChainTx(1, broadcaster)
	net.pbftEndpoints[0].pbft.manager.queue() <- req
	time.Sleep(5 * millisUntilTimeout)
	// Now we all have executed seqNo 100.  After triggering a
	// view change, the new primary should pick up right after
	// that.

	net.pbftEndpoints[0].pbft.sendViewChange()
	net.pbftEndpoints[1].pbft.sendViewChange()
	time.Sleep(5 * millisUntilTimeout)

	req = createPbftRequestWithChainTx(2, broadcaster)
	net.pbftEndpoints[1].pbft.manager.queue() <- req
	time.Sleep(5 * millisUntilTimeout)

	net.stop()
	for i, pep := range net.pbftEndpoints {
		if pep.pbft.view < 1 {
			t.Errorf("Should have reached view 3, got %d instead for replica %d", pep.pbft.view, i)
		}
		executionsExpected := uint64(2)
		if pep.sc.executions != executionsExpected {
			t.Errorf("Should have executed %d, got %d instead for replica %d", executionsExpected, pep.sc.executions, i)
		}
	}
}

// Test for issue #1119
func TestSendQueueThrottling(t *testing.T) {
	prePreparesSent := 0

	mock := &omniProto{}
	instance := newPbftCore(0, loadConfig(), mock)
	instance.f = 1
	instance.K = 2
	instance.L = 4
	instance.consumer = &omniProto{
		validateImpl: func(p []byte) error { return nil },
		broadcastImpl: func(p []byte) {
			prePreparesSent++
		},
	}
	defer instance.close()

	for j := 0; j < 4; j++ {
		sendEvent(instance, &Request{
			Timestamp: &gp.Timestamp{Seconds: int64(j), Nanos: 0},
			Payload:   []byte(fmt.Sprintf("%d", j)),
		})
	}

	expected := 2
	if prePreparesSent != expected {
		t.Fatalf("Expected to send only %d pre-prepares, but got %d messages", expected, prePreparesSent)
	}
}

// From issue #687
func TestWitnessCheckpointOutOfBounds(t *testing.T) {
	mock := &omniProto{}
	instance := newPbftCore(1, loadConfig(), mock)
	instance.f = 1
	instance.K = 2
	instance.L = 4
	defer instance.close()

	sendEvent(instance, &Checkpoint{
		SequenceNumber: 6,
		ReplicaId:      0,
	})

	instance.moveWatermarks(6)

	// This causes the list of high checkpoints to grow to be f+1
	// even though there are not f+1 checkpoints witnessed outside our range
	// historically, this caused an index out of bounds error
	sendEvent(instance, &Checkpoint{
		SequenceNumber: 10,
		ReplicaId:      3,
	})
}

// From issue #687
func TestWitnessFallBehindMissingPrePrepare(t *testing.T) {
	mock := &omniProto{}
	instance := newPbftCore(1, loadConfig(), mock)
	instance.f = 1
	instance.K = 2
	instance.L = 4
	defer instance.close()

	sendEvent(instance, &Commit{
		SequenceNumber: 2,
		ReplicaId:      0,
	})

	// Historically, the lack of prePrepare associated with the commit would cause
	// a nil pointer reference
	instance.moveWatermarks(6)
}

func TestFallBehind(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", 2)
	config.Set("general.logmultiplier", 2)
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	execReq := func(iter int64, skipThree bool) {
		// Create a message of type `Message_CHAIN_TRANSACTION`
		txTime := &gp.Timestamp{Seconds: iter, Nanos: 0}
		tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
		txPacked, err := proto.Marshal(tx)
		if err != nil {
			t.Fatalf("Failed to marshal TX block: %s", err)
		}

		msg := &Message{&Message_Request{&Request{Payload: txPacked, ReplicaId: uint64(generateBroadcaster(validatorCount))}}}

		net.pbftEndpoints[0].pbft.manager.queue() <- pbftMessageEvent{msg: msg, sender: msg.GetRequest().ReplicaId}

		if skipThree {
			// Send the request for consensus to everone but replica 3
			net.filterFn = func(src, replica int, msg []byte) []byte {
				if src != -1 && replica == 3 {
					return nil
				}

				return msg
			}
		} else {
			// Send the request for consensus to everone
			net.filterFn = nil
		}
		err = net.process()

		if err != nil {
			t.Fatalf("Processing failed: %s", err)
		}
	}

	pep := net.pbftEndpoints[3]
	pbft := pep.pbft
	// Send enough requests to get to a checkpoint quorum certificate with sequence number L+K
	execReq(1, true)
	for request := int64(2); uint64(request) <= pbft.L+pbft.K; request++ {
		execReq(request, false)
	}

	if !pbft.skipInProgress {
		t.Fatalf("Replica did not detect that it has fallen behind.")
	}

	if len(pbft.chkpts) != 0 {
		t.Fatalf("Expected no checkpoints, found %d", len(pbft.chkpts))
	}

	if pbft.h != pbft.L+pbft.K {
		t.Fatalf("Expected low water mark to be %d, got %d", pbft.L+pbft.K, pbft.h)
	}

	// Send enough requests to get to a weak checkpoint certificate certain with sequence number L+K*2
	for request := int64(pbft.L + pbft.K + 1); uint64(request) <= pbft.L+pbft.K*2; request++ {
		execReq(request, false)
	}

	if !pep.sc.skipOccurred {
		t.Fatalf("Request failed to kick off state transfer")
	}

	execReq(int64(pbft.L+pbft.K*2+1), false)

	if pep.sc.executions < pbft.L+pbft.K*2 {
		t.Fatalf("Replica did not perform state transfer")
	}

	// XXX currently disabled, need to resync view# during/after state transfer
	// if pep.sc.executions != pbft.L+pbft.K*2+1 {
	// 	t.Fatalf("Replica did not begin participating normally after state transfer completed")
	//}
}

func TestPbftF0(t *testing.T) {
	net := makePBFTNetwork(1, nil)
	defer net.stop()

	req := createPbftRequestWithChainTx(1, 0)

	pep0 := net.pbftEndpoints[0]

	pep0.pbft.manager.queue() <- req

	err := net.process()
	if err != nil {
		t.Fatalf("Processing failed: %s", err)
	}

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions < 1 {
			t.Errorf("Instance %d did not execute transaction", pep.id)
			continue
		}
		if pep.sc.executions >= 2 {
			t.Errorf("Instance %d executed more than one transaction", pep.id)
			continue
		}
		if !reflect.DeepEqual(pep.sc.lastExecution, req.Payload) {
			t.Errorf("Instance %d executed wrong transaction, %x should be %x",
				pep.id, pep.sc.lastExecution, req.Payload)
		}
	}
}

// Make sure the request timer doesn't inflate the view timeout by firing during view change
func TestRequestTimerDuringViewChange(t *testing.T) {
	mock := &omniProto{
		validateImpl: func(txRaw []byte) error { return nil },
		signImpl:     func(msg []byte) ([]byte, error) { return msg, nil },
		verifyImpl:   func(senderID uint64, signature []byte, message []byte) error { return nil },
		broadcastImpl: func(msg []byte) {
			t.Errorf("Should not send the view change message during a view change")
		},
	}
	instance := newPbftCore(1, loadConfig(), mock)
	instance.manager.start()
	instance.f = 1
	instance.K = 2
	instance.L = 4
	instance.requestTimeout = time.Millisecond
	instance.activeView = false
	defer instance.close()

	txTime := &gp.Timestamp{Seconds: 1, Nanos: 0}
	tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
	txPacked, _ := proto.Marshal(tx)

	req := &Request{
		Timestamp: &gp.Timestamp{Seconds: 1, Nanos: 0},
		Payload:   txPacked,
		ReplicaId: 1, // Not the primary
	}

	instance.manager.queue() <- req

	time.Sleep(100 * time.Millisecond)
}

// TestReplicaCrash1 simulates the restart of replicas 0 and 1 after
// some state has been built (one request executed).  At the time of
// the restart, replica 0 is also the primary.  All three requests
// submitted should also be executed on all replicas.
func TestReplicaCrash1(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", 2)
	config.Set("general.logmultiplier", 2)
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	mkreq := func(n int64) *Request {
		txTime := &gp.Timestamp{Seconds: n, Nanos: 0}
		tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
		txPacked, _ := proto.Marshal(tx)

		return &Request{
			Timestamp: &gp.Timestamp{Seconds: n, Nanos: 0},
			Payload:   txPacked,
			ReplicaId: uint64(generateBroadcaster(validatorCount)),
		}
	}

	net.pbftEndpoints[0].pbft.manager.queue() <- mkreq(1)
	net.process()

	for id := 0; id < 2; id++ {
		pe := net.pbftEndpoints[id]
		pe.pbft = newPbftCore(uint64(id), loadConfig(), pe.sc)
		pe.pbft.manager.start()
		pe.pbft.N = 4
		pe.pbft.f = (4 - 1) / 3
		pe.pbft.K = 2
		pe.pbft.L = 2 * pe.pbft.K
	}

	net.pbftEndpoints[0].pbft.manager.queue() <- mkreq(2)
	net.pbftEndpoints[0].pbft.manager.queue() <- (mkreq(3))
	net.process()

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions != 3 {
			t.Errorf("Expected 3 executions on replica %d, got %d", pep.id, pep.sc.executions)
			continue
		}

		if pep.pbft.view != 0 {
			t.Errorf("Replica %d should still be in view 0, is %v %d", pep.id, pep.pbft.activeView, pep.pbft.view)
		}
	}
}

// TestReplicaCrash2 is a misnomer.  It simulates a situation where
// one replica (#3) is byzantine and does not participate at all.
// Additionally, for view<2 and seqno=1, the network drops commit
// messages to all but replica 1.
func TestReplicaCrash2(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.timeout.request", "800ms")
	config.Set("general.timeout.viewchange", "800ms")
	config.Set("general.K", 2)
	config.Set("general.logmultiplier", 2)
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	filterMsg := true
	net.filterFn = func(src int, dst int, msg []byte) []byte {
		if dst == 3 { // 3 is byz
			return nil
		}
		pm := &Message{}
		err := proto.Unmarshal(msg, pm)
		if err != nil {
			t.Fatal(err)
		}
		// filter commits to all but 1
		commit := pm.GetCommit()
		if filterMsg && dst != -1 && dst != 1 && commit != nil && commit.View < 2 {
			logger.Info("filtering commit message from %d to %d", src, dst)
			return nil
		}
		return msg
	}

	mkreq := func(n int64) *Request {
		txTime := &gp.Timestamp{Seconds: n, Nanos: 0}
		tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
		txPacked, _ := proto.Marshal(tx)

		return &Request{
			Timestamp: &gp.Timestamp{Seconds: n, Nanos: 0},
			Payload:   txPacked,
			ReplicaId: uint64(generateBroadcaster(validatorCount)),
		}
	}

	net.pbftEndpoints[0].pbft.manager.queue() <- (mkreq(1))
	net.process()

	logger.Info("stopping filtering")
	filterMsg = false
	primary := net.pbftEndpoints[0].pbft.primary(net.pbftEndpoints[0].pbft.view)
	net.pbftEndpoints[primary].pbft.manager.queue() <- (mkreq(2))
	net.pbftEndpoints[primary].pbft.manager.queue() <- (mkreq(3))
	net.pbftEndpoints[primary].pbft.manager.queue() <- (mkreq(4))
	go net.processContinually()
	time.Sleep(5 * time.Second)

	for _, pep := range net.pbftEndpoints {
		if pep.id != 3 && pep.sc.executions != 4 {
			t.Errorf("Expected 4 executions on replica %d, got %d", pep.id, pep.sc.executions)
			continue
		}
		if pep.id == 3 && pep.sc.executions > 0 {
			t.Errorf("Expected no execution")
			continue
		}
	}
}

// TestReplicaCrash3 simulates the restart requiring a view change
// to a checkpoint which was restored from the persistence state
// Replicas 0,1,2 participate up to a checkpoint, then all crash
// Then replicas 0,1,3 start back up, and a view change must be
// triggered to get vp3 up to speed
func TestReplicaCrash3(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", 2)
	config.Set("general.logmultiplier", 2)
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	twoOffline := false
	threeOffline := true
	net.filterFn = func(src int, dst int, msg []byte) []byte {
		if twoOffline && dst == 2 { // 2 is 'offline'
			return nil
		}
		if threeOffline && dst == 3 { // 3 is 'offline'
			return nil
		}
		return msg
	}

	mkreq := func(n int64) *Request {
		txTime := &gp.Timestamp{Seconds: n, Nanos: 0}
		tx := &pb.Transaction{Type: pb.Transaction_CHAINCODE_DEPLOY, Timestamp: txTime}
		txPacked, _ := proto.Marshal(tx)

		return &Request{
			Timestamp: &gp.Timestamp{Seconds: n, Nanos: 0},
			Payload:   txPacked,
			ReplicaId: uint64(generateBroadcaster(validatorCount)),
		}
	}

	for i := int64(1); i <= 8; i++ {
		net.pbftEndpoints[0].pbft.manager.queue() <- (mkreq(i))
	}
	net.process() // vp0,1,2 should have a stable checkpoint for seqNo 8

	// Create new pbft instances to restore from persistence
	for id := 0; id < 2; id++ {
		pe := net.pbftEndpoints[id]
		config := loadConfig()
		config.Set("general.K", "2")
		pe.pbft = newPbftCore(uint64(id), config, pe.sc)
		pe.pbft.manager.start()
		pe.pbft.N = 4
		pe.pbft.f = (4 - 1) / 3
		pe.pbft.requestTimeout = 200 * time.Millisecond
	}

	threeOffline = false
	twoOffline = true

	// Because vp2 is 'offline', and vp3 is still at the genesis block, the network needs to make a view change

	net.pbftEndpoints[0].pbft.manager.queue() <- (mkreq(9))
	net.process()

	// Now vp0,1,3 should be in sync with 9 executions in view 1, and vp2 should be at 8 executions in view 0
	for i, pep := range net.pbftEndpoints {

		if i == 2 {
			// 2 is 'offline'
			if pep.pbft.view != 0 {
				t.Errorf("Expected replica %d to be in view 0, got %d", pep.id, pep.pbft.view)
			}
			expectedExecutions := uint64(8)
			if pep.sc.executions != expectedExecutions {
				t.Errorf("Expected %d executions on replica %d, got %d", expectedExecutions, pep.id, pep.sc.executions)
			}
			continue
		}

		if pep.pbft.view != 1 {
			t.Errorf("Expected replica %d to be in view 1, got %d", pep.id, pep.pbft.view)
		}

		expectedExecutions := uint64(9)
		if pep.sc.executions != expectedExecutions {
			t.Errorf("Expected %d executions on replica %d, got %d", expectedExecutions, pep.id, pep.sc.executions)
		}
	}
}

func TestReplicaPersistQSet(t *testing.T) {
	persist := make(map[string][]byte)

	stack := &omniProto{
		validateImpl: func(b []byte) error {
			return nil
		},
		broadcastImpl: func(msg []byte) {
		},
		StoreStateImpl: func(key string, value []byte) error {
			persist[key] = value
			return nil
		},
		DelStateImpl: func(key string) {
			delete(persist, key)
		},
		ReadStateImpl: func(key string) ([]byte, error) {
			if val, ok := persist[key]; ok {
				return val, nil
			}
			return nil, fmt.Errorf("key not found")
		},
		ReadStateSetImpl: func(prefix string) (map[string][]byte, error) {
			r := make(map[string][]byte)
			for k, v := range persist {
				if len(k) >= len(prefix) && k[0:len(prefix)] == prefix {
					r[k] = v
				}
			}
			return r, nil
		},
	}
	p := newPbftCore(1, loadConfig(), stack)
	req := &Request{
		Timestamp: &gp.Timestamp{Seconds: 1, Nanos: 0},
		Payload:   []byte("foo"),
		ReplicaId: uint64(0),
	}
	sendEvent(p, &PrePrepare{
		View:           0,
		SequenceNumber: 1,
		RequestDigest:  hashReq(req),
		Request:        req,
		ReplicaId:      uint64(0),
	})
	p.close()

	p = newPbftCore(1, loadConfig(), stack)
	if !p.prePrepared(hashReq(req), 0, 1) {
		t.Errorf("did not restore qset properly")
	}
}

func TestReplicaPersistDelete(t *testing.T) {
	persist := make(map[string][]byte)

	stack := &omniProto{
		StoreStateImpl: func(key string, value []byte) error {
			persist[key] = value
			return nil
		},
		DelStateImpl: func(key string) {
			delete(persist, key)
		},
	}
	p := newPbftCore(1, loadConfig(), stack)
	p.reqStore["a"] = &Request{}
	p.persistRequest("a")
	if len(persist) != 1 {
		t.Error("expected one persisted entry")
	}
	p.persistDelRequest("a")
	if len(persist) != 0 {
		t.Error("expected no persisted entry")
	}
}

func TestNilCurrentExec(t *testing.T) {
	p := newPbftCore(1, loadConfig(), &omniProto{})
	p.execDoneSync() // Per issue 1538, this would cause a Nil pointer dereference
}

func TestNetworkNullRequests(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.timeout.nullrequest", "200ms")
	config.Set("general.timeout.request", "500ms")
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	msg := createPbftRequestWithChainTx(1, 0)
	net.pbftEndpoints[0].pbft.manager.queue() <- msg

	go net.processContinually()
	time.Sleep(2 * time.Second)

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions != 1 {
			t.Errorf("Instance %d executed incorrect number of transactions: %d", pep.id, pep.sc.executions)
		}
		if pep.pbft.lastExec <= 1 {
			t.Errorf("Instance %d: no null requests processed", pep.id)
		}
		if pep.pbft.view != 0 {
			t.Errorf("Instance %d: expected view=0", pep.id)
		}
	}
}

func TestNetworkNullRequestMissing(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.timeout.nullrequest", "200ms")
	config.Set("general.timeout.request", "500ms")
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	net.pbftEndpoints[0].pbft.nullRequestTimeout = 0

	msg := createPbftRequestWithChainTx(1, 0)
	net.pbftEndpoints[0].pbft.manager.queue() <- msg

	go net.processContinually()
	time.Sleep(3 * time.Second) // Bumped from 2 to 3 seconds because of sporadic CI failures

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions != 1 {
			t.Errorf("Instance %d executed incorrect number of transactions: %d", pep.id, pep.sc.executions)
		}
		if pep.pbft.lastExec <= 1 {
			t.Errorf("Instance %d: no null requests processed", pep.id)
		}
		if pep.pbft.view != 1 {
			t.Errorf("Instance %d: expected view=1", pep.id)
		}
	}
}

func TestNetworkPeriodicViewChange(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", "2")
	config.Set("general.logmultiplier", "2")
	config.Set("general.timeout.request", "500ms")
	config.Set("general.viewchangeperiod", "1")
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	for n := 1; n < 6; n++ {
		msg := createPbftRequestWithChainTx(int64(n), 0)
		for _, pe := range net.pbftEndpoints {
			pe.pbft.manager.queue() <- msg
		}
		net.process()
	}

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions != 5 {
			t.Errorf("Instance %d executed incorrect number of transactions: %d", pep.id, pep.sc.executions)
		}
		if pep.pbft.view != 2 {
			t.Errorf("Instance %d: expected view=2", pep.id)
		}
	}
}

func TestNetworkPeriodicViewChangeMissing(t *testing.T) {
	validatorCount := 4
	config := loadConfig()
	config.Set("general.K", "2")
	config.Set("general.logmultiplier", "2")
	config.Set("general.timeout.request", "500ms")
	config.Set("general.viewchangeperiod", "1")
	net := makePBFTNetwork(validatorCount, config)
	defer net.stop()

	net.pbftEndpoints[0].pbft.viewChangePeriod = 0
	net.pbftEndpoints[0].pbft.viewChangeSeqNo = ^uint64(0)

	for n := 1; n < 3; n++ {
		msg := createPbftRequestWithChainTx(int64(n), 0)
		for _, pe := range net.pbftEndpoints {
			pe.pbft.manager.queue() <- msg
		}
		net.process()
	}

	for _, pep := range net.pbftEndpoints {
		if pep.sc.executions != 2 {
			t.Errorf("Instance %d executed incorrect number of transactions: %d", pep.id, pep.sc.executions)
		}
		if pep.pbft.view != 1 {
			t.Errorf("Instance %d: expected view=1", pep.id)
		}
	}
}
