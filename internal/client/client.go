// Copyright 2021 TiKV Authors
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

// NOTE: The code in this file is based on code from the
// TiDB project, licensed under the Apache License v 2.0
//
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/client/client.go
//

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
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package client provides tcp connection to kvserver.
package client

import (
	"context"
	"fmt"
	"io"
	"math"
	"runtime/trace"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	grpc_opentracing "github.com/grpc-ecosystem/go-grpc-middleware/tracing/opentracing"
	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/kvproto/pkg/coprocessor"
	"github.com/pingcap/kvproto/pkg/debugpb"
	"github.com/pingcap/kvproto/pkg/mpp"
	"github.com/pingcap/kvproto/pkg/tikvpb"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/client-go/v2/config"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/logutil"
	"github.com/tikv/client-go/v2/metrics"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
)

// MaxRecvMsgSize set max gRPC receive message size received from server. If any message size is larger than
// current value, an error will be reported from gRPC.
var MaxRecvMsgSize = math.MaxInt64 - 1

// Timeout durations.
const (
	dialTimeout       = 5 * time.Second
	ReadTimeoutShort  = 30 * time.Second // For requests that read/write several key-values.
	ReadTimeoutMedium = 60 * time.Second // For requests that may need scan region.

	// MaxWriteExecutionTime is the MaxExecutionDurationMs field for write requests.
	// Because the last deadline check is before proposing, let us give it 10 more seconds
	// after proposing.
	MaxWriteExecutionTime = ReadTimeoutShort - 10*time.Second
)

// Grpc window size
const (
	GrpcInitialWindowSize     = 1 << 30
	GrpcInitialConnWindowSize = 1 << 30
)

// forwardMetadataKey is the key of gRPC metadata which represents a forwarded request.
const forwardMetadataKey = "tikv-forwarded-host"

// Client is a client that sends RPC.
// It should not be used after calling Close().
type Client interface {
	// Close should release all data.
	Close() error
	// CloseAddr closes gRPC connections to the address. It will reconnect the next time it's used.
	CloseAddr(addr string) error
	// SendRequest sends Request.
	SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error)
}

type connArray struct {
	// The target host.
	target string

	index uint32
	v     []*grpc.ClientConn
	// streamTimeout binds with a background goroutine to process coprocessor streaming timeout.
	streamTimeout chan *tikvrpc.Lease
	dialTimeout   time.Duration
	// batchConn is not null when batch is enabled.
	*batchConn
	done chan struct{}
}

func newConnArray(maxSize uint, addr string, security config.Security, idleNotify *uint32, enableBatch bool, dialTimeout time.Duration) (*connArray, error) {
	a := &connArray{
		index:         0,
		v:             make([]*grpc.ClientConn, maxSize),
		streamTimeout: make(chan *tikvrpc.Lease, 1024),
		done:          make(chan struct{}),
		dialTimeout:   dialTimeout,
	}
	if err := a.Init(addr, security, idleNotify, enableBatch); err != nil {
		return nil, err
	}
	return a, nil
}

