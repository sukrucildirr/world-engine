package cardinal

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/rotisserie/eris"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/errgroup"

	"pkg.world.dev/world-engine/cardinal/component"
	"pkg.world.dev/world-engine/cardinal/filter"
	"pkg.world.dev/world-engine/cardinal/gamestate"
	ecslog "pkg.world.dev/world-engine/cardinal/log"
	"pkg.world.dev/world-engine/cardinal/receipt"
	"pkg.world.dev/world-engine/cardinal/router"
	"pkg.world.dev/world-engine/cardinal/server"
	"pkg.world.dev/world-engine/cardinal/server/handler/cql"
	servertypes "pkg.world.dev/world-engine/cardinal/server/types"
	"pkg.world.dev/world-engine/cardinal/storage/redis"
	"pkg.world.dev/world-engine/cardinal/telemetry"
	"pkg.world.dev/world-engine/cardinal/txpool"
	"pkg.world.dev/world-engine/cardinal/types"
	"pkg.world.dev/world-engine/cardinal/worldstage"
	"pkg.world.dev/world-engine/sign"
)

const (
	DefaultHistoricalTicksToStore = 10
	RedisDialTimeOut              = 150
)

var _ router.Provider = &World{}           //nolint:exhaustruct
var _ servertypes.ProviderWorld = &World{} //nolint:exhaustruct

type World struct {
	SystemManager
	MessageManager
	QueryManager
	component.ComponentManager

	namespace     Namespace
	rollupEnabled bool
	cancel        context.CancelFunc

	// Storage
	redisStorage *redis.Storage
	entityStore  gamestate.Manager

	// Networking
	server        *server.Server
	serverOptions []server.Option

	// Core modules
	worldStage *worldstage.Manager
	router     router.Router
	txPool     *txpool.TxPool

	// Receipt
	receiptHistory *receipt.History
	evmTxReceipts  map[string]EVMTxReceipt

	// Telemetry
	telemetry *telemetry.Manager
	tracer    trace.Tracer // Tracer for World

	// Tick
	tick            *atomic.Uint64
	timestamp       *atomic.Uint64
	tickResults     *TickResults
	tickChannel     <-chan time.Time
	tickDoneChannel chan<- uint64
	// addChannelWaitingForNextTick accepts a channel which will be closed after a tick has been completed.
	addChannelWaitingForNextTick chan chan struct{}
}

// NewWorld creates a new World object using Redis as the storage layer.
func NewWorld(opts ...WorldOption) (*World, error) {
	serverOptions, routerOptions, cardinalOptions := separateOptions(opts)

	// Load config. Fallback value is used if it's not set.
	cfg, err := loadWorldConfig()
	if err != nil {
		return nil, eris.Wrap(err, "Failed to load config to start world")
	}

	if cfg.CardinalRollupEnabled {
		log.Info().Msgf("Creating a new Cardinal world in rollup mode")
	} else {
		log.Warn().Msg("Cardinal is running in development mode without rollup sequencing. " +
			"If you intended to run this for production use, set CARDINAL_ROLLUP=true")
	}

	// Initialize telemetry
	var tm *telemetry.Manager
	if cfg.TelemetryTraceEnabled {
		tm, err = telemetry.New(cfg.TelemetryTraceEnabled, cfg.CardinalNamespace)
		if err != nil {
			return nil, eris.Wrap(err, "failed to create telemetry manager")
		}
	}

	redisMetaStore := redis.NewRedisStorage(redis.Options{
		Addr:        cfg.RedisAddress,
		Password:    cfg.RedisPassword,
		DB:          0,                              // use default DB
		DialTimeout: RedisDialTimeOut * time.Second, // Increase startup dial timeout
	}, cfg.CardinalNamespace)

	redisStore := gamestate.NewRedisPrimitiveStorage(redisMetaStore.Client)
	entityCommandBuffer, err := gamestate.NewEntityCommandBuffer(&redisStore)
	if err != nil {
		return nil, err
	}

	tick := new(atomic.Uint64)
	world := &World{
		namespace:     Namespace(cfg.CardinalNamespace),
		rollupEnabled: cfg.CardinalRollupEnabled,
		cancel:        nil,

		// Storage
		redisStorage: &redisMetaStore,
		entityStore:  entityCommandBuffer,

		// Networking
		server:        nil, // Will be initialized in StartGame
		serverOptions: serverOptions,

		// Core modules
		worldStage:       worldstage.NewManager(),
		MessageManager:   newMessageManager(),
		SystemManager:    newSystemManager(),
		ComponentManager: component.NewManager(&redisMetaStore),
		QueryManager:     nil,
		router:           nil, // Will be set if run mode is production or its injected via options
		txPool:           txpool.New(),

		// Receipt
		receiptHistory: receipt.NewHistory(tick.Load(), DefaultHistoricalTicksToStore),
		evmTxReceipts:  make(map[string]EVMTxReceipt),

		// Telemetry
		telemetry: tm,
		tracer:    otel.Tracer("world"),

		// Tick
		tick:                         tick,
		timestamp:                    new(atomic.Uint64),
		tickResults:                  NewTickResults(tick.Load()),
		tickChannel:                  time.Tick(time.Second),
		tickDoneChannel:              nil, // Will be injected via options
		addChannelWaitingForNextTick: make(chan chan struct{}),
	}

	world.QueryManager = newQueryManager(world)

	// Initialize shard router if running in rollup mode
	if cfg.CardinalRollupEnabled {
		world.router, err = router.New(
			cfg.CardinalNamespace,
			cfg.BaseShardSequencerAddress,
			cfg.BaseShardRouterKey,
			world,
			routerOptions...,
		)
		if err != nil {
			return nil, eris.Wrap(err, "Failed to initialize shard router")
		}
	}

	// Set tick rate if provided
	// it will be overridden by WithTickChannel option if provided
	if cfg.CardinalTickRate > 0 {
		world.tickChannel = time.Tick(time.Second / time.Duration(cfg.CardinalTickRate)) //nolint:gosec
	}

	// Apply options
	for _, opt := range cardinalOptions {
		opt(world)
	}

	// Register internal plugins
	world.RegisterPlugin(newPersonaPlugin())
	world.RegisterPlugin(newFutureTaskPlugin())

	return world, nil
}

