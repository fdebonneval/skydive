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

package probes

import (
	"encoding/json"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pmylund/go-cache"
	"github.com/socketplane/libovsdb"

	"github.com/redhat-cip/skydive/analyzer"
	"github.com/redhat-cip/skydive/api"
	"github.com/redhat-cip/skydive/config"
	"github.com/redhat-cip/skydive/flow/mappings"
	"github.com/redhat-cip/skydive/logging"
	"github.com/redhat-cip/skydive/ovs"
	"github.com/redhat-cip/skydive/sflow"
	"github.com/redhat-cip/skydive/topology"
	"github.com/redhat-cip/skydive/topology/graph"
	"github.com/redhat-cip/skydive/topology/probes"
)

type OvsSFlowProbe struct {
	ID         string
	Interface  string
	Target     string
	HeaderSize uint32
	Sampling   uint32
	Polling    uint32
}

type OvsSFlowProbesHandler struct {
	Graph            *graph.Graph
	ovsClient        *ovsdb.OvsClient
	agent            *sflow.SFlowAgent
	agentLock        sync.Mutex
	cache            *cache.Cache
	cacheUpdaterChan chan int64
	done             chan bool
	running          atomic.Value
	wg               sync.WaitGroup
}

func (o *OvsSFlowProbesHandler) lookupForProbePath(index int64) string {
	o.Graph.Lock()
	defer o.Graph.Unlock()

	intfs := o.Graph.LookupNodes(graph.Metadata{"IfIndex": index})
	if len(intfs) == 0 {
		return ""
	}

	// lookup for the interface that is a part of an ovs bridge
	for _, intf := range intfs {
		nodes := o.Graph.LookupShortestPath(intf, graph.Metadata{"Type": "ovsbridge"}, topology.IsLayer2Edge)
		if len(nodes) == 0 {
			continue
		}

		bridge := nodes[len(nodes)-1]
		nodes = o.Graph.LookupShortestPath(bridge, graph.Metadata{"Type": "host"}, topology.IsOwnershipEdge)
		if len(nodes) == 0 {
			continue
		}

		return topology.NodePath{nodes}.Marshal()
	}

	return ""
}

func (o *OvsSFlowProbesHandler) cacheUpdater() {
	o.wg.Add(1)
	defer o.wg.Done()

	logging.GetLogger().Debug("Start OVS Sflow probe cache updater")

	var index int64
	for o.running.Load() == true {
		select {
		case index = <-o.cacheUpdaterChan:
			logging.GetLogger().Debugf("OVS Sflow probe request received: %d", index)

			path := o.lookupForProbePath(index)
			if path != "" {
				o.cache.Set(strconv.FormatInt(index, 10), path, cache.DefaultExpiration)
			}

		case <-o.done:
			return
		}
	}
}

func (o *OvsSFlowProbesHandler) GetProbePath(index int64) string {
	p, f := o.cache.Get(strconv.FormatInt(index, 10))
	if f {
		path := p.(string)
		return path
	}
	o.cacheUpdaterChan <- index

	return ""
}

func newInsertSFlowProbeOP(probe OvsSFlowProbe) (*libovsdb.Operation, error) {
	sFlowRow := make(map[string]interface{})
	sFlowRow["agent"] = probe.Interface
	sFlowRow["targets"] = probe.Target
	sFlowRow["header"] = probe.HeaderSize
	sFlowRow["sampling"] = probe.Sampling
	sFlowRow["polling"] = probe.Polling

	extIds := make(map[string]string)
	extIds["probe-id"] = probe.ID
	ovsMap, err := libovsdb.NewOvsMap(extIds)
	if err != nil {
		return nil, err
	}
	sFlowRow["external_ids"] = ovsMap

	insertOp := libovsdb.Operation{
		Op:       "insert",
		Table:    "sFlow",
		Row:      sFlowRow,
		UUIDName: probe.ID,
	}

	return &insertOp, nil
}

