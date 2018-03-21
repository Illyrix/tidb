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

package tikv

import (
	"time"

	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/pd/pd-client"
	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/metrics"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
	"golang.org/x/net/context"
	"bytes"
)

var (
	// MaxRawKVScanLimit is the maximum scan limit for rawkv Scan.
	MaxRawKVScanLimit = 10240
	// ErrMaxScanLimitExceeded is returned when the limit for rawkv Scan is to large.
	ErrMaxScanLimitExceeded = errors.New("limit should be less than MaxRawKVScanLimit")
)

// RawKVClient is a client of TiKV server which is used as a key-value storage,
// only GET/PUT/DELETE commands are supported.
type RawKVClient struct {
	clusterID   uint64
	regionCache *RegionCache
	pdClient    pd.Client
	rpcClient   Client
}

// NewRawKVClient creates a client with PD cluster addrs.
func NewRawKVClient(pdAddrs []string, security config.Security) (*RawKVClient, error) {
	pdCli, err := pd.NewClient(pdAddrs, pd.SecurityOption{
		CAPath:   security.ClusterSSLCA,
		CertPath: security.ClusterSSLCert,
		KeyPath:  security.ClusterSSLKey,
	})
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &RawKVClient{
		clusterID:   pdCli.GetClusterID(context.TODO()),
		regionCache: NewRegionCache(pdCli),
		pdClient:    pdCli,
		rpcClient:   newRPCClient(security),
	}, nil
}

// Close closes the client.
func (c *RawKVClient) Close() error {
	c.pdClient.Close()
	return c.rpcClient.Close()
}

// ClusterID returns the TiKV cluster ID.
func (c *RawKVClient) ClusterID() uint64 {
	return c.clusterID
}

// Get queries value with the key. When the key does not exist, it returns `nil, nil`.
func (c *RawKVClient) Get(key []byte) ([]byte, error) {
	start := time.Now()
	defer func() { metrics.TiKVRawkvCmdHistogram.WithLabelValues("get").Observe(time.Since(start).Seconds()) }()

	req := &tikvrpc.Request{
		Type: tikvrpc.CmdRawGet,
		RawGet: &kvrpcpb.RawGetRequest{
			Key: key,
		},
	}
	resp, _, err := c.sendReq(key, req)
	if err != nil {
		return nil, errors.Trace(err)
	}
	cmdResp := resp.RawGet
	if cmdResp == nil {
		return nil, errors.Trace(ErrBodyMissing)
	}
	if cmdResp.GetError() != "" {
		return nil, errors.New(cmdResp.GetError())
	}
	if len(cmdResp.Value) == 0 {
		return nil, nil
	}
	return cmdResp.Value, nil
}

// Put stores a key-value pair to TiKV.
func (c *RawKVClient) Put(key, value []byte) error {
	start := time.Now()
	defer func() { metrics.TiKVRawkvCmdHistogram.WithLabelValues("put").Observe(time.Since(start).Seconds()) }()
	metrics.TiKVRawkvSizeHistogram.WithLabelValues("key").Observe(float64(len(key)))
	metrics.TiKVRawkvSizeHistogram.WithLabelValues("value").Observe(float64(len(value)))

	if len(value) == 0 {
		return errors.New("empty value is not supported")
	}

	req := &tikvrpc.Request{
		Type: tikvrpc.CmdRawPut,
		RawPut: &kvrpcpb.RawPutRequest{
			Key:   key,
			Value: value,
		},
	}
	resp, _, err := c.sendReq(key, req)
	if err != nil {
		return errors.Trace(err)
	}
	cmdResp := resp.RawPut
	if cmdResp == nil {
		return errors.Trace(ErrBodyMissing)
	}
	if cmdResp.GetError() != "" {
		return errors.New(cmdResp.GetError())
	}
	return nil
}

// Delete deletes a key-value pair from TiKV.
func (c *RawKVClient) Delete(key []byte) error {
	start := time.Now()
	defer func() { metrics.TiKVRawkvCmdHistogram.WithLabelValues("delete").Observe(time.Since(start).Seconds()) }()

	req := &tikvrpc.Request{
		Type: tikvrpc.CmdRawDelete,
		RawDelete: &kvrpcpb.RawDeleteRequest{
			Key: key,
		},
	}
	resp, _, err := c.sendReq(key, req)
	if err != nil {
		return errors.Trace(err)
	}
	cmdResp := resp.RawDelete
	if cmdResp == nil {
		return errors.Trace(ErrBodyMissing)
	}
	if cmdResp.GetError() != "" {
		return errors.New(cmdResp.GetError())
	}
	return nil
}

