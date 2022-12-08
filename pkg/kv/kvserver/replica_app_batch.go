// Copyright 2022 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package kvserver

import (
	"context"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/apply"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvadmission"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvserverpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/storage"
	"github.com/cockroachdb/cockroach/pkg/storage/enginepb"
	"github.com/cockroachdb/cockroach/pkg/util/envutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/timeutil"
	"github.com/cockroachdb/errors"
	"github.com/cockroachdb/redact"
)

// replicaAppBatch implements the apply.Batch interface.
//
// The structure accumulates state due to the application of raft commands.
// Committed raft commands are applied to the state machine in a multi-stage
// process whereby individual commands are prepared for application relative
// to the current view of ReplicaState and staged in the batch. The batch is
// committed to the state machine's storage engine atomically.
type replicaAppBatch struct {
	r  *Replica
	sm *replicaStateMachine

	// batch accumulates writes implied by the raft entries in this batch.
	batch storage.Batch
	// state is this batch's view of the replica's state. It is copied from
	// under the Replica.mu when the batch is initialized and is updated in
	// stageTrivialReplicatedEvalResult.
	state kvserverpb.ReplicaState
	// closedTimestampSetter maintains historical information about the
	// advancement of the closed timestamp.
	closedTimestampSetter closedTimestampSetterInfo
	// stats is stored on the application batch to avoid an allocation in
	// tracking the batch's view of replicaState. All pointer fields in
	// replicaState other than Stats are overwritten completely rather than
	// updated in-place.
	stats enginepb.MVCCStats
	// changeRemovesReplica tracks whether the command in the batch (there must
	// be only one) removes this replica from the range.
	changeRemovesReplica bool

	// Statistics.
	entries                 int
	entryBytes              int64
	emptyEntries            int
	mutations               int
	start                   time.Time
	followerStoreWriteBytes kvadmission.FollowerStoreWriteBytes

	// Reused by addAppliedStateKeyToBatch to avoid heap allocations.
	asAlloc enginepb.RangeAppliedState
}