func compareProbeID(row *map[string]interface{}, probe OvsSFlowProbe) (bool, error) {
	extIds := (*row)["external_ids"]
	switch extIds.(type) {
	case []interface{}:
		sl := extIds.([]interface{})
		bSliced, err := json.Marshal(sl)
		if err != nil {
			return false, err
		}

		switch sl[0] {
		case "map":
			var oMap libovsdb.OvsMap
			err = json.Unmarshal(bSliced, &oMap)
			if err != nil {
				return false, err
			}

			if value, ok := oMap.GoMap["probe-id"]; ok {
				if value == probe.ID {
					return true, nil
				}
			}
		}
	}

	return false, nil
}

func (o *OvsSFlowProbesHandler) makeOvsSFlowProbe() OvsSFlowProbe {
	// TODO(safchain) add config parameter
	o.agentLock.Lock()
	defer o.agentLock.Unlock()
	return OvsSFlowProbe{
		ID:         "SkydiveSFlowProbe",
		Interface:  "eth0",
		Target:     o.agent.GetTarget(),
		HeaderSize: 256,
		Sampling:   1,
		Polling:    0,
	}
}

func (o *OvsSFlowProbesHandler) retrieveSFlowProbeUUID(probe OvsSFlowProbe) (string, error) {
	/* FIX(safchain) don't find a way to send a null condition */
	condition := libovsdb.NewCondition("_uuid", "!=", libovsdb.UUID{"abc"})
	selectOp := libovsdb.Operation{
		Op:    "select",
		Table: "sFlow",
		Where: []interface{}{condition},
	}

	operations := []libovsdb.Operation{selectOp}
	result, err := o.ovsClient.Exec(operations...)
	if err != nil {
		return "", err
	}

	for _, o := range result {
		for _, row := range o.Rows {
			u := row["_uuid"].([]interface{})[1]
			uuid := u.(string)

			if targets, ok := row["targets"]; ok {
				if targets != probe.Target {
					continue
				}
			}

			if polling, ok := row["polling"]; ok {
				if uint32(polling.(float64)) != probe.Polling {
					continue
				}
			}

			if sampling, ok := row["sampling"]; ok {
				if uint32(sampling.(float64)) != probe.Sampling {
					continue
				}
			}

			if ok, _ := compareProbeID(&row, probe); ok {
				return uuid, nil
			}
		}
	}

	return "", nil
}

func (o *OvsSFlowProbesHandler) registerSFlowProbeOnBridge(probe OvsSFlowProbe, bridgeUUID string) error {
	probeUUID, err := o.retrieveSFlowProbeUUID(probe)
	if err != nil {
		return err
	}

	operations := []libovsdb.Operation{}

	var uuid libovsdb.UUID
	if probeUUID != "" {
		uuid = libovsdb.UUID{probeUUID}

		logging.GetLogger().Infof("Using already registered OVS SFlow probe \"%s(%s)\"", probe.ID, uuid)
	} else {
		insertOp, err := newInsertSFlowProbeOP(probe)
		if err != nil {
			return err
		}
		uuid = libovsdb.UUID{insertOp.UUIDName}
		logging.GetLogger().Infof("Registering new OVS SFlow probe \"%s(%s)\"", probe.ID, uuid)

		operations = append(operations, *insertOp)
	}

	bridgeRow := make(map[string]interface{})
	bridgeRow["sflow"] = uuid

	condition := libovsdb.NewCondition("_uuid", "==", libovsdb.UUID{bridgeUUID})
	updateOp := libovsdb.Operation{
		Op:    "update",
		Table: "Bridge",
		Row:   bridgeRow,
		Where: []interface{}{condition},
	}

	operations = append(operations, updateOp)
	_, err = o.ovsClient.Exec(operations...)
	if err != nil {
		return err
	}
	return nil
}

func (o *OvsSFlowProbesHandler) UnregisterSFlowProbeFromBridge(probe OvsSFlowProbe, bridgeUUID string) error {
	probeUUID, err := o.retrieveSFlowProbeUUID(probe)
	if err != nil {
		return err
	}
	if probeUUID == "" {
		return nil
	}

	operations := []libovsdb.Operation{}

	bridgeRow := make(map[string]interface{})
	bridgeRow["sflow"] = libovsdb.OvsSet{make([]interface{}, 0)}

	condition := libovsdb.NewCondition("_uuid", "==", libovsdb.UUID{bridgeUUID})
	updateOp := libovsdb.Operation{
		Op:    "update",
		Table: "Bridge",
		Row:   bridgeRow,
		Where: []interface{}{condition},
	}

	operations = append(operations, updateOp)
	_, err = o.ovsClient.Exec(operations...)
	if err != nil {
		return err
	}
	return nil
}

