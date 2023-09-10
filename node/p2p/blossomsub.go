package p2p

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"math/big"
	"sync"

	"github.com/libp2p/go-libp2p"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	libp2pconfig "github.com/libp2p/go-libp2p/config"
	"github.com/libp2p/go-libp2p/core/crypto"
	"github.com/libp2p/go-libp2p/core/host"
	"github.com/libp2p/go-libp2p/core/peer"
	"github.com/libp2p/go-libp2p/p2p/discovery/routing"
	"github.com/libp2p/go-libp2p/p2p/discovery/util"
	"github.com/pkg/errors"
	"go.uber.org/zap"
	blossomsub "source.quilibrium.com/quilibrium/monorepo/go-libp2p-blossomsub"
	"source.quilibrium.com/quilibrium/monorepo/go-libp2p-blossomsub/pb"
	"source.quilibrium.com/quilibrium/monorepo/node/config"
)

type BlossomSub struct {
	ps         *blossomsub.PubSub
	ctx        context.Context
	logger     *zap.Logger
	peerID     peer.ID
	bitmaskMap map[string]*blossomsub.Bitmask
	h          host.Host
}

var _ PubSub = (*BlossomSub)(nil)
var ErrNoPeersAvailable = errors.New("no peers available")

// Crucial note, bitmask lengths should always be a power of two so as to reduce
// index bias with hash functions
var BITMASK_ALL = []byte{
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
	0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff,
}

func NewBlossomSub(
	p2pConfig *config.P2PConfig,
	logger *zap.Logger,
) *BlossomSub {
	ctx := context.Background()

	opts := []libp2pconfig.Option{
		libp2p.ListenAddrStrings(p2pConfig.ListenMultiaddr),
	}

	if p2pConfig.PeerPrivKey != "" {
		peerPrivKey, err := hex.DecodeString(p2pConfig.PeerPrivKey)
		if err != nil {
			panic(errors.Wrap(err, "error unmarshaling peerkey"))
		}

		privKey, err := crypto.UnmarshalEd448PrivateKey(peerPrivKey)
		if err != nil {
			panic(errors.Wrap(err, "error unmarshaling peerkey"))
		}

		opts = append(opts, libp2p.Identity(privKey))
	}

	h, err := libp2p.New(opts...)
	if err != nil {
		panic(errors.Wrap(err, "error constructing p2p"))
	}

	logger.Info("established peer id", zap.String("peer_id", h.ID().String()))

	go discoverPeers(p2pConfig, ctx, logger, h)

	var tracer *blossomsub.JSONTracer
	if p2pConfig.TraceLogFile == "" {
		tracer, err = blossomsub.NewStdoutJSONTracer()
		if err != nil {
			panic(errors.Wrap(err, "error building stdout tracer"))
		}
	} else {
		tracer, err = blossomsub.NewJSONTracer(p2pConfig.TraceLogFile)
		if err != nil {
			panic(errors.Wrap(err, "error building file tracer"))
		}
	}

	blossomOpts := []blossomsub.Option{
		blossomsub.WithEventTracer(tracer),
	}

	params := mergeDefaults(p2pConfig)
	rt := blossomsub.NewBlossomSubRouter(h, params)
	ps, err := blossomsub.NewBlossomSubWithRouter(ctx, h, rt, blossomOpts...)
	if err != nil {
		panic(err)
	}

	peerID := h.ID()

	return &BlossomSub{
		ps,
		ctx,
		logger,
		peerID,
		make(map[string]*blossomsub.Bitmask),
		h,
	}
}

func (b *BlossomSub) PublishToBitmask(bitmask []byte, data []byte) error {
	bm, ok := b.bitmaskMap[string(bitmask)]
	if !ok {
		b.logger.Error(
			"error while publishing to bitmask",
			zap.Error(errors.New("not subscribed to bitmask")),
			zap.Binary("bitmask", bitmask),
		)
		return errors.New("not subscribed to bitmask")
	}

	return bm.Publish(b.ctx, data)
}

func (b *BlossomSub) Publish(data []byte) error {
	bitmask := getBloomFilterIndices(data, 256, 3)
	return b.PublishToBitmask(bitmask, data)
}

func (b *BlossomSub) Subscribe(
	bitmask []byte,
	handler func(message *pb.Message) error,
	raw bool,
) {
	eval := func(bitmask []byte) error {
		_, ok := b.bitmaskMap[string(bitmask)]
		if ok {
			return nil
		}

		b.logger.Info("joining broadcast")
		bm, err := b.ps.Join(bitmask)
		if err != nil {
			b.logger.Error("join failed", zap.Error(err))
			return errors.Wrap(err, "subscribe")
		}

		b.bitmaskMap[string(bitmask)] = bm

		b.logger.Info("subscribe to bitmask", zap.Binary("bitmask", bitmask))
		sub, err := bm.Subscribe()
		if err != nil {
			b.logger.Error("subscription failed", zap.Error(err))
			return errors.Wrap(err, "subscribe")
		}

		b.logger.Info(
			"begin streaming from bitmask",
			zap.Binary("bitmask", bitmask),
		)
		go func() {
			for {
				m, err := sub.Next(b.ctx)
				if err != nil {
					b.logger.Error(
						"got error when fetching the next message",
						zap.Error(err),
					)
				}

				if err = handler(m.Message); err != nil {
					b.logger.Error("message handler returned error", zap.Error(err))
				}
			}
		}()

		return nil
	}

	if raw {
		if err := eval(bitmask); err != nil {
			b.logger.Error("subscribe returned error", zap.Error(err))
		}
	} else {
		if err := generateBitSlices(bitmask, eval); err != nil {
			b.logger.Error("bloom subscribe returned error", zap.Error(err))
		}
	}
}

