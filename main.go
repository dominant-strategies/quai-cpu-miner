package main

import (
	"context"
	"fmt"
	"log"
	"math/big"

	"math/rand"
	"os"
	"strconv"
	"time"

	"github.com/INFURA/go-ethlibs/jsonrpc"
	"github.com/TwiN/go-color"
	"github.com/dominant-strategies/go-quai/consensus"
	"github.com/dominant-strategies/go-quai/params"
	"github.com/dominant-strategies/go-quai/quaiclient"
	"github.com/sirupsen/logrus"
	"google.golang.org/protobuf/proto"

	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/consensus/blake3pow"
	"github.com/dominant-strategies/go-quai/consensus/progpow"
	"github.com/dominant-strategies/go-quai/core/types"

	"github.com/dominant-strategies/quai-cpu-miner/util"
	lru "github.com/hnlq715/golang-lru"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize       = 10
	maxRetryDelay         = 60 * 60 * 4 // 4 hours
	USER_AGENT_VER        = "0.1"
	miningWorkRefreshRate = 2 * time.Second
)

var (
	exit     = make(chan bool)
	big2e256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0)) // 2^256

)

type Miner struct {
	// Miner config object
	config util.Config

	// Progpow consensus engine used to seal a block
	engine consensus.Engine

	// Current header to mine
	header *types.WorkObject

	// RPC client connection to mining proxy
	proxyClient *util.MinerSession

	// RPC client connections to the Quai nodes
	sliceClients SliceClients

	// woCh receives the pending header from the subscription
	woCh chan []byte

	// Channel to receive header updates
	updateCh chan *types.WorkObject

	// Channel to submit completed work
	resultCh chan *types.WorkObject

	// Track previous block number for pretty printing
	previousNumber [common.HierarchyDepth]uint64

	miningWorkRefresh *time.Ticker

	// Tracks the latest JSON RPC ID to send to the proxy or node.
	latestId uint64

	stopCh chan struct{}

	miningCache *lru.Cache
}

// Clients for RPC connection to the Prime, region, & zone ports belonging to the
// slice we are actively mining
type SliceClients [common.HierarchyDepth]*quaiclient.Client

// Creates a MinerSession object that is connected to the single proxy node.
func connectToProxy(config util.Config) *util.MinerSession {
	proxyConnected := false
	var client *util.MinerSession
	var err error
	for !proxyConnected {
		if config.ProxyURL != "" && !proxyConnected {
			client, err = util.NewMinerConn(config.ProxyURL)
			if err != nil {
				log.Println("Unable to connect to proxy: ", config.ProxyURL)
			} else {
				proxyConnected = true
			}
		}
	}
	return client
}

// connectToSlice takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func connectToSlice(config util.Config, logger *logrus.Logger) SliceClients {
	var err error
	loc := config.Location
	clients := SliceClients{}
	primeConnected := false
	regionConnected := false
	zoneConnected := false
	for !primeConnected || !regionConnected || !zoneConnected {
		if config.PrimeURL != "" && !primeConnected {
			clients[common.PRIME_CTX], err = quaiclient.Dial(config.PrimeURL, logger)
			if err != nil {
				log.Println("Unable to connect to node:", "Prime", config.PrimeURL)
			} else {
				primeConnected = true
			}
		}
		if config.RegionURLs[loc.Region()] != "" && !regionConnected {
			clients[common.REGION_CTX], err = quaiclient.Dial(config.RegionURLs[loc.Region()], logger)
			if err != nil {
				log.Println("Unable to connect to node:", "Region", config.RegionURLs[loc.Region()])
			} else {
				regionConnected = true
			}
		}
		if config.ZoneURLs[loc.Region()][loc.Zone()] != "" && !zoneConnected {
			clients[common.ZONE_CTX], err = quaiclient.Dial(config.ZoneURLs[loc.Region()][loc.Zone()], logger)
			if err != nil {
				log.Println("Unable to connect to node:", "Zone", config.ZoneURLs[loc.Region()][loc.Zone()])
			} else {
				zoneConnected = true
			}
		}
	}
	return clients
}

func init() {
	log.SetFlags(log.LstdFlags | log.Lshortfile)
}

