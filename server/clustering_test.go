// Copyright 2017 Apcera Inc. All rights reserved.

package server

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	natsdTest "github.com/nats-io/gnatsd/test"
	"github.com/nats-io/go-nats-streaming"
	"github.com/nats-io/go-nats-streaming/pb"
	"github.com/nats-io/nats-streaming-server/stores"
)

var defaultRaftLog string

func init() {
	tmpDir, err := ioutil.TempDir("", "raft_logs_")
	if err != nil {
		panic("Could not create tmp dir")
	}
	if err := os.Remove(tmpDir); err != nil {
		panic(fmt.Errorf("Error removing temp dir: %v", err))
	}
	defaultRaftLog = tmpDir
}

func cleanupRaftLog(t *testing.T) {
	if err := os.RemoveAll(defaultRaftLog); err != nil {
		stackFatalf(t, "Error cleaning up raft log: %v", err)
	}
}

func getTestDefaultOptsForClustering(id string, peers []string) *Options {
	opts := GetDefaultOptions()
	opts.StoreType = stores.TypeFile
	opts.FilestoreDir = filepath.Join(defaultDataStore, id)
	opts.FileStoreOpts.BufferSize = 1024
	opts.ClusterPeers = peers
	opts.ClusterNodeID = id
	opts.RaftLogPath = filepath.Join(defaultRaftLog, id)
	opts.LogCacheSize = DefaultLogCacheSize
	opts.LogSnapshots = 1
	opts.NATSServerURL = "nats://localhost:4222"
	return opts
}

func getChannelLeader(t *testing.T, channel string, timeout time.Duration, servers ...*StanServer) *StanServer {
	var (
		leader   *StanServer
		deadline = time.Now().Add(timeout)
	)
	for time.Now().Before(deadline) {
		for i := 0; i < len(servers); i++ {
			s := servers[i]
			if s.state == Shutdown {
				continue
			}
			c := s.channels.get(channel)
			if c == nil || c.raft == nil {
				continue
			}
			if c.isLeader() {
				if leader != nil {
					stackFatalf(t, "Found more than one channel leader")
				}
				leader = s
			}
		}
		if leader != nil {
			break
		}
	}
	if leader == nil {
		stackFatalf(t, "Unable to find the channel leader")
	}
	return leader
}

func verifyNoLeader(t *testing.T, channel string, timeout time.Duration, servers ...*StanServer) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, server := range servers {
			c := server.channels.get(channel)
			if c == nil || c.raft == nil {
				continue
			}
			if c.isLeader() {
				time.Sleep(100 * time.Millisecond)
				break
			}
		}
		return
	}
	stackFatalf(t, "Found unexpected leader for channel %s", channel)
}

type msg struct {
	sequence uint64
	data     []byte
}

func verifyChannelConsistency(t *testing.T, channel string, timeout time.Duration,
	expectedFirstSeq, expectedLastSeq uint64, expectedMsgs []msg, servers ...*StanServer) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
	INNER:
		for _, server := range servers {
			store := server.channels.get(channel).store.Msgs
			first, last, err := store.FirstAndLastSequence()
			if err != nil {
				stackFatalf(t, "Error getting sequence numbers: %v", err)
			}
			if first != expectedFirstSeq {
				time.Sleep(100 * time.Millisecond)
				break INNER
			}
			if last != expectedLastSeq {
				time.Sleep(100 * time.Millisecond)
				break INNER
			}
			for i := first; i <= last; i++ {
				msg, err := store.Lookup(i)
				if err != nil {
					t.Fatalf("Error getting message %d: %v", i, err)
				}
				if !compareMsg(t, *msg, expectedMsgs[i].data, expectedMsgs[i].sequence) {
					time.Sleep(100 * time.Millisecond)
					break INNER
				}
			}
		}
		return
	}
	stackFatalf(t, "Message stores are inconsistent")
}

func removeServer(servers []*StanServer, s *StanServer) []*StanServer {
	for i, srv := range servers {
		if srv == s {
			servers = append(servers[:i], servers[i+1:]...)
		}
	}
	return servers
}