// Stage implements the apply.Batch interface. The method handles the first
// phase of applying a command to the replica state machine.
//
// The first thing the method does is determine whether the command should be
// applied at all or whether it should be rejected and replaced with an empty
// entry. The determination is based on the following rules: the command's
// MaxLeaseIndex must move the state machine's LeaseAppliedIndex forward, the
// proposer's lease (or rather its sequence number) must match that of the state
// machine, and lastly the GCThreshold must be below the timestamp that the
// command evaluated at. If any of the checks fail, the proposal's content is
// wiped and we apply an empty log entry instead. If a rejected command was
// proposed locally, the error will eventually be communicated to the waiting
// proposer. The two typical cases in which errors occur are lease mismatch (in
// which case the caller tries to send the command to the actual leaseholder)
// and violation of the LeaseAppliedIndex (in which case the proposal is retried
// if it was proposed locally).
//
// Assuming all checks were passed, the command's write batch is applied to the
// application batch. Its trivial ReplicatedState updates are then staged in
// the batch. This allows the batch to make an accurate determination about
// whether to accept or reject the next command that is staged without needing
// to actually update the replica state machine in between.
func (b *replicaAppBatch) Stage(
	ctx context.Context, cmdI apply.Command,
) (apply.CheckedCommand, error) {
	cmd := cmdI.(*replicatedCmd)
	if cmd.Index() == 0 {
		return nil, errors.AssertionFailedf("processRaftCommand requires a non-zero index")
	}
	if idx, applied := cmd.Index(), b.state.RaftAppliedIndex; idx != applied+1 {
		// If we have an out of order index, there's corruption. No sense in
		// trying to update anything or running the command. Simply return.
		return nil, errors.AssertionFailedf("applied index jumped from %d to %d", applied, idx)
	}
	if log.V(4) {
		log.Infof(ctx, "processing command %x: raftIndex=%d maxLeaseIndex=%d closedts=%s",
			cmd.ID, cmd.Index(), cmd.Cmd.MaxLeaseIndex, cmd.Cmd.ClosedTimestamp)
	}

	// Determine whether the command should be applied to the replicated state
	// machine or whether it should be rejected (and replaced by an empty command).
	// This check is deterministic on all replicas, so if one replica decides to
	// reject a command, all will.
	if !b.r.shouldApplyCommand(ctx, cmd, &b.state) {
		log.VEventf(ctx, 1, "applying command with forced error: %s", cmd.ForcedErr)

		// Apply an empty command.
		cmd.Cmd.ReplicatedEvalResult = kvserverpb.ReplicatedEvalResult{}
		cmd.Cmd.WriteBatch = nil
		cmd.Cmd.LogicalOpLog = nil
		cmd.Cmd.ClosedTimestamp = nil
	} else {
		if err := b.assertNoCmdClosedTimestampRegression(ctx, cmd); err != nil {
			return nil, err
		}
		if err := b.assertNoWriteBelowClosedTimestamp(cmd); err != nil {
			return nil, err
		}
		log.Event(ctx, "applying command")
	}

	// Acquire the split or merge lock, if necessary. If a split or merge
	// command was rejected with a below-Raft forced error then its replicated
	// result was just cleared and this will be a no-op.
	//
	// TODO(tbg): can't this happen in splitPreApply which is called from
	// b.runPreApplyTriggersAfterStagingWriteBatch and similar for merges? That
	// way, it would become less of a one-off.
	if splitMergeUnlock, err := b.r.maybeAcquireSplitMergeLock(ctx, cmd.Cmd); err != nil {
		if cmd.Cmd.ReplicatedEvalResult.Split != nil {
			err = errors.Wrap(err, "unable to acquire split lock")
		} else {
			err = errors.Wrap(err, "unable to acquire merge lock")
		}
		return nil, err
	} else if splitMergeUnlock != nil {
		// Set the splitMergeUnlock on the replicaAppBatch to be called
		// after the batch has been applied (see replicaAppBatch.commit).
		cmd.splitMergeUnlock = splitMergeUnlock
	}

	// Normalize the command, accounting for past migrations.
	b.migrateReplicatedResult(ctx, cmd)

	// Run any triggers that should occur before the batch is applied
	// and before the write batch is staged in the batch.
	if err := b.runPreApplyTriggersBeforeStagingWriteBatch(ctx, cmd); err != nil {
		return nil, err
	}

	// We do the stats for store write byte sizes here since the code below may
	// fiddle with these fields e.g. runPreApplyTriggersAfterStagingWriteBatch
	// nils the AddSSTable field.
	if !cmd.IsLocal() {
		writeBytes, ingestedBytes := cmd.getStoreWriteByteSizes()
		b.followerStoreWriteBytes.NumEntries++
		b.followerStoreWriteBytes.WriteBytes += writeBytes
		b.followerStoreWriteBytes.IngestedBytes += ingestedBytes
	}

	// Stage the command's write batch in the application batch.
	if err := b.stageWriteBatch(ctx, cmd); err != nil {
		return nil, err
	}

	// Run any triggers that should occur before the batch is applied
	// but after the write batch is staged in the batch.
	if err := b.runPreApplyTriggersAfterStagingWriteBatch(ctx, cmd); err != nil {
		return nil, err
	}

	// Stage the command's trivial ReplicatedState updates in the batch. Any
	// non-trivial commands will be in their own batch, so delaying their
	// non-trivial ReplicatedState updates until later (without ever staging
	// them in the batch) is sufficient.
	b.stageTrivialReplicatedEvalResult(ctx, cmd)
	b.entries++
	size := len(cmd.Data)
	b.entryBytes += int64(size)
	if size == 0 {
		b.emptyEntries++
	}

	// The command was checked by shouldApplyCommand, so it can be returned
	// as an apply.CheckedCommand.
	return cmd, nil
}

// migrateReplicatedResult performs any migrations necessary on the command to
// normalize it before applying it to the batch. This may modify the command.
func (b *replicaAppBatch) migrateReplicatedResult(ctx context.Context, cmd *replicatedCmd) {
	// If the command was using the deprecated version of the MVCCStats proto,
	// migrate it to the new version and clear out the field.
	res := cmd.ReplicatedResult()
	if deprecatedDelta := res.DeprecatedDelta; deprecatedDelta != nil {
		if res.Delta != (enginepb.MVCCStatsDelta{}) {
			log.Fatalf(ctx, "stats delta not empty but deprecated delta provided: %+v", cmd)
		}
		res.Delta = deprecatedDelta.ToStatsDelta()
		res.DeprecatedDelta = nil
	}
}

// stageWriteBatch applies the command's write batch to the application batch's
// RocksDB batch. This batch is committed to Pebble in replicaAppBatch.commit.
func (b *replicaAppBatch) stageWriteBatch(ctx context.Context, cmd *replicatedCmd) error {
	wb := cmd.Cmd.WriteBatch
	if wb == nil {
		return nil
	}
	if mutations, err := storage.PebbleBatchCount(wb.Data); err != nil {
		log.Errorf(ctx, "unable to read header of committed WriteBatch: %+v", err)
	} else {
		b.mutations += mutations
	}
	if err := b.batch.ApplyBatchRepr(wb.Data, false); err != nil {
		return errors.Wrapf(err, "unable to apply WriteBatch")
	}
	return nil
}

