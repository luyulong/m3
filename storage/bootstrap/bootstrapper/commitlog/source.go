// Copyright (c) 2016 Uber Technologies, Inc.
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

package commitlog

import (
	"fmt"
	"io"
	"math"
	"sync"
	"time"

	"github.com/m3db/m3db/encoding"
	"github.com/m3db/m3db/persist"
	"github.com/m3db/m3db/persist/fs"
	"github.com/m3db/m3db/persist/fs/commitlog"
	"github.com/m3db/m3db/persist/fs/msgpack"
	"github.com/m3db/m3db/storage/block"
	"github.com/m3db/m3db/storage/bootstrap"
	"github.com/m3db/m3db/storage/bootstrap/result"
	"github.com/m3db/m3db/storage/namespace"
	"github.com/m3db/m3db/ts"
	"github.com/m3db/m3db/x/xio"
	"github.com/m3db/m3x/checked"
	"github.com/m3db/m3x/ident"
	xlog "github.com/m3db/m3x/log"
	"github.com/m3db/m3x/pool"
	xsync "github.com/m3db/m3x/sync"
	xtime "github.com/m3db/m3x/time"
)

const encoderChanBufSize = 1000

type newIteratorFn func(opts commitlog.IteratorOpts) (commitlog.Iterator, error)

type commitLogSource struct {
	opts Options
	log  xlog.Logger

	// Mockable for testing
	newIteratorFn   newIteratorFn
	snapshotFilesFn snapshotFilesFn
	snapshotTimeFn  snapshotTimeFn
	newReaderFn     newReaderFn

	inspection FilesystemInspection
}

type encoder struct {
	lastWriteAt time.Time
	enc         encoding.Encoder
}

type snapshotFilesFn func(filePathPrefix string, namespace ident.ID, shard uint32) (fs.SnapshotFilesSlice, error)
type snapshotTimeFn func(filePathPrefix string, id fs.FilesetFileIdentifier, readerBufferSize int, decoder *msgpack.Decoder) (time.Time, error)
type newReaderFn func(bytesPool pool.CheckedBytesPool, opts fs.Options) (fs.FileSetReader, error)

func newCommitLogSource(opts Options, inspection FilesystemInspection) bootstrap.Source {
	return &commitLogSource{
		opts: opts,
		log:  opts.ResultOptions().InstrumentOptions().Logger(),

		newIteratorFn:   commitlog.NewIterator,
		snapshotFilesFn: fs.SnapshotFiles,
		snapshotTimeFn:  fs.SnapshotTime,
		newReaderFn:     fs.NewReader,

		inspection: inspection,
	}
}
func (s *commitLogSource) Can(strategy bootstrap.Strategy) bool {
	switch strategy {
	case bootstrap.BootstrapSequential:
		return true
	}
	return false
}

func (s *commitLogSource) Available(
	ns namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
) result.ShardTimeRanges {
	// Commit log bootstrapper is a last ditch effort, so fulfill all
	// time ranges requested even if not enough data, just to succeed
	// the bootstrap
	return shardsTimeRanges
}

