package committee

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/prometheus/client_golang/prometheus"

	beacon "github.com/oasisprotocol/oasis-core/go/beacon/api"
	"github.com/oasisprotocol/oasis-core/go/common/identity"
	"github.com/oasisprotocol/oasis-core/go/common/logging"
	consensus "github.com/oasisprotocol/oasis-core/go/consensus/api"
	control "github.com/oasisprotocol/oasis-core/go/control/api"
	keymanagerApi "github.com/oasisprotocol/oasis-core/go/keymanager/api"
	keymanagerClient "github.com/oasisprotocol/oasis-core/go/keymanager/client"
	registry "github.com/oasisprotocol/oasis-core/go/registry/api"
	roothash "github.com/oasisprotocol/oasis-core/go/roothash/api"
	"github.com/oasisprotocol/oasis-core/go/roothash/api/block"
	"github.com/oasisprotocol/oasis-core/go/runtime/host"
	runtimeRegistry "github.com/oasisprotocol/oasis-core/go/runtime/registry"
	"github.com/oasisprotocol/oasis-core/go/runtime/txpool"
	"github.com/oasisprotocol/oasis-core/go/worker/common/api"
	"github.com/oasisprotocol/oasis-core/go/worker/common/p2p"
)

var (
	processedBlockCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oasis_worker_processed_block_count",
			Help: "Number of processed roothash blocks.",
		},
		[]string{"runtime"},
	)
	processedEventCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oasis_worker_processed_event_count",
			Help: "Number of processed roothash events.",
		},
		[]string{"runtime"},
	)
	failedRoundCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oasis_worker_failed_round_count",
			Help: "Number of failed roothash rounds.",
		},
		[]string{"runtime"},
	)
	epochTransitionCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "oasis_worker_epoch_transition_count",
			Help: "Number of epoch transitions.",
		},
		[]string{"runtime"},
	)
	epochNumber = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "oasis_worker_epoch_number",
			Help: "Current epoch number as seen by the worker.",
		},
		[]string{"runtime"},
	)

	nodeCollectors = []prometheus.Collector{
		processedBlockCount,
		processedEventCount,
		failedRoundCount,
		epochTransitionCount,
		epochNumber,
	}

	metricsOnce sync.Once
)

// NodeHooks defines a worker's duties at common events.
// These are called from the runtime's common node's worker.
type NodeHooks interface {
	// HandlePeerTx handles a transaction received from a (non-local) peer.
	HandlePeerTx(ctx context.Context, tx []byte) error

	// Guarded by CrossNode.
	HandleEpochTransitionLocked(*EpochSnapshot)
	// Guarded by CrossNode.
	HandleNewBlockEarlyLocked(*block.Block)
	// Guarded by CrossNode.
	HandleNewBlockLocked(*block.Block)
	// Guarded by CrossNode.
	HandleNewEventLocked(*roothash.Event)
	HandleRuntimeHostEvent(*host.Event)

	// Initialized returns a channel that will be closed when the worker is initialized and ready
	// to service requests.
	Initialized() <-chan struct{}
}

// Node is a committee node.
type Node struct {
	*runtimeRegistry.RuntimeHostNode

	Runtime runtimeRegistry.Runtime

	HostNode control.ControlledNode

	Identity         *identity.Identity
	KeyManager       keymanagerApi.Backend
	KeyManagerClient *keymanagerClient.Client
	Consensus        consensus.Backend
	Group            *Group
	P2P              *p2p.P2P
	TxPool           txpool.TransactionPool

	ctx       context.Context
	cancelCtx context.CancelFunc
	stopCh    chan struct{}
	stopOnce  sync.Once
	quitCh    chan struct{}
	initCh    chan struct{}

	hooks []NodeHooks

	// Mutable and shared between nodes' workers.
	// Guarded by .CrossNode.
	CrossNode             sync.Mutex
	CurrentBlock          *block.Block
	CurrentBlockHeight    int64
	CurrentConsensusBlock *consensus.LightBlock
	CurrentDescriptor     *registry.Runtime
	CurrentEpoch          beacon.EpochTime
	Height                int64

	logger *logging.Logger
}

// Name returns the service name.
func (n *Node) Name() string {
	return "committee node"
}

// Start starts the service.
func (n *Node) Start() error {
	if err := n.Group.Start(); err != nil {
		return fmt.Errorf("failed to start group services: %w", err)
	}

	// Start the transaction pool.
	if err := n.TxPool.Start(); err != nil {
		return fmt.Errorf("failed to start transaction pool: %w", err)
	}

	go n.worker()
	return nil
}

