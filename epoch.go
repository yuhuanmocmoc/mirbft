/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package mirbft

import (
	pb "github.com/IBM/mirbft/mirbftpb"
	"github.com/golang/protobuf/proto"
)

// epochConfig is the information required by the various
// state machines whose state is scoped to an epoch
type epochConfig struct {
	// myConfig is the configuration specific to this node
	myConfig *Config

	// number is the epoch number this config applies to
	number uint64

	// plannedExpiration is when this epoch ends, if it ends gracefully
	plannedExpiration SeqNo

	// F is the total number of faults tolerated by the network
	f int

	// CheckpointInterval is the number of sequence numbers to commit before broadcasting a checkpoint
	checkpointInterval SeqNo

	// nodes is all the node ids in the network
	nodes []NodeID

	// buckets is a map from bucket ID to leader ID
	buckets map[BucketID]NodeID
}

// intersectionQuorum is the number of nodes required to agree
// such that any two sets intersected will each contain some same
// correct node.  This is ceil((n+f+1)/2), which is equivalent to
// (n+f+2)/2 under truncating integer math.
func (ec *epochConfig) intersectionQuorum() int {
	return (len(ec.nodes) + ec.f + 2) / 2
}

// weakQuorum is f+1
func (ec *epochConfig) someCorrectQuorum() int {
	return ec.f + 1
}

type epochState int

const (
	prepending epochState = iota
	pending
	echoing
	readying
	active
	done
)

type epoch struct {
	// config contains the static components of the epoch
	config *epochConfig

	ticks uint64

	myNewEpoch *pb.NewEpoch

	proposer *proposer

	state epochState

	inactiveTicks int

	echos map[NodeID]*pb.EpochConfig

	readies map[NodeID]*pb.EpochConfig

	changes map[NodeID]*pb.EpochChange

	suspicions map[NodeID]struct{}

	checkpointWindows []*checkpointWindow

	baseCheckpointValue []byte
}

// newEpoch creates a new epoch.  It uses the supplied initial checkpointWindows until
// new checkpoint windows are created using the given epochConfig.  The initialCheckpoint
// windows may be empty, of length 1, or length 2.
func newEpoch(lastSeqNo SeqNo, startingCheckpointValue []byte, config *epochConfig, initialCheckpointWindows []*checkpointWindow) *epoch {
	proposer := newProposer(config)
	proposer.maxAssignable = config.checkpointInterval

	checkpointWindows := make([]*checkpointWindow, 2)
	lastEnd := lastSeqNo
	for i := 0; i < 2; i++ {
		if len(initialCheckpointWindows) > i {
			checkpointWindows[i] = initialCheckpointWindows[i].clone()
			lastEnd = checkpointWindows[i].end
			continue
		}
		newLastEnd := lastEnd + config.checkpointInterval
		checkpointWindows[i] = newCheckpointWindow(lastEnd+1, newLastEnd, config)
		lastEnd = newLastEnd
	}

	return &epoch{
		baseCheckpointValue: startingCheckpointValue,
		config:              config,
		echos:               map[NodeID]*pb.EpochConfig{},
		readies:             map[NodeID]*pb.EpochConfig{},
		suspicions:          map[NodeID]struct{}{},
		changes:             map[NodeID]*pb.EpochChange{},
		checkpointWindows:   checkpointWindows,
		proposer:            proposer,
	}
}

// Summary of Bracha reliable broadcast from:
//   https://dcl.epfl.ch/site/_media/education/sdc_byzconsensus.pdf
//
// upon r-broadcast(m): // only Ps
// send message (SEND, m) to all
//
// upon receiving a message (SEND, m) from Ps:
// send message (ECHO, m) to all
//
// upon receiving ceil((n+t+1)/2)
// e messages(ECHO, m) and not having sent a READY message:
// send message (READY, m) to all
//
// upon receiving t+1 messages(READY, m) and not having sent a READY message:
// send message (READY, m) to all
// upon receiving 2t + 1 messages (READY, m):
//
// r-deliver(m)

func (e *epoch) applyNewEpochMsg(msg *pb.NewEpoch) *Actions {
	if e.state != pending {
		// TODO log oddity? maybe ensure not possible via nodemsgs
		return &Actions{}
	}

	epochChanges := map[NodeID]*pb.EpochChange{}
	for _, remoteEpochChange := range msg.EpochChanges {
		if _, ok := epochChanges[NodeID(remoteEpochChange.NodeId)]; ok {
			// TODO, malformed, log oddity
			return &Actions{}
		}

		epochChanges[NodeID(remoteEpochChange.NodeId)] = remoteEpochChange.EpochChange
	}

	// XXX need to validate the signatures on the epoch changes

	newEpochConfig := constructNewEpochConfig(epochChanges)

	if !proto.Equal(newEpochConfig, msg.Config) {
		// TODO byzantine, log oddity
		return &Actions{}
	}

	e.state = echoing

	return &Actions{
		Broadcast: []*pb.Msg{
			{
				Type: &pb.Msg_NewEpochEcho{
					NewEpochEcho: &pb.NewEpochEcho{
						Config: msg.Config,
					},
				},
			},
		},
	}
}

