// Copyright 2018 The Cockroach Authors.
//
// Licensed as a CockroachDB Enterprise file under the Cockroach Community
// License (the "License"); you may not use this file except in compliance with
// the License. You may obtain a copy of the License at
//
//     https://github.com/cockroachdb/cockroach/blob/master/licenses/CCL.txt

package schemafeed

import (
	"context"
	"testing"

	"github.com/cockroachdb/cockroach/pkg/jobs/jobspb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/sql/sqlbase"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/leaktest"
	"github.com/pkg/errors"
	"github.com/stretchr/testify/require"
)

func TestTableHistoryIngestionTracking(t *testing.T) {
	defer leaktest.AfterTest(t)()

	ctx := context.Background()
	ts := func(wt int64) hlc.Timestamp { return hlc.Timestamp{WallTime: wt} }
	descKVs := func(descs []*sqlbase.TableDescriptor) []roachpb.KeyValue {
		var kvs []roachpb.KeyValue
		for _, desc := range descs {
			kv := roachpb.KeyValue{
				Key:   sqlbase.MakeDescMetadataKey(desc.ID),
				Value: roachpb.Value{Timestamp: desc.ModificationTime},
			}
			descNoTime := *desc
			descNoTime.ModificationTime = hlc.Timestamp{}
			require.NoError(t, kv.Value.SetProto(sqlbase.WrapDescriptor(&descNoTime)))
			kvs = append(kvs, kv)
		}
		return kvs
	}
	validateFn := func(_ context.Context, desc *sqlbase.TableDescriptor) error {
		if desc.Name != `` {
			return errors.New(desc.Name)
		}
		return nil
	}
	requireChannelEmpty := func(t *testing.T, ch chan error) {
		t.Helper()
		select {
		case err := <-ch:
			t.Fatalf(`expected empty channel got %v`, err)
		default:
		}
	}

	m := SchemaFeed{
		targets: jobspb.ChangefeedTargets{1: {}},
	}
	m.mu.highWater = ts(0)

	require.Equal(t, ts(0), m.highWater())

	// advance
	require.NoError(t, m.ingestDescriptors(ctx, ts(0), ts(1), nil, validateFn))
	require.Equal(t, ts(1), m.highWater())
	require.NoError(t, m.ingestDescriptors(ctx, ts(1), ts(2), nil, validateFn))
	require.Equal(t, ts(2), m.highWater())

	// no-ops
	require.NoError(t, m.ingestDescriptors(ctx, ts(0), ts(1), nil, validateFn))
	require.Equal(t, ts(2), m.highWater())
	require.NoError(t, m.ingestDescriptors(ctx, ts(1), ts(2), nil, validateFn))
	require.Equal(t, ts(2), m.highWater())

	// overlap
	require.NoError(t, m.ingestDescriptors(ctx, ts(1), ts(3), nil, validateFn))
	require.Equal(t, ts(3), m.highWater())

	// gap
	require.EqualError(t, m.ingestDescriptors(ctx, ts(4), ts(5), nil, validateFn),
		`gap between 0.000000003,0 and 0.000000004,0`)
	require.Equal(t, ts(3), m.highWater())

	// validates
	require.NoError(t, m.ingestDescriptors(ctx, ts(3), ts(4), descKVs([]*sqlbase.TableDescriptor{
		{ID: 1, ModificationTime: ts(4)},
	}), validateFn))
	require.Equal(t, ts(4), m.highWater())

	// high-water already high enough. fast-path
	require.NoError(t, m.waitForTS(ctx, ts(3)))
	require.NoError(t, m.waitForTS(ctx, ts(4)))

	// high-water not there yet. blocks
	errCh6 := make(chan error, 1)
	errCh7 := make(chan error, 1)
	go func() { errCh7 <- m.waitForTS(ctx, ts(7)) }()
	go func() { errCh6 <- m.waitForTS(ctx, ts(6)) }()
	requireChannelEmpty(t, errCh6)
	requireChannelEmpty(t, errCh7)

	// high-water advances, but not enough
	require.NoError(t, m.ingestDescriptors(ctx, ts(4), ts(5), nil, validateFn))
	requireChannelEmpty(t, errCh6)
	requireChannelEmpty(t, errCh7)

	// high-water advances, unblocks only errCh6
	require.NoError(t, m.ingestDescriptors(ctx, ts(5), ts(6), nil, validateFn))
	require.NoError(t, <-errCh6)
	requireChannelEmpty(t, errCh7)

	// high-water advances again, unblocks errCh7
	require.NoError(t, m.ingestDescriptors(ctx, ts(6), ts(7), nil, validateFn))
	require.NoError(t, <-errCh7)

	// validate ctx cancellation
	errCh8 := make(chan error, 1)
	ctxTS8, cancelTS8 := context.WithCancel(ctx)
	go func() { errCh8 <- m.waitForTS(ctxTS8, ts(8)) }()
	requireChannelEmpty(t, errCh8)
	cancelTS8()
	require.EqualError(t, <-errCh8, `context canceled`)

	// does not validate, high-water does not change
	require.EqualError(t, m.ingestDescriptors(ctx, ts(7), ts(10), descKVs([]*sqlbase.TableDescriptor{
		{ID: 1, Name: `whoops!`, ModificationTime: ts(10)},
	}), validateFn), `whoops!`)
	require.Equal(t, ts(7), m.highWater())

	// ts 10 has errored, so validate can return its error without blocking
	require.EqualError(t, m.waitForTS(ctx, ts(10)), `whoops!`)

	// ts 8 and 9 are still unknown
	errCh8 = make(chan error, 1)
	errCh9 := make(chan error, 1)
	go func() { errCh8 <- m.waitForTS(ctx, ts(8)) }()
	go func() { errCh9 <- m.waitForTS(ctx, ts(9)) }()
	requireChannelEmpty(t, errCh8)
	requireChannelEmpty(t, errCh9)

	// turns out ts 10 is not a tight bound. ts 9 also has an error
	require.EqualError(t, m.ingestDescriptors(ctx, ts(7), ts(9), descKVs([]*sqlbase.TableDescriptor{
		{ID: 1, Name: `oh no!`, ModificationTime: ts(9)},
	}), validateFn), `oh no!`)
	require.Equal(t, ts(7), m.highWater())
	require.EqualError(t, <-errCh9, `oh no!`)

	// ts 8 is still unknown
	requireChannelEmpty(t, errCh8)

	// always return the earlist error seen (so waiting for ts 10 immediately
	// returns the 9 error now, it returned the ts 10 error above)
	require.EqualError(t, m.waitForTS(ctx, ts(9)), `oh no!`)

	// something earlier than ts 10 can still be okay
	require.NoError(t, m.ingestDescriptors(ctx, ts(7), ts(8), nil, validateFn))
	require.Equal(t, ts(8), m.highWater())
	require.NoError(t, <-errCh8)
}