func (o *OvsSFlowProbesHandler) RegisterProbeOnBridge(bridgeUUID string) error {
	probe := o.makeOvsSFlowProbe()
	err := o.registerSFlowProbeOnBridge(probe, bridgeUUID)
	if err != nil {
		return err
	}
	return nil
}

func isOvsBridge(n *graph.Node) bool {
	return n.Metadata()["UUID"] != "" && n.Metadata()["Type"] == "ovsbridge"
}

func (o *OvsSFlowProbesHandler) RegisterProbe(n *graph.Node, capture *api.Capture) error {
	if isOvsBridge(n) {
		err := o.RegisterProbeOnBridge(n.Metadata()["UUID"].(string))
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *OvsSFlowProbesHandler) unregisterProbe(bridgeUUID string) error {
	probe := o.makeOvsSFlowProbe()
	err := o.UnregisterSFlowProbeFromBridge(probe, bridgeUUID)
	if err != nil {
		return err
	}
	return nil
}

func (o *OvsSFlowProbesHandler) UnregisterProbe(n *graph.Node) error {
	if isOvsBridge(n) {
		err := o.unregisterProbe(n.Metadata()["UUID"].(string))
		if err != nil {
			return err
		}
	}
	return nil
}

func (o *OvsSFlowProbesHandler) Start() {
	// start index/mac cache updater
	go o.cacheUpdater()

	o.agentLock.Lock()
	o.agent.Start()
	o.agentLock.Unlock()
}

func (o *OvsSFlowProbesHandler) Stop() {
	// TODO(safchain) call RemoveMonitorHandler when implemented
	o.agentLock.Lock()
	o.agent.Stop()
	o.agentLock.Unlock()

	o.running.Store(false)
	o.done <- true
	o.wg.Wait()
}

func (o *OvsSFlowProbesHandler) Flush() {
	o.agentLock.Lock()
	o.agent.Flush()
	o.agentLock.Unlock()
}

func NewOvsSFlowProbesHandler(p *probes.OvsdbProbe, agent *sflow.SFlowAgent, expire int, cleanup int) *OvsSFlowProbesHandler {
	o := &OvsSFlowProbesHandler{
		Graph:     p.Graph,
		ovsClient: p.OvsMon.OvsClient,
		agent:     agent,
	}

	o.cache = cache.New(time.Duration(expire)*time.Second, time.Duration(cleanup)*time.Second)
	o.cacheUpdaterChan = make(chan int64, 200)
	o.done = make(chan bool)
	o.running.Store(true)

	return o
}

func NewOvsSFlowProbesHandlerFromConfig(tb *probes.TopologyProbeBundle, g *graph.Graph, p *mappings.FlowMappingPipeline, a *analyzer.Client) *OvsSFlowProbesHandler {
	probe := tb.GetProbe("ovsdb")
	if probe == nil {
		logging.GetLogger().Error("Agent.ovssflow probe depends on agent.ovsdb topology probe: agent.ovssflow probe can't start properly")
		return nil
	}

	agent, err := sflow.NewSFlowAgentFromConfig(g)
	if err != nil {
		logging.GetLogger().Errorf("Unable to start an OVS SFlow probe handler: %s", err.Error())
		return nil
	}
	agent.SetMappingPipeline(p)

	if a != nil {
		agent.SetAnalyzerClient(a)
	}

	expire := config.GetConfig().GetInt("cache.expire")
	cleanup := config.GetConfig().GetInt("cache.cleanup")

	o := NewOvsSFlowProbesHandler(probe.(*probes.OvsdbProbe), agent, expire, cleanup)

	agent.SetProbePathGetter(o)

	return o
}