func assertMsg(t *testing.T, msg pb.MsgProto, expectedData []byte, expectedSeq uint64) {
	if msg.Sequence != expectedSeq {
		stackFatalf(t, "Msg sequence incorrect, expected: %d, got: %d", expectedSeq, msg.Sequence)
	}
	if !bytes.Equal(msg.Data, expectedData) {
		stackFatalf(t, "Msg data incorrect, expected: %s, got: %s", expectedData, msg.Data)
	}
}

func compareMsg(t *testing.T, msg pb.MsgProto, expectedData []byte, expectedSeq uint64) bool {
	if !bytes.Equal(msg.Data, expectedData) {
		return false
	}
	return msg.Sequence == expectedSeq
}

func publishWithRetry(t *testing.T, sc stan.Conn, channel string, payload []byte) {
	// TODO: there is a race where connection might not be established on
	// leader so publish can fail, so retry a few times if necessary. Remove
	// this once connection replication is implemented.
	for i := 0; i < 10; i++ {
		if err := sc.Publish(channel, payload); err != nil {
			if i == 9 {
				stackFatalf(t, "Unexpected error on publish: %v", err)
			}
			time.Sleep(time.Millisecond)
			continue
		}
		break
	}
}

func TestClusteringConfig(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	opts := GetDefaultOptions()
	opts.ClusterPeers = []string{"a", "b"}
	s, err := RunServerWithOpts(opts, nil)
	if s != nil || err == nil {
		if s != nil {
			s.Shutdown()
		}
		t.Fatal("Server should have failed to start with cluster peers and no cluster node id")
	}
}

