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

package graph

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/token"
	"io"
	"os"
	"path"
	"sync"
	"time"

	etcd "github.com/coreos/etcd/client"
	"github.com/nu7hatch/gouuid"
	eval "github.com/sbinet/go-eval"
	"golang.org/x/net/context"

	"github.com/redhat-cip/skydive/logging"
)

type UUID uuid.UUID

func (id *UUID) String() string {
	i := uuid.UUID(*id)
	return i.String()
}

func (id *UUID) MarshalJSON() ([]byte, error) {
	return []byte("\"" + id.String() + "\""), nil
}

func (id *UUID) UnmarshalJSON(data []byte) error {
	uid, err := uuid.ParseHex(string(data[1 : len(data)-1]))
	if err != nil {
		return err
	}
	*id = UUID(*uid)
	return nil
}

type AlertManager struct {
	DefaultGraphListener
	Graph          *Graph
	alerts         map[UUID]AlertTest
	alertsLock     sync.RWMutex
	eventListeners map[AlertEventListener]AlertEventListener
	etcdKeyAPI     etcd.KeysAPI
}

type AlertType int

const (
	FIXED AlertType = 1 + iota
	THRESHOLD
)

type AlertTestParam struct {
	Name        string
	Description string
	Select      string
	Test        string
	Action      string
}

type AlertTest struct {
	AlertTestParam
	UUID       *UUID
	CreateTime time.Time
	Type       AlertType
	Count      int
}

type jsonAlertEncoder struct {
	w       io.Writer
	encoder *json.Encoder
}

func NewjsonAlertEncoder(w io.Writer) *jsonAlertEncoder {
	return &jsonAlertEncoder{w: w, encoder: json.NewEncoder(w)}
}

func (e *jsonAlertEncoder) Encode(v interface{}) error {
	switch t := v.(type) {
	case map[UUID]AlertTest:
		e.w.Write([]byte(`{`))
		first := true
		for _, v := range t {
			if !first {
				e.w.Write([]byte(`,`))
			}
			first = false
			e.w.Write([]byte(`"`))
			e.w.Write([]byte(v.UUID.String()))
			e.w.Write([]byte(`":`))
			e.encoder.Encode(v)
		}
		e.w.Write([]byte(`}`))
	default:
		e.encoder.Encode(v)
	}
	return nil
}

func etcdPath(id UUID) string {
	return fmt.Sprintf("/alert/%s", id.String())
}

func (a *AlertTest) etcdPath() string {
	return etcdPath(*a.UUID)
}

type AlertMessage struct {
	UUID       UUID
	Type       AlertType
	Timestamp  time.Time
	Count      int
	Reason     string
	ReasonData interface{}
}

func (am *AlertMessage) Marshal() []byte {
	j, _ := json.Marshal(am)
	return j
}

func (am *AlertMessage) String() string {
	return string(am.Marshal())
}

type AlertEventListener interface {
	OnAlert(n *AlertMessage)
}

func (a *AlertManager) AddEventListener(l AlertEventListener) {
	a.eventListeners[l] = l
}

func (a *AlertManager) DelEventListener(l AlertEventListener) {
	delete(a.eventListeners, l)
}

func (a *AlertManager) EvalNodes() {
	a.alertsLock.RLock()
	defer a.alertsLock.RUnlock()

	for _, al := range a.alerts {
		nodes := a.Graph.LookupNodesFromKey(al.Select)
		for _, n := range nodes {
			w := eval.NewWorld()
			defConst := func(name string, val interface{}) {
				t, v := toTypeValue(val)
				w.DefineConst(name, t, v)
			}
			for k, v := range n.metadata {
				defConst(k, v)
			}
			fs := token.NewFileSet()
			toEval := "(" + al.Test + ") == true"
			expr, err := w.Compile(fs, toEval)
			if err != nil {
				logging.GetLogger().Error("Can't compile expression : " + toEval)
				continue
			}
			ret, err := expr.Run()
			if err != nil {
				logging.GetLogger().Error("Can't evaluate expression : " + toEval)
				continue
			}

			if ret.String() == "true" {
				al.Count++

				msg := AlertMessage{
					UUID:       *al.UUID,
					Type:       FIXED,
					Timestamp:  time.Now(),
					Count:      al.Count,
					Reason:     al.Action,
					ReasonData: n,
				}

				logging.GetLogger().Debugf("AlertMessage to WS : " + al.UUID.String() + " " + msg.String())
				for _, l := range a.eventListeners {
					l.OnAlert(&msg)
				}
			}
		}
	}
}

