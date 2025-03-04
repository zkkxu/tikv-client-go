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
// https://github.com/pingcap/tidb/tree/cc5e161ac06827589c4966674597c137cc9e809c/store/tikv/prewrite.go
//

// Copyright 2020 PingCAP, Inc.
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

package transaction

import (
	"encoding/hex"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pkg/errors"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/tikv/client-go/v2/config"
	tikverr "github.com/tikv/client-go/v2/error"
	"github.com/tikv/client-go/v2/internal/client"
	"github.com/tikv/client-go/v2/internal/locate"
	"github.com/tikv/client-go/v2/internal/logutil"
	"github.com/tikv/client-go/v2/internal/retry"
	"github.com/tikv/client-go/v2/metrics"
	"github.com/tikv/client-go/v2/tikvrpc"
	"github.com/tikv/client-go/v2/txnkv/txnlock"
	"github.com/tikv/client-go/v2/util"
	"go.uber.org/zap"
)

type actionPrewrite struct{ retry bool }

var _ twoPhaseCommitAction = actionPrewrite{}

func (actionPrewrite) String() string {
	return "prewrite"
}

func (actionPrewrite) tiKVTxnRegionsNumHistogram() prometheus.Observer {
	return metrics.TxnRegionsNumHistogramPrewrite
}

func (c *twoPhaseCommitter) buildPrewriteRequest(batch batchMutations, txnSize uint64) *tikvrpc.Request {
	m := batch.mutations
	mutations := make([]*kvrpcpb.Mutation, m.Len())
	isPessimisticLock := make([]bool, m.Len())
	for i := 0; i < m.Len(); i++ {
		assertion := kvrpcpb.Assertion_None
		if m.IsAssertExists(i) {
			assertion = kvrpcpb.Assertion_Exist
		}
		if m.IsAssertNotExist(i) {
			assertion = kvrpcpb.Assertion_NotExist
		}
		mutations[i] = &kvrpcpb.Mutation{
			Op:        m.GetOp(i),
			Key:       m.GetKey(i),
			Value:     m.GetValue(i),
			Assertion: assertion,
		}
		isPessimisticLock[i] = m.IsPessimisticLock(i)
	}
	c.mu.Lock()
	minCommitTS := c.minCommitTS
	c.mu.Unlock()
	if c.forUpdateTS > 0 && c.forUpdateTS >= minCommitTS {
		minCommitTS = c.forUpdateTS + 1
	} else if c.startTS >= minCommitTS {
		minCommitTS = c.startTS + 1
	}

	if val, err := util.EvalFailpoint("mockZeroCommitTS"); err == nil {
		// Should be val.(uint64) but failpoint doesn't support that.
		if tmp, ok := val.(int); ok && uint64(tmp) == c.startTS {
			minCommitTS = 0
		}
	}

	ttl := c.lockTTL

	if c.sessionID > 0 {
		if _, err := util.EvalFailpoint("twoPCShortLockTTL"); err == nil {
			ttl = 1
			keys := make([]string, 0, len(mutations))
			for _, m := range mutations {
				keys = append(keys, hex.EncodeToString(m.Key))
			}
			logutil.BgLogger().Info("[failpoint] injected lock ttl = 1 on prewrite",
				zap.Uint64("txnStartTS", c.startTS), zap.Strings("keys", keys))
		}
	}

	assertionLevel := c.txn.assertionLevel
	if _, err := util.EvalFailpoint("assertionSkipCheckFromPrewrite"); err == nil {
		assertionLevel = kvrpcpb.AssertionLevel_Off
	}

	req := &kvrpcpb.PrewriteRequest{
		Mutations:         mutations,
		PrimaryLock:       c.primary(),
		StartVersion:      c.startTS,
		LockTtl:           ttl,
		IsPessimisticLock: isPessimisticLock,
		ForUpdateTs:       c.forUpdateTS,
		TxnSize:           txnSize,
		MinCommitTs:       minCommitTS,
		MaxCommitTs:       c.maxCommitTS,
		AssertionLevel:    assertionLevel,
	}

	if _, err := util.EvalFailpoint("invalidMaxCommitTS"); err == nil {
		if req.MaxCommitTs > 0 {
			req.MaxCommitTs = minCommitTS - 1
		}
	}

	if c.isAsyncCommit() {
		if batch.isPrimary {
			req.Secondaries = c.asyncSecondaries()
		}
		req.UseAsyncCommit = true
	}

	if c.isOnePC() {
		req.TryOnePc = true
	}

	r := tikvrpc.NewRequest(tikvrpc.CmdPrewrite, req,
		kvrpcpb.Context{Priority: c.priority, SyncLog: c.syncLog, ResourceGroupTag: c.resourceGroupTag,
			DiskFullOpt: c.diskFullOpt, MaxExecutionDurationMs: uint64(client.MaxWriteExecutionTime.Milliseconds())})
	if c.resourceGroupTag == nil && c.resourceGroupTagger != nil {
		c.resourceGroupTagger(r)
	}
	return r
}