// Ensure basic replication works as expected. This test starts three servers
// in a cluster, publishes messages to the cluster, kills the leader, publishes
// more messages, kills the new leader, verifies progress cannot be made when
// there is no leader, then brings the cluster back online and verifies
// catchup and consistency.
func TestClusteringBasic(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	// For this test, use a central NATS server.
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	// Configure first server
	s1sOpts := getTestDefaultOptsForClustering("a", []string{"b", "c"})
	s1 := runServerWithOpts(t, s1sOpts, nil)
	defer s1.Shutdown()

	// Configure second server.
	s2sOpts := getTestDefaultOptsForClustering("b", []string{"a", "c"})
	s2 := runServerWithOpts(t, s2sOpts, nil)
	defer s2.Shutdown()

	// Configure third server.
	s3sOpts := getTestDefaultOptsForClustering("c", []string{"a", "b"})
	s3 := runServerWithOpts(t, s3sOpts, nil)
	defer s3.Shutdown()

	servers := []*StanServer{s1, s2, s3}
	for _, s := range servers {
		checkState(t, s, Clustered)
	}

	// Create a client connection.
	sc, err := stan.Connect(clusterName, clientName)
	if err != nil {
		t.Fatalf("Expected to connect correctly, got err %v", err)
	}
	defer sc.Close()

	// Publish a message (this will create the channel and form the Raft group).
	channel := "foo"
	publishWithRetry(t, sc, channel, []byte("hello"))

	ch := make(chan *stan.Msg, 100)
	sub, err := sc.Subscribe(channel, func(msg *stan.Msg) {
		ch <- msg
	}, stan.DeliverAllAvailable(), stan.MaxInflight(1))
	if err != nil {
		t.Fatalf("Error subscribing: %v", err)
	}

	select {
	case msg := <-ch:
		assertMsg(t, msg.MsgProto, []byte("hello"), 1)
	case <-time.After(2 * time.Second):
		t.Fatal("expected msg")
	}

	sub.Unsubscribe()

	stopped := []*StanServer{}

	// Take down the leader.
	leader := getChannelLeader(t, channel, 10*time.Second, servers...)
	leader.Shutdown()
	stopped = append(stopped, leader)
	servers = removeServer(servers, leader)

	// Wait for the new leader to be elected.
	leader = getChannelLeader(t, channel, 10*time.Second, servers...)

	// Publish some more messages.
	for i := 0; i < 5; i++ {
		if err := sc.Publish(channel, []byte(strconv.Itoa(i))); err != nil {
			t.Fatalf("Unexpected error on publish %d: %v", i, err)
		}
	}

	// Read everything back from the channel.
	sub, err = sc.Subscribe(channel, func(msg *stan.Msg) {
		ch <- msg
	}, stan.DeliverAllAvailable(), stan.MaxInflight(1))
	if err != nil {
		t.Fatalf("Error subscribing: %v", err)
	}
	select {
	case msg := <-ch:
		assertMsg(t, msg.MsgProto, []byte("hello"), 1)
	case <-time.After(2 * time.Second):
		t.Fatal("expected msg")
	}
	for i := 0; i < 5; i++ {
		select {
		case msg := <-ch:
			assertMsg(t, msg.MsgProto, []byte(strconv.Itoa(i)), uint64(i+2))
		case <-time.After(2 * time.Second):
			t.Fatal("expected msg")
		}
	}

	sub.Unsubscribe()

	// Take down the leader.
	leader.Shutdown()
	stopped = append(stopped, leader)
	servers = removeServer(servers, leader)

	// Create a new connection with lower timeouts to test when we don't have a leader.
	sc2, err := stan.Connect(clusterName, clientName+"-2", stan.PubAckWait(time.Second), stan.ConnectWait(time.Second))
	if err != nil {
		t.Fatalf("Expected to connect correctly, got err %v", err)
	}
	defer sc2.Close()

	// New subscriptions should be rejected since no leader can be established.
	_, err = sc2.Subscribe(channel, func(msg *stan.Msg) {}, stan.DeliverAllAvailable(), stan.MaxInflight(1))
	if err == nil {
		t.Fatal("Expected error on subscribe")
	}

	// Publishes should also fail.
	if err := sc2.Publish(channel, []byte("foo")); err == nil {
		t.Fatal("Expected error on publish")
	}

	// Bring one node back up.
	s := stopped[0]
	stopped = stopped[1:]
	s = runServerWithOpts(t, s.opts, nil)
	servers = append(servers, s)
	defer s.Shutdown()

	// Wait for the new leader to be elected.
	getChannelLeader(t, channel, 10*time.Second, servers...)

	// Publish some more messages.
	for i := 0; i < 5; i++ {
		if err := sc.Publish(channel, []byte("foo-"+strconv.Itoa(i))); err != nil {
			t.Fatalf("Unexpected error on publish %d: %v", i, err)
		}
	}

	// Bring the last node back up.
	s = stopped[0]
	s = runServerWithOpts(t, s.opts, nil)
	servers = append(servers, s)
	defer s.Shutdown()

	// Ensure there is still a leader.
	getChannelLeader(t, channel, 10*time.Second, servers...)

	// Publish one more message.
	if err := sc.Publish(channel, []byte("goodbye")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}

	// Verify the server stores are consistent.
	expected := make([]msg, 12)
	expected[0] = msg{sequence: 1, data: []byte("hello")}
	for i := 0; i < 5; i++ {
		expected[i+1] = msg{sequence: uint64(i + 2), data: []byte(strconv.Itoa(i))}
	}
	for i := 0; i < 5; i++ {
		expected[i+6] = msg{sequence: uint64(i + 7), data: []byte("foo-" + strconv.Itoa(i))}
	}
	expected[11] = msg{sequence: 12, data: []byte("goodbye")}
	verifyChannelConsistency(t, channel, 10*time.Second, 1, 12, expected, servers...)
}

func TestClusteringNoPanicOnShutdown(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	// For this test, use a central NATS server.
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	// Configure first server
	s1sOpts := getTestDefaultOptsForClustering("a", []string{"b"})
	s1 := runServerWithOpts(t, s1sOpts, nil)
	defer s1.Shutdown()

	// Configure second server.
	s2sOpts := getTestDefaultOptsForClustering("b", []string{"a"})
	s2 := runServerWithOpts(t, s2sOpts, nil)
	defer s2.Shutdown()

	servers := []*StanServer{s1, s2}

	sc, err := stan.Connect(clusterName, clientName, stan.PubAckWait(time.Second), stan.ConnectWait(10*time.Second))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sc.Close()

	sub, err := sc.Subscribe("foo", func(_ *stan.Msg) {})
	if err != nil {
		t.Fatalf("Unexpected error on subscribe: %v", err)
	}

	leader := getChannelLeader(t, "foo", 10*time.Second, servers...)

	// Unsubscribe since this is not about that
	sub.Unsubscribe()

	wg := sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()

		for {
			if err := sc.Publish("foo", []byte("msg")); err != nil {
				return
			}
		}
	}()

	// Wait so that go-routine is in middle of sending messages
	time.Sleep(time.Duration(rand.Intn(500)+100) * time.Millisecond)

	// We shutdown the follower, it should not panic.
	if s1 == leader {
		s2.Shutdown()
	} else {
		s1.Shutdown()
	}
	wg.Wait()
}