// Read will read a combination of the available snapshot files and commit log files to restore
// as much unflushed data from disk as possible. The logic for performing this correctly is as
// follows:
//
// 1. For every shard/blockStart combination, find the most recently written and complete (I.E
//    has a checkpoint file) snapshot. Bootstrap that file.
// 2. For every shard/blockStart combination, determine the SnapshotTime time for the snapshot file.
//    This value corresponds to the (local) moment in time right before the snapshotting process
// 	  began.
// 3. Find the minimum SnapshotTime time for all of the shards and block starts (call it t0), and replay all
//    commit log entries starting at t0.Add(-max(bufferPast, bufferFuture)). Note that commit log entries should be filtered
//    by the local system timestamp for when they were written, not for the timestamp of the data point
//    itself.
//
// The rationale for this is that for a given shard / block, if we have a snapshot file that was
// written at t0, then we can be guaranteed that the snapshot file contains every write for that
// shard/blockStart up until (t0 - max(bufferPast, bufferFuture)). Lets start by imagining a
// scenario where taking into account the bufferPast value is important:
//
// BlockSize: 2hr, bufferPast: 5m, bufferFuture: 20m
// Trying to bootstrap shard 0 for time period 12PM -> 2PM
// Snapshot file was written at 1:50PM then:
//
// Snapshot file contains all writes for (shard 0, blockStart 12PM) up until 1:45PM (1:50-5)
// because we started snapshotting at 1:50PM and a write at 1:50:01PM for a datapoint at 1:45PM would
// be rejected for trying to write too far into the past.
//
// As a result, we might conclude that reading the commit log from 1:45PM onwards would be sufficient,
// however, we also need to consider the value of bufferFuture. Reading the commit log starting at
// 1:45PM actually would not be sufficient because we could have received a write at 1:42 system-time
// (within the 20m bufferFuture range) for a datapoint at 2:02PM. This write would belong to the 2PM
// block, not the 12PM block, and as a result would not be captured in the snapshot file, because snapshot
// files are block-specific. As a result, we actually need to read everything in the commit log starting
// from 1:30PM (1:50-20).
// TODO: Diagram
func (s *commitLogSource) Read(
	ns namespace.Metadata,
	shardsTimeRanges result.ShardTimeRanges,
	_ bootstrap.RunOptions,
) (result.BootstrapResult, error) {
	fmt.Println(shardsTimeRanges.MinMax())
	if shardsTimeRanges.IsEmpty() {
		return nil, nil
	}

	var (
		snapshotFilesByShard = map[uint32]fs.SnapshotFilesSlice{}
		fsOpts               = s.opts.CommitLogOptions().FilesystemOptions()
		filePathPrefix       = fsOpts.FilePathPrefix()
	)
	for shard := range shardsTimeRanges {
		snapshotFiles, err := s.snapshotFilesFn(filePathPrefix, ns.ID(), shard)
		if err != nil {
			return nil, err
		}
		if len(snapshotFiles) == 0 {
			s.log.Errorf("no snapshot files for shard: %d", shard)
		}
		snapshotFilesByShard[shard] = snapshotFiles
	}

	var (
		bopts = s.opts.ResultOptions()
		// bytesPool  = bopts.DatabaseBlockOptions().BytesPool()
		// blocksPool = bopts.DatabaseBlockOptions().DatabaseBlockPool()
		blockSize = ns.Options().RetentionOptions().BlockSize()
		// snapshotShardResults = make(map[uint32]result.ShardResult)
	)

	// Start off by bootstrapping the most recent and complete snapshot file for each shard for each of the
	// blocks.
	// snapshotShardResults, err := s.bootstrapAvailableSnapshotFiles(
	// 	ns.ID(), shardsTimeRanges, blockSize, snapshotFilesByShard, fsOpts, bytesPool, blocksPool)
	// if err != nil {
	// 	return nil, err
	// }

	// At this point we've bootstrapped all the snapshot files that we can, and we need
	// to decide which commitlogs to read. In order to do that, we'll need to figure out the
	// minimum most recent snapshot timefor each block, then we can use that information to decide
	// how much of the commit log we need to read for each block that we're bootstrapping. To start,
	// for each block that we're bootstrapping, we need to figure out the most recent snapshot that
	// was taken for each shard. I.E we want to create a datastructure that looks like this:
	// map[blockStart]map[shard]mostRecentSnapshotTime
	mostRecentCompleteSnapshotTimeByBlockShard := s.mostRecentCompleteSnapshotTimeByBlockShard(
		shardsTimeRanges, blockSize, snapshotFilesByShard, s.opts.CommitLogOptions().FilesystemOptions())

	// Once we have the desired datastructure, we next need to figure out the minimum most recent snapshot
	// for that block across all shards. This will help us determine how much of the commit log we need to
	// read. The new datastructure we're trying to generate looks like:
	// map[blockStart]minimumMostRecentSnapshotTime (across all shards)
	// This structure is important because it tells us how much of the commit log we need to read for each
	// block that we're trying to bootstrap (because the commit log is shared across all shards).
	minimumMostRecentSnapshotTimeByBlock := s.minimumMostRecentSnapshotTimeByBlock(
		shardsTimeRanges, blockSize, mostRecentCompleteSnapshotTimeByBlockShard)

	// TODO: Move this all into a helper?
	// Now that we have the minimum most recent snapshot time for each block, we can use that data to decide
	// how much of the commit log we need to read for each block that we're bootstrapping. We'll construct a
	// new predicate based on the data-structure we constructed earlier where the new predicate will check if
	// there is any overlap between a commit log file and a temporary range we construct that begins with the
	// minimum snapshot time and ends with the end of that block
	var (
		bufferPast   = ns.Options().RetentionOptions().BufferPast()
		bufferFuture = ns.Options().RetentionOptions().BufferFuture()
	)
	rangesToCheck := []xtime.Range{}
	for blockStart, minimumMostRecentSnapshotTime := range minimumMostRecentSnapshotTimeByBlock {
		maxBufferPastAndFuture := math.Max(float64(int(bufferPast)), float64(int(bufferFuture)))

		s.log.Infof(
			"minimum most recent snapshot time for blockStart %d is %d, creating range from: %d to: %d",
			blockStart, minimumMostRecentSnapshotTime.Unix(),
			minimumMostRecentSnapshotTime.Add(-time.Duration(maxBufferPastAndFuture)).Unix(),
			blockStart.ToTime().Add(blockSize).Unix())
		rangesToCheck = append(rangesToCheck, xtime.Range{
			// We have to subtract Max(bufferPast, bufferFuture) for the reasons described
			// in the method documentation.
			// TODO: Not 100% sure we still need the max() thing
			Start: minimumMostRecentSnapshotTime.Add(-time.Duration(maxBufferPastAndFuture)),
			End:   blockStart.ToTime().Add(blockSize),
		})
	}

	commitlogFilesSet := s.inspection.CommitlogFilesSet()
	readCommitlogPred := func(filename string, commitLogFileStart time.Time, commitLogFileBlockSize time.Duration) bool {
		_, ok := commitlogFilesSet[filename]
		if !ok {
			// This commitlog file did not exist before the node started which means it was created
			// as part of the existing process and the data already exists in memory.
			return false
		}

		// Note that the rangesToCheck that we generated above are *logical* ranges not
		// physical ones. I.E a range of 12:30PM to 2:00PM means that we need all data with
		// a timestamp between 12:30PM and 2:00PM which is strictly different than all datapoints
		// that *arrived* between 12:30PM and 2:00PM due to the bufferFuture and bufferPast
		// semantics.
		// Since the commitlog file ranges represent physical ranges, we will first convert them
		// to logical ranges, and *then* we will perform a range overlap comparison.
		for _, rangeToCheck := range rangesToCheck {
			commitLogEntryRange := xtime.Range{
				// Commit log filetime and duration represent system time, not the logical timestamps
				// of the values contained within.
				// Imagine the following scenario:
				// 		Namespace blockSize: 2 hours
				// 		Namespace bufferPast: 10 minutes
				// 		Namespace bufferFuture: 20 minutes
				// 		Commitlog file start: 12:30PM
				// 		Commitlog file blockSize: 15 minutes
				//
				// While the commitlog file only contains writes that were physically received
				// between 12:30PM and 12:45PM system-time, it *could* contain datapoints with
				// *logical* timestamps anywhere between 12:20PM and 1:05PM.
				//
				// I.E A write that arrives at exactly 12:30PM (system) with a timestamp of 12:20PM
				// (logical) would be within the 10 minute bufferPast period. Similarly, a write that
				// arrives at exactly 12:45PM (system) with a timestamp of 1:05PM (logical) would be
				// within the 20 minute bufferFuture period.
				Start: commitLogFileStart.Add(-bufferPast),
				End:   commitLogFileStart.Add(commitLogFileBlockSize).Add(bufferFuture),
			}

			if commitLogEntryRange.Overlaps(rangeToCheck) {
				s.log.Infof("choosing to read commitlog with start: %d", commitLogFileStart.Unix())
				return true
			}
		}

		s.log.Infof("choosing to skip commitlog with start: %d", commitLogFileStart.Unix())
		return false
	}

	readSeriesPredicate := newReadSeriesPredicate(ns)
	iterOpts := commitlog.IteratorOpts{
		CommitLogOptions:      s.opts.CommitLogOptions(),
		FileFilterPredicate:   readCommitlogPred,
		SeriesFilterPredicate: readSeriesPredicate,
	}
	iter, err := s.newIteratorFn(iterOpts)
	if err != nil {
		return nil, fmt.Errorf("unable to create commit log iterator: %v", err)
	}

	defer iter.Close()

	var (
		// +1 so we can use the shard number as an index throughout without constantly
		// remembering to subtract 1 to convert to zero-based indexing
		numShards   = s.findHighestShard(shardsTimeRanges) + 1
		numConc     = s.opts.EncodingConcurrency()
		blopts      = bopts.DatabaseBlockOptions()
		encoderPool = bopts.DatabaseBlockOptions().EncoderPool()
		workerErrs  = make([]int, numConc)
	)

	unmerged := make([]encodersAndRanges, numShards)
	for shard := range shardsTimeRanges {
		unmerged[shard] = encodersAndRanges{
			encodersBySeries: make(map[xtime.UnixNano]map[ident.Hash]encodersAndID),
			ranges:           shardsTimeRanges[shard],
		}
	}

	encoderChans := make([]chan encoderArg, numConc)
	for i := 0; i < numConc; i++ {
		encoderChans[i] = make(chan encoderArg, encoderChanBufSize)
	}

	// Spin up numConc background go-routines to handle M3TSZ encoding. This must
	// happen before we start reading to prevent infinitely blocking writes to
	// the encoderChans.
	wg := &sync.WaitGroup{}
	for workerNum, encoderChan := range encoderChans {
		wg.Add(1)
		go s.startM3TSZEncodingWorker(
			workerNum,
			encoderChan,
			unmerged,
			encoderPool,
			workerErrs,
			blopts,
			wg,
		)
	}

	t1 := time.Now()
	for iter.Next() {
		// TODO: Can skip some datapoints here if they're captured in the snapshot
		series, dp, unit, annotation := iter.Current()
		if !s.shouldEncodeSeries(unmerged, blockSize, series, dp) {
			continue
		}

		// Distribute work such that each encoder goroutine is responsible for
		// approximately numShards / numConc shards. This also means that all
		// datapoints for a given shard/series will be processed in a serialized
		// manner.
		// We choose to distribute work by shard instead of series.UniqueIndex
		// because it means that all accesses to the unmerged slice don't need
		// to be synchronized because each index belongs to a single shard so it
		// will only be accessed serially from a single worker routine.
		encoderChans[series.Shard%uint32(numConc)] <- encoderArg{
			series:     series,
			dp:         dp,
			unit:       unit,
			annotation: annotation,
			blockStart: dp.Timestamp.Truncate(blockSize),
		}
	}
	s.log.Infof("took %v to read commitlogs", time.Now().Sub(t1))

	for _, encoderChan := range encoderChans {
		close(encoderChan)
	}

	// Block until all data has been read and encoded by the worker goroutines
	wg.Wait()
	s.logEncodingOutcome(workerErrs, iter)

	t1 = time.Now()
	commitLogBootstrapResult := s.mergeShards(
		ns, int(numShards), shardsTimeRanges, fsOpts, bopts, blopts, encoderPool, unmerged, snapshotFilesByShard)
	s.log.Infof("took %v to merge shards", time.Now().Sub(t1))

	return commitLogBootstrapResult, nil
}