func (w *World) CurrentTick() uint64 {
	return w.tick.Load()
}

// doTick performs one game tick. This consists of taking a snapshot of all pending transactions, then calling
// each system in turn with the snapshot of transactions.
func (w *World) doTick(ctx context.Context, timestamp uint64) (err error) {
	ctx, span := w.tracer.Start(ctx, "world.tick")
	defer span.End()

	startTime := time.Now()

	// The world can only perform a tick if:
	// - We're in a recovery tick
	// - The world is currently running
	// - The world is shutting down (this will be the last or penultimate tick)
	if w.worldStage.Current() != worldstage.Recovering &&
		w.worldStage.Current() != worldstage.Running &&
		w.worldStage.Current() != worldstage.ShuttingDown {
		err := eris.Errorf("world is not in a valid state to tick %s", w.worldStage.Current())
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return err
	}

	// This defer is here to catch any panics that occur during the tick. It will log the current tick and the
	// current system that is running.
	defer w.handleTickPanic()

	// Copy the transactions from the pool so that we can safely modify the pool while the tick is running.
	txPool := w.txPool.CopyTransactions(ctx)

	// Store the timestamp for this tick
	w.timestamp.Store(timestamp)

	// Create the engine context to inject into systems
	wCtx := newWorldContextForTick(w, txPool)

	// Run all registered systems.
	// This will run the registered init systems if the current tick is 0
	if err := w.SystemManager.runSystems(ctx, wCtx); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return err
	}

	if err := w.entityStore.FinalizeTick(ctx); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		return err
	}

	w.setEvmResults(txPool.GetEVMTxs())

	// Handle tx data blob submission
	// Only submit transactions when the following criteria is satisfied:
	// 1. The shard router is set
	// 2. The world is not in the recovering stage (we don't want to resubmit past transactions)
	if w.router != nil && w.worldStage.Current() != worldstage.Recovering {
		err := w.router.SubmitTxBlob(ctx, txPool.Transactions(), w.tick.Load(), w.timestamp.Load())
		if err != nil {
			span.SetStatus(codes.Error, eris.ToString(err, true))
			span.RecordError(err)
			return eris.Wrap(err, "failed to submit transactions to base shard")
		}
	}

	// Increment the tick
	w.tick.Add(1)
	w.receiptHistory.NextTick() // todo(scott): use channels

	if w.worldStage.Current() != worldstage.Recovering {
		// Populate world.TickResults for the current tick and emit it as an Event
		w.broadcastTickResults(ctx)
	}

	log.Info().
		Int64("tick", int64(w.CurrentTick()-1)). //nolint:gosec // G115: ignoring integer overflow conversion
		Str("duration", time.Since(startTime).String()).
		Int("tx_count", txPool.GetAmountOfTxs()).
		Msg("Tick completed")

	return nil
}

