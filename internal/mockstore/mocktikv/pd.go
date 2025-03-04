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
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/mockstore/mocktikv/pd.go
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

package mocktikv

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"sync"
	"time"

	"github.com/pingcap/kvproto/pkg/metapb"
	"github.com/pingcap/kvproto/pkg/pdpb"
	"github.com/pkg/errors"
	pd "github.com/tikv/pd/client"
)

// Use global variables to prevent pdClients from creating duplicate timestamps.
var tsMu = struct {
	sync.Mutex
	physicalTS int64
	logicalTS  int64
}{}

type pdClient struct {
	cluster *Cluster
	// SafePoint set by `UpdateGCSafePoint`. Not to be confused with SafePointKV.
	gcSafePoint uint64
	// Represents the current safePoint of all services including TiDB, representing how much data they want to retain
	// in GC.
	serviceSafePoints map[string]uint64
	gcSafePointMu     sync.Mutex
}

// NewPDClient creates a mock pd.Client that uses local timestamp and meta data
// from a Cluster.
func NewPDClient(cluster *Cluster) pd.Client {
	return &pdClient{
		cluster:           cluster,
		serviceSafePoints: make(map[string]uint64),
	}
}

func (c *pdClient) LoadGlobalConfig(ctx context.Context, names []string) ([]pd.GlobalConfigItem, error) {
	return nil, nil
}

func (c *pdClient) StoreGlobalConfig(ctx context.Context, items []pd.GlobalConfigItem) error {
	return nil
}

func (c *pdClient) WatchGlobalConfig(ctx context.Context) (chan []pd.GlobalConfigItem, error) {
	return nil, nil
}

func (c *pdClient) GetClusterID(ctx context.Context) uint64 {
	return 1
}

func (c *pdClient) GetTS(context.Context) (int64, int64, error) {
	tsMu.Lock()
	defer tsMu.Unlock()

	ts := time.Now().UnixNano() / int64(time.Millisecond)
	if tsMu.physicalTS >= ts {
		tsMu.logicalTS++
	} else {
		tsMu.physicalTS = ts
		tsMu.logicalTS = 0
	}
	return tsMu.physicalTS, tsMu.logicalTS, nil
}

func (c *pdClient) GetLocalTS(ctx context.Context, dcLocation string) (int64, int64, error) {
	return c.GetTS(ctx)
}

func (c *pdClient) GetTSAsync(ctx context.Context) pd.TSFuture {
	return &mockTSFuture{c, ctx, false}
}

func (c *pdClient) GetLocalTSAsync(ctx context.Context, dcLocation string) pd.TSFuture {
	return c.GetTSAsync(ctx)
}

type mockTSFuture struct {
	pdc  *pdClient
	ctx  context.Context
	used bool
}

func (m *mockTSFuture) Wait() (int64, int64, error) {
	if m.used {
		return 0, 0, errors.New("cannot wait tso twice")
	}
	m.used = true
	return m.pdc.GetTS(m.ctx)
}

func (c *pdClient) GetRegion(ctx context.Context, key []byte, opts ...pd.GetRegionOption) (*pd.Region, error) {
	region, peer, buckets := c.cluster.GetRegionByKey(key)
	if len(opts) == 0 {
		buckets = nil
	}
	return &pd.Region{Meta: region, Leader: peer, Buckets: buckets}, nil
}

func (c *pdClient) GetRegionFromMember(ctx context.Context, key []byte, memberURLs []string) (*pd.Region, error) {
	return &pd.Region{}, nil
}

func (c *pdClient) GetPrevRegion(ctx context.Context, key []byte, opts ...pd.GetRegionOption) (*pd.Region, error) {
	region, peer, buckets := c.cluster.GetPrevRegionByKey(key)
	if len(opts) == 0 {
		buckets = nil
	}
	return &pd.Region{Meta: region, Leader: peer, Buckets: buckets}, nil
}

func (c *pdClient) GetRegionByID(ctx context.Context, regionID uint64, opts ...pd.GetRegionOption) (*pd.Region, error) {
	region, peer, buckets := c.cluster.GetRegionByID(regionID)
	return &pd.Region{Meta: region, Leader: peer, Buckets: buckets}, nil
}

