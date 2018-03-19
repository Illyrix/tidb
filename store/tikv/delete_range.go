// Copyright 2018 PingCAP, Inc.
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
	"bytes"
	"context"

	"github.com/juju/errors"
	"github.com/pingcap/kvproto/pkg/kvrpcpb"
	"github.com/pingcap/tidb/store/tikv/tikvrpc"
)

// DeleteRangeTask is used to delete all keys in a range. After
// performing DeleteRange, it keeps how many ranges it affects and
// if the task was canceled or not.
type DeleteRangeTask struct {
	regions  int
	canceled bool
	store    Storage
	ctx      context.Context
	bo       *Backoffer
	startKey []byte
	endKey   []byte
}

// NewDeleteRangeTask creates a DeleteRangeTask. Deleting will not be performed right away.
func NewDeleteRangeTask(ctx context.Context, store Storage, bo *Backoffer, startKey []byte, endKey []byte) DeleteRangeTask {
	return DeleteRangeTask{
		regions:  0,
		canceled: false,
		store:    store,
		ctx:      ctx,
		bo:       bo,
		startKey: startKey,
		endKey:   endKey,
	}
}

// Execute performs the delete range operation.
func (t DeleteRangeTask) Execute() error {
	startKey, rangeEndKey := t.startKey, t.endKey
	for {
		select {
		case <-t.ctx.Done():
			t.canceled = true
			return nil
		default:
		}

		loc, err := t.store.GetRegionCache().LocateKey(t.bo, startKey)
		if err != nil {
			return errors.Trace(err)
		}

		endKey := loc.EndKey
		if loc.Contains(rangeEndKey) {
			endKey = rangeEndKey
		}

		req := &tikvrpc.Request{
			Type: tikvrpc.CmdDeleteRange,
			DeleteRange: &kvrpcpb.DeleteRangeRequest{
				StartKey: startKey,
				EndKey:   endKey,
			},
		}

		resp, err := t.store.SendReq(t.bo, req, loc.Region, ReadTimeoutMedium)
		if err != nil {
			return errors.Trace(err)
		}
		regionErr, err := resp.GetRegionError()
		if err != nil {
			return errors.Trace(err)
		}
		if regionErr != nil {
			err = t.bo.Backoff(BoRegionMiss, errors.New(regionErr.String()))
			if err != nil {
				return errors.Trace(err)
			}
			continue
		}
		deleteRangeResp := resp.DeleteRange
		if deleteRangeResp == nil {
			return errors.Trace(ErrBodyMissing)
		}
		if err := deleteRangeResp.GetError(); err != "" {
			return errors.Errorf("unexpected delete range err: %v", err)
		}
		t.regions++
		if bytes.Equal(endKey, rangeEndKey) {
			break
		}
		startKey = endKey
	}

	return nil
}

// Regions returns the number of regions that are affected by this delete range task
func (t DeleteRangeTask) Regions() int {
	return t.regions
}

// IsCanceled returns true if the delete range operation was canceled on the half way
func (t DeleteRangeTask) IsCanceled() bool {
	return t.canceled
}