// changeRemovesStore returns true if any of the removals in this change have storeID.
func changeRemovesStore(
	desc *roachpb.RangeDescriptor, change *kvserverpb.ChangeReplicas, storeID roachpb.StoreID,
) (removesStore bool) {
	// NB: We don't use change.Removed() because it will include replicas being
	// transitioned to VOTER_OUTGOING.

	// We know we're removed if we do not appear in the new descriptor.
	_, existsInChange := change.Desc.GetReplicaDescriptor(storeID)
	return !existsInChange
}

// runPreApplyTriggersBeforeStagingWriteBatch runs any triggers that must fire
// before a command is applied to the state machine but after the command is
// staged in the replicaAppBatch's write batch. It may modify the command.
func (b *replicaAppBatch) runPreApplyTriggersBeforeStagingWriteBatch(
	ctx context.Context, cmd *replicatedCmd,
) error {
	if ops := cmd.Cmd.LogicalOpLog; ops != nil {
		b.r.populatePrevValsInLogicalOpLogRaftMuLocked(ctx, ops, b.batch)
	}
	return nil
}

// runPreApplyTriggersAfterStagingWriteBatch runs any triggers that must fire
// before a command is applied to the state machine but after the command is
// staged in the replicaAppBatch's write batch. It may modify the command.
func (b *replicaAppBatch) runPreApplyTriggersAfterStagingWriteBatch(
	ctx context.Context, cmd *replicatedCmd,
) error {
	res := cmd.ReplicatedResult()

	// MVCC history mutations violate the closed timestamp, modifying data that
	// has already been emitted and checkpointed via a rangefeed. Callers are
	// expected to ensure that no rangefeeds are currently active across such
	// spans, but as a safeguard we disconnect the overlapping rangefeeds
	// with a non-retriable error anyway.
	if res.MVCCHistoryMutation != nil {
		for _, span := range res.MVCCHistoryMutation.Spans {
			b.r.disconnectRangefeedSpanWithErr(span, roachpb.NewError(&roachpb.MVCCHistoryMutationError{
				Span: span,
			}))
		}
	}

	// AddSSTable ingestions run before the actual batch gets written to the
	// storage engine. This makes sure that when the Raft command is applied,
	// the ingestion has definitely succeeded. Note that we have taken
	// precautions during command evaluation to avoid having mutations in the
	// WriteBatch that affect the SSTable. Not doing so could result in order
	// reversal (and missing values) here.
	//
	// NB: any command which has an AddSSTable is non-trivial and will be
	// applied in its own batch so it's not possible that any other commands
	// which precede this command can shadow writes from this SSTable.
	if res.AddSSTable != nil {
		copied := addSSTablePreApply(
			ctx,
			b.r.store.cfg.Settings,
			b.r.store.engine,
			b.r.raftMu.sideloaded,
			cmd.Term,
			cmd.Index(),
			*res.AddSSTable,
			b.r.store.limiters.BulkIOWriteRate,
		)
		b.r.store.metrics.AddSSTableApplications.Inc(1)
		if copied {
			b.r.store.metrics.AddSSTableApplicationCopies.Inc(1)
		}
		if added := res.Delta.KeyCount; added > 0 {
			b.r.loadStats.writeKeys.RecordCount(float64(added), 0)
		}
		if res.AddSSTable.AtWriteTimestamp {
			b.r.handleSSTableRaftMuLocked(
				ctx, res.AddSSTable.Data, res.AddSSTable.Span, res.WriteTimestamp)
		}
		res.AddSSTable = nil
	}

	if res.Split != nil {
		// Splits require a new HardState to be written to the new RHS
		// range (and this needs to be atomic with the main batch). This
		// cannot be constructed at evaluation time because it differs
		// on each replica (votes may have already been cast on the
		// uninitialized replica). Write this new hardstate to the batch too.
		// See https://github.com/cockroachdb/cockroach/issues/20629.
		//
		// Alternatively if we discover that the RHS has already been removed
		// from this store, clean up its data.
		splitPreApply(ctx, b.r, b.batch, res.Split.SplitTrigger, cmd.Cmd.ClosedTimestamp)

		// The rangefeed processor will no longer be provided logical ops for
		// its entire range, so it needs to be shut down and all registrations
		// need to retry.
		// TODO(nvanbenschoten): It should be possible to only reject registrations
		// that overlap with the new range of the split and keep registrations that
		// are only interested in keys that are still on the original range running.
		b.r.disconnectRangefeedWithReason(
			roachpb.RangeFeedRetryError_REASON_RANGE_SPLIT,
		)
	}

	if merge := res.Merge; merge != nil {
		// Merges require the subsumed range to be atomically deleted when the
		// merge transaction commits.

		// An initialized replica is always contained in its descriptor.
		rhsRepl, err := b.r.store.GetReplica(merge.RightDesc.RangeID)
		if err != nil {
			return errors.Wrapf(err, "unable to get replica for merge")
		}
		// We should already have acquired the raftMu for the rhsRepl and now hold
		// its unlock method in cmd.splitMergeUnlock.
		rhsRepl.raftMu.AssertHeld()

		// We mark the replica as destroyed so that new commands are not
		// accepted. This destroy status will be detected after the batch
		// commits by handleMergeResult() to finish the removal.
		rhsRepl.readOnlyCmdMu.Lock()
		rhsRepl.mu.Lock()
		rhsRepl.mu.destroyStatus.Set(
			roachpb.NewRangeNotFoundError(rhsRepl.RangeID, rhsRepl.store.StoreID()),
			destroyReasonRemoved)
		rhsRepl.mu.Unlock()
		rhsRepl.readOnlyCmdMu.Unlock()

		// Use math.MaxInt32 (mergedTombstoneReplicaID) as the nextReplicaID as an
		// extra safeguard against creating new replicas of the RHS. This isn't
		// required for correctness, since the merge protocol should guarantee that
		// no new replicas of the RHS can ever be created, but it doesn't hurt to
		// be careful.
		const clearRangeIDLocalOnly = true
		const mustClearRange = false
		if err := rhsRepl.preDestroyRaftMuLocked(
			ctx, b.batch, b.batch, mergedTombstoneReplicaID, clearRangeIDLocalOnly, mustClearRange,
		); err != nil {
			return errors.Wrapf(err, "unable to destroy replica before merge")
		}

		// Shut down rangefeed processors on either side of the merge.
		//
		// NB: It is critical to shut-down a rangefeed processor on the surviving
		// replica primarily do deal with the possibility that there are logical ops
		// for the RHS to resolve intents written by the merge transaction. In
		// practice, the only such intents that exist are on the RangeEventTable,
		// but it's good to be consistent here and allow the merge transaction to
		// write to the RHS of a merge. See batcheval.resolveLocalLocks for details
		// on why we resolve RHS intents when committing a merge transaction.
		//
		// TODO(nvanbenschoten): Alternatively we could just adjust the bounds of
		// b.r.Processor to include the rhsRepl span.
		//
		// NB: removeInitializedReplicaRaftMuLocked also disconnects any initialized
		// rangefeeds with REASON_REPLICA_REMOVED. That's ok because we will have
		// already disconnected the rangefeed here.
		b.r.disconnectRangefeedWithReason(
			roachpb.RangeFeedRetryError_REASON_RANGE_MERGED,
		)
		rhsRepl.disconnectRangefeedWithReason(
			roachpb.RangeFeedRetryError_REASON_RANGE_MERGED,
		)
	}

	if res.State != nil && res.State.GCThreshold != nil {
		// NB: The GCThreshold is a pre-apply side effect because readers rely on
		// the invariant that the in-memory GC threshold is bumped before the actual
		// garbage collection command is applied. This is because readers capture a
		// snapshot of the storage engine state and then subsequently validate that
		// snapshot by ensuring that the in-memory GC threshold is below the read's
		// timestamp. Since the in-memory GC threshold is bumped before the GC
		// command is applied, the reader is guaranteed to see the un-GC'ed, correct
		// state of the engine if this validation succeeds.
		//
		// NB2: However, as of the time of writing this comment (June 2022),
		// the mvccGCQueue issues GC requests in 2 phases: the first that simply
		// bumps the in-memory GC threshold, and the second one that performs the
		// actual garbage collection. This is just a historical quirk and might be
		// changed soon.
		//
		// TODO(aayush): Update the comment above once we do make the mvccGCQueue
		// issue GC requests in a single phase.
		b.r.handleGCThresholdResult(ctx, res.State.GCThreshold)
		res.State.GCThreshold = nil
	}

	if res.State != nil && res.State.TruncatedState != nil {
		var err error
		// Typically one should not be checking the cluster version below raft,
		// since it can cause state machine divergence. However, this check is
		// only for deciding how to truncate the raft log, which is not part of
		// the state machine. Also, we will eventually eliminate this check by
		// only supporting loosely coupled truncation.
		looselyCoupledTruncation := isLooselyCoupledRaftLogTruncationEnabled(ctx, b.r.ClusterSettings())
		// In addition to cluster version and cluster settings, we also apply
		// immediately if RaftExpectedFirstIndex is not populated (see comment in
		// that proto).
		//
		// In the release following LooselyCoupledRaftLogTruncation, we will
		// retire the strongly coupled path. It is possible that some replica
		// still has a truncation sitting in a raft log that never populated
		// RaftExpectedFirstIndex, which will be interpreted as 0. When applying
		// it, the loosely coupled code will mark the log size as untrusted and
		// will recompute the size. This has no correctness impact, so we are not
		// going to bother with a long-running migration.
		apply := !looselyCoupledTruncation || res.RaftExpectedFirstIndex == 0
		if apply {
			if apply, err = handleTruncatedStateBelowRaftPreApply(
				ctx, b.state.TruncatedState, res.State.TruncatedState, b.r.raftMu.stateLoader, b.batch,
			); err != nil {
				return errors.Wrap(err, "unable to handle truncated state")
			}
		} else {
			b.r.store.raftTruncator.addPendingTruncation(
				ctx, (*raftTruncatorReplica)(b.r), *res.State.TruncatedState, res.RaftExpectedFirstIndex,
				res.RaftLogDelta)
		}
		if !apply {
			// The truncated state was discarded, or we are queuing a pending
			// truncation, so make sure we don't apply it to our in-memory state.
			res.State.TruncatedState = nil
			res.RaftLogDelta = 0
			res.RaftExpectedFirstIndex = 0
			if !looselyCoupledTruncation {
				// TODO(ajwerner): consider moving this code.
				// We received a truncation that doesn't apply to us, so we know that
				// there's a leaseholder out there with a log that has earlier entries
				// than ours. That leader also guided our log size computations by
				// giving us RaftLogDeltas for past truncations, and this was likely
				// off. Mark our Raft log size is not trustworthy so that, assuming
				// we step up as leader at some point in the future, we recompute
				// our numbers.
				// TODO(sumeer): this code will be deleted when there is no
				// !looselyCoupledTruncation code path.
				b.r.mu.Lock()
				b.r.mu.raftLogSizeTrusted = false
				b.r.mu.Unlock()
			}
		}
	}

	// Detect if this command will remove us from the range.
	// If so we stage the removal of all of our range data into this batch.
	// We'll complete the removal when it commits. Later logic detects the
	// removal by inspecting the destroy status.
	//
	// NB: This is the last step in the preApply which durably writes to the
	// replica state so that if it removes the replica it removes everything.
	if change := res.ChangeReplicas; change != nil &&
		changeRemovesStore(b.state.Desc, change, b.r.store.StoreID()) &&
		// Don't remove the data if the testing knobs ask us not to.
		!b.r.store.TestingKnobs().DisableEagerReplicaRemoval {

		// We mark the replica as destroyed so that new commands are not
		// accepted. This destroy status will be detected after the batch
		// commits by handleChangeReplicasResult() to finish the removal.
		//
		// NB: we must be holding the raftMu here because we're in the midst of
		// application.
		b.r.readOnlyCmdMu.Lock()
		b.r.mu.Lock()
		b.r.mu.destroyStatus.Set(
			roachpb.NewRangeNotFoundError(b.r.RangeID, b.r.store.StoreID()),
			destroyReasonRemoved)
		b.r.mu.Unlock()
		b.r.readOnlyCmdMu.Unlock()
		b.changeRemovesReplica = true

		// Delete all of the local data. We're going to delete the hard state too.
		// In order for this to be safe we need code above this to promise that we're
		// never going to write hard state in response to a message for a later
		// replica (with a different replica ID) to this range state.
		if err := b.r.preDestroyRaftMuLocked(
			ctx,
			b.batch,
			b.batch,
			change.NextReplicaID(),
			false, /* clearRangeIDLocalOnly */
			false, /* mustUseClearRange */
		); err != nil {
			return errors.Wrapf(err, "unable to destroy replica before removal")
		}
	}

	// Provide the command's corresponding logical operations to the Replica's
	// rangefeed. Only do so if the WriteBatch is non-nil, in which case the
	// rangefeed requires there to be a corresponding logical operation log or
	// it will shut down with an error. If the WriteBatch is nil then we expect
	// the logical operation log to also be nil. We don't want to trigger a
	// shutdown of the rangefeed in that situation, so we don't pass anything to
	// the rangefeed. If no rangefeed is running at all, this call will be a noop.
	if ops := cmd.Cmd.LogicalOpLog; cmd.Cmd.WriteBatch != nil {
		b.r.handleLogicalOpLogRaftMuLocked(ctx, ops, b.batch)
	} else if ops != nil {
		log.Fatalf(ctx, "non-nil logical op log with nil write batch: %v", cmd.Cmd)
	}

	return nil
}