func (c *pdClient) ScanRegions(ctx context.Context, startKey []byte, endKey []byte, limit int) ([]*pd.Region, error) {
	regions := c.cluster.ScanRegions(startKey, endKey, limit)
	return regions, nil
}

func (c *pdClient) GetStore(ctx context.Context, storeID uint64) (*metapb.Store, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
	}
	store := c.cluster.GetStore(storeID)
	// It's same as PD's implementation.
	if store == nil {
		return nil, fmt.Errorf("invalid store ID %d, not found", storeID)
	}
	if store.GetState() == metapb.StoreState_Tombstone {
		return nil, nil
	}
	return store, nil
}

func (c *pdClient) GetAllStores(ctx context.Context, opts ...pd.GetStoreOption) ([]*metapb.Store, error) {
	return c.cluster.GetAllStores(), nil
}

func (c *pdClient) UpdateGCSafePoint(ctx context.Context, safePoint uint64) (uint64, error) {
	c.gcSafePointMu.Lock()
	defer c.gcSafePointMu.Unlock()

	if safePoint > c.gcSafePoint {
		c.gcSafePoint = safePoint
	}
	return c.gcSafePoint, nil
}

func (c *pdClient) UpdateServiceGCSafePoint(ctx context.Context, serviceID string, ttl int64, safePoint uint64) (uint64, error) {
	c.gcSafePointMu.Lock()
	defer c.gcSafePointMu.Unlock()

	if ttl == 0 {
		delete(c.serviceSafePoints, serviceID)
	} else {
		var minSafePoint uint64 = math.MaxUint64
		for _, ssp := range c.serviceSafePoints {
			if ssp < minSafePoint {
				minSafePoint = ssp
			}
		}

		if len(c.serviceSafePoints) == 0 || minSafePoint <= safePoint {
			c.serviceSafePoints[serviceID] = safePoint
		}
	}

	// The minSafePoint may have changed. Reload it.
	var minSafePoint uint64 = math.MaxUint64
	for _, ssp := range c.serviceSafePoints {
		if ssp < minSafePoint {
			minSafePoint = ssp
		}
	}
	return minSafePoint, nil
}

func (c *pdClient) Close() {
}

func (c *pdClient) ScatterRegion(ctx context.Context, regionID uint64) error {
	return nil
}

func (c *pdClient) ScatterRegions(ctx context.Context, regionsID []uint64, opts ...pd.RegionsOption) (*pdpb.ScatterRegionResponse, error) {
	return nil, nil
}

func (c *pdClient) SplitRegions(ctx context.Context, splitKeys [][]byte, opts ...pd.RegionsOption) (*pdpb.SplitRegionsResponse, error) {
	regionsID := make([]uint64, 0, len(splitKeys))
	for i, key := range splitKeys {
		k := NewMvccKey(key)
		region, _, _ := c.cluster.GetRegionByKey(k)
		if bytes.Equal(region.GetStartKey(), key) {
			continue
		}
		if i == 0 {
			regionsID = append(regionsID, region.Id)
		}
		newRegionID, newPeerIDs := c.cluster.AllocID(), c.cluster.AllocIDs(len(region.Peers))
		newRegion := c.cluster.SplitRaw(region.GetId(), newRegionID, k, newPeerIDs, newPeerIDs[0])
		regionsID = append(regionsID, newRegion.Id)
	}
	response := &pdpb.SplitRegionsResponse{
		Header:             &pdpb.ResponseHeader{},
		FinishedPercentage: 100,
		RegionsId:          regionsID,
	}
	return response, nil
}

func (c *pdClient) GetOperator(ctx context.Context, regionID uint64) (*pdpb.GetOperatorResponse, error) {
	return &pdpb.GetOperatorResponse{Status: pdpb.OperatorStatus_SUCCESS}, nil
}

func (c *pdClient) GetAllMembers(ctx context.Context) ([]*pdpb.Member, error) {
	return nil, nil
}

func (c *pdClient) GetLeaderAddr() string { return "mockpd" }

func (c *pdClient) UpdateOption(option pd.DynamicOption, value interface{}) error {
	return nil
}