func (a *connArray) Init(addr string, security config.Security, idleNotify *uint32, enableBatch bool) error {
	a.target = addr

	opt := grpc.WithTransportCredentials(insecure.NewCredentials())
	if len(security.ClusterSSLCA) != 0 {
		tlsConfig, err := security.ToTLSConfig()
		if err != nil {
			return errors.WithStack(err)
		}
		opt = grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig))
	}

	cfg := config.GetGlobalConfig()
	var (
		unaryInterceptor  grpc.UnaryClientInterceptor
		streamInterceptor grpc.StreamClientInterceptor
	)
	if cfg.OpenTracingEnable {
		unaryInterceptor = grpc_opentracing.UnaryClientInterceptor()
		streamInterceptor = grpc_opentracing.StreamClientInterceptor()
	}

	allowBatch := (cfg.TiKVClient.MaxBatchSize > 0) && enableBatch
	if allowBatch {
		a.batchConn = newBatchConn(uint(len(a.v)), cfg.TiKVClient.MaxBatchSize, idleNotify)
		a.pendingRequests = metrics.TiKVBatchPendingRequests.WithLabelValues(a.target)
		a.batchSize = metrics.TiKVBatchRequests.WithLabelValues(a.target)
	}
	keepAlive := cfg.TiKVClient.GrpcKeepAliveTime
	keepAliveTimeout := cfg.TiKVClient.GrpcKeepAliveTimeout
	for i := range a.v {
		ctx, cancel := context.WithTimeout(context.Background(), a.dialTimeout)
		var callOptions []grpc.CallOption
		callOptions = append(callOptions, grpc.MaxCallRecvMsgSize(MaxRecvMsgSize))
		if cfg.TiKVClient.GrpcCompressionType == gzip.Name {
			callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
		}
		conn, err := grpc.DialContext(
			ctx,
			addr,
			opt,
			grpc.WithInitialWindowSize(GrpcInitialWindowSize),
			grpc.WithInitialConnWindowSize(GrpcInitialConnWindowSize),
			grpc.WithUnaryInterceptor(unaryInterceptor),
			grpc.WithStreamInterceptor(streamInterceptor),
			grpc.WithDefaultCallOptions(callOptions...),
			grpc.WithConnectParams(grpc.ConnectParams{
				Backoff: backoff.Config{
					BaseDelay:  100 * time.Millisecond, // Default was 1s.
					Multiplier: 1.6,                    // Default
					Jitter:     0.2,                    // Default
					MaxDelay:   3 * time.Second,        // Default was 120s.
				},
				MinConnectTimeout: a.dialTimeout,
			}),
			grpc.WithKeepaliveParams(keepalive.ClientParameters{
				Time:                time.Duration(keepAlive) * time.Second,
				Timeout:             time.Duration(keepAliveTimeout) * time.Second,
				PermitWithoutStream: true,
			}),
		)
		cancel()
		if err != nil {
			// Cleanup if the initialization fails.
			a.Close()
			return errors.WithStack(err)
		}
		a.v[i] = conn

		if allowBatch {
			batchClient := &batchCommandsClient{
				target:           a.target,
				conn:             conn,
				forwardedClients: make(map[string]*batchCommandsStream),
				batched:          sync.Map{},
				epoch:            0,
				closed:           0,
				tikvClientCfg:    cfg.TiKVClient,
				tikvLoad:         &a.tikvTransportLayerLoad,
				dialTimeout:      a.dialTimeout,
				tryLock:          tryLock{sync.NewCond(new(sync.Mutex)), false},
			}
			a.batchCommandsClients = append(a.batchCommandsClients, batchClient)
		}
	}
	go tikvrpc.CheckStreamTimeoutLoop(a.streamTimeout, a.done)
	if allowBatch {
		go a.batchSendLoop(cfg.TiKVClient)
	}

	return nil
}

func (a *connArray) Get() *grpc.ClientConn {
	next := atomic.AddUint32(&a.index, 1) % uint32(len(a.v))
	return a.v[next]
}

func (a *connArray) Close() {
	if a.batchConn != nil {
		a.batchConn.Close()
	}

	for _, c := range a.v {
		err := c.Close()
		tikverr.Log(err)
	}

	close(a.done)
}

// Opt is the option for the client.
type Opt func(*RPCClient)

// WithSecurity is used to set the security config.
func WithSecurity(security config.Security) Opt {
	return func(c *RPCClient) {
		c.security = security
	}
}

// RPCClient is RPC client struct.
// TODO: Add flow control between RPC clients in TiDB ond RPC servers in TiKV.
// Since we use shared client connection to communicate to the same TiKV, it's possible
// that there are too many concurrent requests which overload the service of TiKV.
type RPCClient struct {
	sync.RWMutex

	conns    map[string]*connArray
	security config.Security

	idleNotify uint32

	// Periodically check whether there is any connection that is idle and then close and remove these connections.
	// Implement background cleanup.
	isClosed    bool
	dialTimeout time.Duration
}

// NewRPCClient creates a client that manages connections and rpc calls with tikv-servers.
func NewRPCClient(opts ...Opt) *RPCClient {
	cli := &RPCClient{
		conns:       make(map[string]*connArray),
		dialTimeout: dialTimeout,
	}
	for _, opt := range opts {
		opt(cli)
	}
	return cli
}

func (c *RPCClient) getConnArray(addr string, enableBatch bool, opt ...func(cfg *config.TiKVClient)) (*connArray, error) {
	c.RLock()
	if c.isClosed {
		c.RUnlock()
		return nil, errors.Errorf("rpcClient is closed")
	}
	array, ok := c.conns[addr]
	c.RUnlock()
	if !ok {
		var err error
		array, err = c.createConnArray(addr, enableBatch, opt...)
		if err != nil {
			return nil, err
		}
	}

	// An idle connArray will not change to active again, this avoid the race condition
	// that recycling idle connection close an active connection unexpectedly (idle -> active).
	if array.batchConn != nil && array.isIdle() {
		return nil, errors.Errorf("rpcClient is idle")
	}

	return array, nil
}