// stageTrivialReplicatedEvalResult applies the trivial portions of the
// command's ReplicatedEvalResult to the batch's ReplicaState. This function
// modifies the receiver's ReplicaState but does not modify ReplicatedEvalResult
// in order to give the TestingPostApplyFilter testing knob an opportunity to
// inspect the command's ReplicatedEvalResult.
func (b *replicaAppBatch) stageTrivialReplicatedEvalResult(
	ctx context.Context, cmd *replicatedCmd,
) {
	if cmd.Index() == 0 {
		log.Fatalf(ctx, "raft entry with index 0")
	}
	b.state.RaftAppliedIndex = cmd.Index()
	b.state.RaftAppliedIndexTerm = cmd.Term

	if leaseAppliedIndex := cmd.LeaseIndex; leaseAppliedIndex != 0 {
		b.state.LeaseAppliedIndex = leaseAppliedIndex
	}
	if cts := cmd.Cmd.ClosedTimestamp; cts != nil && !cts.IsEmpty() {
		b.state.RaftClosedTimestamp = *cts
		b.closedTimestampSetter.record(cmd, b.state.Lease)
	}

	res := cmd.ReplicatedResult()

	// Special-cased MVCC stats handling to exploit commutativity of stats delta
	// upgrades. Thanks to commutativity, the spanlatch manager does not have to
	// serialize on the stats key.
	deltaStats := res.Delta.ToStats()
	b.state.Stats.Add(deltaStats)

	if res.State != nil && res.State.GCHint != nil {
		b.r.handleGCHintResult(ctx, res.State.GCHint)
		res.State.GCHint = nil
	}
}