// mostRecentCompleteSnapshotTimeByBlockShard returns the most recent (i.e latest) complete (i.e has a checkpoint file)
// snapshot for every blockStart/shard combination that we're trying to bootstrap. It returns a data structure
// that looks like map[blockStart]map[shard]mostRecentCompleteSnapshotTime
func (s *commitLogSource) mostRecentCompleteSnapshotTimeByBlockShard(
	shardsTimeRanges result.ShardTimeRanges,
	blockSize time.Duration,
	snapshotFilesByShard map[uint32]fs.SnapshotFilesSlice,
	fsOpts fs.Options,
) map[xtime.UnixNano]map[uint32]time.Time {
	minBlock, maxBlock := shardsTimeRanges.MinMax()
	decoder := msgpack.NewDecoder(nil)
	mostRecentSnapshotsByBlockShard := map[xtime.UnixNano]map[uint32]time.Time{}
	for currBlock := minBlock.Truncate(blockSize); currBlock.Before(maxBlock); currBlock = currBlock.Add(blockSize) {
		for shard := range shardsTimeRanges {
			func() {
				var (
					mostRecentSnapshotTime time.Time
					mostRecentSnapshot     fs.SnapshotFile
					err                    error
				)

				defer func() {
					existing := mostRecentSnapshotsByBlockShard[xtime.ToUnixNano(currBlock)]
					if existing == nil {
						existing = map[uint32]time.Time{}
					}
					existing[shard] = mostRecentSnapshotTime
					mostRecentSnapshotsByBlockShard[xtime.ToUnixNano(currBlock)] = existing
				}()

				snapshotFiles, ok := snapshotFilesByShard[shard]
				if !ok {
					// If there are no snapshot files for this shard, then for this block we will
					// need to read the entire commit log for that period so we just set the most
					// recent snapshot to the beginning of the block.
					mostRecentSnapshotTime = currBlock
					return
				}

				mostRecentSnapshot, ok = snapshotFiles.LatestValidForBlock(currBlock)
				if !ok {
					// If there are no snapshot files for this block, then for this block we will
					// need to read the entire commit log for that period so we just set the most
					// recent snapshot to the beginning of the block.
					mostRecentSnapshotTime = currBlock
					return
				}

				var (
					filePathPrefix       = s.opts.CommitLogOptions().FilesystemOptions().FilePathPrefix()
					infoReaderBufferSize = s.opts.CommitLogOptions().FilesystemOptions().InfoReaderBufferSize()
				)
				// Performs I/O
				mostRecentSnapshotTime, err = s.snapshotTimeFn(
					filePathPrefix, mostRecentSnapshot.ID, infoReaderBufferSize, decoder)
				if err != nil {
					s.log.
						WithFields(
							xlog.NewField("namespace", mostRecentSnapshot.ID.Namespace),
							xlog.NewField("blockStart", mostRecentSnapshot.ID.BlockStart),
							xlog.NewField("shard", mostRecentSnapshot.ID.Shard),
							xlog.NewField("index", mostRecentSnapshot.ID.Index),
						).
						Errorf("error resolving snapshot time for snapshot")
					// Can't read the snapshot file for this block, then we will need to read
					// the entire commit log for that period so we just set the most recent snapshot
					// to the beginning of the block
					mostRecentSnapshotTime = currBlock
					return
				}
			}()
		}
	}

	return mostRecentSnapshotsByBlockShard
}