func (action actionPrewrite) handleSingleBatch(c *twoPhaseCommitter, bo *retry.Backoffer, batch batchMutations) (err error) {
	// WARNING: This function only tries to send a single request to a single region, so it don't
	// need to unset the `useOnePC` flag when it fails. A special case is that when TiKV returns
	// regionErr, it's uncertain if the request will be splitted into multiple and sent to multiple
	// regions. It invokes `prewriteMutations` recursively here, and the number of batches will be
	// checked there.

	if c.sessionID > 0 {
		if batch.isPrimary {
			if _, err := util.EvalFailpoint("prewritePrimaryFail"); err == nil {
				// Delay to avoid cancelling other normally ongoing prewrite requests.
				time.Sleep(time.Millisecond * 50)
				logutil.Logger(bo.GetCtx()).Info("[failpoint] injected error on prewriting primary batch",
					zap.Uint64("txnStartTS", c.startTS))
				return errors.New("injected error on prewriting primary batch")
			}
			util.EvalFailpoint("prewritePrimary") // for other failures like sleep or pause
		} else {
			if _, err := util.EvalFailpoint("prewriteSecondaryFail"); err == nil {
				// Delay to avoid cancelling other normally ongoing prewrite requests.
				time.Sleep(time.Millisecond * 50)
				logutil.Logger(bo.GetCtx()).Info("[failpoint] injected error on prewriting secondary batch",
					zap.Uint64("txnStartTS", c.startTS))
				return errors.New("injected error on prewriting secondary batch")
			}
			util.EvalFailpoint("prewriteSecondary") // for other failures like sleep or pause
			// concurrent failpoint sleep doesn't work as expected. So we need a separate fail point.
			// `1*sleep()` can block multiple concurrent threads that meet the failpoint.
			if val, err := util.EvalFailpoint("prewriteSecondarySleep"); err == nil {
				time.Sleep(time.Millisecond * time.Duration(val.(int)))
			}
		}
	}

	txnSize := uint64(c.regionTxnSize[batch.region.GetID()])
	// When we retry because of a region miss, we don't know the transaction size. We set the transaction size here
	// to MaxUint64 to avoid unexpected "resolve lock lite".
	if action.retry {
		txnSize = math.MaxUint64
	}

	tBegin := time.Now()
	attempts := 0

	req := c.buildPrewriteRequest(batch, txnSize)
	sender := locate.NewRegionRequestSender(c.store.GetRegionCache(), c.store.GetTiKVClient())
	defer func() {
		if err != nil {
			// If we fail to receive response for async commit prewrite, it will be undetermined whether this
			// transaction has been successfully committed.
			// If prewrite has been cancelled, all ongoing prewrite RPCs will become errors, we needn't set undetermined
			// errors.
			if (c.isAsyncCommit() || c.isOnePC()) && sender.GetRPCError() != nil && atomic.LoadUint32(&c.prewriteCancelled) == 0 {
				c.setUndeterminedErr(sender.GetRPCError())
			}
		}
	}()
	for {
		attempts++
		if time.Since(tBegin) > slowRequestThreshold {
			logutil.BgLogger().Warn("slow prewrite request", zap.Uint64("startTS", c.startTS), zap.Stringer("region", &batch.region), zap.Int("attempts", attempts))
			tBegin = time.Now()
		}

		resp, err := sender.SendReq(bo, req, batch.region, client.ReadTimeoutShort)
		// Unexpected error occurs, return it
		if err != nil {
			return err
		}

		regionErr, err := resp.GetRegionError()
		if err != nil {
			return err
		}
		if regionErr != nil {
			// For other region error and the fake region error, backoff because
			// there's something wrong.
			// For the real EpochNotMatch error, don't backoff.
			if regionErr.GetEpochNotMatch() == nil || locate.IsFakeRegionError(regionErr) {
				err = bo.Backoff(retry.BoRegionMiss, errors.New(regionErr.String()))
				if err != nil {
					return err
				}
			}
			if regionErr.GetDiskFull() != nil {
				storeIds := regionErr.GetDiskFull().GetStoreId()
				desc := " "
				for _, i := range storeIds {
					desc += strconv.FormatUint(i, 10) + " "
				}

				logutil.Logger(bo.GetCtx()).Error("Request failed cause of TiKV disk full",
					zap.String("store_id", desc),
					zap.String("reason", regionErr.GetDiskFull().GetReason()))

				return errors.New(regionErr.String())
			}
			same, err := batch.relocate(bo, c.store.GetRegionCache())
			if err != nil {
				return err
			}
			if same {
				continue
			}
			err = c.doActionOnMutations(bo, actionPrewrite{true}, batch.mutations)
			return err
		}

		if resp.Resp == nil {
			return errors.WithStack(tikverr.ErrBodyMissing)
		}
		prewriteResp := resp.Resp.(*kvrpcpb.PrewriteResponse)
		keyErrs := prewriteResp.GetErrors()
		if len(keyErrs) == 0 {
			// Clear the RPC Error since the request is evaluated successfully.
			sender.SetRPCError(nil)

			if batch.isPrimary {
				// After writing the primary key, if the size of the transaction is larger than 32M,
				// start the ttlManager. The ttlManager will be closed in tikvTxn.Commit().
				// In this case 1PC is not expected to be used, but still check it for safety.
				if int64(c.txnSize) > config.GetGlobalConfig().TiKVClient.TTLRefreshedTxnSize &&
					prewriteResp.OnePcCommitTs == 0 {
					c.run(c, nil)
				}
			}

			if c.isOnePC() {
				if prewriteResp.OnePcCommitTs == 0 {
					if prewriteResp.MinCommitTs != 0 {
						return errors.New("MinCommitTs must be 0 when 1pc falls back to 2pc")
					}
					logutil.Logger(bo.GetCtx()).Warn("1pc failed and fallbacks to normal commit procedure",
						zap.Uint64("startTS", c.startTS))
					metrics.OnePCTxnCounterFallback.Inc()
					c.setOnePC(false)
					c.setAsyncCommit(false)
				} else {
					// For 1PC, there's no racing to access to access `onePCCommmitTS` so it's safe
					// not to lock the mutex.
					if c.onePCCommitTS != 0 {
						logutil.Logger(bo.GetCtx()).Fatal("one pc happened multiple times",
							zap.Uint64("startTS", c.startTS))
					}
					c.onePCCommitTS = prewriteResp.OnePcCommitTs
				}
				return nil
			} else if prewriteResp.OnePcCommitTs != 0 {
				logutil.Logger(bo.GetCtx()).Fatal("tikv committed a non-1pc transaction with 1pc protocol",
					zap.Uint64("startTS", c.startTS))
			}
			if c.isAsyncCommit() {
				// 0 if the min_commit_ts is not ready or any other reason that async
				// commit cannot proceed. The client can then fallback to normal way to
				// continue committing the transaction if prewrite are all finished.
				if prewriteResp.MinCommitTs == 0 {
					if c.testingKnobs.noFallBack {
						return nil
					}
					logutil.Logger(bo.GetCtx()).Warn("async commit cannot proceed since the returned minCommitTS is zero, "+
						"fallback to normal path", zap.Uint64("startTS", c.startTS))
					c.setAsyncCommit(false)
				} else {
					c.mu.Lock()
					if prewriteResp.MinCommitTs > c.minCommitTS {
						c.minCommitTS = prewriteResp.MinCommitTs
					}
					c.mu.Unlock()
				}
			}
			return nil
		}
		var locks []*txnlock.Lock
		for _, keyErr := range keyErrs {
			// Check already exists error
			if alreadyExist := keyErr.GetAlreadyExist(); alreadyExist != nil {
				e := &tikverr.ErrKeyExist{AlreadyExist: alreadyExist}
				return c.extractKeyExistsErr(e)
			}

			// Extract lock from key error
			lock, err1 := txnlock.ExtractLockFromKeyErr(keyErr)
			if err1 != nil {
				return err1
			}
			logutil.BgLogger().Info("prewrite encounters lock",
				zap.Uint64("session", c.sessionID),
				zap.Uint64("txnID", c.startTS),
				zap.Stringer("lock", lock))
			// If an optimistic transaction encounters a lock with larger TS, this transaction will certainly
			// fail due to a WriteConflict error. So we can construct and return an error here early.
			// Pessimistic transactions don't need such an optimization. If this key needs a pessimistic lock,
			// TiKV will return a PessimisticLockNotFound error directly if it encounters a different lock. Otherwise,
			// TiKV returns lock.TTL = 0, and we still need to resolve the lock.
			if lock.TxnID > c.startTS && !c.isPessimistic {
				return tikverr.NewErrWriteConfictWithArgs(c.startTS, lock.TxnID, 0, lock.Key)
			}
			locks = append(locks, lock)
		}
		start := time.Now()
		msBeforeExpired, err := c.store.GetLockResolver().ResolveLocks(bo, c.startTS, locks)
		if err != nil {
			return err
		}
		atomic.AddInt64(&c.getDetail().ResolveLockTime, int64(time.Since(start)))
		if msBeforeExpired > 0 {
			err = bo.BackoffWithCfgAndMaxSleep(retry.BoTxnLock, int(msBeforeExpired), errors.Errorf("2PC prewrite lockedKeys: %d", len(locks)))
			if err != nil {
				return err
			}
		}
	}
}

func (c *twoPhaseCommitter) prewriteMutations(bo *retry.Backoffer, mutations CommitterMutations) error {
	if span := opentracing.SpanFromContext(bo.GetCtx()); span != nil && span.Tracer() != nil {
		span1 := span.Tracer().StartSpan("twoPhaseCommitter.prewriteMutations", opentracing.ChildOf(span.Context()))
		defer span1.Finish()
		bo.SetCtx(opentracing.ContextWithSpan(bo.GetCtx(), span1))
	}

	// `doActionOnMutations` will unset `useOnePC` if the mutations is splitted into multiple batches.
	return c.doActionOnMutations(bo, actionPrewrite{}, mutations)
}