func TestClusteringLeaderFlap(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	// For this test, use a central NATS server.
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	// Configure first server
	s1sOpts := getTestDefaultOptsForClustering("a", []string{"b"})
	s1 := runServerWithOpts(t, s1sOpts, nil)
	defer s1.Shutdown()

	// Configure second server.
	s2sOpts := getTestDefaultOptsForClustering("b", []string{"a"})
	s2 := runServerWithOpts(t, s2sOpts, nil)
	defer s2.Shutdown()

	servers := []*StanServer{s1, s2}

	sc, err := stan.Connect(clusterName, clientName, stan.PubAckWait(2*time.Second))
	if err != nil {
		t.Fatalf("Error on connect: %v", err)
	}
	defer sc.Close()

	// Publish a message (this will create the channel and form the Raft group).
	channel := "foo"
	publishWithRetry(t, sc, channel, []byte("hello"))

	// Wait for leader to be elected.
	leader := getChannelLeader(t, channel, 10*time.Second, servers...)

	// Kill the follower.
	var follower *StanServer
	if s1 == leader {
		s2.Shutdown()
		follower = s2
	} else {
		s1.Shutdown()
		follower = s1
	}

	// Ensure there is no leader now.
	verifyNoLeader(t, channel, 5*time.Second, s1, s2)

	// Bring the follower back up.
	follower = runServerWithOpts(t, follower.opts, nil)
	servers = []*StanServer{leader, follower}
	defer follower.Shutdown()

	// Ensure there is a new leader.
	getChannelLeader(t, channel, 10*time.Second, servers...)
}

func TestClusteringLogSnapshotCatchup(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	// For this test, use a central NATS server.
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	// Configure first server
	s1sOpts := getTestDefaultOptsForClustering("a", []string{"b", "c"})
	s1sOpts.TrailingLogs = 0
	s1 := runServerWithOpts(t, s1sOpts, nil)
	defer s1.Shutdown()

	// Configure second server.
	s2sOpts := getTestDefaultOptsForClustering("b", []string{"a", "c"})
	s2sOpts.TrailingLogs = 0
	s2 := runServerWithOpts(t, s2sOpts, nil)
	defer s2.Shutdown()

	// Configure third server.
	s3sOpts := getTestDefaultOptsForClustering("c", []string{"a", "b"})
	s3sOpts.TrailingLogs = 0
	s3 := runServerWithOpts(t, s3sOpts, nil)
	defer s3.Shutdown()

	servers := []*StanServer{s1, s2, s3}
	for _, s := range servers {
		checkState(t, s, Clustered)
	}

	// Create a client connection.
	sc, err := stan.Connect(clusterName, clientName)
	if err != nil {
		t.Fatalf("Expected to connect correctly, got err %v", err)
	}
	defer sc.Close()

	// Publish a message (this will create the channel and form the Raft group).
	channel := "foo"
	publishWithRetry(t, sc, channel, []byte("hello"))

	// Publish some more messages.
	for i := 0; i < 5; i++ {
		if err := sc.Publish(channel, []byte(strconv.Itoa(i+1))); err != nil {
			t.Fatalf("Unexpected error on publish: %v", err)
		}
	}

	// Wait for leader to be elected.
	leader := getChannelLeader(t, channel, 10*time.Second, servers...)

	// Kill a follower.
	var follower *StanServer
	for _, s := range servers {
		if leader != s {
			follower = s
			break
		}
	}
	follower.Shutdown()

	// Publish some more messages.
	for i := 0; i < 5; i++ {
		if err := sc.Publish(channel, []byte(strconv.Itoa(i+6))); err != nil {
			t.Fatalf("Unexpected error on publish: %v", err)
		}
	}

	// Force a log compaction on the leader.
	if err := leader.channels.get(channel).raft.Snapshot().Error(); err != nil {
		t.Fatalf("Unexpected error on snapshot: %v", err)
	}

	// Bring the follower back up.
	follower = runServerWithOpts(t, follower.opts, nil)
	defer follower.Shutdown()
	for i, server := range servers {
		if server.opts.ClusterNodeID == follower.opts.ClusterNodeID {
			servers[i] = follower
			break
		}
	}

	// Ensure there is a leader before publishing.
	getChannelLeader(t, channel, 10*time.Second, servers...)

	// Publish a message to force a timely catch up.
	if err := sc.Publish(channel, []byte("11")); err != nil {
		t.Fatalf("Unexpected error on publish: %v", err)
	}

	// Verify the server stores are consistent.
	expected := make([]msg, 12)
	expected[0] = msg{sequence: 1, data: []byte("hello")}
	for i := 2; i < 13; i++ {
		expected[i-1] = msg{sequence: uint64(i), data: []byte(strconv.Itoa(i))}
	}
	verifyChannelConsistency(t, channel, 10*time.Second, 1, 11, expected, servers...)
}