func (s *commitLogSource) minimumMostRecentSnapshotTimeByBlock(
	shardsTimeRanges result.ShardTimeRanges,
	blockSize time.Duration,
	mostRecentSnapshotByBlockShard map[xtime.UnixNano]map[uint32]time.Time,
) map[xtime.UnixNano]time.Time {
	minimumMostRecentSnapshotTimeByBlock := map[xtime.UnixNano]time.Time{}
	for blockStart, mostRecentSnapshotsByShard := range mostRecentSnapshotByBlockShard {
		// Since we're trying to find a minimum, all subsequent comparisons will be checking
		// if the new value is less than the current value so we initialize with the maximum
		// possible value.
		// minMostRecentSnapshot := time.Time{}
		// TODO: This is not accurate always since the snapshot time can extend into bufferPast
		minMostRecentSnapshot := blockStart.ToTime().Add(blockSize)
		for shard, mostRecentSnapshotForShard := range mostRecentSnapshotsByShard {
			blockRange := xtime.Range{Start: blockStart.ToTime(), End: blockStart.ToTime().Add(blockSize)}
			if !shardsTimeRanges[shard].Overlaps(blockRange) {
				// In order for a minimum most recent snapshot to be valid, it needs to be for
				// a block that we need to actually bootstrap for that shard. This check may
				// seem unnecessary, but it ensures that our algorithm doesn't do any extra work
				// even if we're bootstrapping different blocks for various shards.
				continue
			}
			if mostRecentSnapshotForShard.Before(minMostRecentSnapshot) {
				minMostRecentSnapshot = mostRecentSnapshotForShard
			}
		}
		minimumMostRecentSnapshotTimeByBlock[blockStart] = minMostRecentSnapshot
	}

	return minimumMostRecentSnapshotTimeByBlock
}

