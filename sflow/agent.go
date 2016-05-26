/*
 * Copyright (C) 2015 Red Hat, Inc.
 *
 * Licensed to the Apache Software Foundation (ASF) under one
 * or more contributor license agreements.  See the NOTICE file
 * distributed with this work for additional information
 * regarding copyright ownership.  The ASF licenses this file
 * to you under the Apache License, Version 2.0 (the
 * "License"); you may not use this file except in compliance
 * with the License.  You may obtain a copy of the License at
 *
 *  http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing,
 * software distributed under the License is distributed on an
 * "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY
 * KIND, either express or implied.  See the License for the
 * specific language governing permissions and limitations
 * under the License.
 *
 */

package sflow

import (
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"

	"github.com/redhat-cip/skydive/analyzer"
	"github.com/redhat-cip/skydive/config"
	"github.com/redhat-cip/skydive/flow"
	"github.com/redhat-cip/skydive/flow/mappings"
	"github.com/redhat-cip/skydive/logging"
)

const (
	maxDgramSize = 1500
)

var (
	AgentAlreadyAllocated error = errors.New("agent already allocated for this uuid")
)

type SFlowAgent struct {
	UUID                string
	Addr                string
	Port                int
	AnalyzerClient      *analyzer.Client
	flowTable           *flow.Table
	FlowMappingPipeline *mappings.FlowMappingPipeline
	FlowProbePathSetter flow.FlowProbePathSetter
	running             atomic.Value
	wg                  sync.WaitGroup
	flush               chan bool
	flushDone           chan bool
}

type SFlowAgentAllocator struct {
	sync.RWMutex
	AnalyzerClient      *analyzer.Client
	FlowMappingPipeline *mappings.FlowMappingPipeline
	FlowProbePathSetter flow.FlowProbePathSetter
	Addr                string
	MinPort             int
	MaxPort             int
	allocated           map[int]*SFlowAgent
}

func (sfa *SFlowAgent) GetTarget() string {
	target := []string{sfa.Addr, strconv.FormatInt(int64(sfa.Port), 10)}
	return strings.Join(target, ":")
}

func (sfa *SFlowAgent) feedFlowTable(conn *net.UDPConn) {
	var buf [maxDgramSize]byte
	_, _, err := conn.ReadFromUDP(buf[:])
	if err != nil {
		conn.SetDeadline(time.Now().Add(1 * time.Second))
		return
	}

	p := gopacket.NewPacket(buf[:], layers.LayerTypeSFlow, gopacket.Default)
	sflowLayer := p.Layer(layers.LayerTypeSFlow)
	sflowPacket, ok := sflowLayer.(*layers.SFlowDatagram)
	if !ok {
		return
	}

	if sflowPacket.SampleCount > 0 {
		for _, sample := range sflowPacket.FlowSamples {
			flows := flow.FlowsFromSFlowSample(sfa.flowTable, &sample, sfa.FlowProbePathSetter)
			logging.GetLogger().Debugf("%d flows captured", len(flows))
		}
	}
}

func (sfa *SFlowAgent) asyncFlowPipeline(flows []*flow.Flow) {
	if sfa.FlowMappingPipeline != nil {
		sfa.FlowMappingPipeline.Enhance(flows)
	}
	if sfa.AnalyzerClient != nil {
		sfa.AnalyzerClient.SendFlows(flows)
	}
}

func (sfa *SFlowAgent) start() error {
	addr := net.UDPAddr{
		Port: sfa.Port,
		IP:   net.ParseIP(sfa.Addr),
	}
	conn, err := net.ListenUDP("udp", &addr)
	if err != nil {
		logging.GetLogger().Errorf("Unable to listen on port %d: %s", sfa.Port, err.Error())
		return err
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(1 * time.Second))

	sfa.wg.Add(1)
	defer sfa.wg.Done()

	sfa.running.Store(true)

	sfa.flowTable = flow.NewTable()
	defer sfa.flowTable.UnregisterAll()

	cfgFlowtable_expire := config.GetConfig().GetInt("agent.flowtable_expire")
	sfa.flowTable.RegisterExpire(sfa.asyncFlowPipeline, time.Duration(cfgFlowtable_expire)*time.Second)

	cfgFlowtable_update := config.GetConfig().GetInt("agent.flowtable_update")
	sfa.flowTable.RegisterUpdated(sfa.asyncFlowPipeline, time.Duration(cfgFlowtable_update)*time.Second)

	for sfa.running.Load() == true {
		select {
		case now := <-sfa.flowTable.GetExpireTicker():
			sfa.flowTable.Expire(now)
		case now := <-sfa.flowTable.GetUpdatedTicker():
			sfa.flowTable.Updated(now)
		case <-sfa.flush:
			sfa.flowTable.ExpireNow()
			sfa.flushDone <- true
		default:
			sfa.feedFlowTable(conn)
		}
	}

	return nil
}