func main() {
	// Load config
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Print("Could not load config: ", err)
		return
	}
	// Parse mining location from args
	if len(os.Args) > 2 {
		raw := os.Args[1:3]
		region, _ := strconv.Atoi(raw[0])
		zone, _ := strconv.Atoi(raw[1])
		config.Location = common.Location{byte(region), byte(zone)}
	}
	var engine consensus.Engine
	logger := logrus.New()

	if config.RunBlake3 {
		engine = blake3pow.New(blake3pow.Config{NotifyFull: true, NodeLocation: common.Location{0, 0}}, nil, false, logger)
	} else {
		engine = progpow.New(progpow.Config{NotifyFull: true, NodeLocation: common.Location{0, 0}}, nil, false, logger)
	}

	miningCache, _ := lru.New(3)

	m := &Miner{
		config:            config,
		engine:            engine,
		woCh:              make(chan []byte, resultQueueSize),
		updateCh:          make(chan *types.WorkObject, resultQueueSize),
		resultCh:          make(chan *types.WorkObject, resultQueueSize),
		previousNumber:    [common.HierarchyDepth]uint64{0, 0, 0},
		miningWorkRefresh: time.NewTicker(miningWorkRefreshRate),
		miningCache:       miningCache,
	}
	log.Println("Starting Quai cpu miner in location ", config.Location)
	if config.Proxy {
		m.proxyClient = connectToProxy(config)
		go m.fetchPendingHeaderProxy()
		go m.subscribeProxy()
	} else {
		m.sliceClients = connectToSlice(config, logger)
		go m.fetchPendingHeaderNode()
		// No separate call needed to start listeners.
		go m.subscribeNode()
		go m.listenNewPendingHeader()
	}
	go m.resultLoop()
	go m.miningLoop()
	go m.refreshMiningWork()
	defer m.miningWorkRefresh.Stop()
	<-exit
}

// subscribeProxy subscribes to the head of the mining nodes in order to pass
// the most up to date block to the miner within the manager.
func (m *Miner) subscribeProxy() error {
	address := m.config.RewardAddress
	password := m.config.Password

	msg, err := jsonrpc.MakeRequest(int(m.incrementLatestID()), "quai_submitLogin", address, password)
	if err != nil {
		log.Fatalf("Unable to create login request: %v", err)
	}

	return m.proxyClient.SendTCPRequest(*msg)
}

// Subscribes to the zone node in order to get pending header updates.
func (m *Miner) subscribeNode() {
	if _, err := m.sliceClients[common.ZONE_CTX].SubscribePendingHeader(context.Background(), m.woCh); err != nil {
		log.Fatal("Failed to subscribe to pending header events", err)
	}
}