func (a *AlertManager) triggerResync() {
	logging.GetLogger().Infof("Start a resync of the alert")

	hostname, err := os.Hostname()
	if err != nil {
		logging.GetLogger().Errorf("Unable to retrieve the hostname: %s", err.Error())
		return
	}

	a.Graph.Lock()
	defer a.Graph.Unlock()

	// request for deletion of everything belonging to host node
	root := a.Graph.GetNode(Identifier(hostname))
	if root == nil {
		return
	}
}

func (a *AlertManager) OnConnected() {
	a.triggerResync()
}

func (a *AlertManager) OnDisconnected() {
}

func (a *AlertManager) OnNodeUpdated(n *Node) {
	a.EvalNodes()
}

func (a *AlertManager) OnNodeAdded(n *Node) {
	a.EvalNodes()
}

func (a *AlertManager) SetAlertTest(at *AlertTest) {
	a.alertsLock.Lock()
	a.alerts[*at.UUID] = *at
	a.alertsLock.Unlock()
}

func (a *AlertManager) DeleteAlertTest(id *UUID) error {
	a.alertsLock.Lock()
	defer a.alertsLock.Unlock()

	_, ok := a.alerts[*id]
	if !ok {
		return errors.New("Not found")
	}
	delete(a.alerts, *id)

	return nil
}

func (a *AlertManager) Name() string {
	return "alert"
}

func (a *AlertManager) New() interface{} {
	return &AlertTest{}
}

func (a *AlertManager) Index() map[string]interface{} {
	a.alertsLock.RLock()
	defer a.alertsLock.RUnlock()

	alerts := make(map[string]interface{})
	for _, alert := range a.alerts {
		alerts[alert.UUID.String()] = alert
	}

	return alerts
}

func (a *AlertManager) Get(id string) (interface{}, bool) {
	alertUUID, err := uuid.ParseHex(id)
	if err != nil {
		return nil, false
	}

	a.alertsLock.RLock()
	defer a.alertsLock.RUnlock()
	alert, ok := a.alerts[UUID(*alertUUID)]
	return &alert, ok
}

func (a *AlertManager) Create(resource interface{}) error {
	at := resource.(*AlertTest)

	id, err := uuid.NewV4()
	if err != nil {
		return err
	}

	uid := UUID(*id)
	at.UUID = &uid
	at.CreateTime = time.Now()
	at.Type = FIXED
	at.Count = 0

	data, err := json.Marshal(&resource)
	if err != nil {
		return err
	}

	_, err = a.etcdKeyAPI.Set(context.Background(), at.etcdPath(), string(data), nil)
	return err
}

func (a *AlertManager) Delete(id string) error {
	alertUUID, err := uuid.ParseHex(id)
	if err != nil {
		return err
	}

	_, err = a.etcdKeyAPI.Delete(context.Background(), etcdPath(UUID(*alertUUID)), nil)
	return err
}

func alertTestFromData(data []byte) (*AlertTest, error) {
	at := AlertTest{}
	if err := json.Unmarshal(data, &at); err != nil {
		return nil, err
	}
	return &at, nil
}