func (c *RPCClient) createConnArray(addr string, enableBatch bool, opts ...func(cfg *config.TiKVClient)) (*connArray, error) {
	c.Lock()
	defer c.Unlock()
	array, ok := c.conns[addr]
	if !ok {
		var err error
		client := config.GetGlobalConfig().TiKVClient
		for _, opt := range opts {
			opt(&client)
		}
		array, err = newConnArray(client.GrpcConnectionCount, addr, c.security, &c.idleNotify, enableBatch, c.dialTimeout)
		if err != nil {
			return nil, err
		}
		c.conns[addr] = array
	}
	return array, nil
}

func (c *RPCClient) closeConns() {
	c.Lock()
	if !c.isClosed {
		c.isClosed = true
		// close all connections
		for _, array := range c.conns {
			array.Close()
		}
	}
	c.Unlock()
}

var sendReqHistCache sync.Map

type sendReqHistCacheKey struct {
	tp       tikvrpc.CmdType
	id       uint64
	staleRad bool
}

func (c *RPCClient) updateTiKVSendReqHistogram(req *tikvrpc.Request, start time.Time, staleRead bool) {
	key := sendReqHistCacheKey{
		req.Type,
		req.Context.GetPeer().GetStoreId(),
		staleRead,
	}

	v, ok := sendReqHistCache.Load(key)
	if !ok {
		reqType := req.Type.String()
		storeID := strconv.FormatUint(req.Context.GetPeer().GetStoreId(), 10)
		v = metrics.TiKVSendReqHistogram.WithLabelValues(reqType, storeID, strconv.FormatBool(staleRead))
		sendReqHistCache.Store(key, v)
	}

	v.(prometheus.Observer).Observe(time.Since(start).Seconds())
}

// SendRequest sends a Request to server and receives Response.
func (c *RPCClient) SendRequest(ctx context.Context, addr string, req *tikvrpc.Request, timeout time.Duration) (*tikvrpc.Response, error) {
	if span := opentracing.SpanFromContext(ctx); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan(fmt.Sprintf("rpcClient.SendRequest, region ID: %d, type: %s", req.RegionId, req.Type), opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		ctx = opentracing.ContextWithSpan(ctx, span1)
	}

	if atomic.CompareAndSwapUint32(&c.idleNotify, 1, 0) {
		go c.recycleIdleConnArray()
	}

	// TiDB will not send batch commands to TiFlash, to resolve the conflict with Batch Cop Request.
	enableBatch := req.StoreTp != tikvrpc.TiDB && req.StoreTp != tikvrpc.TiFlash
	connArray, err := c.getConnArray(addr, enableBatch)
	if err != nil {
		return nil, err
	}

	start := time.Now()
	staleRead := req.GetStaleRead()
	defer func() {
		stmtExec := ctx.Value(util.ExecDetailsKey)
		if stmtExec != nil {
			detail := stmtExec.(*util.ExecDetails)
			atomic.AddInt64(&detail.WaitKVRespDuration, int64(time.Since(start)))
		}
		c.updateTiKVSendReqHistogram(req, start, staleRead)
	}()

	// TiDB RPC server supports batch RPC, but batch connection will send heart beat, It's not necessary since
	// request to TiDB is not high frequency.
	if config.GetGlobalConfig().TiKVClient.MaxBatchSize > 0 && enableBatch {
		if batchReq := req.ToBatchCommandsRequest(); batchReq != nil {
			defer trace.StartRegion(ctx, req.Type.String()).End()
			return sendBatchRequest(ctx, addr, req.ForwardedHost, connArray.batchConn, batchReq, timeout)
		}
	}

	clientConn := connArray.Get()
	if state := clientConn.GetState(); state == connectivity.TransientFailure {
		storeID := strconv.FormatUint(req.Context.GetPeer().GetStoreId(), 10)
		metrics.TiKVGRPCConnTransientFailureCounter.WithLabelValues(addr, storeID).Inc()
	}

	if req.IsDebugReq() {
		client := debugpb.NewDebugClient(clientConn)
		ctx1, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		return tikvrpc.CallDebugRPC(ctx1, client, req)
	}

	client := tikvpb.NewTikvClient(clientConn)

	// Set metadata for request forwarding. Needn't forward DebugReq.
	if req.ForwardedHost != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, forwardMetadataKey, req.ForwardedHost)
	}
	switch req.Type {
	case tikvrpc.CmdBatchCop:
		return c.getBatchCopStreamResponse(ctx, client, req, timeout, connArray)
	case tikvrpc.CmdCopStream:
		return c.getCopStreamResponse(ctx, client, req, timeout, connArray)
	case tikvrpc.CmdMPPConn:
		return c.getMPPStreamResponse(ctx, client, req, timeout, connArray)
	}
	// Or else it's a unary call.
	ctx1, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return tikvrpc.CallRPC(ctx1, client, req)
}