func (s *commitLogSource) bootstrapAvailableSnapshotFiles(
	nsID ident.ID,
	shardsTimeRanges result.ShardTimeRanges,
	blockSize time.Duration,
	snapshotFilesByShard map[uint32]fs.SnapshotFilesSlice,
	fsOpts fs.Options,
	bytesPool pool.CheckedBytesPool,
	blocksPool block.DatabaseBlockPool,
) (map[uint32]result.ShardResult, error) {
	snapshotShardResults := make(map[uint32]result.ShardResult)

	for shard, tr := range shardsTimeRanges {
		rangeIter := tr.Iter()
		for hasMore := rangeIter.Next(); hasMore; hasMore = rangeIter.Next() {
			currRange := rangeIter.Value()

			currRangeDuration := currRange.End.Unix() - currRange.Start.Unix()
			isMultipleOfBlockSize := currRangeDuration/int64(blockSize) == 0
			if !isMultipleOfBlockSize {
				return nil, fmt.Errorf(
					"received bootstrap range that is not multiple of blocksize, blockSize: %d, start: %d, end: %d",
					blockSize, currRange.End.Unix(), currRange.Start.Unix())
			}

			// TODO: Add function for this iteration
			// TODO: Estimate capacity better
			shardResult := result.NewShardResult(0, s.opts.ResultOptions())
			for blockStart := currRange.Start.Truncate(blockSize); blockStart.Before(currRange.End); blockStart = blockStart.Add(blockSize) {
				snapshotFiles := snapshotFilesByShard[shard]

				// TODO: Already called this FN, maybe should just re-use results somehow?
				latestSnapshot, ok := snapshotFiles.LatestValidForBlock(blockStart)
				if !ok {
					// There is no snapshot file for this shard / block combination
					continue
				}

				// Bootstrap the snapshot file
				reader, err := s.newReaderFn(bytesPool, fsOpts)
				if err != nil {
					// TODO: In this case we want to emit an error log, and somehow propagate that
					// we were unable to read this snapshot file to the subsequent code which determines
					// how much commitlog to read. We might even want to try and read the next earliest
					// file if it exists.
					// Actually since the commit log file no longer exists, we might just want to mark
					// this as unfulfilled somehow and get on with it.
					return nil, err
				}
				err = reader.Open(fs.ReaderOpenOptions{
					Identifier: fs.FilesetFileIdentifier{
						Namespace:  nsID,
						BlockStart: blockStart,
						Shard:      shard,
						Index:      latestSnapshot.ID.Index,
					},
					FilesetType: persist.FilesetSnapshotType,
				})
				if err != nil {
					// TODO: Same comment as above
					return nil, err
				}

				for {
					// TODO: Verify checksum?
					id, data, _, err := reader.Read()
					if err != nil && err != io.EOF {
						return nil, err
					}

					if err == io.EOF {
						break
					}

					dbBlock := blocksPool.Get()
					dbBlock.Reset(blockStart, ts.NewSegment(data, nil, ts.FinalizeHead))

					shardResult.AddBlock(id, dbBlock)
				}
			}
			snapshotShardResults[shard] = shardResult
		}
	}

	return snapshotShardResults, nil
}

// TODO: Move me
type dataAndID struct {
	data checked.Bytes
	id   ident.ID
}

func (s *commitLogSource) bootstrapLatestValidSnapshotFile(
	nsID ident.ID,
	shard uint32,
	blockStart time.Time,
	snapshotFilesByShard map[uint32]fs.SnapshotFilesSlice,
	fsOpts fs.Options,
	bytesPool pool.CheckedBytesPool,
	blocksPool block.DatabaseBlockPool,
) (map[ident.Hash]dataAndID, error) {
	snapshotFiles := snapshotFilesByShard[shard]

	latestSnapshot, ok := snapshotFiles.LatestValidForBlock(blockStart)
	if !ok {
		s.log.Infof("no snapshot file ")
		// TODO: Error handling here?
		return nil, nil
	}

	return s.bootstrapSnapshotFile(nsID, shard, blockStart, latestSnapshot.ID.Index,
		fsOpts, bytesPool, blocksPool)
}