func (e *epoch) applyNewEpochEchoMsg(source NodeID, msg *pb.NewEpochEcho) *Actions {
	if _, ok := e.echos[source]; ok {
		// TODO, if different, byzantine, oddities
		return &Actions{}
	}

	e.echos[source] = msg.Config

	if len(e.echos) < e.config.intersectionQuorum() {
		return &Actions{}
	}

	// XXX we need to verify that the configs actually match, but
	// since we have not computed a digest, this is potentially expensive
	// so deferring the implementation.

	if e.state > echoing {
		return &Actions{}
	}

	e.state = readying

	return &Actions{
		Broadcast: []*pb.Msg{
			{
				Type: &pb.Msg_NewEpochReady{
					NewEpochReady: &pb.NewEpochReady{
						Config: msg.Config,
					},
				},
			},
		},
	}
}

func (e *epoch) applyNewEpochReadyMsg(source NodeID, msg *pb.NewEpochReady) *Actions {
	if _, ok := e.readies[source]; ok {
		// TODO, if different, byzantine, oddities
		return &Actions{}
	}

	e.readies[source] = msg.Config

	if e.state > readying {
		// We've already accepted the epoch config, move along
		return &Actions{}
	}

	// XXX we need to verify that the configs actually match, but
	// since we have not computed a digest, this is potentially expensive
	// so deferring the implementation.

	if len(e.readies) < e.config.someCorrectQuorum() {
		return &Actions{}
	}

	if e.state < readying {
		e.state = readying

		return &Actions{
			Broadcast: []*pb.Msg{
				{
					Type: &pb.Msg_NewEpochReady{
						NewEpochReady: &pb.NewEpochReady{
							Config: msg.Config,
						},
					},
				},
			},
		}
	}

	if len(e.readies) >= e.config.intersectionQuorum() {
		e.state = active
	}

	return &Actions{}
}

func (e *epoch) checkpointWindowForSeqNo(seqNo SeqNo) *checkpointWindow {
	if e.config.plannedExpiration < seqNo {
		return nil
	}

	if e.checkpointWindows[0].start > seqNo {
		return nil
	}

	offset := seqNo - SeqNo(e.checkpointWindows[0].start)
	index := offset / SeqNo(e.config.checkpointInterval)
	if int(index) >= len(e.checkpointWindows) {
		return nil
	}
	return e.checkpointWindows[index]
}

func (e *epoch) applyPreprepareMsg(source NodeID, msg *pb.Preprepare) *Actions {
	if e.state == done {
		return &Actions{}
	}

	return e.checkpointWindowForSeqNo(SeqNo(msg.SeqNo)).applyPreprepareMsg(source, SeqNo(msg.SeqNo), BucketID(msg.Bucket), msg.Batch)
}

func (e *epoch) applyPrepareMsg(source NodeID, msg *pb.Prepare) *Actions {
	if e.state == done {
		return &Actions{}
	}

	return e.checkpointWindowForSeqNo(SeqNo(msg.SeqNo)).applyPrepareMsg(source, SeqNo(msg.SeqNo), BucketID(msg.Bucket), msg.Digest)
}

func (e *epoch) applyCommitMsg(source NodeID, msg *pb.Commit) *Actions {
	if e.state == done {
		return &Actions{}
	}

	return e.checkpointWindowForSeqNo(SeqNo(msg.SeqNo)).applyCommitMsg(source, SeqNo(msg.SeqNo), BucketID(msg.Bucket), msg.Digest)
}

func (e *epoch) applyCheckpointMsg(source NodeID, seqNo SeqNo, value []byte) *Actions {
	if e.state == done {
		return &Actions{}
	}

	lastCW := e.checkpointWindows[len(e.checkpointWindows)-1]

	if lastCW.epochConfig.plannedExpiration == lastCW.end {
		// This epoch is about to end gracefully, don't allocate new windows
		// so no need to go into allocation or garbage collection logic.
		return &Actions{}
	}

	cw := e.checkpointWindowForSeqNo(seqNo)

	secondToLastCW := e.checkpointWindows[len(e.checkpointWindows)-2]
	actions := cw.applyCheckpointMsg(source, value)

	if secondToLastCW.garbageCollectible {
		e.proposer.maxAssignable = lastCW.end
		e.checkpointWindows = append(
			e.checkpointWindows,
			newCheckpointWindow(
				lastCW.end+1,
				lastCW.end+e.config.checkpointInterval,
				e.config,
			),
		)
	}

	actions.Append(e.proposer.drainQueue())
	for len(e.checkpointWindows) > 2 && (e.checkpointWindows[0].obsolete || e.checkpointWindows[1].garbageCollectible) {
		e.baseCheckpointValue = e.checkpointWindows[0].myValue
		e.checkpointWindows = e.checkpointWindows[1:]
	}

	return actions
}