// Stop halts the service.
func (n *Node) Stop() {
	n.stopOnce.Do(func() {
		close(n.stopCh)
		n.TxPool.Stop()
	})
}

// Quit returns a channel that will be closed when the service terminates.
func (n *Node) Quit() <-chan struct{} {
	return n.quitCh
}

// Cleanup performs the service specific post-termination cleanup.
func (n *Node) Cleanup() {
}

// Initialized returns a channel that will be closed when the node is
// initialized and ready to service requests.
func (n *Node) Initialized() <-chan struct{} {
	return n.initCh
}

// AddHooks adds a NodeHooks to be called.
// There is no going back.
func (n *Node) AddHooks(hooks NodeHooks) {
	n.hooks = append(n.hooks, hooks)
}

// GetStatus returns the common committee node status.
func (n *Node) GetStatus(ctx context.Context) (*api.Status, error) {
	n.CrossNode.Lock()
	defer n.CrossNode.Unlock()

	var status api.Status
	if n.CurrentBlock != nil {
		status.LatestRound = n.CurrentBlock.Header.Round
		status.LatestHeight = n.CurrentBlockHeight
	}

	epoch := n.Group.GetEpochSnapshot()
	if cmte := epoch.GetExecutorCommittee(); cmte != nil {
		status.ExecutorRoles = cmte.Roles
	}
	status.IsTransactionScheduler = epoch.IsTransactionScheduler(status.LatestRound)

	status.Peers = n.P2P.Peers(n.Runtime.ID())

	return &status, nil
}

func (n *Node) getMetricLabels() prometheus.Labels {
	return prometheus.Labels{
		"runtime": n.Runtime.ID().String(),
	}
}

// Guarded by n.CrossNode.
func (n *Node) handleEpochTransitionLocked(height int64) {
	n.logger.Info("epoch transition has occurred")

	epochTransitionCount.With(n.getMetricLabels()).Inc()

	// Transition group.
	if err := n.Group.EpochTransition(n.ctx, height); err != nil {
		n.logger.Error("unable to handle epoch transition",
			"err", err,
		)
	}

	epoch := n.Group.GetEpochSnapshot()
	epochNumber.With(n.getMetricLabels()).Set(float64(epoch.epochNumber))
	for _, hooks := range n.hooks {
		hooks.HandleEpochTransitionLocked(epoch)
	}
}

// Guarded by n.CrossNode.
func (n *Node) handleSuspendLocked(height int64) {
	n.logger.Warn("runtime has been suspended")

	// Suspend group.
	n.Group.Suspend(n.ctx)

	epoch := n.Group.GetEpochSnapshot()
	for _, hooks := range n.hooks {
		hooks.HandleEpochTransitionLocked(epoch)
	}
}