// Gets the latest pending header from the proxy.
// This only runs upon initialization, further proxy pending headers are received in listenTCP.
func (m *Miner) fetchPendingHeaderProxy() {
	retryDelay := 1 // Start retry at 1 second
	for {
		msg, err := jsonrpc.MakeRequest(int(m.incrementLatestID()), "quai_getPendingHeader", nil)
		if err != nil {
			log.Fatalf("Unable to make pending header request: %v", err)
		}
		err = m.proxyClient.SendTCPRequest(*msg)
		header := <-m.updateCh

		if err != nil {
			log.Println("Pending block not found error: ", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			m.updateCh <- header
			break
		}
	}
}

// Gets the latest pending header from the zone client.
func (m *Miner) fetchPendingHeaderNode() {
	retryDelay := 1 // Start retry at 1 second
	for {
		header, err := m.sliceClients[common.ZONE_CTX].GetPendingHeader(context.Background())
		if err != nil {
			log.Println("Pending block not found error: ", err)
			time.Sleep(time.Duration(retryDelay) * time.Second)
			retryDelay *= 2
			if retryDelay > maxRetryDelay {
				retryDelay = maxRetryDelay
			}
		} else {
			m.updateCh <- header
			break
		}
	}
}

func (m *Miner) listenNewPendingHeader() {
	for {
		select {
		case newPh := <-m.woCh:
			if len(newPh) > 0 {
				protoWo := &types.ProtoWorkObject{}
				err := proto.Unmarshal(newPh, protoWo)
				if err != nil {
					log.Println("Error unmarshalling new pending header", "err", err)
					continue
				}
				wo := &types.WorkObject{}
				err = wo.ProtoDecode(protoWo, m.config.Location, types.PEtxObject)
				if err != nil {
					log.Println("Error decoding new pending header", "err", err)
					continue
				}
				m.updateCh <- wo
			}
		default:
		}
	}
}

func (m *Miner) interrupt() {
	if m.stopCh != nil {
		close(m.stopCh)
		m.stopCh = nil
	}
}

// miningLoop iterates on a new header and passes the result to m.resultCh. The result is called within the method.
func (m *Miner) miningLoop() error {
	for {
		select {
		case newHead := <-m.updateCh:

			if m.header != nil && newHead.SealHash() == m.header.SealHash() {
				continue
			}

			m.miningCache.ContainsOrAdd(newHead.SealHash(), newHead)

			keys := m.miningCache.Keys()
			if len(keys) > 0 {
				value, exists := m.miningCache.Get(keys[rand.Intn(len(keys))])
				if exists {
					m.header = value.(*types.WorkObject)
				}
			}

			m.miningWorkRefresh.Reset(miningWorkRefreshRate)
			// Mine the header here
			// Return the valid header with proper nonce and mix digest
			// Interrupt previous sealing operation
			m.interrupt()
			m.stopCh = make(chan struct{})
			number := [common.HierarchyDepth]uint64{m.header.NumberU64(common.PRIME_CTX), m.header.NumberU64(common.REGION_CTX), m.header.NumberU64(common.ZONE_CTX)}
			primeStr := fmt.Sprint(number[common.PRIME_CTX])
			regionStr := fmt.Sprint(number[common.REGION_CTX])
			zoneStr := fmt.Sprint(number[common.ZONE_CTX])
			if number != m.previousNumber {
				if number[common.PRIME_CTX] != m.previousNumber[common.PRIME_CTX] {
					primeStr = color.Ize(color.Red, primeStr)
					regionStr = color.Ize(color.Red, regionStr)
					zoneStr = color.Ize(color.Red, zoneStr)
				} else if number[common.REGION_CTX] != m.previousNumber[common.REGION_CTX] {
					regionStr = color.Ize(color.Yellow, regionStr)
					zoneStr = color.Ize(color.Yellow, zoneStr)
				} else if number[common.ZONE_CTX] != m.previousNumber[common.ZONE_CTX] {
					zoneStr = color.Ize(color.Blue, zoneStr)
				}
				log.Println("Mining Block: ", fmt.Sprintf("[%s %s %s]", primeStr, regionStr, zoneStr), "location", m.header.Location(), "difficulty", m.header.Difficulty())
			}
			m.previousNumber = [common.HierarchyDepth]uint64{m.header.NumberU64(common.PRIME_CTX), m.header.NumberU64(common.REGION_CTX), m.header.NumberU64(common.ZONE_CTX)}
			if err := m.engine.Seal(m.header, m.resultCh, m.stopCh); err != nil {
				log.Println("Block sealing failed", "err", err)
			}
		default:
		}
	}
}

// refreshMiningWork is a simple method to refresh the mining the work
func (m *Miner) refreshMiningWork() {
	for {
		select {
		case <-m.miningWorkRefresh.C:
			m.fetchPendingHeaderNode()
		}
	}
}

// resultLoop takes in the result and passes to the proper channels for receiving.
func (m *Miner) resultLoop() {
	for {
		select {
		case header := <-m.resultCh:
			// check if the mined object is a workshare or a block
			workShareTarget, err := consensus.CalcWorkShareThreshold(header.WorkObjectHeader(), params.WorkSharesThresholdDiff)
			if err != nil {
				log.Println("Err computing the work share target: ", err)
				continue
			}
			powHash, err := m.engine.ComputePowHash(header.WorkObjectHeader())
			if err != nil {
				log.Println("Error computing pow hash: ", err)
				continue
			}
			powHashBigInt := new(big.Int).SetBytes(powHash.Bytes())

			// Check if satisfies workShareTarget
			if powHashBigInt.Cmp(workShareTarget) < 0 {
				// Check if also satisfies block target
				if powHashBigInt.Cmp(consensus.DifficultyToTarget(header.Difficulty())) < 0 {
					order, err := m.sliceClients[common.ZONE_CTX].CalcOrder(context.Background(), header)
					if err != nil {
						log.Println("Error calculating order: ", err)
						continue
					}
					switch order {
					case common.PRIME_CTX:
						log.Println(color.Ize(color.Red, "PRIME block : "), header.NumberArray(), header.Hash())
					case common.REGION_CTX:
						log.Println(color.Ize(color.Yellow, "REGION block: "), header.NumberArray(), header.Hash())
					case common.ZONE_CTX:
						log.Println(color.Ize(color.Blue, "ZONE block  : "), header.NumberArray(), header.Hash())
					}
					if !m.config.Proxy {
						for i := common.HierarchyDepth - 1; i >= order; i-- {
							err := m.sendMinedHeaderNodes(i, header)
							if err != nil {
								// Go back to waiting on the next block.
								fmt.Errorf("error submitting block to context %d: %v", order, err)
								continue
							}
						}
					}
				} else {
					log.Println("Mined a work share", header.Hash())
					// Restart the Mining work
					m.stopCh = make(chan struct{})
					if err := m.engine.Seal(m.header, m.resultCh, m.stopCh); err != nil {
						log.Println("Block sealing failed", "err", err)
					}
					m.sliceClients[common.ZONE_CTX].ReceiveWorkShare(context.Background(), header.WorkObjectHeader())
					continue
				}
			}
		}
	}
}

// Sends the mined header to its mining client.
func (m *Miner) sendMinedHeaderNodes(order int, header *types.WorkObject) error {
	return m.sliceClients[order].ReceiveMinedHeader(context.Background(), header)
}

// Used for sequencing JSON RPC messages.
func (m *Miner) incrementLatestID() uint64 {
	cur := m.latestId
	m.latestId += 1
	return cur
}
