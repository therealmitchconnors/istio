// Copyright Istio Authors
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

package v2

import v3 "istio.io/istio/pilot/pkg/proxy/envoy/v3"

// EventType represents the type of object we are tracking. This is distinct from Envoy's TypeUrl
// as TypeUrl is versioned, whereas EventType is not
type EventType string

const (
	ClusterEventType  EventType = "Cluster"
	ListenerEventType EventType = "Listener"
	RouteEventType    EventType = "Route"
	EndpointEventType EventType = "Endpoint"
	UnknownEventType  EventType = ""
)

var AllEventTypes = []EventType{
	ClusterEventType,
	ListenerEventType,
	RouteEventType,
	EndpointEventType,
}

func TypeURLToEventType(typeURL string) EventType {
	switch typeURL {
	case ClusterType, v3.ClusterType:
		return ClusterEventType
	case EndpointType, v3.EndpointType:
		return EndpointEventType
	case RouteType, v3.RouteType:
		return RouteEventType
	case ListenerType, v3.ListenerType:
		return ListenerEventType
	default:
		return UnknownEventType
	}
}

func EventTypeToTypeV3URL(eventType EventType) string {
	switch eventType{
	case ClusterEventType:
		return v3.ClusterType
	case EndpointEventType:
		return v3.EndpointType
	case ListenerEventType:
		return v3.ListenerType
	case RouteEventType:
		return v3.RouteType
	default:
		return "Unknown"
	}
}

// EventHandler allows for generic monitoring of xDS ACKS and disconnects, for the purpose of tracking
// Config distribution through the mesh.
type DistributionStatusCache interface {
	DistributionEventHandler
	QueryLastNonce(conID string, eventType EventType) (noncePrefix string)
}

type DistributionEventHandler interface {
	// RegisterACK notifies the implementer of an xDS ACK, and must be non-blocking
	RegisterACK(conID string, eventType EventType, nonce string)
	RegisterDisconnect(connID string, types []EventType)
}

func NewAggregateDistributionTracker(primary DistributionStatusCache, secondaries []DistributionEventHandler) DistributionStatusCache {
	return &aggregateDistribitionTracker{primary, secondaries}
}

type aggregateDistribitionTracker struct {
	primary DistributionStatusCache
	secondaries []DistributionEventHandler
}

func (a aggregateDistribitionTracker) RegisterACK(conID string, eventType EventType, nonce string) {
	a.primary.RegisterACK(conID, eventType, nonce)
	for _, s := range a.secondaries {
		s.RegisterACK(conID, eventType, nonce)
	}
}

func (a aggregateDistribitionTracker) RegisterDisconnect(s string, types []EventType) {
	a.primary.RegisterDisconnect(s, types)
	for _, sec := range a.secondaries {
		sec.RegisterDisconnect(s, types)
	}
}

func (a aggregateDistribitionTracker) QueryLastNonce(conID string, eventType EventType) (noncePrefix string) {
	return a.primary.QueryLastNonce(conID, eventType)
}


