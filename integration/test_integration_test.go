// +build integration

// Copyright (c) 2017 Uber Technologies, Inc.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in
// all copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
// THE SOFTWARE.

package integration

import (
	"fmt"
	"testing"
	"time"

	"github.com/m3db/m3db/integration/generate"
	"github.com/m3db/m3db/retention"
	"github.com/m3db/m3db/storage/bootstrap"
	"github.com/m3db/m3db/storage/bootstrap/bootstrapper"
	bcl "github.com/m3db/m3db/storage/bootstrap/bootstrapper/commitlog"
	"github.com/m3db/m3db/storage/bootstrap/result"
	"github.com/m3db/m3db/storage/namespace"
	"github.com/m3db/m3db/ts"

	"github.com/stretchr/testify/require"
)

func TestReproduceBug(t *testing.T) {
	if testing.Short() {
		t.SkipNow() // Just skip if we're doing a short run
	}

	// Test setup
	var (
		ropts = retention.NewOptions().
			SetRetentionPeriod(12 * time.Hour).
			SetBufferPast(5 * time.Minute).
			SetBufferFuture(5 * time.Minute)
		blockSize = ropts.BlockSize()
	)
	ns1, err := namespace.NewMetadata(testNamespaces[0], namespace.NewOptions().SetRetentionOptions(ropts))
	require.NoError(t, err)
	ns2, err := namespace.NewMetadata(testNamespaces[1], namespace.NewOptions().SetRetentionOptions(ropts))
	require.NoError(t, err)
	opts := newTestOptions(t).
		SetCommitLogRetentionPeriod(ropts.RetentionPeriod()).
		SetCommitLogBlockSize(blockSize).
		SetNamespaces([]namespace.Metadata{ns1, ns2})

	setup, err := newTestSetup(t, opts, nil)
	require.NoError(t, err)
	defer setup.close()

	commitLogOpts := setup.storageOpts.CommitLogOptions().
		SetFlushInterval(defaultIntegrationTestFlushInterval)
	setup.storageOpts = setup.storageOpts.SetCommitLogOptions(commitLogOpts)

	log := setup.storageOpts.InstrumentOptions().Logger()
	log.Info("commit log bootstrap test")

	// Write test data
	log.Info("generating data")
	now := setup.getNowFn()
	seriesMaps := generateSeriesMaps(30, now.Add(time.Second), now.Add(blockSize))
	log.Info("writing data")
	// TODO: Set the snapshot time
	numDatapointsNotInSnapshots := 0
	writeSnapshotsWithPredicate(t, setup, commitLogOpts, seriesMaps, ns1, nil, func(dp ts.Datapoint) bool {
		blockStart := dp.Timestamp.Truncate(blockSize)
		// TODO: Make this less ghetto
		if dp.Timestamp.Before(blockStart.Add(10 * time.Second)) {
			// TODO: Update this comment
			// Snapshot files will only contain writes from the first minute of the block
			return true
		}

		numDatapointsNotInSnapshots++
		return false
	})

	numDatapointsNotInCommitLogs := 0
	writeCommitLogDataWithPredicate(t, setup, commitLogOpts, seriesMaps, ns1, func(dp ts.Datapoint) bool {
		blockStart := dp.Timestamp.Truncate(blockSize)
		// TODO: Make this less ghetto
		if dp.Timestamp.Equal(blockStart.Add(10*time.Second)) || dp.Timestamp.After(blockStart.Add(10*time.Second)) {
			return true
		}

		numDatapointsNotInCommitLogs++
		return false
	})

	// Make sure we actually excluded some datapoints from the snapshot and commitlog files
	require.True(t, numDatapointsNotInSnapshots > 0)
	require.True(t, numDatapointsNotInCommitLogs > 0)

	log.Info("finished writing data")

	fmt.Println("now: ", now)
	fmt.Println("it is now: ", now.Add(ropts.BufferPast()).Add(time.Second))
	setup.setNowFn(now.Add(ropts.BufferPast()).Add(time.Second))

	bsOpts := newDefaulTestResultOptions(setup.storageOpts)
	signalCh := make(chan struct{})
	test := NewTestBootstrapperSource(TestBootstrapperOptions{
		read: func(namespace.Metadata, result.ShardTimeRanges, bootstrap.RunOptions) (result.BootstrapResult, error) {
			// panic("wtf")
			fmt.Println("waiting for signal!")
			<-signalCh
			return result.NewBootstrapResult(), nil
		},
	}, bsOpts, nil)
	// noOpAll := bootstrapper.NewNoOpAllBootstrapper()
	bclOpts := bcl.NewOptions().
		SetResultOptions(bsOpts).
		SetCommitLogOptions(commitLogOpts)

	inspection, err := bcl.InspectFilesystem(commitLogOpts.FilesystemOptions())
	require.NoError(t, err)
	bs, err := bcl.NewCommitLogBootstrapper(bclOpts, inspection, test)
	require.NoError(t, err)
	process := bootstrap.NewProcess(bs, bsOpts)
	setup.storageOpts = setup.storageOpts.SetBootstrapProcess(process)

	// Start the server with filesystem bootstrapper
	go func() {
		setup.sleepFor10xTickMinimumInterval()
		for _, data := range seriesMaps {
			err := setup.writeBatch(ns1.ID(), data)
			if err != nil {
				panic(err)
			}
		}
		setup.sleepFor10xTickMinimumInterval()
		fmt.Println("sending signal!")
		signalCh <- struct{}{}
	}()
	require.NoError(t, setup.startServer())
	log.Debug("server is now up")

	// Stop the server
	defer func() {
		require.NoError(t, setup.stopServer())
		log.Debug("server is now down")
	}()

	// Verify in-memory data match what we expect - all writes from seriesMaps
	// should be present
	metadatasByShard := testSetupMetadatas(t, setup, testNamespaces[0], now.Add(-2*blockSize), now)
	observedSeriesMaps := testSetupToSeriesMaps(t, setup, ns1, metadatasByShard)
	verifySeriesMapsEqual(t, seriesMaps, observedSeriesMaps)

	// Verify in-memory data match what we expect - no writes should be present
	// because we didn't issue any writes for this namespaces
	emptySeriesMaps := make(generate.SeriesBlocksByStart)
	metadatasByShard2 := testSetupMetadatas(t, setup, testNamespaces[1], now.Add(-2*blockSize), now)
	observedSeriesMaps2 := testSetupToSeriesMaps(t, setup, ns2, metadatasByShard2)
	verifySeriesMapsEqual(t, emptySeriesMaps, observedSeriesMaps2)

}

