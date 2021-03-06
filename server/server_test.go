// Copyright 2016 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"fmt"
	"math/rand"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coreos/etcd/clientv3"
	"github.com/ngaut/log"
	. "github.com/pingcap/check"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pingcap/pd/pkg/testutil"
)

func TestServer(t *testing.T) {
	TestingT(t)
}

type cleanupFunc func()

func newTestServer(c *C) (*Server, cleanUpFunc) {
	cfg := NewTestSingleConfig()

	svr, err := NewServer(cfg)
	c.Assert(err, IsNil)

	cleanup := func() {
		svr.Close()
		cleanServer(svr.cfg)
	}

	return svr, cleanup
}

func mustRunTestServer(c *C) (*Server, cleanUpFunc) {
	server, cleanup := newTestServer(c)
	go server.Run()
	mustWaitLeader(c, []*Server{server})
	return server, cleanup
}

var stripUnix = strings.NewReplacer("unix://", "")

func cleanServer(cfg *Config) {
	// Clean data directory
	os.RemoveAll(cfg.DataDir)

	// Clean unix sockets
	os.Remove(stripUnix.Replace(cfg.PeerUrls))
	os.Remove(stripUnix.Replace(cfg.ClientUrls))
	os.Remove(stripUnix.Replace(cfg.AdvertisePeerUrls))
	os.Remove(stripUnix.Replace(cfg.AdvertiseClientUrls))
}

func newMultiTestServers(c *C, count int) ([]*Server, cleanupFunc) {
	svrs := make([]*Server, 0, count)
	cfgs := NewTestMultiConfig(count)

	ch := make(chan *Server, count)
	for i := 0; i < count; i++ {
		cfg := cfgs[i]

		go func() {
			svr, err := NewServer(cfg)
			c.Assert(err, IsNil)
			ch <- svr
		}()
	}

	for i := 0; i < count; i++ {
		svr := <-ch
		go svr.Run()
		svrs = append(svrs, svr)
	}

	mustWaitLeader(c, svrs)

	cleanup := func() {
		for _, svr := range svrs {
			svr.Close()
		}

		for _, cfg := range cfgs {
			cleanServer(cfg)
		}
	}

	return svrs, cleanup
}