// StartGame starts running the world game loop. Each time a message arrives on the tickChannel, a world tick is
// attempted. In addition, an HTTP server (listening on the given port) is created so that game messages can be sent
// to this world. After StartGame is called, RegisterComponent, registerMessagesByName,
// RegisterQueries, and RegisterSystems may not be called. If StartGame doesn't encounter any errors, it will
// block forever, running the server and ticking the game in the background.
func (w *World) StartGame() error {
	defer w.cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel

	// Handles SIGINT and SIGTERM signals and starts the shutdown process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		w.Shutdown()
	}()

	// World stage: Init -> Starting
	ok := w.worldStage.CompareAndSwap(worldstage.Init, worldstage.Starting)
	if !ok {
		return errors.New("game has already been started")
	}

	// TODO(scott): entityStore.RegisterComponents is ambiguous with cardinal.RegisterComponent.
	//  We should probably rename this to LoadComponents or something.
	if err := w.entityStore.RegisterComponents(w.GetComponents()); err != nil {
		return eris.Wrap(err, "failed to register components")
	}

	// Log world info
	ecslog.World(&log.Logger, w, zerolog.InfoLevel)

	// Start router if it is set
	if w.router != nil {
		if err := w.router.Start(); err != nil {
			return eris.Wrap(err, "failed to start router service")
		}
		if err := w.router.RegisterGameShard(ctx); err != nil {
			return eris.Wrap(err, "failed to register game shard to base shard")
		}
	}

	w.worldStage.Store(worldstage.Recovering)
	tick, err := w.entityStore.GetLastFinalizedTick()
	if err != nil {
		return eris.Wrap(err, "failed to get latest finalized tick")
	}
	w.tick.Store(tick)

	// If Cardinal is in rollup mode and router is set, recover any old state of Cardinal from base shard.
	if w.rollupEnabled && w.router != nil {
		if err := w.recoverFromChain(ctx); err != nil {
			return eris.Wrap(err, "failed to recover from chain")
		}
	}

	// TODO(scott): i find this manual tracking and incrementing of the tick very footgunny. Why can't we just
	//  use a reliable source of truth for the tick? It's not clear to me why we need to manually increment the
	//  receiptHistory tick separately.
	w.receiptHistory.SetTick(w.CurrentTick())

	// World stage: Ready -> Running
	w.worldStage.Store(worldstage.Running)

	g, ctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return w.startGameLoop(ctx, w.tickChannel, w.tickDoneChannel)
	})
	g.Go(func() error {
		w.server, err = server.New(w, w.GetRegisteredComponents(), w.GetRegisteredMessages(), w.serverOptions...)
		if err != nil {
			return err
		}
		return w.server.Serve(ctx)
	})
	if err := g.Wait(); err != nil {
		return eris.Wrap(err, "error occurred while running cardinal")
	}

	return nil
}

func (w *World) startGameLoop(ctx context.Context, tickStart <-chan time.Time, tickDone chan<- uint64) error {
	log.Info().Msg("Game loop started")
	var waitingChs []chan struct{}

loop:
	for {
		select {
		case <-ctx.Done():
			log.Info().Msg("Shutting down game loop")
			w.drainChannelsWaitingForNextTick()
			closeAllChannels(waitingChs)
			if tickDone != nil {
				close(tickDone)
			}
			// We need to use a labeled loop because the inner break will only break out of the select statement
			break loop

		case _, ok := <-tickStart:
			if !ok {
				return eris.New("tickStart channel has been closed; tick rate is now unbounded.")
			}
			w.tickTheEngine(context.Background(), tickDone)
			closeAllChannels(waitingChs)
			waitingChs = waitingChs[:0]

		case ch := <-w.addChannelWaitingForNextTick:
			waitingChs = append(waitingChs, ch)
		}
	}

	log.Info().Msg("Successfully shut down game loop")
	return nil
}