// ApplyToStateMachine implements the apply.Batch interface. The method handles
// the second phase of applying a command to the replica state machine. It
// writes the application batch's accumulated RocksDB batch to the storage
// engine. This encompasses the persistent state transition portion of entry
// application.
func (b *replicaAppBatch) ApplyToStateMachine(ctx context.Context) error {
	if log.V(4) {
		log.Infof(ctx, "flushing batch %v of %d entries", b.state, b.entries)
	}

	// Add the replica applied state key to the write batch if this change
	// doesn't remove us.
	if !b.changeRemovesReplica {
		if err := b.addAppliedStateKeyToBatch(ctx); err != nil {
			return err
		}
	}

	// Apply the write batch to RockDB. Entry application is done without
	// syncing to disk. The atomicity guarantees of the batch and the fact that
	// the applied state is stored in this batch, ensure that if the batch ends
	// up not being durably committed then the entries in this batch will be
	// applied again upon startup. However, if we're removing the replica's data
	// then we sync this batch as it is not safe to call postDestroyRaftMuLocked
	// before ensuring that the replica's data has been synchronously removed.
	// See handleChangeReplicasResult().
	sync := b.changeRemovesReplica
	if err := b.batch.Commit(sync); err != nil {
		return errors.Wrapf(err, "unable to commit Raft entry batch")
	}
	b.batch.Close()
	b.batch = nil

	// Update the replica's applied indexes, mvcc stats and closed timestamp.
	r := b.r
	r.mu.Lock()
	r.mu.state.RaftAppliedIndex = b.state.RaftAppliedIndex
	r.mu.state.RaftAppliedIndexTerm = b.state.RaftAppliedIndexTerm
	r.mu.state.LeaseAppliedIndex = b.state.LeaseAppliedIndex

	// Sanity check that the RaftClosedTimestamp doesn't go backwards.
	existingClosed := r.mu.state.RaftClosedTimestamp
	newClosed := b.state.RaftClosedTimestamp
	if !newClosed.IsEmpty() && newClosed.Less(existingClosed) && raftClosedTimestampAssertionsEnabled {
		return errors.AssertionFailedf(
			"raft closed timestamp regression; replica has: %s, new batch has: %s.",
			existingClosed.String(), newClosed.String())
	}
	r.mu.closedTimestampSetter = b.closedTimestampSetter

	closedTimestampUpdated := r.mu.state.RaftClosedTimestamp.Forward(b.state.RaftClosedTimestamp)
	prevStats := *r.mu.state.Stats
	*r.mu.state.Stats = *b.state.Stats

	// If the range is now less than its RangeMaxBytes, clear the history of its
	// largest previous max bytes.
	if r.mu.largestPreviousMaxRangeSizeBytes > 0 && b.state.Stats.Total() < r.mu.conf.RangeMaxBytes {
		r.mu.largestPreviousMaxRangeSizeBytes = 0
	}

	// Check the queuing conditions while holding the lock.
	needsSplitBySize := r.needsSplitBySizeRLocked()
	needsMergeBySize := r.needsMergeBySizeRLocked()
	needsTruncationByLogSize := r.needsRaftLogTruncationLocked()
	r.mu.Unlock()
	if closedTimestampUpdated {
		r.handleClosedTimestampUpdateRaftMuLocked(ctx, b.state.RaftClosedTimestamp)
	}

	// Record the stats delta in the StoreMetrics.
	deltaStats := *b.state.Stats
	deltaStats.Subtract(prevStats)
	r.store.metrics.addMVCCStats(ctx, r.tenantMetricsRef, deltaStats)

	// Record the write activity, passing a 0 nodeID because replica.writeStats
	// intentionally doesn't track the origin of the writes.
	b.r.loadStats.writeKeys.RecordCount(float64(b.mutations), 0)

	now := timeutil.Now()
	if needsSplitBySize && r.splitQueueThrottle.ShouldProcess(now) {
		r.store.splitQueue.MaybeAddAsync(ctx, r, r.store.Clock().NowAsClockTimestamp())
	}
	if needsMergeBySize && r.mergeQueueThrottle.ShouldProcess(now) {
		r.store.mergeQueue.MaybeAddAsync(ctx, r, r.store.Clock().NowAsClockTimestamp())
	}
	if needsTruncationByLogSize {
		r.store.raftLogQueue.MaybeAddAsync(ctx, r, r.store.Clock().NowAsClockTimestamp())
	}

	b.recordStatsOnCommit()
	return nil
}