func (b *BlossomSub) Unsubscribe(bitmask []byte, raw bool) {
	bm, ok := b.bitmaskMap[string(bitmask)]
	if !ok {
		return
	}

	bm.Close()
}

func (b *BlossomSub) GetPeerID() []byte {
	return []byte(b.peerID)
}

func (b *BlossomSub) GetRandomPeer(bitmask []byte) ([]byte, error) {
	peers := b.ps.ListPeers(bitmask)
	if len(peers) == 0 {
		return nil, errors.Wrap(
			ErrNoPeersAvailable,
			"get random peer",
		)
	}
	b.logger.Info("selecting from peers", zap.Any("peer_ids", peers))
	sel, err := rand.Int(rand.Reader, big.NewInt(int64(len(peers))))
	if err != nil {
		return nil, errors.Wrap(err, "get random peer")
	}

	return []byte(peers[sel.Int64()]), nil
}

func initDHT(
	ctx context.Context,
	p2pConfig *config.P2PConfig,
	logger *zap.Logger,
	h host.Host,
) *dht.IpfsDHT {
	logger.Info("establishing dht")
	kademliaDHT, err := dht.New(ctx, h)
	if err != nil {
		panic(err)
	}
	if err = kademliaDHT.Bootstrap(ctx); err != nil {
		panic(err)
	}
	var wg sync.WaitGroup

	logger.Info("connecting to bootstrap", zap.String("peer_id", h.ID().String()))

	defaultBootstrapPeers := p2pConfig.BootstrapPeers

	for _, peerAddr := range defaultBootstrapPeers {
		peerinfo, err := peer.AddrInfoFromString(peerAddr)
		if err != nil {
			panic(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := h.Connect(ctx, *peerinfo); err != nil {
				logger.Warn("error while connecting to dht peer", zap.Error(err))
			}
		}()
	}
	wg.Wait()

	return kademliaDHT
}

func (b *BlossomSub) GetPeerstoreCount() int {
	return len(b.h.Peerstore().Peers())
}

func (b *BlossomSub) GetNetworkPeersCount() int {
	return len(b.h.Network().Peers())
}

func discoverPeers(
	p2pConfig *config.P2PConfig,
	ctx context.Context,
	logger *zap.Logger,
	h host.Host,
) {
	logger.Info("initiating peer discovery")

	kademliaDHT := initDHT(ctx, p2pConfig, logger, h)
	routingDiscovery := routing.NewRoutingDiscovery(kademliaDHT)
	util.Advertise(ctx, routingDiscovery, string(BITMASK_ALL))

	peerCount := 0
	for peerCount < p2pConfig.MinPeers {
		peerChan, err := routingDiscovery.FindPeers(ctx, string(BITMASK_ALL))
		if err != nil {
			panic(err)
		}

		for peer := range peerChan {
			if peer.ID == h.ID() {
				continue
			}

			logger.Info("found peer", zap.String("peer_id", peer.ID.Pretty()))
			err := h.Connect(ctx, peer)
			if err != nil {
				logger.Warn(
					"error while connecting to blossomsub peer",
					zap.String("peer_id", peer.ID.Pretty()),
					zap.Error(err),
				)
			} else {
				logger.Info(
					"connected to peer",
					zap.String("peer_id", peer.ID.Pretty()),
				)
				peerCount++
			}
		}
	}

	logger.Info("completed initial peer discovery")
}

func mergeDefaults(p2pConfig *config.P2PConfig) blossomsub.BlossomSubParams {
	if p2pConfig.D == 0 {
		p2pConfig.D = blossomsub.BlossomSubD
	}
	if p2pConfig.DLo == 0 {
		p2pConfig.DLo = blossomsub.BlossomSubDlo
	}
	if p2pConfig.DHi == 0 {
		p2pConfig.DHi = blossomsub.BlossomSubDhi
	}
	if p2pConfig.DScore == 0 {
		p2pConfig.DScore = blossomsub.BlossomSubDscore
	}
	if p2pConfig.DOut == 0 {
		p2pConfig.DOut = blossomsub.BlossomSubDout
	}
	if p2pConfig.HistoryLength == 0 {
		p2pConfig.HistoryLength = blossomsub.BlossomSubHistoryLength
	}
	if p2pConfig.HistoryGossip == 0 {
		p2pConfig.HistoryGossip = blossomsub.BlossomSubHistoryGossip
	}
	if p2pConfig.DLazy == 0 {
		p2pConfig.DLazy = blossomsub.BlossomSubDlazy
	}
	if p2pConfig.GossipFactor == 0 {
		p2pConfig.GossipFactor = blossomsub.BlossomSubGossipFactor
	}
	if p2pConfig.GossipRetransmission == 0 {
		p2pConfig.GossipRetransmission = blossomsub.BlossomSubGossipRetransmission
	}
	if p2pConfig.HeartbeatInitialDelay == 0 {
		p2pConfig.HeartbeatInitialDelay = blossomsub.BlossomSubHeartbeatInitialDelay
	}
	if p2pConfig.HeartbeatInterval == 0 {
		p2pConfig.HeartbeatInterval = blossomsub.BlossomSubHeartbeatInterval
	}
	if p2pConfig.FanoutTTL == 0 {
		p2pConfig.FanoutTTL = blossomsub.BlossomSubFanoutTTL
	}
	if p2pConfig.PrunePeers == 0 {
		p2pConfig.PrunePeers = blossomsub.BlossomSubPrunePeers
	}
	if p2pConfig.PruneBackoff == 0 {
		p2pConfig.PruneBackoff = blossomsub.BlossomSubPruneBackoff
	}
	if p2pConfig.UnsubscribeBackoff == 0 {
		p2pConfig.UnsubscribeBackoff = blossomsub.BlossomSubUnsubscribeBackoff
	}
	if p2pConfig.Connectors == 0 {
		p2pConfig.Connectors = blossomsub.BlossomSubConnectors
	}
	if p2pConfig.MaxPendingConnections == 0 {
		p2pConfig.MaxPendingConnections = blossomsub.BlossomSubMaxPendingConnections
	}
	if p2pConfig.ConnectionTimeout == 0 {
		p2pConfig.ConnectionTimeout = blossomsub.BlossomSubConnectionTimeout
	}
	if p2pConfig.DirectConnectTicks == 0 {
		p2pConfig.DirectConnectTicks = blossomsub.BlossomSubDirectConnectTicks
	}
	if p2pConfig.DirectConnectInitialDelay == 0 {
		p2pConfig.DirectConnectInitialDelay =
			blossomsub.BlossomSubDirectConnectInitialDelay
	}
	if p2pConfig.OpportunisticGraftTicks == 0 {
		p2pConfig.OpportunisticGraftTicks =
			blossomsub.BlossomSubOpportunisticGraftTicks
	}
	if p2pConfig.OpportunisticGraftPeers == 0 {
		p2pConfig.OpportunisticGraftPeers =
			blossomsub.BlossomSubOpportunisticGraftPeers
	}
	if p2pConfig.GraftFloodThreshold == 0 {
		p2pConfig.GraftFloodThreshold = blossomsub.BlossomSubGraftFloodThreshold
	}
	if p2pConfig.MaxIHaveLength == 0 {
		p2pConfig.MaxIHaveLength = blossomsub.BlossomSubMaxIHaveLength
	}
	if p2pConfig.MaxIHaveMessages == 0 {
		p2pConfig.MaxIHaveMessages = blossomsub.BlossomSubMaxIHaveMessages
	}
	if p2pConfig.IWantFollowupTime == 0 {
		p2pConfig.IWantFollowupTime = blossomsub.BlossomSubIWantFollowupTime
	}

	return blossomsub.BlossomSubParams{
		D:                         p2pConfig.D,
		Dlo:                       p2pConfig.DLo,
		Dhi:                       p2pConfig.DHi,
		Dscore:                    p2pConfig.DScore,
		Dout:                      p2pConfig.DOut,
		HistoryLength:             p2pConfig.HistoryLength,
		HistoryGossip:             p2pConfig.HistoryGossip,
		Dlazy:                     p2pConfig.DLazy,
		GossipFactor:              p2pConfig.GossipFactor,
		GossipRetransmission:      p2pConfig.GossipRetransmission,
		HeartbeatInitialDelay:     p2pConfig.HeartbeatInitialDelay,
		HeartbeatInterval:         p2pConfig.HeartbeatInterval,
		FanoutTTL:                 p2pConfig.FanoutTTL,
		PrunePeers:                p2pConfig.PrunePeers,
		PruneBackoff:              p2pConfig.PruneBackoff,
		UnsubscribeBackoff:        p2pConfig.UnsubscribeBackoff,
		Connectors:                p2pConfig.Connectors,
		MaxPendingConnections:     p2pConfig.MaxPendingConnections,
		ConnectionTimeout:         p2pConfig.ConnectionTimeout,
		DirectConnectTicks:        p2pConfig.DirectConnectTicks,
		DirectConnectInitialDelay: p2pConfig.DirectConnectInitialDelay,
		OpportunisticGraftTicks:   p2pConfig.OpportunisticGraftTicks,
		OpportunisticGraftPeers:   p2pConfig.OpportunisticGraftPeers,
		GraftFloodThreshold:       p2pConfig.GraftFloodThreshold,
		MaxIHaveLength:            p2pConfig.MaxIHaveLength,
		MaxIHaveMessages:          p2pConfig.MaxIHaveMessages,
		IWantFollowupTime:         p2pConfig.IWantFollowupTime,
		SlowHeartbeatWarning:      0.1,
	}
}