func (w *World) tickTheEngine(ctx context.Context, tickDone chan<- uint64) {
	currTick := w.CurrentTick()
	// this is the final point where errors bubble up and hit a panic. There are other places where this occurs
	// but this is the highest terminal point.
	// the panic may point you to here, (or the tick function) but the real stack trace is in the error message.
	err := w.doTick(ctx, uint64(time.Now().UnixMilli())) //nolint:gosec // G115: ignoring integer overflow conversion
	if err != nil {
		bytes, errMarshal := json.Marshal(eris.ToJSON(err, true))
		if errMarshal != nil {
			panic(errMarshal)
		}
		panic(string(bytes))
	}
	if tickDone != nil {
		tickDone <- currTick
	}
}

func (w *World) IsGameRunning() bool {
	return w.worldStage.Current() == worldstage.Running
}

// Shutdown will trigger a graceful shutdown of the World.
func (w *World) Shutdown() {
	if w.worldStage.Current() == worldstage.ShutDown || w.worldStage.Current() == worldstage.ShuttingDown {
		log.Warn().Msgf("Cardinal is already %s, ignoring shutdown request", w.worldStage.Current())
		return
	}

	log.Info().Msg("Shutting down cardinal")
	w.worldStage.Store(worldstage.ShuttingDown)

	// Cancel the context used for server and game loop, therefore triggering their shutdown.
	w.cancel()
	<-w.worldStage.NotifyOnStage(worldstage.ShutDown)

	log.Info().Msg("Successfully shut down cardinal")
}

// cleanup is called after StartGame terminates. It does the housekeeping required to cleanly shutdown World.
func (w *World) cleanup() {
	if err := w.redisStorage.Close(); err != nil {
		log.Error().Err(err).Msg("Failed to close storage connection")
	}
	if w.telemetry != nil {
		if err := w.telemetry.Shutdown(); err != nil {
			log.Error().Err(err).Msg("Failed to shut down telemetry")
		}
	}
	w.worldStage.Store(worldstage.ShutDown)
}

func (w *World) handleTickPanic() {
	if r := recover(); r != nil {
		log.Error().Msgf(
			"Tick: %d, Current running system: %s",
			w.CurrentTick(),
			w.SystemManager.GetCurrentSystem(),
		)
		panic(r)
	}
}

func (w *World) RegisterPlugin(plugin Plugin) {
	if err := plugin.Register(w); err != nil {
		log.Fatal().Err(err).Msgf("failed to register plugin: %v", err)
	}
}

func closeAllChannels(chs []chan struct{}) {
	for _, ch := range chs {
		close(ch)
	}
}

// drainChannelsWaitingForNextTick continually closes any channels that are added to the
// addChannelWaitingForNextTick channel. This is used when the engine is shut down; it ensures
// any calls to WaitForNextTick that happen after a shutdown will not block.
func (w *World) drainChannelsWaitingForNextTick() {
	go func() {
		for ch := range w.addChannelWaitingForNextTick {
			close(ch)
		}
	}()
}

// AddTransaction adds a transaction to the transaction pool. This should not be used directly.
// Instead, use a MessageType.addTransaction to ensure type consistency. Returns the tick this transaction will be
// executed in.
func (w *World) AddTransaction(id types.MessageID, v any, sig *sign.Transaction) (
	tick uint64, txHash types.TxHash,
) {
	// TODO: There's no locking between getting the tick and adding the transaction, so there's no guarantee that this
	// transaction is actually added to the returned tick.
	tick = w.CurrentTick()
	txHash = w.txPool.AddTransaction(id, v, sig)
	return tick, txHash
}

func (w *World) AddEVMTransaction(
	id types.MessageID,
	v any,
	sig *sign.Transaction,
	evmTxHash string,
) (
	tick uint64, txHash types.TxHash,
) {
	tick = w.CurrentTick()
	txHash = w.txPool.AddEVMTransaction(id, v, sig, evmTxHash)
	return tick, txHash
}

func (w *World) UseNonce(signerAddress string, nonce uint64) error {
	return w.redisStorage.UseNonce(signerAddress, nonce)
}

