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

import (
	"context"
	"errors"
	"istio.io/pkg/log"
	"strings"
	"sync/atomic"

	core "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	status "github.com/envoyproxy/go-control-plane/envoy/service/status/v3"
	envoy_type_matcher_v3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	_ "github.com/envoyproxy/go-control-plane/pkg/cache/v3"
	"github.com/hashicorp/go-multierror"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcstatus "google.golang.org/grpc/status"
)

type CSDSServer struct {
	ctx context.Context
	w2  map[chan status.ClientStatusResponse]status.ClientStatusRequest
	ds  *DiscoveryServer
}

func (cs *CSDSServer) Start(server *grpc.Server, ctx context.Context) {
	status.RegisterClientStatusDiscoveryServiceServer(server, cs)
	cs.ctx = ctx
	cs.ds.StatusReporter = NewAggregateDistributionTracker(cs.ds.StatusReporter, []DistributionEventHandler{cs})
}

func (cs *CSDSServer) StreamClientStatus(stream status.ClientStatusDiscoveryService_StreamClientStatusServer) error {
	return cs.handler(stream)
	//stream.
	return nil
}
func (cs *CSDSServer) FetchClientStatus(ctx context.Context, req *status.ClientStatusRequest) (res *status.ClientStatusResponse, err error) {
	result := &status.ClientStatusResponse{
		Config: []*status.ClientConfig{},
	}
	for _, c := range cs.ds.adsClients {
		node := c.xdsNode
		for _, nm := range req.NodeMatchers {
			matches, err := matches(nm, node)
			if err != nil {
				return nil, err
			}
			if matches {
				cfg, ierr := cs.BuildResponseForConn(c)
				if ierr != nil {
					err = multierror.Append(err, ierr)
				} else {
					result.Config = append(result.Config, cfg)
				}
			}
		}
	}
	return result, nil
}

func anymatches(matchers []*envoy_type_matcher_v3.NodeMatcher, node *core.Node) (bool, error) {
	var err error
	var res bool
	for _, nm := range matchers {
		res, err = matches(nm, node)
		if res {
			return true, nil
		}
	}
	return false, err
}

func matches(matcher *envoy_type_matcher_v3.NodeMatcher, node *core.Node) (bool, error) {
	success, err := nodematches(matcher.NodeId, node)
	if err != nil {
		return false, err
	}
	if success {
		return true, nil
	} else {
		for _, metamatch := range matcher.NodeMetadatas {
			success, err = metamatches(metamatch, node)
			if err != nil {
				return false, err
			}
			if success {
				return true, nil
			}
		}
	}
	return false, nil
}

func metamatches(_ *envoy_type_matcher_v3.StructMatcher, _ *core.Node) (bool, error) {
	// TODO:
	return false, errors.New("metadata matching not yet supported")
}

func nodematches(matcher *envoy_type_matcher_v3.StringMatcher, node *core.Node) (bool, error) {
	nodeID := node.Id
	exact := matcher.GetExact()
	prefix := matcher.GetPrefix()
	suffix := matcher.GetSuffix()
	if matcher.GetIgnoreCase() {
		nodeID = strings.ToLower(nodeID)
		exact = strings.ToLower(exact)
		prefix = strings.ToLower(prefix)
		suffix = strings.ToLower(suffix)
	}
	if exact != "" && exact == nodeID {
		return true, nil
	}
	if prefix != "" && strings.HasPrefix(nodeID, prefix) {
		return true, nil
	}
	if suffix != "" && strings.HasSuffix(nodeID, suffix) {
		return true, nil
	}
	if matcher.GetSafeRegex() != nil {
		return false, errors.New("regex matching not supported by this server")
	}
	return false, nil
}

func (cs *CSDSServer) BuildResponseForConn(conn *XdsConnection) (*status.ClientConfig, error) {
	return cs.BuildPartialResponseForConn(conn, true, true, true)
}

func (cs *CSDSServer) BuildPartialResponseForConn(conn *XdsConnection, rds bool, lds bool, cds bool) (*status.ClientConfig, error) {
	var masterCfg []*status.PerXdsConfig
	if cds {
		clusters, err := cs.ds.dumpClusters(conn)
		if err != nil {
			return nil, err
		}
		masterCfg = append(masterCfg, &status.PerXdsConfig{
			Status:       status.ConfigStatus_SYNCED, // TODO: calculate
			PerXdsConfig: &status.PerXdsConfig_ClusterConfig{ClusterConfig: clusters},
		})
	}
	if rds {
		routes, err := cs.ds.dumpRoutes(conn)
		if err != nil {
			return nil, err
		}
		masterCfg = append(masterCfg, &status.PerXdsConfig{
			Status:       status.ConfigStatus_SYNCED, // TODO: calculate
			PerXdsConfig: &status.PerXdsConfig_RouteConfig{RouteConfig: routes},
		})
	}
	if lds {
		listeners, err := cs.ds.dumpListeners(conn)
		if err != nil {
			return nil, err
		}
		masterCfg = append(masterCfg, &status.PerXdsConfig{
			Status:       status.ConfigStatus_SYNCED, // TODO: calculate
			PerXdsConfig: &status.PerXdsConfig_ListenerConfig{ListenerConfig: listeners},
		})
	}
	// TODO: handle err
	result := &status.ClientConfig{
		Node:      conn.xdsNode,
		XdsConfig: masterCfg,
	}
	return result, nil
}