// Ensures subscriptions are replicated such that when a leader fails over, the
// subscription continues to deliver messages.
func TestClusteringSubscriberFailover(t *testing.T) {
	var (
		channel  = "foo"
		queue    = "queue"
		sc1, sc2 stan.Conn
		err      error
		ch       = make(chan *stan.Msg, 100)
		cb       = func(msg *stan.Msg) { ch <- msg }
	)
	testCases := []struct {
		name      string
		subscribe func() error
	}{
		{
			"normal",
			func() error {
				_, err := sc1.Subscribe(channel, cb,
					stan.DeliverAllAvailable(),
					stan.MaxInflight(1),
					stan.AckWait(2*time.Second))
				return err
			},
		},
		{
			"durable",
			func() error {
				_, err := sc1.Subscribe(channel, cb,
					stan.DeliverAllAvailable(),
					stan.DurableName("durable"),
					stan.MaxInflight(1),
					stan.AckWait(2*time.Second))
				return err
			},
		},
		{
			"queue",
			func() error {
				_, err := sc1.QueueSubscribe(channel, queue, cb,
					stan.DeliverAllAvailable(),
					stan.MaxInflight(1),
					stan.AckWait(2*time.Second))
				if err != nil {
					return err
				}
				_, err = sc2.QueueSubscribe(channel, queue, cb,
					stan.DeliverAllAvailable(),
					stan.MaxInflight(1),
					stan.AckWait(2*time.Second))
				return err
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			cleanupDatastore(t)
			defer cleanupDatastore(t)
			cleanupRaftLog(t)
			defer cleanupRaftLog(t)

			// For this test, use a central NATS server.
			ns := natsdTest.RunDefaultServer()
			defer ns.Shutdown()

			// Configure first server
			s1sOpts := getTestDefaultOptsForClustering("a", []string{"b", "c"})
			s1 := runServerWithOpts(t, s1sOpts, nil)
			defer s1.Shutdown()

			// Configure second server.
			s2sOpts := getTestDefaultOptsForClustering("b", []string{"a", "c"})
			s2 := runServerWithOpts(t, s2sOpts, nil)
			defer s2.Shutdown()

			// Configure third server.
			s3sOpts := getTestDefaultOptsForClustering("c", []string{"a", "b"})
			s3 := runServerWithOpts(t, s3sOpts, nil)
			defer s3.Shutdown()

			servers := []*StanServer{s1, s2, s3}
			for _, s := range servers {
				checkState(t, s, Clustered)
			}

			// Create client connections.
			sc1, err = stan.Connect(clusterName, clientName)
			if err != nil {
				t.Fatalf("Expected to connect correctly, got err %v", err)
			}
			defer sc1.Close()
			sc2, err = stan.Connect(clusterName, clientName+"-2")
			if err != nil {
				t.Fatalf("Expected to connect correctly, got err %v", err)
			}
			defer sc2.Close()

			// Publish a message (this will create the channel and form the Raft group).
			publishWithRetry(t, sc1, channel, []byte("hello"))

			if err := tc.subscribe(); err != nil {
				t.Fatalf("Error subscribing: %v", err)
			}

			select {
			case msg := <-ch:
				assertMsg(t, msg.MsgProto, []byte("hello"), 1)
			case <-time.After(2 * time.Second):
				t.Fatal("expected msg")
			}

			// Take down the leader.
			leader := getChannelLeader(t, channel, 10*time.Second, servers...)
			leader.Shutdown()
			servers = removeServer(servers, leader)

			// Wait for the new leader to be elected.
			getChannelLeader(t, channel, 10*time.Second, servers...)

			// Publish some more messages.
			for i := 0; i < 5; i++ {
				if err := sc1.Publish(channel, []byte(strconv.Itoa(i))); err != nil {
					t.Fatalf("Unexpected error on publish %d: %v", i, err)
				}
			}

			// We will receive the first message again because acks are not being
			// replicated yet. TODO: remove this once acks are replicated.
			select {
			case msg := <-ch:
				assertMsg(t, msg.MsgProto, []byte("hello"), 1)
			case <-time.After(2 * time.Second):
				t.Fatal("expected msg")
			}

			// Ensure we received the new messages.
			for i := 0; i < 5; i++ {
				select {
				case msg := <-ch:
					assertMsg(t, msg.MsgProto, []byte(strconv.Itoa(i)), uint64(i+2))
				case <-time.After(2 * time.Second):
					t.Fatal("expected msg")
				}
			}
		})
	}
}