// addAppliedStateKeyToBatch adds the applied state key to the application
// batch's RocksDB batch. This records the highest raft and lease index that
// have been applied as of this batch. It also records the Range's mvcc stats.
func (b *replicaAppBatch) addAppliedStateKeyToBatch(ctx context.Context) error {
	// Set the range applied state, which includes the last applied raft and
	// lease index along with the mvcc stats, all in one key.
	loader := &b.r.raftMu.stateLoader
	return loader.SetRangeAppliedState(
		ctx, b.batch, b.state.RaftAppliedIndex, b.state.LeaseAppliedIndex, b.state.RaftAppliedIndexTerm,
		b.state.Stats, b.state.RaftClosedTimestamp, &b.asAlloc,
	)
}

func (b *replicaAppBatch) recordStatsOnCommit() {
	b.sm.stats.entriesProcessed += b.entries
	b.sm.stats.entriesProcessedBytes += b.entryBytes
	b.sm.stats.numEmptyEntries += b.emptyEntries
	b.sm.stats.batchesProcessed++
	b.sm.stats.followerStoreWriteBytes.Merge(b.followerStoreWriteBytes)

	elapsed := timeutil.Since(b.start)
	b.r.store.metrics.RaftCommandCommitLatency.RecordValue(elapsed.Nanoseconds())
}