func (c *RPCClient) getCopStreamResponse(ctx context.Context, client tikvpb.TikvClient, req *tikvrpc.Request, timeout time.Duration, connArray *connArray) (*tikvrpc.Response, error) {
	// Coprocessor streaming request.
	// Use context to support timeout for grpc streaming client.
	ctx1, cancel := context.WithCancel(ctx)
	// Should NOT call defer cancel() here because it will cancel further stream.Recv()
	// We put it in copStream.Lease.Cancel call this cancel at copStream.Close
	// TODO: add unit test for SendRequest.
	resp, err := tikvrpc.CallRPC(ctx1, client, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// Put the lease object to the timeout channel, so it would be checked periodically.
	copStream := resp.Resp.(*tikvrpc.CopStreamResponse)
	copStream.Timeout = timeout
	copStream.Lease.Cancel = cancel
	connArray.streamTimeout <- &copStream.Lease

	// Read the first streaming response to get CopStreamResponse.
	// This can make error handling much easier, because SendReq() retry on
	// region error automatically.
	var first *coprocessor.Response
	first, err = copStream.Recv()
	if err != nil {
		if errors.Cause(err) != io.EOF {
			return nil, errors.WithStack(err)
		}
		logutil.BgLogger().Debug("copstream returns nothing for the request.")
	}
	copStream.Response = first
	return resp, nil

}

func (c *RPCClient) getBatchCopStreamResponse(ctx context.Context, client tikvpb.TikvClient, req *tikvrpc.Request, timeout time.Duration, connArray *connArray) (*tikvrpc.Response, error) {
	// Coprocessor streaming request.
	// Use context to support timeout for grpc streaming client.
	ctx1, cancel := context.WithCancel(ctx)
	// Should NOT call defer cancel() here because it will cancel further stream.Recv()
	// We put it in copStream.Lease.Cancel call this cancel at copStream.Close
	// TODO: add unit test for SendRequest.
	resp, err := tikvrpc.CallRPC(ctx1, client, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// Put the lease object to the timeout channel, so it would be checked periodically.
	copStream := resp.Resp.(*tikvrpc.BatchCopStreamResponse)
	copStream.Timeout = timeout
	copStream.Lease.Cancel = cancel
	connArray.streamTimeout <- &copStream.Lease

	// Read the first streaming response to get CopStreamResponse.
	// This can make error handling much easier, because SendReq() retry on
	// region error automatically.
	var first *coprocessor.BatchResponse
	first, err = copStream.Recv()
	if err != nil {
		if errors.Cause(err) != io.EOF {
			return nil, errors.WithStack(err)
		}
		logutil.BgLogger().Debug("batch copstream returns nothing for the request.")
	}
	copStream.BatchResponse = first
	return resp, nil
}

func (c *RPCClient) getMPPStreamResponse(ctx context.Context, client tikvpb.TikvClient, req *tikvrpc.Request, timeout time.Duration, connArray *connArray) (*tikvrpc.Response, error) {
	// MPP streaming request.
	// Use context to support timeout for grpc streaming client.
	ctx1, cancel := context.WithCancel(ctx)
	// Should NOT call defer cancel() here because it will cancel further stream.Recv()
	// We put it in copStream.Lease.Cancel call this cancel at copStream.Close
	// TODO: add unit test for SendRequest.
	resp, err := tikvrpc.CallRPC(ctx1, client, req)
	if err != nil {
		cancel()
		return nil, err
	}

	// Put the lease object to the timeout channel, so it would be checked periodically.
	copStream := resp.Resp.(*tikvrpc.MPPStreamResponse)
	copStream.Timeout = timeout
	copStream.Lease.Cancel = cancel
	connArray.streamTimeout <- &copStream.Lease

	// Read the first streaming response to get CopStreamResponse.
	// This can make error handling much easier, because SendReq() retry on
	// region error automatically.
	var first *mpp.MPPDataPacket
	first, err = copStream.Recv()
	if err != nil {
		if errors.Cause(err) != io.EOF {
			return nil, errors.WithStack(err)
		}
	}
	copStream.MPPDataPacket = first
	return resp, nil
}

// Close closes all connections.
func (c *RPCClient) Close() error {
	// TODO: add a unit test for SendRequest After Closed
	c.closeConns()
	return nil
}

// CloseAddr closes gRPC connections to the address.
func (c *RPCClient) CloseAddr(addr string) error {
	c.Lock()
	conn, ok := c.conns[addr]
	if ok {
		delete(c.conns, addr)
		logutil.BgLogger().Debug("close connection", zap.String("target", addr))
	}
	c.Unlock()

	if conn != nil {
		conn.Close()
	}
	return nil
}