// Guarded by n.CrossNode.
func (n *Node) handleNewBlockLocked(blk *block.Block, height int64) {
	processedBlockCount.With(n.getMetricLabels()).Inc()

	header := blk.Header

	// The first received block will be treated an epoch transition (if valid).
	// This will refresh the committee on the first block,
	// instead of waiting for the next epoch transition to occur.
	// Helps in cases where node is restarted mid epoch.
	firstBlockReceived := n.CurrentBlock == nil

	// Fetch light consensus block.
	consensusBlk, err := n.Consensus.GetLightBlock(n.ctx, height)
	if err != nil {
		n.logger.Error("failed to query light block",
			"err", err,
			"height", height,
			"round", blk.Header.Round,
		)
		return
	}

	// Update the current block.
	n.CurrentBlock = blk
	n.CurrentBlockHeight = height
	n.CurrentConsensusBlock = consensusBlk

	// Update active descriptor on epoch transitions.
	if firstBlockReceived || header.HeaderType == block.EpochTransition {
		var ad *registry.Runtime
		ad, err = n.Consensus.Registry().GetRuntime(n.ctx, &registry.NamespaceQuery{
			ID:     n.Runtime.ID(),
			Height: height,
		})
		switch {
		case err == nil:
			n.CurrentDescriptor = ad
		case errors.Is(err, registry.ErrNoSuchRuntime):
			// Runtime was probably suspended, just keep the current descriptor as is.
		default:
			n.logger.Error("failed to query runtime descriptor",
				"err", err,
			)
			return
		}

		n.CurrentEpoch, err = n.Consensus.Beacon().GetEpoch(n.ctx, height)
		if err != nil {
			n.logger.Error("failed to fetch current epoch",
				"err", err,
			)
			return
		}
	}

	for _, hooks := range n.hooks {
		hooks.HandleNewBlockEarlyLocked(blk)
	}

	// Perform actions based on block type.
	switch header.HeaderType {
	case block.Normal:
		if firstBlockReceived {
			n.logger.Warn("forcing an epoch transition on first received block")
			n.handleEpochTransitionLocked(height)
		} else {
			// Normal block.
			n.Group.RoundTransition()
		}
	case block.RoundFailed:
		if firstBlockReceived {
			n.logger.Warn("forcing an epoch transition on first received block")
			n.handleEpochTransitionLocked(height)
		} else {
			// Round has failed.
			n.logger.Warn("round has failed")
			n.Group.RoundTransition()

			failedRoundCount.With(n.getMetricLabels()).Inc()
		}
	case block.EpochTransition:
		// Process an epoch transition.
		n.handleEpochTransitionLocked(height)
	case block.Suspended:
		// Process runtime being suspended.
		n.handleSuspendLocked(height)
	default:
		n.logger.Error("invalid block type",
			"block", blk,
		)
		return
	}

	err = n.TxPool.ProcessBlock(&txpool.BlockInfo{
		RuntimeBlock:     n.CurrentBlock,
		ConsensusBlock:   n.CurrentConsensusBlock,
		Epoch:            n.CurrentEpoch,
		ActiveDescriptor: n.CurrentDescriptor,
	})
	if err != nil {
		n.logger.Error("failed to process block in transaction pool",
			"err", err,
		)
	}

	for _, hooks := range n.hooks {
		hooks.HandleNewBlockLocked(blk)
	}
}

// Guarded by n.CrossNode.
func (n *Node) handleNewEventLocked(ev *roothash.Event) {
	processedEventCount.With(n.getMetricLabels()).Inc()

	for _, hooks := range n.hooks {
		hooks.HandleNewEventLocked(ev)
	}
}

func (n *Node) handleRuntimeHostEvent(ev *host.Event) {
	for _, hooks := range n.hooks {
		hooks.HandleRuntimeHostEvent(ev)
	}
}