// Close implements the apply.Batch interface.
func (b *replicaAppBatch) Close() {
	if b.batch != nil {
		b.batch.Close()
	}
	*b = replicaAppBatch{}
}

// raftClosedTimestampAssertionsEnabled provides an emergency way of shutting
// down assertions.
var raftClosedTimestampAssertionsEnabled = envutil.EnvOrDefaultBool("COCKROACH_RAFT_CLOSEDTS_ASSERTIONS_ENABLED", true)

// Assert that the current command is not writing under the closed timestamp.
// This check only applies to certain write commands, mainly IsIntentWrite,
// since others (for example, EndTxn) can operate below the closed timestamp.
//
// Note that we check that we're we're writing under b.state.RaftClosedTimestamp
// (i.e. below the timestamp closed by previous commands), not below
// cmd.Cmd.ClosedTimestamp. A command is allowed to write below the closed
// timestamp carried by itself; in other words cmd.Cmd.ClosedTimestamp is a
// promise about future commands, not the command carrying it.
func (b *replicaAppBatch) assertNoWriteBelowClosedTimestamp(cmd *replicatedCmd) error {
	if !cmd.IsLocal() || !cmd.proposal.Request.AppliesTimestampCache() {
		return nil
	}
	if !raftClosedTimestampAssertionsEnabled {
		return nil
	}
	wts := cmd.Cmd.ReplicatedEvalResult.WriteTimestamp
	if !wts.IsEmpty() && wts.LessEq(b.state.RaftClosedTimestamp) {
		wts := wts // Make a shadow variable that escapes to the heap.
		var req redact.StringBuilder
		if cmd.proposal != nil {
			req.Print(cmd.proposal.Request)
		} else {
			req.SafeString("request unknown; not leaseholder")
		}
		return errors.AssertionFailedf(
			"command writing below closed timestamp; cmd: %x, write ts: %s, "+
				"batch state closed: %s, command closed: %s, request: %s, lease: %s.\n"+
				"This assertion will fire again on restart; to ignore run with env var\n"+
				"COCKROACH_RAFT_CLOSEDTS_ASSERTIONS_ENABLED=false",
			cmd.ID, wts,
			b.state.RaftClosedTimestamp, cmd.Cmd.ClosedTimestamp,
			req, b.state.Lease)
	}
	return nil
}