func NewAlertManager(g *Graph, kapi etcd.KeysAPI) (*AlertManager, error) {
	f := &AlertManager{
		Graph:          g,
		alerts:         make(map[UUID]AlertTest),
		eventListeners: make(map[AlertEventListener]AlertEventListener),
		etcdKeyAPI:     kapi,
	}

	resp, err := kapi.Get(context.Background(), "/alert/", nil)
	if err == nil {
		for _, node := range resp.Node.Nodes {
			if at, err := alertTestFromData([]byte(node.Value)); err == nil {
				f.SetAlertTest(at)
			}
		}
	} else {
		resp, err = kapi.Set(context.Background(), "/alert", "", &etcd.SetOptions{Dir: true})
		if err != nil {
			return nil, err
		}
	}

	g.AddEventListener(f)

	watcher := kapi.Watcher("/alert/", &etcd.WatcherOptions{Recursive: true, AfterIndex: resp.Index})
	go func() {
		for {
			resp, err := watcher.Next(context.Background())
			if err != nil {
				return
			}

			if resp.Node.Dir {
				continue
			}

			switch resp.Action {
			case "create":
				fallthrough
			case "set":
				fallthrough
			case "update":
				at, err := alertTestFromData([]byte(resp.Node.Value))
				if err != nil {
					logging.GetLogger().Debugf("Error handling etcd event: %s", err.Error())
					continue
				}
				f.SetAlertTest(at)
			case "expire":
				fallthrough
			case "delete":
				id := path.Base(resp.Node.Key)
				if id, err := uuid.ParseHex(id); err == nil {
					uid := UUID(*id)
					f.DeleteAlertTest(&uid)
				}
			}
		}
	}()

	return f, nil
}

/*
 * go-eval helpers
 */

type boolV bool

func (v *boolV) String() string                      { return fmt.Sprint(*v) }
func (v *boolV) Assign(t *eval.Thread, o eval.Value) { *v = boolV(o.(eval.BoolValue).Get(t)) }
func (v *boolV) Get(*eval.Thread) bool               { return bool(*v) }
func (v *boolV) Set(t *eval.Thread, x bool)          { *v = boolV(x) }

type uint8V uint8

func (v *uint8V) String() string                      { return fmt.Sprint(*v) }
func (v *uint8V) Assign(t *eval.Thread, o eval.Value) { *v = uint8V(o.(eval.UintValue).Get(t)) }
func (v *uint8V) Get(*eval.Thread) uint64             { return uint64(*v) }
func (v *uint8V) Set(t *eval.Thread, x uint64)        { *v = uint8V(x) }

type uint16V uint16

func (v *uint16V) String() string                      { return fmt.Sprint(*v) }
func (v *uint16V) Assign(t *eval.Thread, o eval.Value) { *v = uint16V(o.(eval.UintValue).Get(t)) }
func (v *uint16V) Get(*eval.Thread) uint64             { return uint64(*v) }
func (v *uint16V) Set(t *eval.Thread, x uint64)        { *v = uint16V(x) }

type uint32V uint32

func (v *uint32V) String() string                      { return fmt.Sprint(*v) }
func (v *uint32V) Assign(t *eval.Thread, o eval.Value) { *v = uint32V(o.(eval.UintValue).Get(t)) }
func (v *uint32V) Get(*eval.Thread) uint64             { return uint64(*v) }
func (v *uint32V) Set(t *eval.Thread, x uint64)        { *v = uint32V(x) }

type uint64V uint64

func (v *uint64V) String() string                      { return fmt.Sprint(*v) }
func (v *uint64V) Assign(t *eval.Thread, o eval.Value) { *v = uint64V(o.(eval.UintValue).Get(t)) }
func (v *uint64V) Get(*eval.Thread) uint64             { return uint64(*v) }
func (v *uint64V) Set(t *eval.Thread, x uint64)        { *v = uint64V(x) }

type uintV uint

func (v *uintV) String() string                      { return fmt.Sprint(*v) }
func (v *uintV) Assign(t *eval.Thread, o eval.Value) { *v = uintV(o.(eval.UintValue).Get(t)) }
func (v *uintV) Get(*eval.Thread) uint64             { return uint64(*v) }
func (v *uintV) Set(t *eval.Thread, x uint64)        { *v = uintV(x) }

type uintptrV uintptr

func (v *uintptrV) String() string                      { return fmt.Sprint(*v) }
func (v *uintptrV) Assign(t *eval.Thread, o eval.Value) { *v = uintptrV(o.(eval.UintValue).Get(t)) }
func (v *uintptrV) Get(*eval.Thread) uint64             { return uint64(*v) }
func (v *uintptrV) Set(t *eval.Thread, x uint64)        { *v = uintptrV(x) }