// Ensures durable subscription updates are replicated (i.e. closing/reopening
// subscription).
func TestClusteringUpdateDurableSubscriber(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	// For this test, use a central NATS server.
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	// Configure first server
	s1sOpts := getTestDefaultOptsForClustering("a", []string{"b", "c"})
	s1 := runServerWithOpts(t, s1sOpts, nil)
	defer s1.Shutdown()

	// Configure second server.
	s2sOpts := getTestDefaultOptsForClustering("b", []string{"a", "c"})
	s2 := runServerWithOpts(t, s2sOpts, nil)
	defer s2.Shutdown()

	// Configure third server.
	s3sOpts := getTestDefaultOptsForClustering("c", []string{"a", "b"})
	s3 := runServerWithOpts(t, s3sOpts, nil)
	defer s3.Shutdown()

	servers := []*StanServer{s1, s2, s3}
	for _, s := range servers {
		checkState(t, s, Clustered)
	}

	// Create a client connection.
	sc, err := stan.Connect(clusterName, clientName)
	if err != nil {
		t.Fatalf("Expected to connect correctly, got err %v", err)
	}
	defer sc.Close()

	// Publish a message (this will create the channel and form the Raft group).
	channel := "foo"
	publishWithRetry(t, sc, channel, []byte("hello"))

	ch := make(chan *stan.Msg, 100)
	sub, err := sc.Subscribe(channel, func(msg *stan.Msg) {
		ch <- msg
	}, stan.DeliverAllAvailable(), stan.DurableName("durable"), stan.MaxInflight(1), stan.AckWait(2*time.Second))
	if err != nil {
		t.Fatalf("Error subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	select {
	case msg := <-ch:
		assertMsg(t, msg.MsgProto, []byte("hello"), 1)
	case <-time.After(2 * time.Second):
		t.Fatal("expected msg")
	}

	// Close (but don't remove) the subscription.
	if err := sub.Close(); err != nil {
		t.Fatalf("Unexpected error on close: %v", err)
	}

	// Take down the leader.
	leader := getChannelLeader(t, channel, 10*time.Second, servers...)
	leader.Shutdown()
	servers = removeServer(servers, leader)

	// Wait for the new leader to be elected.
	getChannelLeader(t, channel, 10*time.Second, servers...)

	// Publish some more messages.
	for i := 0; i < 5; i++ {
		if err := sc.Publish(channel, []byte(strconv.Itoa(i))); err != nil {
			t.Fatalf("Unexpected error on publish %d: %v", i, err)
		}
	}

	// Reopen subscription.
	sub, err = sc.Subscribe(channel, func(msg *stan.Msg) {
		ch <- msg
	}, stan.DurableName("durable"), stan.MaxInflight(1), stan.AckWait(2*time.Second))
	if err != nil {
		t.Fatalf("Error subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	// We will receive the first message again because acks are not being
	// replicated yet. TODO: remove this once acks are replicated.
	select {
	case msg := <-ch:
		assertMsg(t, msg.MsgProto, []byte("hello"), 1)
	case <-time.After(2 * time.Second):
		t.Fatal("expected msg")
	}

	// Ensure we received the new messages.
	for i := 0; i < 5; i++ {
		select {
		case msg := <-ch:
			assertMsg(t, msg.MsgProto, []byte(strconv.Itoa(i)), uint64(i+2))
		case <-time.After(2 * time.Second):
			t.Fatal("expected msg")
		}
	}
}

// Ensure unsubscribes are replicated such that when a leader fails over, the
// subscription does not continue delivering messages.
func TestClusteringReplicateUnsubscribe(t *testing.T) {
	cleanupDatastore(t)
	defer cleanupDatastore(t)
	cleanupRaftLog(t)
	defer cleanupRaftLog(t)

	// For this test, use a central NATS server.
	ns := natsdTest.RunDefaultServer()
	defer ns.Shutdown()

	// Configure first server
	s1sOpts := getTestDefaultOptsForClustering("a", []string{"b", "c"})
	s1 := runServerWithOpts(t, s1sOpts, nil)
	defer s1.Shutdown()

	// Configure second server.
	s2sOpts := getTestDefaultOptsForClustering("b", []string{"a", "c"})
	s2 := runServerWithOpts(t, s2sOpts, nil)
	defer s2.Shutdown()

	// Configure third server.
	s3sOpts := getTestDefaultOptsForClustering("c", []string{"a", "b"})
	s3 := runServerWithOpts(t, s3sOpts, nil)
	defer s3.Shutdown()

	servers := []*StanServer{s1, s2, s3}
	for _, s := range servers {
		checkState(t, s, Clustered)
	}

	// Create a client connection.
	sc, err := stan.Connect(clusterName, clientName)
	if err != nil {
		t.Fatalf("Expected to connect correctly, got err %v", err)
	}
	defer sc.Close()

	// Publish a message (this will create the channel and form the Raft group).
	channel := "foo"
	publishWithRetry(t, sc, channel, []byte("hello"))

	ch := make(chan *stan.Msg, 100)
	sub, err := sc.Subscribe(channel, func(msg *stan.Msg) {
		ch <- msg
	}, stan.DeliverAllAvailable(), stan.MaxInflight(1))
	if err != nil {
		t.Fatalf("Error subscribing: %v", err)
	}
	defer sub.Unsubscribe()

	select {
	case msg := <-ch:
		assertMsg(t, msg.MsgProto, []byte("hello"), 1)
	case <-time.After(2 * time.Second):
		t.Fatal("expected msg")
	}

	// Unsubscribe.
	if err := sub.Unsubscribe(); err != nil {
		t.Fatalf("Unexpected error on unsubscribe: %v", err)
	}

	// Take down the leader.
	leader := getChannelLeader(t, channel, 10*time.Second, servers...)
	leader.Shutdown()
	servers = removeServer(servers, leader)

	// Wait for the new leader to be elected.
	getChannelLeader(t, channel, 10*time.Second, servers...)

	// Publish some more messages.
	for i := 0; i < 5; i++ {
		if err := sc.Publish(channel, []byte(strconv.Itoa(i))); err != nil {
			t.Fatalf("Unexpected error on publish %d: %v", i, err)
		}
	}

	// Ensure we don't receive new messages.
	time.Sleep(200 * time.Millisecond)
	select {
	case <-ch:
		t.Fatal("Unexpected msg")
	default:
	}
}