func (sfa *SFlowAgent) Start() {
	go sfa.start()
}

func (sfa *SFlowAgent) Stop() {
	if sfa.running.Load() == true {
		sfa.running.Store(false)
		sfa.wg.Wait()
	}
}

func (sfa *SFlowAgent) Flush() {
	logging.GetLogger().Critical("Flush() MUST be called for testing purpose only, not in production")
	sfa.flush <- true
	<-sfa.flushDone
}

func (sfa *SFlowAgent) SetFlowProbePathSetter(p flow.FlowProbePathSetter) {
	sfa.FlowProbePathSetter = p
}

func NewSFlowAgent(u string, a string, p int, c *analyzer.Client, m *mappings.FlowMappingPipeline) *SFlowAgent {
	return &SFlowAgent{
		UUID:                u,
		Addr:                a,
		Port:                p,
		AnalyzerClient:      c,
		FlowMappingPipeline: m,
		flush:               make(chan bool),
		flushDone:           make(chan bool),
	}
}

func NewSFlowAgentFromConfig(u string, a *analyzer.Client, m *mappings.FlowMappingPipeline) (*SFlowAgent, error) {
	addr, port, err := config.GetHostPortAttributes("sflow", "listen")
	if err != nil {
		return nil, err
	}

	return NewSFlowAgent(u, addr, port, a, m), nil
}

func (a *SFlowAgentAllocator) Agents() []*SFlowAgent {
	a.Lock()
	defer a.Unlock()

	agents := make([]*SFlowAgent, 0)

	for _, agent := range a.allocated {
		agents = append(agents, agent)
	}

	return agents
}

func (a *SFlowAgentAllocator) Release(uuid string) {
	a.Lock()
	defer a.Unlock()

	for i, agent := range a.allocated {
		if uuid == agent.UUID {
			agent.Stop()

			delete(a.allocated, i)
		}
	}
}

func (a *SFlowAgentAllocator) ReleaseAll() {
	a.Lock()
	defer a.Unlock()

	for i, agent := range a.allocated {
		agent.Stop()

		delete(a.allocated, i)
	}
}

func (a *SFlowAgentAllocator) Alloc(uuid string, p flow.FlowProbePathSetter) (*SFlowAgent, error) {
	address := config.GetConfig().GetString("sflow.bind_address")
	if address == "" {
		address = "127.0.0.1"
	}

	min := config.GetConfig().GetInt("sflow.port_min")
	if min == 0 {
		min = 6345
	}

	max := config.GetConfig().GetInt("sflow.port_max")
	if max == 0 {
		max = 6355
	}

	a.Lock()
	defer a.Unlock()

	// check if there is an already allocated agent for this uuid
	for _, agent := range a.allocated {
		if uuid == agent.UUID {
			return agent, AgentAlreadyAllocated
		}
	}

	for i := min; i != max+1; i++ {
		if _, ok := a.allocated[i]; !ok {
			s := NewSFlowAgent(uuid, address, i, a.AnalyzerClient, a.FlowMappingPipeline)
			s.SetFlowProbePathSetter(p)

			a.allocated[i] = s

			s.Start()

			return s, nil
		}
	}

	return nil, errors.New("sflow port exhausted")
}

func NewSFlowAgentAllocator(a *analyzer.Client, m *mappings.FlowMappingPipeline) *SFlowAgentAllocator {
	return &SFlowAgentAllocator{
		AnalyzerClient:      a,
		FlowMappingPipeline: m,
		allocated:           make(map[int]*SFlowAgent),
	}
}