func NewTestBootstrapperSource(opts TestBootstrapperOptions, resultOpts result.Options, next bootstrap.Bootstrapper) TestBootstrapperSource {
	b := TestBootstrapperSource{}
	if opts.can != nil {
		b.can = opts.can
	} else {
		b.can = func(bootstrap.Strategy) bool { return true }
	}

	if opts.available != nil {
		b.available = opts.available
	} else {
		b.available = func(ns namespace.Metadata, shardsTimeRanges result.ShardTimeRanges) result.ShardTimeRanges {
			return shardsTimeRanges
		}
	}

	if opts.read != nil {
		b.read = opts.read
	} else {
		b.read = func(namespace.Metadata, result.ShardTimeRanges, bootstrap.RunOptions) (result.BootstrapResult, error) {
			return result.NewBootstrapResult(), nil
		}
	}

	b.Bootstrapper = bootstrapper.NewBaseBootstrapper(b.String(), b, resultOpts, next)
	return b
}

type TestBootstrapperOptions struct {
	can       func(bootstrap.Strategy) bool
	available func(namespace.Metadata, result.ShardTimeRanges) result.ShardTimeRanges
	read      func(namespace.Metadata, result.ShardTimeRanges, bootstrap.RunOptions) (result.BootstrapResult, error)
}

type TestBootstrapperSource struct {
	bootstrap.Bootstrapper
	can       func(bootstrap.Strategy) bool
	available func(namespace.Metadata, result.ShardTimeRanges) result.ShardTimeRanges
	read      func(namespace.Metadata, result.ShardTimeRanges, bootstrap.RunOptions) (result.BootstrapResult, error)
}

func (t TestBootstrapperSource) Can(strategy bootstrap.Strategy) bool {
	return t.can(strategy)
}

func (t TestBootstrapperSource) Available(
	ns namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
) result.ShardTimeRanges {
	return t.available(ns, shardsTimeRanges)
}

func (t TestBootstrapperSource) Read(
	ns namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
	opts bootstrap.RunOptions,
) (result.BootstrapResult, error) {
	return t.read(ns, shardsTimeRanges, opts)
}

func (t TestBootstrapperSource) String() string {
	return "test-bootstrapper"
}