func (s *commitLogSource) bootstrapSnapshotFile(
	nsID ident.ID,
	shard uint32,
	blockStart time.Time,
	index int,
	fsOpts fs.Options,
	bytesPool pool.CheckedBytesPool,
	blocksPool block.DatabaseBlockPool,
) (map[ident.Hash]dataAndID, error) {
	reader, err := s.newReaderFn(bytesPool, fsOpts)
	if err != nil {
		// TODO: In this case we want to emit an error log, and somehow propagate that
		// we were unable to read this snapshot file to the subsequent code which determines
		// how much commitlog to read. We might even want to try and read the next earliest
		// file if it exists.
		// Actually since the commit log file no longer exists, we might just want to mark
		// this as unfulfilled somehow and get on with it.
		return nil, err
	}
	err = reader.Open(fs.ReaderOpenOptions{
		Identifier: fs.FilesetFileIdentifier{
			Namespace:  nsID,
			BlockStart: blockStart,
			Shard:      shard,
			Index:      index,
		},
		FilesetType: persist.FilesetSnapshotType,
	})
	if err != nil {
		// TODO: Same comment as above
		return nil, err
	}
	defer reader.Close()

	result := make(map[ident.Hash]dataAndID)
	for {
		// TODO: Verify checksum?
		id, data, _, err := reader.Read()
		if err != nil && err != io.EOF {
			return nil, err
		}

		if err == io.EOF {
			break
		}

		result[id.Hash()] = dataAndID{
			data: data,
			id:   id,
		}
	}

	return result, nil
}

func (s *commitLogSource) startM3TSZEncodingWorker(
	workerNum int,
	ec <-chan encoderArg,
	unmerged []encodersAndRanges,
	encoderPool encoding.EncoderPool,
	workerErrs []int,
	blopts block.Options,
	wg *sync.WaitGroup,
) {
	for arg := range ec {
		var (
			series     = arg.series
			dp         = arg.dp
			unit       = arg.unit
			annotation = arg.annotation
			blockStart = arg.blockStart
		)

		unmergedShard := unmerged[series.Shard].encodersBySeries
		unmergedShardBlock, ok := unmergedShard[xtime.ToUnixNano(blockStart)]
		if !ok {
			unmergedShardBlock = make(map[ident.Hash]encodersAndID)
			unmergedShard[xtime.ToUnixNano(blockStart)] = unmergedShardBlock
		}

		hash := series.ID.Hash()
		unmergedSeriesBlock, ok := unmergedShardBlock[hash]
		if !ok {
			unmergedSeriesBlock = encodersAndID{
				id:       series.ID,
				encoders: nil,
			}
			unmergedShardBlock[hash] = unmergedSeriesBlock
		}

		var (
			err           error
			wroteExisting = false
		)
		for i := range unmergedSeriesBlock.encoders {
			if unmergedSeriesBlock.encoders[i].lastWriteAt.Before(dp.Timestamp) {
				unmergedSeriesBlock.encoders[i].lastWriteAt = dp.Timestamp
				err = unmergedSeriesBlock.encoders[i].enc.Encode(dp, unit, annotation)
				wroteExisting = true
				break
			}
		}
		if !wroteExisting {
			enc := encoderPool.Get()
			enc.Reset(blockStart, blopts.DatabaseBlockAllocSize())

			err = enc.Encode(dp, unit, annotation)
			if err == nil {
				unmergedSeriesBlock.encoders = append(unmergedSeriesBlock.encoders, encoder{
					lastWriteAt: dp.Timestamp,
					enc:         enc,
				})
				unmergedShardBlock[hash] = unmergedSeriesBlock
			}
		}
		if err != nil {
			workerErrs[workerNum]++
		}
	}
	wg.Done()
}

func (s *commitLogSource) shouldEncodeSeries(
	unmerged []encodersAndRanges,
	blockSize time.Duration,
	series commitlog.Series,
	dp ts.Datapoint,
) bool {
	// Check if the shard number is higher the amount of space we pre-allocated.
	// If it is, then it's not one of the shards we're trying to bootstrap
	if series.Shard > uint32(len(unmerged)-1) {
		return false
	}

	// Check if the shard is one of the shards we're trying to bootstrap
	ranges := unmerged[series.Shard].ranges
	if ranges.IsEmpty() {
		// Did not allocate map for this shard so not expecting data for it
		return false
	}

	// Check if the block corresponds to the time-range that we're trying to bootstrap
	blockStart := dp.Timestamp.Truncate(blockSize)
	blockEnd := blockStart.Add(blockSize)
	blockRange := xtime.Range{
		Start: blockStart,
		End:   blockEnd,
	}

	return ranges.Overlaps(blockRange)
}