func (cs *CSDSServer) RegisterDisconnect(conID string, types []EventType){
	// TODO: make this non-blocking
	conn := cs.ds.getProxyConnection(conID)
	lateboundResponse := func() (*status.ClientStatusResponse, error) {
		return &status.ClientStatusResponse{
			Config: []*status.ClientConfig{
				{
					Node: conn.xdsNode,
				},
			},
		}, nil
	}
	cs.SendToAllInterestedStreams(conn, lateboundResponse)

}

func (cs *CSDSServer) RegisterACK(conID string, eventType EventType, nonce string){
	// TODO: make this non-blocking!
	conn := cs.ds.getProxyConnection(conID)
	lateboundResponse := func() (*status.ClientStatusResponse, error) {
		resp, err := cs.BuildPartialResponseForConn(conn,
			eventType == RouteEventType, eventType == ListenerEventType, eventType == ClusterEventType)
		return &status.ClientStatusResponse{Config: []*status.ClientConfig{resp}}, err
	}

	cs.SendToAllInterestedStreams(conn, lateboundResponse)
}

func (cs *CSDSServer) SendToAllInterestedStreams(conn *XdsConnection, lateBoundResponse func() (*status.ClientStatusResponse, error)) {
	var resp *status.ClientStatusResponse

	for sendchan, request := range cs.w2 {
		shouldSend, err := anymatches(request.NodeMatchers, conn.xdsNode)
		if shouldSend {
			if resp == nil {
				resp, _ = lateBoundResponse()
				// TODO: handle err
			}
			// non-blocking send
			go func() {
				sendchan <- *resp
			}()
		} else if err != nil {
			log.Errorf("Error running csds node matcher: %v", err)
		}
	}
}

// handler converts a blocking read call to channels and initiates stream processing
func (cs *CSDSServer) handler(stream status.ClientStatusDiscoveryService_StreamClientStatusServer,) error {
	// a channel for receiving incoming requests
	reqCh := make(chan *status.ClientStatusRequest)
	reqStop := int32(0)
	go func() {
		for {
			req, err := stream.Recv()
			if atomic.LoadInt32(&reqStop) != 0 {
				return
			}
			if err != nil {
				close(reqCh)
				return
			}
			reqCh <- req
		}
	}()

	err := cs.process(stream, reqCh)

	// prevents writing to a closed channel if send failed on blocked recv
	// TODO(kuat) figure out how to unblock recv through gRPC API
	atomic.StoreInt32(&reqStop, 1)

	return err
}

// process handles a bi-di stream request
func (cs *CSDSServer) process(stream status.ClientStatusDiscoveryService_StreamClientStatusServer, reqCh <-chan *status.ClientStatusRequest) error {
	// a collection of watches per request type
	var values watches
	defer func() {
		values.clientsCancel()
	}()

	for {
		select {
		case <-cs.ctx.Done():
			return nil
		// config watcher can send the requested resources types in any order
		case resp, more := <-values.clientstatuses:
			if !more {
				return grpcstatus.Errorf(codes.Unavailable, "endpoints watch failed")
			}
			err := stream.Send(&resp)
			if err != nil {
				return err
			}
		case req, more := <-reqCh:
			// input stream ended or errored out
			if !more {
				return nil
			}
			if req == nil {
				return grpcstatus.Errorf(codes.Unavailable, "empty request")
			}
			// cancel existing watches to (re-)request a newer version
			if values.clientsCancel != nil {
				values.clientsCancel()
			}
			values.clientsCancel, values.clientstatuses = cs.setupWatch(*req)
		}
	}
}

func (cs *CSDSServer) setupWatch(request status.ClientStatusRequest) (func(), chan status.ClientStatusResponse) {
	c := make(chan status.ClientStatusResponse)
	// TODO: add mutex
	cs.w2[c] = request
	f := func() {
		delete(cs.w2, c)
		close(c)
	}
	return f, c
}

// watches for all xDS resource types
type watches struct {
	clientstatuses chan status.ClientStatusResponse
	clientsCancel  func()
}