// Assert that the closed timestamp carried by the command is not below one from
// previous commands.
func (b *replicaAppBatch) assertNoCmdClosedTimestampRegression(
	ctx context.Context, cmd *replicatedCmd,
) error {
	if !raftClosedTimestampAssertionsEnabled {
		return nil
	}
	existingClosed := &b.state.RaftClosedTimestamp
	newClosed := cmd.Cmd.ClosedTimestamp
	if newClosed != nil && !newClosed.IsEmpty() && newClosed.Less(*existingClosed) {
		var req redact.StringBuilder
		if cmd.IsLocal() {
			req.Print(cmd.proposal.Request)
		} else {
			req.SafeString("<unknown; not leaseholder>")
		}
		var prevReq redact.StringBuilder
		if req := b.closedTimestampSetter.leaseReq; req != nil {
			prevReq.Printf("lease acquisition: %s (prev: %s)", req.Lease, req.PrevLease)
		} else {
			prevReq.SafeString("<unknown; not leaseholder or not lease request>")
		}

		logTail, err := b.r.printRaftTail(ctx, 100 /* maxEntries */, 2000 /* maxCharsPerEntry */)
		if err != nil {
			if logTail != "" {
				logTail = logTail + "\n; error printing log: " + err.Error()
			} else {
				logTail = "error printing log: " + err.Error()
			}
		}

		return errors.AssertionFailedf(
			"raft closed timestamp regression in cmd: %x (term: %d, index: %d); batch state: %s, command: %s, lease: %s, req: %s, applying at LAI: %d.\n"+
				"Closed timestamp was set by req: %s under lease: %s; applied at LAI: %d. Batch idx: %d.\n"+
				"This assertion will fire again on restart; to ignore run with env var COCKROACH_RAFT_CLOSEDTS_ASSERTIONS_ENABLED=false\n"+
				"Raft log tail:\n%s",
			cmd.ID, cmd.Term, cmd.Index(), existingClosed, newClosed, b.state.Lease, req, cmd.LeaseIndex,
			prevReq, b.closedTimestampSetter.lease, b.closedTimestampSetter.leaseIdx, b.entries,
			logTail)
	}
	return nil
}

// ephemeralReplicaAppBatch implements the apply.Batch interface.
//
// The batch performs the bare-minimum amount of work to be able to
// determine whether a replicated command should be rejected or applied.
type ephemeralReplicaAppBatch struct {
	r     *Replica
	state kvserverpb.ReplicaState
}

// Stage implements the apply.Batch interface.
func (mb *ephemeralReplicaAppBatch) Stage(
	ctx context.Context, cmdI apply.Command,
) (apply.CheckedCommand, error) {
	cmd := cmdI.(*replicatedCmd)

	mb.r.shouldApplyCommand(ctx, cmd, &mb.state)
	mb.state.LeaseAppliedIndex = cmd.LeaseIndex
	return cmd, nil
}

// Close implements the apply.Batch interface.
func (mb *ephemeralReplicaAppBatch) Close() {
	*mb = ephemeralReplicaAppBatch{}
}