func (s *commitLogSource) mergeShards(
	ns namespace.Metadata,
	numShards int,
	shardsTimeRanges result.ShardTimeRanges,
	fsOpts fs.Options,
	bopts result.Options,
	blopts block.Options,
	encoderPool encoding.EncoderPool,
	unmerged []encodersAndRanges,
	snapshotFilesByShard map[uint32]fs.SnapshotFilesSlice,
) result.BootstrapResult {
	var (
		shardErrs       = make([]int, numShards)
		shardEmptyErrs  = make([]int, numShards)
		bootstrapResult = result.NewBootstrapResult()
		fsWorkerPool    = xsync.NewWorkerPool(1)
		// Controls how many shards can be merged in parallel
		workerPool          = xsync.NewWorkerPool(s.opts.MergeShardsConcurrency())
		bootstrapResultLock sync.Mutex
		wg                  sync.WaitGroup
	)

	fsWorkerPool.Init()
	workerPool.Init()

	for shard, unmergedShard := range unmerged {
		if unmergedShard.encodersBySeries == nil {
			continue
		}
		wg.Add(1)
		shard, unmergedShard := shard, unmergedShard
		mergeShardFunc := func() {
			var shardResult result.ShardResult
			shardResult, shardEmptyErrs[shard], shardErrs[shard] = s.mergeShard(
				ns, uint32(shard), shardsTimeRanges[uint32(shard)], snapshotFilesByShard, unmergedShard, fsOpts, bopts, blopts, fsWorkerPool)
			if shardResult != nil && len(shardResult.AllSeries()) > 0 {
				// Prevent race conditions while updating bootstrapResult from multiple go-routines
				bootstrapResultLock.Lock()
				// Shard is a slice index so conversion to uint32 is safe
				bootstrapResult.Add(uint32(shard), shardResult, shardsTimeRanges[uint32(shard)])
				bootstrapResultLock.Unlock()
			}
			wg.Done()
		}
		workerPool.Go(mergeShardFunc)
	}

	// Wait for all merge goroutines to complete
	wg.Wait()
	s.logMergeShardsOutcome(shardErrs, shardEmptyErrs)
	return bootstrapResult
}

func (s *commitLogSource) mergeShard(
	ns namespace.Metadata,
	shard uint32,
	shardTimeRanges xtime.Ranges,
	snapshotFilesByShard map[uint32]fs.SnapshotFilesSlice,
	unmergedShard encodersAndRanges,
	fsOpts fs.Options,
	bopts result.Options,
	blopts block.Options,
	fsWorkerPool xsync.WorkerPool,
) (result.ShardResult, int, int) {
	var shardResult result.ShardResult
	var numShardEmptyErrs int
	var numErrs int

	var (
		blockSize               = ns.Options().RetentionOptions().BlockSize()
		bytesPool               = bopts.DatabaseBlockOptions().BytesPool()
		blocksPool              = bopts.DatabaseBlockOptions().DatabaseBlockPool()
		encoderPool             = bopts.DatabaseBlockOptions().EncoderPool()
		multiReaderIteratorPool = blopts.MultiReaderIteratorPool()
	)

	rangeIter := shardTimeRanges.Iter()
	for hasMore := rangeIter.Next(); hasMore; hasMore = rangeIter.Next() {
		currRange := rangeIter.Value()

		currRangeDuration := currRange.End.Unix() - currRange.Start.Unix()
		isMultipleOfBlockSize := currRangeDuration/int64(blockSize) == 0
		if !isMultipleOfBlockSize {
			// TODO: Fix me
			panic("NOT MULTIPLE")
			// return nil, fmt.Errorf(
			// 	"received bootstrap range that is not multiple of blocksize, blockSize: %d, start: %d, end: %d",
			// 	blockSize, currRange.End.Unix(), currRange.Start.Unix())
		}

		for blockStart := currRange.Start.Truncate(blockSize); blockStart.Before(currRange.End); blockStart = blockStart.Add(blockSize) {
			unmergedSeriesBlocks := unmergedShard.encodersBySeries[xtime.ToUnixNano(blockStart)]

			var (
				signalCh     = make(chan struct{})
				snapshotData map[ident.Hash]dataAndID
				err          error
			)

			fsWorkerPool.Go(func() {
				snapshotData, err = s.bootstrapLatestValidSnapshotFile(
					ns.ID(), shard, blockStart, snapshotFilesByShard, fsOpts, bytesPool, blocksPool)
				if err != nil {
					// TODO: Handle err
					panic(err)
				}
				signalCh <- struct{}{}
			})
			<-signalCh

			s.log.Infof(
				"merging shard: %d for blockStart: %d, have: %d series from commitlog and %d series from snapshot",
				shard, blockStart.Unix(), len(unmergedSeriesBlocks), len(snapshotData))
			for hash, encodersAndID := range unmergedSeriesBlocks {
				dbbBlock, numSeriesEmptyErrs, numSeriesErrs := s.mergeSeries(
					blockStart,
					encodersAndID,
					snapshotData[hash].data,
					blocksPool,
					multiReaderIteratorPool,
					encoderPool,
					blopts,
				)

				if dbbBlock != nil {
					if shardResult == nil {
						shardResult = result.NewShardResult(len(unmergedShard.encodersBySeries), s.opts.ResultOptions())
					}
					shardResult.AddBlock(encodersAndID.id, dbbBlock)
				}

				numShardEmptyErrs += numSeriesEmptyErrs
				numErrs += numSeriesErrs
			}

			for hash, data := range snapshotData {
				_, ok := unmergedSeriesBlocks[hash]
				if ok {
					// We already merged this data in
					continue
				}

				// This data did not have an equivalent in the commitlog so it wasn't
				// merged prior
				if shardResult == nil {
					// TODO: FIx this and the other one to set the size based on the length of both
					shardResult = result.NewShardResult(len(snapshotData), s.opts.ResultOptions())
				}
				pooledBlock := blocksPool.Get()
				pooledBlock.Reset(blockStart, ts.NewSegment(data.data, nil, ts.FinalizeHead))
				shardResult.AddBlock(data.id, pooledBlock)
			}
		}

		if shardResult != nil {
			s.log.Infof("final shardResult for shard: %d has %d series", shard, shardResult.NumSeries())
		}
	}

	return shardResult, numShardEmptyErrs, numErrs
}