func (e *epoch) applyPreprocessResult(preprocessResult PreprocessResult) *Actions {
	if e.state == done {
		return &Actions{}
	}

	bucketID := BucketID(preprocessResult.Cup % uint64(len(e.config.buckets)))
	nodeID := e.config.buckets[bucketID]
	if nodeID == NodeID(e.config.myConfig.ID) {
		return e.proposer.propose(preprocessResult.Proposal.Data)
	}

	if preprocessResult.Proposal.Source == e.config.myConfig.ID {
		// I originated this proposal, but someone else leads this bucket,
		// forward the message to them
		return &Actions{
			Unicast: []Unicast{
				{
					Target: uint64(nodeID),
					Msg: &pb.Msg{
						Type: &pb.Msg_Forward{
							Forward: &pb.Forward{
								Epoch:  e.config.number,
								Bucket: uint64(bucketID),
								Data:   preprocessResult.Proposal.Data,
							},
						},
					},
				},
			},
		}
	}

	// Someone forwarded me this proposal, but I'm not responsible for it's bucket
	// TODO, log oddity? Assign it to the wrong bucket? Forward it again?
	return &Actions{}
}

func (e *epoch) applyDigestResult(seqNo SeqNo, bucketID BucketID, digest []byte) *Actions {
	if e.state == done {
		return &Actions{}
	}

	return e.checkpointWindowForSeqNo(seqNo).applyDigestResult(seqNo, bucketID, digest)
}

func (e *epoch) applyValidateResult(seqNo SeqNo, bucketID BucketID, valid bool) *Actions {
	if e.state == done {
		return &Actions{}
	}

	return e.checkpointWindowForSeqNo(seqNo).applyValidateResult(seqNo, bucketID, valid)
}

func (e *epoch) applyCheckpointResult(seqNo SeqNo, value []byte) *Actions {
	if e.state == done {
		return &Actions{}
	}

	cw := e.checkpointWindowForSeqNo(seqNo)
	if cw == nil {
		panic("received an unexpected checkpoint result")
	}
	return cw.applyCheckpointResult(value)
}

func (e *epoch) tick() *Actions {
	e.ticks++

	actions := &Actions{}

	if e.state != done {
		// This is done first, as this tick may transition
		// the state to done.
		actions.Append(e.tickNotDone())
	}

	switch e.state {
	case prepending:
		actions.Append(e.tickPrepending())
	case pending:
		actions.Append(e.tickPending())
	case active:
		actions.Append(e.tickActive())
	default: // case done:
	}

	return actions
}

func (e *epoch) tickPrepending() *Actions {
	newEpoch := e.constructNewEpoch() // TODO, recomputing over and over again isn't useful unless we've gotten new epoch change messages in the meantime, should we somehow store the last one we computed?

	if newEpoch == nil {
		return &Actions{}
	}

	e.state = pending
	e.myNewEpoch = newEpoch

	if e.config.number%uint64(len(e.config.nodes)) == e.config.myConfig.ID {
		return &Actions{
			Broadcast: []*pb.Msg{
				{
					Type: &pb.Msg_NewEpoch{
						NewEpoch: newEpoch,
					},
				},
			},
		}
	}

	return &Actions{}
}

func (e *epoch) tickPending() *Actions {
	e.inactiveTicks++
	// TODO new view timeout
	return &Actions{}
}

func (e *epoch) tickNotDone() *Actions {
	if len(e.suspicions) < e.config.intersectionQuorum() {
		return &Actions{}
	}

	e.state = done

	return &Actions{
		Broadcast: []*pb.Msg{
			{
				Type: &pb.Msg_EpochChange{
					EpochChange: e.constructEpochChange(),
				},
			},
		},
	}

}

func (e *epoch) tickActive() *Actions {
	actions := &Actions{}
	if e.config.myConfig.HeartbeatTicks != 0 && e.ticks%uint64(e.config.myConfig.HeartbeatTicks) == 0 {
		actions.Append(e.proposer.noopAdvance())
	}

	for _, cw := range e.checkpointWindows {
		actions.Append(cw.tick())
	}

	return actions
}

func (e *epoch) constructEpochChange() *pb.EpochChange {
	epochChange := &pb.EpochChange{}

	if e.checkpointWindows[0].myValue == nil {

	}

	for _, cw := range e.checkpointWindows {
		if cw.myValue == nil {
			// Checkpoints necessarily generated in order, no further checkpoints are ready
			break
		}
		epochChange.Checkpoints = append(epochChange.Checkpoints, &pb.Checkpoint{
			SeqNo: uint64(cw.end),
			Value: cw.myValue,
		})
	}

	// XXX implement

	return epochChange
}

func (e *epoch) constructNewEpoch() *pb.NewEpoch {
	config := constructNewEpochConfig(e.changes)
	if config == nil {
		return nil
	}

	remoteChanges := make([]*pb.NewEpoch_RemoteEpochChange, len(e.changes), 0)
	for nodeID, change := range e.changes {
		remoteChanges = append(remoteChanges, &pb.NewEpoch_RemoteEpochChange{
			NodeId:      uint64(nodeID),
			EpochChange: change,
		})
	}

	return &pb.NewEpoch{
		Config:       config,
		EpochChanges: remoteChanges,
	}
}

func constructNewEpochConfig(epochChanges map[NodeID]*pb.EpochChange) *pb.EpochConfig {
	// XXX implement

	return &pb.EpochConfig{}
}