func (n *Node) worker() {
	n.logger.Info("starting committee node")

	defer close(n.quitCh)
	defer (n.cancelCtx)()

	// Wait for consensus sync.
	n.logger.Info("delaying worker start until after initial synchronization")
	select {
	case <-n.stopCh:
		return
	case <-n.Consensus.Synced():
	}
	n.logger.Info("consensus has finished initial synchronization")

	// Wait for the runtime.
	rt, err := n.Runtime.ActiveDescriptor(n.ctx)
	if err != nil {
		n.logger.Error("failed to wait for registry descriptor",
			"err", err,
		)
		return
	}

	n.logger.Info("runtime is registered with the registry")

	// Initialize the CurrentDescriptor to make sure there is one even if the runtime gets
	// suspended.
	n.CurrentDescriptor = rt

	// If the runtime requires a key manager, wait for the key manager to actually become available
	// before processing any requests.
	if rt.KeyManager != nil {
		n.logger.Info("runtime indicates a key manager is required, waiting for it to be ready")

		n.KeyManagerClient, err = keymanagerClient.New(n.ctx, n.Runtime, n.Consensus, n.Identity)
		if err != nil {
			n.logger.Error("failed to create key manager client",
				"err", err,
			)
			return
		}

		select {
		case <-n.ctx.Done():
			n.logger.Error("failed to wait for key manager",
				"err", err,
			)
			return
		case <-n.KeyManagerClient.Initialized():
		}

		n.logger.Info("runtime has a key manager available")
	}

	// Start watching consensus blocks.
	consensusBlocks, consensusBlocksSub, err := n.Consensus.WatchBlocks(n.ctx)
	if err != nil {
		n.logger.Error("failed to subscribe to consensus blocks",
			"err", err,
		)
		return
	}
	defer consensusBlocksSub.Close()

	// Start watching roothash blocks.
	blocks, blocksSub, err := n.Consensus.RootHash().WatchBlocks(n.ctx, n.Runtime.ID())
	if err != nil {
		n.logger.Error("failed to subscribe to roothash blocks",
			"err", err,
		)
		return
	}
	defer blocksSub.Close()

	// Start watching roothash events.
	events, eventsSub, err := n.Consensus.RootHash().WatchEvents(n.ctx, n.Runtime.ID())
	if err != nil {
		n.logger.Error("failed to subscribe to roothash events",
			"err", err,
		)
		return
	}
	defer eventsSub.Close()

	// Provision the hosted runtime.
	hrt, hrtNotifier, err := n.ProvisionHostedRuntime(n.ctx)
	if err != nil {
		n.logger.Error("failed to provision hosted runtime",
			"err", err,
		)
		return
	}

	hrtEventCh, hrtSub, err := hrt.WatchEvents(n.ctx)
	if err != nil {
		n.logger.Error("failed to subscribe to hosted runtime events",
			"err", err,
		)
		return
	}
	defer hrtSub.Close()

	if err = hrt.Start(); err != nil {
		n.logger.Error("failed to start hosted runtime",
			"err", err,
		)
		return
	}
	defer hrt.Stop()

	if err = hrtNotifier.Start(); err != nil {
		n.logger.Error("failed to start runtime notifier",
			"err", err,
		)
		return
	}
	defer hrtNotifier.Stop()

	initialized := false
	for {
		select {
		case <-n.stopCh:
			n.logger.Info("termination requested")
			return
		case blk := <-consensusBlocks:
			if blk == nil {
				return
			}
			func() {
				n.CrossNode.Lock()
				defer n.CrossNode.Unlock()
				n.Height = blk.Height
			}()
		case blk := <-blocks:
			// We are initialized after we have received the first block. This makes sure that any
			// history reindexing has been completed.
			if !initialized {
				n.logger.Debug("common worker is initialized")

				close(n.initCh)
				initialized = true

				// Wait for all child workers to initialize as well.
				n.logger.Debug("waiting for child worker initialization")
				for _, hooks := range n.hooks {
					select {
					case <-hooks.Initialized():
					case <-n.stopCh:
						n.logger.Info("termination requested while waiting for child worker initialization")
						return
					}
				}
				n.logger.Debug("all child workers are initialized")
			}

			// Received a block (annotated).
			func() {
				n.CrossNode.Lock()
				defer n.CrossNode.Unlock()
				n.handleNewBlockLocked(blk.Block, blk.Height)
			}()
		case ev := <-events:
			// Received an event.
			func() {
				n.CrossNode.Lock()
				defer n.CrossNode.Unlock()
				n.handleNewEventLocked(ev)
			}()
		case ev := <-hrtEventCh:
			// Received a hosted runtime event.
			n.handleRuntimeHostEvent(ev)
		}
	}
}

func NewNode(
	hostNode control.ControlledNode,
	runtime runtimeRegistry.Runtime,
	identity *identity.Identity,
	keymanager keymanagerApi.Backend,
	consensus consensus.Backend,
	p2pHost *p2p.P2P,
	txPoolCfg *txpool.Config,
) (*Node, error) {
	metricsOnce.Do(func() {
		prometheus.MustRegister(nodeCollectors...)
	})

	ctx, cancel := context.WithCancel(context.Background())

	// Prepare committee group services.
	group, err := NewGroup(ctx, identity, runtime, consensus)
	if err != nil {
		cancel()
		return nil, err
	}

	n := &Node{
		HostNode:   hostNode,
		Runtime:    runtime,
		Identity:   identity,
		KeyManager: keymanager,
		Consensus:  consensus,
		Group:      group,
		P2P:        p2pHost,
		ctx:        ctx,
		cancelCtx:  cancel,
		stopCh:     make(chan struct{}),
		quitCh:     make(chan struct{}),
		initCh:     make(chan struct{}),
		logger:     logging.GetLogger("worker/common/committee").With("runtime_id", runtime.ID()),
	}

	// Prepare the runtime host node helpers.
	rhn, err := runtimeRegistry.NewRuntimeHostNode(n)
	if err != nil {
		return nil, err
	}
	n.RuntimeHostNode = rhn

	// Prepare transaction pool.
	txPool, err := txpool.New(runtime.ID(), txPoolCfg, n, n)
	if err != nil {
		return nil, fmt.Errorf("error creating transaction pool: %w", err)
	}
	n.TxPool = txPool

	// Register transaction message handler as that is something that all workers must handle.
	p2pHost.RegisterHandler(runtime.ID(), p2p.TopicKindTx, &txMsgHandler{n})

	return n, nil
}