func (w *World) GetDebugState() ([]types.DebugStateElement, error) {
	result := make([]types.DebugStateElement, 0)
	s := w.Search(filter.All())
	var eachClosureErr error
	wCtx := NewReadOnlyWorldContext(w)
	searchEachErr := s.Each(wCtx,
		func(id types.EntityID) bool {
			var components []types.ComponentMetadata
			components, eachClosureErr = w.StoreReader().GetComponentTypesForEntity(id)
			if eachClosureErr != nil {
				return false
			}
			resultElement := types.DebugStateElement{
				ID:         id,
				Components: make(map[string]json.RawMessage),
			}
			for _, c := range components {
				var data json.RawMessage
				data, eachClosureErr = w.StoreReader().GetComponentForEntityInRawJSON(c, id)
				if eachClosureErr != nil {
					return false
				}
				resultElement.Components[c.Name()] = data
			}
			result = append(result, resultElement)
			return true
		},
	)
	if eachClosureErr != nil {
		return nil, eachClosureErr
	}
	if searchEachErr != nil {
		return nil, searchEachErr
	}
	return result, nil
}

func (w *World) Namespace() string {
	return string(w.namespace)
}

func (w *World) GameStateManager() gamestate.Manager {
	return w.entityStore
}

// WaitForNextTick blocks until at least one game tick has completed. It returns true if it successfully waited for a
// tick. False may be returned if the engine was shut down while waiting for the next tick to complete.
func (w *World) WaitForNextTick() (success bool) {
	startTick := w.CurrentTick()
	ch := make(chan struct{})
	w.addChannelWaitingForNextTick <- ch
	<-ch
	return w.CurrentTick() > startTick
}

func (w *World) Search(filter filter.ComponentFilter) EntitySearch {
	return NewLegacySearch(filter)
}

func (w *World) StoreReader() gamestate.Reader {
	return w.entityStore.ToReadOnly()
}

func (w *World) GetRegisteredComponents() []types.ComponentMetadata {
	return w.GetComponents()
}

func (w *World) GetReadOnlyCtx() WorldContext {
	return NewReadOnlyWorldContext(w)
}

func (w *World) GetMessageByID(id types.MessageID) (types.Message, bool) {
	msg := w.MessageManager.GetMessageByID(id)
	return msg, msg != nil
}

func (w *World) broadcastTickResults(ctx context.Context) {
	_, span := w.tracer.Start(ctx, "world.tick.broadcast_tick_results")
	defer span.End()

	// TODO(scott): this "- 1" is hacky because the receipt history manager doesn't allow you to get receipts for the
	//  current tick. We should fix this.
	receipts, err := w.receiptHistory.GetReceiptsForTick(w.CurrentTick() - 1)
	if err != nil {
		log.Error().Err(err).Msgf("failed to get receipts for tick %d", w.CurrentTick()-1)
	}
	w.tickResults.SetReceipts(receipts)
	w.tickResults.SetTick(w.CurrentTick() - 1)

	// Broadcast the tick results to all clients
	if err := w.server.BroadcastEvent(w.tickResults); err != nil {
		span.SetStatus(codes.Error, eris.ToString(err, true))
		span.RecordError(err)
		log.Err(err).Msgf("failed to broadcast tick results")
	}

	// Clear the TickResults for this tick in preparation for the next tick
	w.tickResults.Clear()
}

func (w *World) ReceiptHistorySize() uint64 {
	return w.receiptHistory.Size()
}

func (w *World) EvaluateCQL(cqlString string) ([]types.EntityStateElement, error) {
	// getComponentByName is a wrapper function that casts component.ComponentMetadata from ctx.getComponentByName
	// to types.Component
	getComponentByName := func(name string) (types.Component, error) {
		comp, err := w.GetComponentByName(name)
		if err != nil {
			return nil, err
		}
		return comp, nil
	}

	// Parse the CQL string into a filter
	cqlFilter, err := cql.Parse(cqlString, getComponentByName)
	if err != nil {
		return nil, eris.Errorf("failed to parse cql string: %s", cqlString)
	}
	result := make([]types.EntityStateElement, 0)
	var eachError error
	wCtx := NewReadOnlyWorldContext(w)
	searchErr := w.Search(cqlFilter).Each(wCtx,
		func(id types.EntityID) bool {
			components, err := w.StoreReader().GetComponentTypesForEntity(id)
			if err != nil {
				eachError = err
				return false
			}
			resultElement := types.EntityStateElement{
				ID:   id,
				Data: make([]json.RawMessage, 0),
			}

			for _, c := range components {
				data, err := w.StoreReader().GetComponentForEntityInRawJSON(c, id)
				if err != nil {
					eachError = err
					return false
				}
				resultElement.Data = append(resultElement.Data, data)
			}
			result = append(result, resultElement)
			return true
		},
	)
	if eachError != nil {
		return nil, eachError
	} else if searchErr != nil {
		return nil, searchErr
	}
	return result, nil
}