func mustWaitLeader(c *C, svrs []*Server) *Server {
	for i := 0; i < 500; i++ {
		for _, s := range svrs {
			if s.IsLeader() {
				return s
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.Fatal("no leader")
	return nil
}

func mustRPCCall(c *C, conn net.Conn, req *pdpb.Request) *pdpb.Response {
	resp, err := rpcCall(conn, uint64(rand.Int63()), req)
	c.Assert(err, IsNil)
	c.Assert(resp, NotNil)
	return resp
}

var _ = Suite(&testLeaderServerSuite{})

type testLeaderServerSuite struct {
	svrs       map[string]*Server
	leaderPath string
}

func mustGetEtcdClient(c *C, svrs map[string]*Server) *clientv3.Client {
	for _, svr := range svrs {
		return svr.GetClient()
	}
	c.Fatal("etcd client none available")
	return nil
}

func (s *testLeaderServerSuite) SetUpSuite(c *C) {
	s.svrs = make(map[string]*Server)

	cfgs := NewTestMultiConfig(3)

	ch := make(chan *Server, 3)
	for i := 0; i < 3; i++ {
		cfg := cfgs[i]

		go func() {
			svr, err := NewServer(cfg)
			c.Assert(err, IsNil)
			ch <- svr
		}()
	}

	for i := 0; i < 3; i++ {
		svr := <-ch
		s.svrs[svr.GetAddr()] = svr
		s.leaderPath = svr.getLeaderPath()
	}
}

func (s *testLeaderServerSuite) TearDownSuite(c *C) {
	for _, svr := range s.svrs {
		svr.Close()
		cleanServer(svr.cfg)
	}
}

func (s *testLeaderServerSuite) TestLeader(c *C) {
	for _, svr := range s.svrs {
		go svr.Run()
	}

	leader1 := mustGetLeader(c, mustGetEtcdClient(c, s.svrs), s.leaderPath)
	svr, ok := s.svrs[leader1.GetAddr()]
	c.Assert(ok, IsTrue)
	svr.Close()
	delete(s.svrs, leader1.GetAddr())

	client := mustGetEtcdClient(c, s.svrs)

	// wait leader changes
	for i := 0; i < 50; i++ {
		leader, _ := getLeader(client, s.leaderPath)
		if leader != nil && leader.GetAddr() != leader1.GetAddr() {
			break
		}

		time.Sleep(500 * time.Millisecond)
	}

	leader2 := mustGetLeader(c, client, s.leaderPath)
	c.Assert(leader1.GetAddr(), Not(Equals), leader2.GetAddr())
}

var _ = Suite(&testServerSuite{})

type testServerSuite struct{}

func newTestServersWithCfgs(c *C, cfgs []*Config) ([]*Server, cleanupFunc) {
	svrs := make([]*Server, 0, len(cfgs))

	ch := make(chan *Server)
	for _, cfg := range cfgs {
		go func(cfg *Config) {
			svr, err := NewServer(cfg)
			c.Assert(err, IsNil)
			go svr.Run()
			ch <- svr
		}(cfg)
	}

	for i := 0; i < len(cfgs); i++ {
		svrs = append(svrs, <-ch)
	}
	mustWaitLeader(c, svrs)

	cleanup := func() {
		for _, svr := range svrs {
			svr.Close()
		}
		for _, cfg := range cfgs {
			cleanServer(cfg)
		}
	}

	return svrs, cleanup
}

func (s *testServerSuite) TestClusterID(c *C) {
	cfgs := NewTestMultiConfig(3)
	for i, cfg := range cfgs {
		cfg.DataDir = fmt.Sprintf("/tmp/test_pd_cluster_id_%d", i)
		cleanServer(cfg)
	}

	svrs, cleanup := newTestServersWithCfgs(c, cfgs)

	// All PDs should have the same cluster ID.
	clusterID := svrs[0].clusterID
	c.Assert(clusterID, Not(Equals), uint64(0))
	for _, svr := range svrs {
		log.Debug(svr.clusterID)
		c.Assert(svr.clusterID, Equals, clusterID)
	}

	// Restart all PDs.
	for _, svr := range svrs {
		svr.Close()
	}
	svrs, cleanup = newTestServersWithCfgs(c, cfgs)
	defer cleanup()

	// All PDs should have the same cluster ID as before.
	for _, svr := range svrs {
		c.Assert(svr.clusterID, Equals, clusterID)
	}
}

func (s *testServerSuite) TestUpdateAdvertiseUrls(c *C) {
	cfgs := NewTestMultiConfig(2)
	for i, cfg := range cfgs {
		cfg.DataDir = fmt.Sprintf("/tmp/test_pd_advertise_%d", i)
		cleanServer(cfg)
	}

	svrs, cleanup := newTestServersWithCfgs(c, cfgs)

	// AdvertisePeerUrls should equals to PeerUrls
	for _, svr := range svrs {
		c.Assert(svr.cfg.AdvertisePeerUrls, Equals, svr.cfg.PeerUrls)
		c.Assert(svr.cfg.AdvertiseClientUrls, Equals, svr.cfg.ClientUrls)
	}

	// Close all PDs.
	for _, svr := range svrs {
		svr.Close()
	}

	// Little malicious tweak.
	overlapPeerURL := "," + testutil.UnixURL()
	for _, cfg := range cfgs {
		cfg.AdvertisePeerUrls += overlapPeerURL
	}

	// Restart all PDs.
	svrs, cleanup = newTestServersWithCfgs(c, cfgs)
	defer cleanup()

	// All PDs should have the same advertise urls as before.
	for _, svr := range svrs {
		c.Assert(svr.cfg.AdvertisePeerUrls, Equals, svr.cfg.PeerUrls)
	}
}

func (s *testServerSuite) TestCheckClusterID(c *C) {
	cfgs := NewTestMultiConfig(2)
	for i, cfg := range cfgs {
		cfg.DataDir = fmt.Sprintf("/tmp/test_pd_check_clusterID_%d", i)
		// Clean up before testing.
		cleanServer(cfg)
	}
	originInitial := cfgs[0].InitialCluster
	for _, cfg := range cfgs {
		cfg.InitialCluster = fmt.Sprintf("%s=%s", cfg.Name, cfg.PeerUrls)
	}

	cfgA, cfgB := cfgs[0], cfgs[1]
	// Start a standalone cluster
	// TODO: clean up. For now tests failed because:
	//    etcdserver: failed to purge snap file ...
	svrsA, _ := newTestServersWithCfgs(c, []*Config{cfgA})
	// Close it.
	for _, svr := range svrsA {
		svr.Close()
	}

	// Start another cluster.
	_, cleanB := newTestServersWithCfgs(c, []*Config{cfgB})
	defer cleanB()

	// Start pervious cluster, expect an error.
	cfgA.InitialCluster = originInitial
	_, err := NewServer(cfgA)
	c.Assert(err, NotNil)
}