// Scan queries continuous kv pairs, starts from startKey, up to limit pairs.
// If you want to exclude the startKey, append a '\0' to the key: `Scan(append(startKey, '\0'), limit)`.
func (c *RawKVClient) Scan(startKey []byte, limit int) (keys [][]byte, values [][]byte, err error) {
	start := time.Now()
	defer func() { metrics.TiKVRawkvCmdHistogram.WithLabelValues("raw_scan").Observe(time.Since(start).Seconds()) }()

	if limit > MaxRawKVScanLimit {
		return nil, nil, errors.Trace(ErrMaxScanLimitExceeded)
	}

	for len(keys) < limit {
		req := &tikvrpc.Request{
			Type: tikvrpc.CmdRawScan,
			RawScan: &kvrpcpb.RawScanRequest{
				StartKey: startKey,
				Limit:    uint32(limit - len(keys)),
			},
		}
		resp, loc, err := c.sendReq(startKey, req)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		cmdResp := resp.RawScan
		if cmdResp == nil {
			return nil, nil, errors.Trace(ErrBodyMissing)
		}
		for _, pair := range cmdResp.Kvs {
			keys = append(keys, pair.Key)
			values = append(values, pair.Value)
		}
		startKey = loc.EndKey
		if len(startKey) == 0 {
			break
		}
	}
	return
}

// DeleteRange deletes all key-value pairs in a range from TiKV
func (c *RawKVClient) DeleteRange(startKey []byte, endKey []byte) error {
	start := time.Now()
	defer func() { metrics.TiKVRawkvCmdHistogram.WithLabelValues("delete_range").Observe(time.Since(start).Seconds()) }()

	// Process each affected region respectively
	for !bytes.Equal(startKey, endKey) {
		resp, actualEndKey, err := c.sendDeleteRangeReq(startKey, endKey)
		if err != nil {
			return errors.Trace(err)
		}
		cmdResp := resp.RawDeleteRange
		if cmdResp == nil {
			return errors.Trace(ErrBodyMissing)
		}
		if cmdResp.GetError() != "" {
			return errors.New(cmdResp.GetError())
		}
		startKey = actualEndKey
	}

	return nil
}

func (c *RawKVClient) sendReq(key []byte, req *tikvrpc.Request) (*tikvrpc.Response, *KeyLocation, error) {
	bo := NewBackoffer(context.Background(), rawkvMaxBackoff)
	sender := NewRegionRequestSender(c.regionCache, c.rpcClient)
	for {
		loc, err := c.regionCache.LocateKey(bo, key)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		resp, err := sender.SendReq(bo, req, loc.Region, readTimeoutShort)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		regionErr, err := resp.GetRegionError()
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		if regionErr != nil {
			err := bo.Backoff(BoRegionMiss, errors.New(regionErr.String()))
			if err != nil {
				return nil, nil, errors.Trace(err)
			}
			continue
		}
		return resp, loc, nil
	}
}

// sendDeleteRangeReq sends a raw delete range request and returns the response and the actual endKey.
// If the given range spans over more than one regions, the actual endKey is the end of the first region.
// We can't use sendReq directly, because we need to know the end of the region before we send the request
// TODO: Is there any better way to avoid duplicating code with func `sendReq` ?
func (c *RawKVClient) sendDeleteRangeReq(startKey []byte, endKey []byte) (*tikvrpc.Response, []byte, error) {
	bo := NewBackoffer(context.Background(), rawkvMaxBackoff)
	sender := NewRegionRequestSender(c.regionCache, c.rpcClient)
	for {
		loc, err := c.regionCache.LocateKey(bo, startKey)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}

		actualEndKey :=  endKey
		if len(loc.EndKey) > 0 && bytes.Compare(loc.EndKey, endKey) < 0 {
			actualEndKey = loc.EndKey
		}

		req := &tikvrpc.Request{
			Type:tikvrpc.CmdRawDeleteRange,
			RawDeleteRange:&kvrpcpb.RawDeleteRangeRequest{
				StartKey: startKey,
				EndKey: actualEndKey,
			},
		}

		resp, err := sender.SendReq(bo, req, loc.Region, readTimeoutShort)
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		regionErr, err := resp.GetRegionError()
		if err != nil {
			return nil, nil, errors.Trace(err)
		}
		if regionErr != nil {
			err := bo.Backoff(BoRegionMiss, errors.New(regionErr.String()))
			if err != nil {
				return nil, nil, errors.Trace(err)
			}
			continue
		}
		return resp, actualEndKey, nil
	}
}