func (s *commitLogSource) mergeSeries(
	start time.Time,
	unmergedEncoders encodersAndID,
	snapshotData checked.Bytes,
	blocksPool block.DatabaseBlockPool,
	multiReaderIteratorPool encoding.MultiReaderIteratorPool,
	encoderPool encoding.EncoderPool,
	blopts block.Options,
) (block.DatabaseBlock, int, int) {
	var numEmptyErrs int
	var numErrs int

	encoders := unmergedEncoders.encoders

	// Convert encoders to readers so we can use iteration helpers
	readers := encoders.newReaders()
	if snapshotData != nil {
		// TODO: Pooling?
		seg := ts.NewSegment(snapshotData, nil, ts.FinalizeHead)
		readers = append(readers, xio.NewSegmentReader(seg))
	}
	iter := multiReaderIteratorPool.Get()
	iter.Reset(readers)

	var err error
	enc := encoderPool.Get()
	enc.Reset(start, blopts.DatabaseBlockAllocSize())
	for iter.Next() {
		dp, unit, annotation := iter.Current()
		encodeErr := enc.Encode(dp, unit, annotation)
		if encodeErr != nil {
			err = encodeErr
			numErrs++
			break
		}
	}

	if iterErr := iter.Err(); iterErr != nil {
		if err == nil {
			err = iter.Err()
		}
		numErrs++
	}

	// Automatically returns iter to the pool
	iter.Close()
	encoders.close()
	readers.close()

	if err != nil {
		// TODO: ?
		panic(err)
	}

	pooledBlock := blocksPool.Get()
	pooledBlock.Reset(start, enc.Discard())
	return pooledBlock, numEmptyErrs, numErrs
}

func (s *commitLogSource) findHighestShard(shardsTimeRanges result.ShardTimeRanges) uint32 {
	var max uint32
	for shard := range shardsTimeRanges {
		if shard > max {
			max = shard
		}
	}
	return max
}

func (s *commitLogSource) logEncodingOutcome(workerErrs []int, iter commitlog.Iterator) {
	errSum := 0
	for _, numErrs := range workerErrs {
		errSum += numErrs
	}
	if errSum > 0 {
		s.log.Errorf("error bootstrapping from commit log: %d block encode errors", errSum)
	}
	if err := iter.Err(); err != nil {
		s.log.Errorf("error reading commit log: %v", err)
	}
}

func (s *commitLogSource) logMergeShardsOutcome(shardErrs []int, shardEmptyErrs []int) {
	errSum := 0
	for _, numErrs := range shardErrs {
		errSum += numErrs
	}
	if errSum > 0 {
		s.log.Errorf("error bootstrapping from commit log: %d merge out of order errors", errSum)
	}

	emptyErrSum := 0
	for _, numEmptyErr := range shardEmptyErrs {
		emptyErrSum += numEmptyErr
	}
	if emptyErrSum > 0 {
		s.log.Errorf("error bootstrapping from commit log: %d empty unmerged blocks errors", emptyErrSum)
	}
}

func newReadSeriesPredicate(ns namespace.Metadata) commitlog.SeriesFilterPredicate {
	nsID := ns.ID()
	return func(id ident.ID, namespace ident.ID) bool {
		return nsID.Equal(namespace)
	}
}

type encodersAndRanges struct {
	encodersBySeries map[xtime.UnixNano]map[ident.Hash]encodersAndID
	ranges           xtime.Ranges
}

type encodersAndID struct {
	id       ident.ID
	encoders encoders
}

// encoderArg contains all the information a worker go-routine needs to encode
// a data point as M3TSZ
type encoderArg struct {
	series     commitlog.Series
	dp         ts.Datapoint
	unit       xtime.Unit
	annotation ts.Annotation
	blockStart time.Time
}

type encoders []encoder

type ioReaders []io.Reader

func (e encoders) newReaders() ioReaders {
	readers := make(ioReaders, len(e))
	for i := range e {
		readers[i] = e[i].enc.Stream()
	}
	return readers
}

func (e encoders) close() {
	for i := range e {
		e[i].enc.Close()
	}
}

func (ir ioReaders) close() {
	for _, r := range ir {
		r.(xio.SegmentReader).Finalize()
	}
}