/*
 * Int
 */

type int8V int8

func (v *int8V) String() string                      { return fmt.Sprint(*v) }
func (v *int8V) Assign(t *eval.Thread, o eval.Value) { *v = int8V(o.(eval.IntValue).Get(t)) }
func (v *int8V) Get(*eval.Thread) int64              { return int64(*v) }
func (v *int8V) Set(t *eval.Thread, x int64)         { *v = int8V(x) }

type int16V int16

func (v *int16V) String() string                      { return fmt.Sprint(*v) }
func (v *int16V) Assign(t *eval.Thread, o eval.Value) { *v = int16V(o.(eval.IntValue).Get(t)) }
func (v *int16V) Get(*eval.Thread) int64              { return int64(*v) }
func (v *int16V) Set(t *eval.Thread, x int64)         { *v = int16V(x) }

type int32V int32

func (v *int32V) String() string                      { return fmt.Sprint(*v) }
func (v *int32V) Assign(t *eval.Thread, o eval.Value) { *v = int32V(o.(eval.IntValue).Get(t)) }
func (v *int32V) Get(*eval.Thread) int64              { return int64(*v) }
func (v *int32V) Set(t *eval.Thread, x int64)         { *v = int32V(x) }

type int64V int64

func (v *int64V) String() string                      { return fmt.Sprint(*v) }
func (v *int64V) Assign(t *eval.Thread, o eval.Value) { *v = int64V(o.(eval.IntValue).Get(t)) }
func (v *int64V) Get(*eval.Thread) int64              { return int64(*v) }
func (v *int64V) Set(t *eval.Thread, x int64)         { *v = int64V(x) }

type intV int

func (v *intV) String() string                      { return fmt.Sprint(*v) }
func (v *intV) Assign(t *eval.Thread, o eval.Value) { *v = intV(o.(eval.IntValue).Get(t)) }
func (v *intV) Get(*eval.Thread) int64              { return int64(*v) }
func (v *intV) Set(t *eval.Thread, x int64)         { *v = intV(x) }

/*
 * Float
 */

type float32V float32

func (v *float32V) String() string                      { return fmt.Sprint(*v) }
func (v *float32V) Assign(t *eval.Thread, o eval.Value) { *v = float32V(o.(eval.FloatValue).Get(t)) }
func (v *float32V) Get(*eval.Thread) float64            { return float64(*v) }
func (v *float32V) Set(t *eval.Thread, x float64)       { *v = float32V(x) }

type float64V float64

func (v *float64V) String() string                      { return fmt.Sprint(*v) }
func (v *float64V) Assign(t *eval.Thread, o eval.Value) { *v = float64V(o.(eval.FloatValue).Get(t)) }
func (v *float64V) Get(*eval.Thread) float64            { return float64(*v) }
func (v *float64V) Set(t *eval.Thread, x float64)       { *v = float64V(x) }

/*
 * String
 */

type stringV string

func (v *stringV) String() string                      { return fmt.Sprint(*v) }
func (v *stringV) Assign(t *eval.Thread, o eval.Value) { *v = stringV(o.(eval.StringValue).Get(t)) }
func (v *stringV) Get(*eval.Thread) string             { return string(*v) }
func (v *stringV) Set(t *eval.Thread, x string)        { *v = stringV(x) }

func toTypeValue(val interface{}) (eval.Type, eval.Value) {
	switch val := val.(type) {
	case bool:
		r := boolV(val)
		return eval.BoolType, &r
	case uint8:
		r := uint8V(val)
		return eval.Uint8Type, &r
	case uint32:
		r := uint32V(val)
		return eval.Uint32Type, &r
	case uint:
		r := uintV(val)
		return eval.Uint64Type, &r
	case int:
		r := intV(val)
		return eval.Int64Type, &r
	case float64:
		r := float64V(val)
		return eval.Float64Type, &r
	case string:
		r := stringV(val)
		return eval.StringType, &r
	}
	logging.GetLogger().Errorf("toValue(%T) not implemented", val)
	return nil, nil
}
