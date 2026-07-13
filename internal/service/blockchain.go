package service

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/HenZenKuriRIP/k2pay/config"
	"github.com/HenZenKuriRIP/k2pay/internal/model"

	"github.com/shopspring/decimal"
)

// 全局 HTTP 客户端（带超时配置）
var httpClient = &http.Client{
	Timeout: 15 * time.Second,
	Transport: &http.Transport{
		MaxIdleConns:        100,
		MaxIdleConnsPerHost: 10,
		IdleConnTimeout:     90 * time.Second,
	},
}

// BlockchainService 区块链监控服务
type BlockchainService struct {
	cfg          *config.Config
	ctx          context.Context
	cancel       context.CancelFunc
	wg           sync.WaitGroup
	listeners    map[string]*ChainListener
	rpcClients   map[string]*RPCClient  // RPC 客户端（支持重试和故障转移）
	metrics      *BlockchainMetrics     // 监控指标
	mu           sync.RWMutex
	walletCache  *WalletCache
	gasPrices    map[string]float64     // Gas 价格缓存
	gasPriceMu   sync.RWMutex
}

// WalletCache 钱包地址缓存
type WalletCache struct {
	mu          sync.RWMutex
	cache       map[string]map[string]bool // chain -> addresses
	lastUpdate  time.Time
	ttl         time.Duration
}

// NewWalletCache 创建钱包缓存
func NewWalletCache(ttl time.Duration) *WalletCache {
	return &WalletCache{
		cache: make(map[string]map[string]bool),
		ttl:   ttl,
	}
}

// GetAddresses 获取指定链的钱包地址（带缓存）
func (c *WalletCache) GetAddresses(chain string) map[string]bool {
	c.mu.RLock()
	// 检查缓存是否过期
	if time.Since(c.lastUpdate) > c.ttl {
		c.mu.RUnlock()
		c.refresh()
		c.mu.RLock()
	}
	addresses := c.cache[chain]
	c.mu.RUnlock()

	if addresses == nil {
		return make(map[string]bool)
	}
	// 返回副本以避免并发问题
	result := make(map[string]bool, len(addresses))
	for k, v := range addresses {
		result[k] = v
	}
	return result
}

// refresh 刷新缓存
func (c *WalletCache) refresh() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// 双重检查
	if time.Since(c.lastUpdate) <= c.ttl {
		return
	}

	// 从数据库加载所有钱包
	var wallets []model.Wallet
	model.GetDB().Where("status = 1").Find(&wallets)

	newCache := make(map[string]map[string]bool)
	for _, w := range wallets {
		if newCache[w.Chain] == nil {
			newCache[w.Chain] = make(map[string]bool)
		}
		newCache[w.Chain][strings.ToLower(w.Address)] = true
	}
	c.cache = newCache
	c.lastUpdate = time.Now()
}

// Invalidate 使缓存失效
func (c *WalletCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastUpdate = time.Time{}
}

// ChainListener 链监听器
type ChainListener struct {
	chain            string
	name             string             // 展示名
	chainType        string             // trx | trc20 | evm | passive
	rpc              string
	rpcBackups       []string           // RPC 备用节点
	contractAddress  string
	confirmations    int
	scanInterval     int
	baseScanInterval int                // 基础扫描间隔
	lastBlock        uint64
	reorgDepth       int                // 重组检测深度
	blockHistory     []uint64           // 区块历史（用于重组检测）
	running          bool
	enabled          bool
	isBuiltin        bool
	stopCh           chan struct{}
	mu               sync.Mutex
	// 可配置的扫描参数（从config.yaml/数据库读取，0表示使用默认值）
	maxBlockRange int     // 单次查询最大区块范围
	maxBatchSize  int     // 批量请求最大数量（1=不使用批量）
	batchDelayMs  int     // 批次间延迟毫秒
	rateLimit     float64 // 每秒最大请求数
}

// Transfer 转账事件
type Transfer struct {
	TxHash      string
	From        string
	To          string
	Amount      decimal.Decimal
	BlockNumber uint64
	Chain       string
}

var blockchainService *BlockchainService
var blockchainOnce sync.Once

// GetBlockchainService 获取区块链服务单例
func GetBlockchainService() *BlockchainService {
	blockchainOnce.Do(func() {
		ctx, cancel := context.WithCancel(context.Background())
		blockchainService = &BlockchainService{
			ctx:         ctx,
			cancel:      cancel,
			listeners:   make(map[string]*ChainListener),
			rpcClients:  make(map[string]*RPCClient),
			metrics:     NewBlockchainMetrics(),
			walletCache: NewWalletCache(30 * time.Second), // 钱包缓存30秒
			gasPrices:   make(map[string]float64),
		}
	})
	return blockchainService
}

// InvalidateWalletCache 使钱包缓存失效（添加/修改/删除钱包后调用）
func (s *BlockchainService) InvalidateWalletCache() {
	if s.walletCache != nil {
		s.walletCache.Invalidate()
	}
}

// SetWalletCacheTTL 设置钱包缓存TTL
func (s *BlockchainService) SetWalletCacheTTL(seconds int) {
	if s.walletCache != nil {
		s.walletCache.mu.Lock()
		s.walletCache.ttl = time.Duration(seconds) * time.Second
		s.walletCache.mu.Unlock()
	}
}

// builtinChainMeta 内置链元信息
var builtinChainMeta = map[string]struct {
	Name      string
	ChainType string
	SortOrder int
}{
	"trx":       {"TRX (Tron)", "trx", 10},
	"trc20":     {"TRC20 (Tron USDT)", "trc20", 20},
	"erc20":     {"ERC20 (Ethereum)", "evm", 30},
	"bep20":     {"BEP20 (BSC)", "evm", 40},
	"polygon":   {"Polygon", "evm", 50},
	"optimism":  {"Optimism", "evm", 60},
	"arbitrum":  {"Arbitrum", "evm", 70},
	"avalanche": {"Avalanche", "evm", 80},
	"base":      {"Base", "evm", 90},
}

// Init 初始化区块链服务
func (s *BlockchainService) Init(cfg *config.Config) {
	s.cfg = cfg
	s.mu.Lock()
	defer s.mu.Unlock()

	// 将 config.yaml 中的内置链配置同步到数据库（仅首次/缺失时写入）
	s.seedBuiltinChainConfigs(cfg)

	// 从数据库加载全部链配置（含自定义链）
	var dbChains []model.ChainConfig
	if err := model.GetDB().Order("sort_order ASC, id ASC").Find(&dbChains).Error; err != nil {
		log.Printf("Failed to load chain configs from DB: %v, fallback to yaml", err)
		s.initFromYAML(cfg)
	} else if len(dbChains) == 0 {
		s.initFromYAML(cfg)
	} else {
		for _, cc := range dbChains {
			s.registerListenerFromDB(cc)
		}
	}

	// 启动 Gas 价格监控
	go s.monitorGasPrices()
}

// seedBuiltinChainConfigs 将 yaml 内置链写入 DB（已存在则不覆盖管理员修改）
func (s *BlockchainService) seedBuiltinChainConfigs(cfg *config.Config) {
	yamlMap := map[string]config.ChainConfig{
		"trx":       cfg.Blockchain.TRX,
		"trc20":     cfg.Blockchain.TRC20,
		"erc20":     cfg.Blockchain.ERC20,
		"bep20":     cfg.Blockchain.BEP20,
		"polygon":   cfg.Blockchain.Polygon,
		"optimism":  cfg.Blockchain.Optimism,
		"arbitrum":  cfg.Blockchain.Arbitrum,
		"avalanche": cfg.Blockchain.Avalanche,
		"base":      cfg.Blockchain.Base,
	}

	for chain, ycfg := range yamlMap {
		meta := builtinChainMeta[chain]
		var existing model.ChainConfig
		if err := model.GetDB().Where("chain = ?", chain).First(&existing).Error; err == nil {
			continue // 已存在，保留管理员修改
		}
		row := model.ChainConfig{
			Chain:           chain,
			Name:            meta.Name,
			ChainType:       meta.ChainType,
			Enabled:         ycfg.Enabled,
			RPC:             ycfg.RPC,
			ContractAddress: ycfg.ContractAddress,
			Confirmations:   ycfg.Confirmations,
			ScanInterval:    ycfg.ScanInterval,
			MaxBlockRange:   ycfg.MaxBlockRange,
			MaxBatchSize:    ycfg.MaxBatchSize,
			BatchDelayMs:    ycfg.BatchDelay,
			RateLimit:       ycfg.RateLimit,
			IsBuiltin:       true,
			SortOrder:       meta.SortOrder,
		}
		if row.ScanInterval <= 0 {
			row.ScanInterval = 30
		}
		if row.Confirmations <= 0 {
			row.Confirmations = 12
		}
		row.SetRPCBackupList(ycfg.RPCBackups)
		if err := model.GetDB().Create(&row).Error; err != nil {
			log.Printf("Failed to seed chain config %s: %v", chain, err)
		}
	}
}

// initFromYAML 仅从 yaml 初始化（DB 不可用时的回退）
func (s *BlockchainService) initFromYAML(cfg *config.Config) {
	chainConfigs := map[string]config.ChainConfig{
		"trx":       cfg.Blockchain.TRX,
		"trc20":     cfg.Blockchain.TRC20,
		"erc20":     cfg.Blockchain.ERC20,
		"bep20":     cfg.Blockchain.BEP20,
		"polygon":   cfg.Blockchain.Polygon,
		"optimism":  cfg.Blockchain.Optimism,
		"arbitrum":  cfg.Blockchain.Arbitrum,
		"avalanche": cfg.Blockchain.Avalanche,
		"base":      cfg.Blockchain.Base,
	}
	for chain, chainCfg := range chainConfigs {
		meta := builtinChainMeta[chain]
		s.registerListener(chain, meta.Name, meta.ChainType, chainCfg, true)
	}
}

func (s *BlockchainService) registerListenerFromDB(cc model.ChainConfig) {
	ycfg := config.ChainConfig{
		Enabled:         cc.Enabled,
		RPC:             cc.RPC,
		RPCBackups:      cc.GetRPCBackupList(),
		ContractAddress: cc.ContractAddress,
		Confirmations:   cc.Confirmations,
		ScanInterval:    cc.ScanInterval,
		MaxBlockRange:   cc.MaxBlockRange,
		MaxBatchSize:    cc.MaxBatchSize,
		BatchDelay:      cc.BatchDelayMs,
		RateLimit:       cc.RateLimit,
	}
	s.registerListener(cc.Chain, cc.Name, cc.ChainType, ycfg, cc.IsBuiltin)
}

func (s *BlockchainService) registerListener(chain, name, chainType string, chainCfg config.ChainConfig, isBuiltin bool) {
	rpcEndpoints := []string{chainCfg.RPC}
	if len(chainCfg.RPCBackups) > 0 {
		rpcEndpoints = append(rpcEndpoints, chainCfg.RPCBackups...)
	}
	rpcClient := NewRPCClient(rpcEndpoints)
	if chainCfg.RateLimit > 0 {
		rpcClient.SetCustomRateLimit(chainCfg.RateLimit)
	}
	s.rpcClients[chain] = rpcClient

	scanInterval := chainCfg.ScanInterval
	if scanInterval <= 0 {
		scanInterval = 30
	}
	confirmations := chainCfg.Confirmations
	if confirmations <= 0 {
		confirmations = 12
	}
	if chainType == "" {
		if meta, ok := builtinChainMeta[chain]; ok {
			chainType = meta.ChainType
		} else {
			chainType = "evm"
		}
	}
	if name == "" {
		name = strings.ToUpper(chain)
	}

	s.listeners[chain] = &ChainListener{
		chain:            chain,
		name:             name,
		chainType:        chainType,
		rpc:              chainCfg.RPC,
		rpcBackups:       chainCfg.RPCBackups,
		contractAddress:  chainCfg.ContractAddress,
		confirmations:    confirmations,
		scanInterval:     scanInterval,
		baseScanInterval: scanInterval,
		reorgDepth:       10,
		blockHistory:     make([]uint64, 0, 20),
		enabled:          chainCfg.Enabled,
		isBuiltin:        isBuiltin,
		stopCh:           make(chan struct{}),
		maxBlockRange:    chainCfg.MaxBlockRange,
		maxBatchSize:     chainCfg.MaxBatchSize,
		batchDelayMs:     chainCfg.BatchDelay,
		rateLimit:        chainCfg.RateLimit,
	}
}

// Start 启动所有已启用的监听器
func (s *BlockchainService) Start() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for chain, listener := range s.listeners {
		if listener.enabled {
			s.wg.Add(1)
			go s.runListener(chain, listener)
		}
	}
	log.Println("Blockchain service started")
}

// Stop 停止所有监听器
func (s *BlockchainService) Stop() {
	s.cancel()
	// 关闭所有监听器的stopCh
	s.mu.RLock()
	for _, listener := range s.listeners {
		listener.mu.Lock()
		if listener.running {
			close(listener.stopCh)
		}
		listener.mu.Unlock()
	}
	s.mu.RUnlock()
	s.wg.Wait()
	log.Println("Blockchain service stopped")
}

// runListener 运行链监听器
func (s *BlockchainService) runListener(chain string, listener *ChainListener) {
	defer s.wg.Done()

	listener.mu.Lock()
	listener.running = true
	listener.mu.Unlock()

	// 从数据库加载上次扫描进度
	var progress model.BlockScanProgress
	if err := model.GetDB().Where("chain = ?", chain).First(&progress).Error; err == nil {
		listener.lastBlock = progress.LastBlock
		log.Printf("Loaded %s scan progress from DB: lastBlock=%d", chain, listener.lastBlock)
	}

	log.Printf("Starting %s listener", chain)

	// 使用动态扫描间隔
	currentInterval := listener.scanInterval
	ticker := time.NewTicker(time.Duration(currentInterval) * time.Second)
	defer ticker.Stop()

	// 智能调频：根据待支付订单数量动态调整扫描间隔
	adjustInterval := func() {
		// 查询该链上是否有待支付订单
		var count int64
		err := model.GetDB().Model(&model.Order{}).
			Where("status = ? AND chain = ?", model.OrderStatusPending, chain).
			Count(&count).Error

		if err == nil {
			var newInterval int
			if count > 0 {
				// 有待支付订单，使用快速扫描（基础间隔）
				newInterval = listener.scanInterval
			} else {
				// 无待支付订单，降低扫描频率（4倍基础间隔，最大60秒）
				newInterval = listener.scanInterval * 4
				if newInterval > 60 {
					newInterval = 60
				}
			}

			// 如果间隔有变化，重置定时器
			if newInterval != currentInterval {
				currentInterval = newInterval
				ticker.Reset(time.Duration(currentInterval) * time.Second)
				log.Printf("%s listener adjusted scan interval to %d seconds (pending orders: %d)", chain, currentInterval, count)
			}
		}
	}

	scanCount := 0

	for {
		select {
		case <-s.ctx.Done():
			listener.mu.Lock()
			listener.running = false
			listener.mu.Unlock()
			log.Printf("Stopped %s listener (context cancelled)", chain)
			return
		case <-listener.stopCh:
			listener.mu.Lock()
			listener.running = false
			listener.mu.Unlock()
			log.Printf("Stopped %s listener", chain)
			return
		case <-ticker.C:
			s.scanChain(listener)
			scanCount++

			// 每5次扫描后调整一次扫描间隔
			if scanCount%5 == 0 {
				adjustInterval()
			}
		}
	}
}

// scanChain 扫描链上交易
func (s *BlockchainService) scanChain(listener *ChainListener) {
	startTime := s.metrics.RecordScanStart(listener.chain)

	// 从缓存获取收款地址
	addresses := s.walletCache.GetAddresses(listener.chain)
	if len(addresses) == 0 {
		return
	}

	var transfers []Transfer
	var err error

	switch {
	case listener.chain == "trx" || listener.chainType == "trx":
		transfers, err = s.scanTRXImproved(listener, addresses)
	case listener.chain == "trc20" || listener.chainType == "trc20":
		transfers, err = s.scanTRC20Improved(listener, addresses)
	case listener.chainType == "evm" ||
		listener.chain == "erc20" || listener.chain == "bep20" ||
		listener.chain == "polygon" || listener.chain == "optimism" ||
		listener.chain == "arbitrum" || listener.chain == "avalanche" ||
		listener.chain == "base":
		transfers, err = s.scanEVMImproved(listener, addresses)
	default:
		// 未知类型默认按 EVM 处理（兼容动态添加的 EVM 兼容链）
		if listener.contractAddress != "" {
			transfers, err = s.scanEVMImproved(listener, addresses)
		} else {
			err = fmt.Errorf("unsupported chain type: %s (chain=%s)", listener.chainType, listener.chain)
		}
	}

	if err != nil {
		log.Printf("[%s] Scan error: %v", listener.chain, err)
		s.metrics.RecordScanFailure(listener.chain, err)

		// 检查是否需要告警
		if shouldAlert, msg := s.metrics.ShouldAlert(listener.chain); shouldAlert {
			log.Printf("⚠️  ALERT: %s", msg)
			// TODO: 发送告警通知（Telegram/邮件等）
		}
		return
	}

	// 记录成功
	s.metrics.RecordScanSuccess(listener.chain, startTime)

	// 持久化扫描进度到数据库
	if listener.lastBlock > 0 {
		model.GetDB().Where("chain = ?", listener.chain).
			Assign(model.BlockScanProgress{Chain: listener.chain, LastBlock: listener.lastBlock}).
			FirstOrCreate(&model.BlockScanProgress{})
	}

	// 记录发现的转账
	if len(transfers) > 0 {
		log.Printf("[%s] Found %d transfers", listener.chain, len(transfers))
		s.metrics.RecordTransfer(listener.chain, len(transfers))
	}

	// 处理转账
	for _, transfer := range transfers {
		s.processTransfer(transfer)
	}

	// 动态调整扫描间隔
	s.adjustScanInterval(listener, len(transfers))
}

// scanTRX 扫描TRX原生代币交易
func (s *BlockchainService) scanTRX(listener *ChainListener, addresses map[string]bool) ([]Transfer, error) {
	var transfers []Transfer

	for addr := range addresses {
		// 使用TronGrid API获取TRX转账记录
		url := fmt.Sprintf("%s/v1/accounts/%s/transactions?only_confirmed=true&limit=50",
			listener.rpc, addr)

		resp, err := httpClient.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			continue
		}

		var result struct {
			Data []struct {
				TxID        string `json:"txID"`
				BlockNumber int64  `json:"blockNumber"`
				RawData     struct {
					Contract []struct {
						Type      string `json:"type"`
						Parameter struct {
							Value struct {
								Amount       int64  `json:"amount"`
								OwnerAddress string `json:"owner_address"`
								ToAddress    string `json:"to_address"`
							} `json:"value"`
						} `json:"parameter"`
					} `json:"contract"`
				} `json:"raw_data"`
				Ret []struct {
					ContractRet string `json:"contractRet"`
				} `json:"ret"`
			} `json:"data"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			continue
		}

		for _, tx := range result.Data {
			// 检查交易是否成功
			if len(tx.Ret) == 0 || tx.Ret[0].ContractRet != "SUCCESS" {
				continue
			}

			// 检查是否是TRX转账
			if len(tx.RawData.Contract) == 0 {
				continue
			}

			contract := tx.RawData.Contract[0]
			if contract.Type != "TransferContract" {
				continue
			}

			// 转换地址格式（hex to base58）
			toAddr := contract.Parameter.Value.ToAddress
			// TronGrid API返回的地址可能是hex格式，需要转换
			if strings.HasPrefix(toAddr, "41") {
				toAddr = hexToBase58(toAddr)
			}

			// 检查是否是转入交易
			if !addresses[strings.ToLower(toAddr)] {
				continue
			}

			// 检查是否已处理
			var count int64
			model.GetDB().Model(&model.TransactionLog{}).Where("tx_hash = ?", tx.TxID).Count(&count)
			if count > 0 {
				continue
			}

			// TRX精度是6位 (1 TRX = 1,000,000 sun)
			amount := decimal.NewFromInt(contract.Parameter.Value.Amount).Div(decimal.NewFromInt(1000000))

			fromAddr := contract.Parameter.Value.OwnerAddress
			if strings.HasPrefix(fromAddr, "41") {
				fromAddr = hexToBase58(fromAddr)
			}

			transfers = append(transfers, Transfer{
				TxHash:      tx.TxID,
				From:        fromAddr,
				To:          toAddr,
				Amount:      amount,
				BlockNumber: uint64(tx.BlockNumber),
				Chain:       "trx",
			})
		}
	}

	return transfers, nil
}

// hexToBase58 moved to blockchain_utils.go

// scanTRC20 扫描TRC20交易
func (s *BlockchainService) scanTRC20(listener *ChainListener, addresses map[string]bool) ([]Transfer, error) {
	var transfers []Transfer

	for addr := range addresses {
		url := fmt.Sprintf("%s/v1/accounts/%s/transactions/trc20?only_confirmed=true&limit=50&contract_address=%s",
			listener.rpc, addr, listener.contractAddress)

		resp, err := httpClient.Get(url)
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			continue
		}

		var result struct {
			Data []struct {
				TransactionID string `json:"transaction_id"`
				From          string `json:"from"`
				To            string `json:"to"`
				Value         string `json:"value"`
				BlockTimestamp int64  `json:"block_timestamp"`
			} `json:"data"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			continue
		}

		for _, tx := range result.Data {
			// 检查是否是转入交易
			if !addresses[strings.ToLower(tx.To)] {
				continue
			}

			// 检查是否已处理
			var count int64
			model.GetDB().Model(&model.TransactionLog{}).Where("tx_hash = ?", tx.TransactionID).Count(&count)
			if count > 0 {
				continue
			}

			// USDT TRC20 精度是6位
			amount := parseTokenAmount(tx.Value, 6)

			transfers = append(transfers, Transfer{
				TxHash: tx.TransactionID,
				From:   tx.From,
				To:     tx.To,
				Amount: amount,
				Chain:  "trc20",
			})
		}
	}

	return transfers, nil
}

// scanEVM 扫描EVM兼容链交易 (ERC20, BEP20, Polygon)
func (s *BlockchainService) scanEVM(listener *ChainListener, addresses map[string]bool) ([]Transfer, error) {
	var transfers []Transfer

	// 获取最新区块号
	currentBlock, err := s.getEVMBlockNumber(listener.rpc)
	if err != nil {
		return nil, err
	}

	// 计算安全区块
	safeBlock := currentBlock - uint64(listener.confirmations)
	if listener.lastBlock == 0 {
		listener.lastBlock = safeBlock - 100 // 首次启动，扫描最近100个区块
	}

	if listener.lastBlock >= safeBlock {
		return nil, nil
	}

	// 构建日志过滤请求
	// Transfer事件签名: 0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef
	transferTopic := "0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef"

	for addr := range addresses {
		// 将地址填充到32字节
		paddedAddr := fmt.Sprintf("0x%064s", strings.TrimPrefix(strings.ToLower(addr), "0x"))
		paddedAddr = strings.Replace(paddedAddr, " ", "0", -1)

		params := map[string]interface{}{
			"jsonrpc": "2.0",
			"method":  "eth_getLogs",
			"params": []interface{}{
				map[string]interface{}{
					"fromBlock": fmt.Sprintf("0x%x", listener.lastBlock+1),
					"toBlock":   fmt.Sprintf("0x%x", safeBlock),
					"address":   listener.contractAddress,
					"topics": []interface{}{
						transferTopic,
						nil, // from address (any)
						paddedAddr, // to address
					},
				},
			},
			"id": 1,
		}

		reqBody, _ := json.Marshal(params)
		resp, err := httpClient.Post(listener.rpc, "application/json", bytes.NewReader(reqBody))
		if err != nil {
			continue
		}
		defer resp.Body.Close()

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			continue
		}

		var result struct {
			Result []struct {
				TransactionHash string   `json:"transactionHash"`
				Topics          []string `json:"topics"`
				Data            string   `json:"data"`
				BlockNumber     string   `json:"blockNumber"`
			} `json:"result"`
		}

		if err := json.Unmarshal(body, &result); err != nil {
			continue
		}

		for _, log := range result.Result {
			// 解析from地址
			from := "0x" + log.Topics[1][26:]

			// 解析金额
			amount := parseHexAmount(log.Data, 6) // USDT精度是6位

			// 检查是否已处理
			var count int64
			model.GetDB().Model(&model.TransactionLog{}).Where("tx_hash = ?", log.TransactionHash).Count(&count)
			if count > 0 {
				continue
			}

			blockNum := parseHexUint64(log.BlockNumber)

			transfers = append(transfers, Transfer{
				TxHash:      log.TransactionHash,
				From:        from,
				To:          addr,
				Amount:      amount,
				BlockNumber: blockNum,
				Chain:       listener.chain,
			})
		}
	}

	listener.lastBlock = safeBlock
	return transfers, nil
}

// getEVMBlockNumber 获取EVM链最新区块号
func (s *BlockchainService) getEVMBlockNumber(rpc string) (uint64, error) {
	params := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_blockNumber",
		"params":  []interface{}{},
		"id":      1,
	}

	reqBody, _ := json.Marshal(params)
	resp, err := httpClient.Post(rpc, "application/json", bytes.NewReader(reqBody))
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}

	var result struct {
		Result string `json:"result"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	return parseHexUint64(result.Result), nil
}

// processTransfer 处理转账事件
func (s *BlockchainService) processTransfer(transfer Transfer) {
	// 检查交易是否已处理（防重复）
	var existingLog model.TransactionLog
	if err := model.GetDB().Where("tx_hash = ?", transfer.TxHash).First(&existingLog).Error; err == nil {
		// 交易已存在，跳过处理
		return
	}

	// 记录交易日志
	txLog := model.TransactionLog{
		Chain:       transfer.Chain,
		TxHash:      transfer.TxHash,
		FromAddress: transfer.From,
		ToAddress:   transfer.To,
		Amount:      transfer.Amount.String(),
		BlockNumber: transfer.BlockNumber,
		Matched:     false,
	}

	if err := model.GetDB().Create(&txLog).Error; err != nil {
		log.Printf("Failed to create transaction log: %v", err)
		return
	}

	// 查找匹配的订单
	order := s.matchOrder(transfer)
	if order != nil {
		// 再次检查订单状态，防止并发重复处理
		if order.Status != model.OrderStatusPending {
			log.Printf("Order %s already processed, status: %d", order.TradeNo, order.Status)
			return
		}

		// 更新订单状态（使用乐观锁）
		now := time.Now()

		// 实际支付金额也需要截断到标准精度（与unique_amount一致）
		var actualAmount decimal.Decimal
		if transfer.Chain == "wechat" || transfer.Chain == "alipay" {
			actualAmount = transfer.Amount.Round(2) // 法币2位
		} else {
			actualAmount = transfer.Amount.Round(6) // 加密货币6位
		}

		updates := map[string]interface{}{
			"status":        model.OrderStatusPaid,
			"tx_hash":       transfer.TxHash,
			"from_address":  transfer.From,
			"actual_amount": actualAmount,
			"paid_at":       &now,
		}

		// 使用 WHERE 条件确保只更新待支付的订单
		result := model.GetDB().Model(order).
			Where("status = ?", model.OrderStatusPending).
			Updates(updates)

		if result.Error != nil {
			log.Printf("Failed to update order: %v", result.Error)
			return
		}

		// 如果没有更新任何行，说明订单已被其他进程处理
		if result.RowsAffected == 0 {
			log.Printf("Order %s already processed by another process", order.TradeNo)
			return
		}

		// 使订单缓存失效
		GetOrderService().InvalidateOrderCache(order.TradeNo)

		// 更新交易日志
		model.GetDB().Model(&txLog).Updates(map[string]interface{}{
			"matched":  true,
			"order_id": order.ID,
		})

		log.Printf("Order %s matched with tx %s, amount: %s", order.TradeNo, transfer.TxHash, transfer.Amount)

		// 记录订单匹配
		s.metrics.RecordOrderMatch(transfer.Chain)

		// 增加商户余额（使用 USD 结算金额）
		settlementAmount, _ := order.SettlementAmount.Float64()
		fee, _ := order.Fee.Float64()
		if err := GetWithdrawService().AddMerchantBalance(order.MerchantID, settlementAmount, fee, order.FeeType); err != nil {
			log.Printf("Failed to add merchant balance for order %s: %v", order.TradeNo, err)
		}

		// 触发回调通知
		go GetNotifyService().NotifyOrder(order.ID)

		// 发送Telegram通知 - 订单支付成功
		go GetTelegramService().NotifyOrderPaid(order)
	}
}

// matchOrder 匹配订单
func (s *BlockchainService) matchOrder(transfer Transfer) *model.Order {
	var order model.Order

	// 确定链的标准精度并截断金额
	// TRC20/ERC20等: 6位小数
	// BEP20: 虽然链上是18位，但我们统一按6位处理
	// TRX: 6位小数
	// 法币: 2位小数
	var normalizedAmount decimal.Decimal
	if transfer.Chain == "wechat" || transfer.Chain == "alipay" {
		normalizedAmount = transfer.Amount.Round(2) // 法币2位
	} else {
		normalizedAmount = transfer.Amount.Round(6) // 加密货币6位
	}

	// 计算过期容忍时间：当前时间 - 1分钟
	// 超过这个时间过期的订单将不再匹配，避免浪费扫描资源
	expiredTolerance := time.Now().Add(-1 * time.Minute)

	// 精确匹配唯一标识金额（无容差）
	// 加密货币: 102.040023 USDT
	// 法币: 100.01 CNY
	// 条件：待支付状态 且 (未过期 或 过期时间在1分钟以内)
	err := model.GetDB().
		Where("chain = ? AND to_address = ? AND unique_amount = ? AND status = ? AND expired_at > ?",
			transfer.Chain,
			strings.ToLower(transfer.To),
			normalizedAmount,
			model.OrderStatusPending,
			expiredTolerance).
		Order("created_at ASC").
		First(&order).Error

	if err != nil {
		// 兼容旧订单：尝试匹配 usdt_amount (旧字段)
		err = model.GetDB().
			Where("chain = ? AND to_address = ? AND usdt_amount = ? AND status = ? AND expired_at > ?",
				transfer.Chain,
				strings.ToLower(transfer.To),
				normalizedAmount,
				model.OrderStatusPending,
				expiredTolerance).
			Order("created_at ASC").
			First(&order).Error

		if err != nil {
			return nil
		}
	}

	return &order
}

// GetListenerStatus 获取监听器状态
func (s *BlockchainService) GetListenerStatus() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	// 查询每个链的钱包数量
	walletCounts := make(map[string]int64)
	var results []struct {
		Chain string
		Count int64
	}
	model.GetDB().Model(&model.Wallet{}).
		Select("chain, COUNT(*) as count").
		Where("status = 1 AND deleted_at IS NULL").
		Group("chain").
		Scan(&results)
	for _, r := range results {
		walletCounts[r.Chain] = r.Count
	}

	status := make(map[string]interface{})
	for chain, listener := range s.listeners {
		listener.mu.Lock()
		chainMetrics := s.metrics.GetChainMetrics(chain)
		status[chain] = map[string]interface{}{
			"name":              listener.name,
			"chain_type":        listener.chainType,
			"enabled":           listener.enabled,
			"running":           listener.running,
			"wallet_count":      walletCounts[chain],
			"rpc":               listener.rpc,
			"rpc_backups":       listener.rpcBackups,
			"contract":          listener.contractAddress,
			"contract_address":  listener.contractAddress,
			"interval":          listener.baseScanInterval,
			"scan_interval":     listener.baseScanInterval,
			"confirmations":     listener.confirmations,
			"max_block_range":   listener.maxBlockRange,
			"max_batch_size":    listener.maxBatchSize,
			"batch_delay_ms":    listener.batchDelayMs,
			"rate_limit":        listener.rateLimit,
			"is_builtin":        listener.isBuiltin,
			"last_block":        listener.lastBlock,
			"current_block":     chainMetrics["current_block"],
			"blocks_behind":     chainMetrics["blocks_behind"],
			"success_rate":      chainMetrics["success_rate"],
			"last_scan_time":    chainMetrics["last_scan_time"],
			"last_error":        chainMetrics["last_error"],
			"scan_count":        chainMetrics["scan_count"],
			"scan_failure":      chainMetrics["scan_failure"],
			"passive":           false,
		}
		listener.mu.Unlock()
	}

	// 添加微信、支付宝（被动推送模式）
	wechatEnabled := true
	alipayEnabled := true

	var wechatConfig model.SystemConfig
	if model.GetDB().Where(`"key" = ?`, model.ConfigKeyWechatEnabled).First(&wechatConfig).Error == nil {
		wechatEnabled = wechatConfig.Value == "1" || wechatConfig.Value == "true"
	}

	var alipayConfig model.SystemConfig
	if model.GetDB().Where(`"key" = ?`, model.ConfigKeyAlipayEnabled).First(&alipayConfig).Error == nil {
		alipayEnabled = alipayConfig.Value == "1" || alipayConfig.Value == "true"
	}

	status["wechat"] = map[string]interface{}{
		"name":         "微信支付",
		"chain_type":   "passive",
		"enabled":      wechatEnabled,
		"running":      wechatEnabled,
		"wallet_count": walletCounts["wechat"],
		"is_builtin":   true,
		"passive":      true,
	}
	status["alipay"] = map[string]interface{}{
		"name":         "支付宝",
		"chain_type":   "passive",
		"enabled":      alipayEnabled,
		"running":      alipayEnabled,
		"wallet_count": walletCounts["alipay"],
		"is_builtin":   true,
		"passive":      true,
	}

	return status
}

// ChainUpdateRequest 更新/创建链配置请求
type ChainUpdateRequest struct {
	Chain           string   `json:"chain"`
	Name            string   `json:"name"`
	ChainType       string   `json:"chain_type"` // trx | trc20 | evm
	Enabled         *bool    `json:"enabled"`
	RPC             string   `json:"rpc"`
	RPCBackups      []string `json:"rpc_backups"`
	ContractAddress string   `json:"contract_address"`
	Confirmations   int      `json:"confirmations"`
	ScanInterval    int      `json:"scan_interval"`
	MaxBlockRange   int      `json:"max_block_range"`
	MaxBatchSize    int      `json:"max_batch_size"`
	BatchDelayMs    int      `json:"batch_delay_ms"`
	RateLimit       float64  `json:"rate_limit"`
	SortOrder       int      `json:"sort_order"`
}

// UpdateChainConfig 更新链配置（RPC/扫描参数等），并热加载到运行时
func (s *BlockchainService) UpdateChainConfig(chain string, req ChainUpdateRequest) error {
	if chain == "wechat" || chain == "alipay" {
		return fmt.Errorf("被动渠道不支持编辑 RPC 配置")
	}

	var row model.ChainConfig
	if err := model.GetDB().Where("chain = ?", chain).First(&row).Error; err != nil {
		return fmt.Errorf("链配置不存在: %s", chain)
	}

	if req.Name != "" {
		row.Name = req.Name
	}
	if req.RPC != "" {
		row.RPC = req.RPC
	}
	if req.RPCBackups != nil {
		row.SetRPCBackupList(req.RPCBackups)
	}
	// 合约地址允许清空（TRX 原生代币）
	row.ContractAddress = req.ContractAddress
	if req.Confirmations > 0 {
		row.Confirmations = req.Confirmations
	}
	if req.ScanInterval > 0 {
		row.ScanInterval = req.ScanInterval
	}
	row.MaxBlockRange = req.MaxBlockRange
	row.MaxBatchSize = req.MaxBatchSize
	row.BatchDelayMs = req.BatchDelayMs
	row.RateLimit = req.RateLimit
	if req.SortOrder != 0 {
		row.SortOrder = req.SortOrder
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}

	if err := model.GetDB().Save(&row).Error; err != nil {
		return err
	}

	// 热更新运行时 listener
	return s.applyDBConfigToRuntime(row)
}

// AddChain 动态添加新链（仅支持 EVM / TRC20 / TRX 类型）
func (s *BlockchainService) AddChain(req ChainUpdateRequest) error {
	chain := strings.ToLower(strings.TrimSpace(req.Chain))
	if chain == "" {
		return fmt.Errorf("链标识不能为空")
	}
	if chain == "wechat" || chain == "alipay" {
		return fmt.Errorf("不能添加被动渠道")
	}
	// 仅允许字母数字下划线
	for _, c := range chain {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_') {
			return fmt.Errorf("链标识仅允许小写字母、数字和下划线")
		}
	}
	if len(chain) > 32 {
		return fmt.Errorf("链标识过长")
	}

	chainType := strings.ToLower(strings.TrimSpace(req.ChainType))
	if chainType == "" {
		chainType = "evm"
	}
	if chainType != "evm" && chainType != "trc20" && chainType != "trx" {
		return fmt.Errorf("不支持的链类型: %s（支持 evm / trc20 / trx）", chainType)
	}
	if req.RPC == "" {
		return fmt.Errorf("RPC 地址不能为空")
	}
	if chainType == "evm" && req.ContractAddress == "" {
		return fmt.Errorf("EVM 链需要填写代币合约地址")
	}
	if chainType == "trc20" && req.ContractAddress == "" {
		return fmt.Errorf("TRC20 链需要填写代币合约地址")
	}

	var count int64
	model.GetDB().Model(&model.ChainConfig{}).Where("chain = ?", chain).Count(&count)
	if count > 0 {
		return fmt.Errorf("链标识已存在: %s", chain)
	}

	s.mu.RLock()
	_, exists := s.listeners[chain]
	s.mu.RUnlock()
	if exists {
		return fmt.Errorf("链已在运行时注册: %s", chain)
	}

	name := req.Name
	if name == "" {
		name = strings.ToUpper(chain)
	}
	scanInterval := req.ScanInterval
	if scanInterval <= 0 {
		scanInterval = 30
	}
	confirmations := req.Confirmations
	if confirmations <= 0 {
		confirmations = 12
	}

	row := model.ChainConfig{
		Chain:           chain,
		Name:            name,
		ChainType:       chainType,
		Enabled:         false,
		RPC:             req.RPC,
		ContractAddress: req.ContractAddress,
		Confirmations:   confirmations,
		ScanInterval:    scanInterval,
		MaxBlockRange:   req.MaxBlockRange,
		MaxBatchSize:    req.MaxBatchSize,
		BatchDelayMs:    req.BatchDelayMs,
		RateLimit:       req.RateLimit,
		IsBuiltin:       false,
		SortOrder:       req.SortOrder,
	}
	if req.Enabled != nil {
		row.Enabled = *req.Enabled
	}
	if req.SortOrder == 0 {
		row.SortOrder = 1000
	}
	row.SetRPCBackupList(req.RPCBackups)

	if err := model.GetDB().Create(&row).Error; err != nil {
		return err
	}

	s.mu.Lock()
	s.registerListenerFromDB(row)
	listener := s.listeners[chain]
	s.mu.Unlock()

	// 若启用则立即启动
	if row.Enabled && listener != nil {
		listener.mu.Lock()
		if !listener.running {
			listener.stopCh = make(chan struct{})
			s.wg.Add(1)
			go s.runListener(chain, listener)
		}
		listener.mu.Unlock()
	}

	log.Printf("Chain %s added (type=%s)", chain, chainType)
	return nil
}

// DeleteChain 删除自定义链（内置链不可删）
func (s *BlockchainService) DeleteChain(chain string) error {
	if chain == "wechat" || chain == "alipay" {
		return fmt.Errorf("被动渠道不可删除")
	}

	var row model.ChainConfig
	if err := model.GetDB().Where("chain = ?", chain).First(&row).Error; err != nil {
		return fmt.Errorf("链配置不存在")
	}
	if row.IsBuiltin {
		return fmt.Errorf("内置链不可删除，请使用禁用")
	}

	// 先停止监听
	_ = s.DisableChain(chain)

	s.mu.Lock()
	delete(s.listeners, chain)
	delete(s.rpcClients, chain)
	s.mu.Unlock()

	if err := model.GetDB().Delete(&row).Error; err != nil {
		return err
	}
	log.Printf("Chain %s deleted", chain)
	return nil
}

// applyDBConfigToRuntime 将 DB 配置应用到运行时 listener（必要时重启）
func (s *BlockchainService) applyDBConfigToRuntime(row model.ChainConfig) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	listener, ok := s.listeners[row.Chain]
	if !ok {
		// 运行时不存在则注册
		s.registerListenerFromDB(row)
		listener = s.listeners[row.Chain]
		if row.Enabled && listener != nil {
			listener.mu.Lock()
			if !listener.running {
				listener.stopCh = make(chan struct{})
				s.wg.Add(1)
				go s.runListener(row.Chain, listener)
			}
			listener.mu.Unlock()
		}
		return nil
	}

	// 更新 RPC 客户端
	rpcEndpoints := []string{row.RPC}
	backups := row.GetRPCBackupList()
	if len(backups) > 0 {
		rpcEndpoints = append(rpcEndpoints, backups...)
	}
	rpcClient := NewRPCClient(rpcEndpoints)
	if row.RateLimit > 0 {
		rpcClient.SetCustomRateLimit(row.RateLimit)
	}
	s.rpcClients[row.Chain] = rpcClient

	listener.mu.Lock()
	wasRunning := listener.running
	wasEnabled := listener.enabled
	listener.name = row.Name
	listener.chainType = row.ChainType
	listener.rpc = row.RPC
	listener.rpcBackups = backups
	listener.contractAddress = row.ContractAddress
	listener.confirmations = row.Confirmations
	listener.scanInterval = row.ScanInterval
	listener.baseScanInterval = row.ScanInterval
	listener.maxBlockRange = row.MaxBlockRange
	listener.maxBatchSize = row.MaxBatchSize
	listener.batchDelayMs = row.BatchDelayMs
	listener.rateLimit = row.RateLimit
	listener.enabled = row.Enabled
	listener.mu.Unlock()

	// 若从禁用变为启用
	if row.Enabled && !wasRunning {
		listener.mu.Lock()
		listener.stopCh = make(chan struct{})
		listener.mu.Unlock()
		s.wg.Add(1)
		go s.runListener(row.Chain, listener)
	}
	// 若从启用变为禁用
	if !row.Enabled && wasEnabled && wasRunning {
		listener.mu.Lock()
		if listener.running {
			close(listener.stopCh)
		}
		listener.mu.Unlock()
	}

	log.Printf("Chain %s config updated (hot reload)", row.Chain)
	return nil
}

// healthGrade 根据分数返回等级文案
func healthGrade(score int) (grade, status string) {
	switch {
	case score >= 90:
		return "优秀", "healthy"
	case score >= 75:
		return "良好", "healthy"
	case score >= 60:
		return "一般", "degraded"
	case score >= 40:
		return "较差", "degraded"
	default:
		return "故障", "down"
	}
}

// CheckChainHealth 检查单条链健康度（百分制评分 0-100）
//
// 区块链评分维度（满分 100）:
//   - RPC 连通性  35 分：能否拿到最新区块
//   - 响应延迟    25 分：按延迟阶梯扣分
//   - 监听状态    15 分：启用且运行中
//   - 扫描成功率  15 分：历史扫描成功比例
//   - 区块同步    10 分：落后区块数
//
// 法币渠道（支付宝/微信）评分维度见 checkFiatChannelHealth。
func (s *BlockchainService) CheckChainHealth(chain string) map[string]interface{} {
	result := map[string]interface{}{
		"chain":        chain,
		"healthy":      false,
		"status":       "unknown",
		"score":        0,
		"grade":        "未知",
		"latency_ms":   int64(0),
		"block_height": uint64(0),
		"message":      "",
		"breakdown":    map[string]interface{}{},
	}

	if chain == "wechat" || chain == "alipay" {
		return s.checkFiatChannelHealth(chain)
	}

	s.mu.RLock()
	listener, ok := s.listeners[chain]
	rpcClient := s.rpcClients[chain]
	s.mu.RUnlock()

	if !ok {
		result["status"] = "not_found"
		result["message"] = "链未配置"
		result["grade"] = "未配置"
		result["score"] = 0
		return result
	}

	listener.mu.Lock()
	rpc := listener.rpc
	chainType := listener.chainType
	enabled := listener.enabled
	running := listener.running
	listener.mu.Unlock()

	result["enabled"] = enabled
	result["running"] = running

	// 未配置 RPC
	if rpc == "" {
		result["status"] = "misconfigured"
		result["message"] = "RPC 地址未配置"
		result["grade"] = "配置错误"
		result["score"] = 0
		result["breakdown"] = map[string]interface{}{
			"rpc": map[string]interface{}{"score": 0, "max": 35, "label": "RPC连通", "detail": "未配置"},
			"latency": map[string]interface{}{"score": 0, "max": 25, "label": "响应延迟", "detail": "—"},
			"listener": map[string]interface{}{"score": 0, "max": 15, "label": "监听状态", "detail": "—"},
			"scan": map[string]interface{}{"score": 0, "max": 15, "label": "扫描成功率", "detail": "—"},
			"sync": map[string]interface{}{"score": 0, "max": 10, "label": "区块同步", "detail": "—"},
		}
		return result
	}

	// --- 1) RPC 连通 35 ---
	start := time.Now()
	var blockHeight uint64
	var err error
	switch {
	case chain == "trx" || chain == "trc20" || chainType == "trx" || chainType == "trc20":
		blockHeight, err = s.probeTronRPC(rpc, rpcClient)
	default:
		blockHeight, err = s.probeEVMRPC(rpc, rpcClient)
	}
	latency := time.Since(start).Milliseconds()
	result["latency_ms"] = latency

	rpcScore := 0
	rpcDetail := "连接失败"
	if err != nil {
		// RPC 挂掉：总分按连通失败结算，其余维度仍可给部分参考分（监听/历史）
		metrics := s.metrics.GetChainMetrics(chain)
		successRate, _ := metrics["success_rate"].(float64)
		listenerScore, listenerDetail := scoreListener(enabled, running)
		scanScore, scanDetail := scoreScanSuccess(successRate)
		// 连通失败时延迟/同步为 0
		total := rpcScore + 0 + listenerScore + scanScore + 0
		// 连通失败上限压到 40 以下
		if total > 35 {
			total = 35
		}
		grade, status := healthGrade(total)
		if !enabled {
			status = "disabled"
			grade = "已禁用"
		}
		result["score"] = total
		result["grade"] = grade
		result["status"] = status
		if status == "down" || total < 40 {
			result["status"] = "down"
			result["grade"] = "故障"
		}
		result["healthy"] = false
		result["message"] = err.Error()
		result["breakdown"] = map[string]interface{}{
			"rpc":      map[string]interface{}{"score": rpcScore, "max": 35, "label": "RPC连通", "detail": err.Error()},
			"latency":  map[string]interface{}{"score": 0, "max": 25, "label": "响应延迟", "detail": "不可达"},
			"listener": map[string]interface{}{"score": listenerScore, "max": 15, "label": "监听状态", "detail": listenerDetail},
			"scan":     map[string]interface{}{"score": scanScore, "max": 15, "label": "扫描成功率", "detail": scanDetail},
			"sync":     map[string]interface{}{"score": 0, "max": 10, "label": "区块同步", "detail": "不可用"},
		}
		return result
	}
	rpcScore = 35
	rpcDetail = fmt.Sprintf("高度 #%d", blockHeight)
	result["block_height"] = blockHeight

	// --- 2) 延迟 25 ---
	latScore, latDetail := scoreLatency(latency)

	// --- 3) 监听 15 ---
	listenerScore, listenerDetail := scoreListener(enabled, running)

	// --- 4) 扫描成功率 15 ---
	metrics := s.metrics.GetChainMetrics(chain)
	successRate, _ := metrics["success_rate"].(float64)
	blocksBehind, _ := metrics["blocks_behind"].(uint64)
	// 用实时高度校正落后：若 last_block 有值
	if lastBlock, ok := metrics["last_block"].(uint64); ok && lastBlock > 0 && blockHeight > lastBlock {
		blocksBehind = blockHeight - lastBlock
	}
	scanScore, scanDetail := scoreScanSuccess(successRate)

	// --- 5) 同步 10 ---
	syncScore, syncDetail := scoreSyncLag(blocksBehind)

	total := rpcScore + latScore + listenerScore + scanScore + syncScore
	if total > 100 {
		total = 100
	}
	if total < 0 {
		total = 0
	}

	grade, status := healthGrade(total)
	msg := fmt.Sprintf("综合评分 %d/100（%s）", total, grade)

	if !enabled {
		status = "disabled"
		grade = "已禁用"
		// 禁用时仍展示可达性评分，但标记 disabled
		msg = fmt.Sprintf("已禁用 · 节点可达评分 %d/100", total)
	} else if !running {
		if status == "healthy" {
			status = "stopped"
		}
		msg = fmt.Sprintf("监听器未运行 · 评分 %d/100（%s）", total, grade)
	}

	result["score"] = total
	result["grade"] = grade
	result["status"] = status
	// 禁用不算故障；启用时 60 分以上视为健康
	if !enabled {
		result["healthy"] = true
	} else {
		result["healthy"] = total >= 60 && running
	}
	result["message"] = msg
	result["success_rate"] = successRate
	result["blocks_behind"] = blocksBehind
	result["breakdown"] = map[string]interface{}{
		"rpc":      map[string]interface{}{"score": rpcScore, "max": 35, "label": "RPC连通", "detail": rpcDetail},
		"latency":  map[string]interface{}{"score": latScore, "max": 25, "label": "响应延迟", "detail": latDetail},
		"listener": map[string]interface{}{"score": listenerScore, "max": 15, "label": "监听状态", "detail": listenerDetail},
		"scan":     map[string]interface{}{"score": scanScore, "max": 15, "label": "扫描成功率", "detail": scanDetail},
		"sync":     map[string]interface{}{"score": syncScore, "max": 10, "label": "区块同步", "detail": syncDetail},
	}
	return result
}

func scoreLatency(ms int64) (int, string) {
	// 满分 25
	switch {
	case ms <= 200:
		return 25, fmt.Sprintf("%dms 极快", ms)
	case ms <= 500:
		return 22, fmt.Sprintf("%dms 很快", ms)
	case ms <= 1000:
		return 18, fmt.Sprintf("%dms 良好", ms)
	case ms <= 2000:
		return 14, fmt.Sprintf("%dms 一般", ms)
	case ms <= 5000:
		return 8, fmt.Sprintf("%dms 偏慢", ms)
	case ms <= 10000:
		return 4, fmt.Sprintf("%dms 很慢", ms)
	default:
		return 0, fmt.Sprintf("%dms 超时级", ms)
	}
}

func scoreListener(enabled, running bool) (int, string) {
	// 满分 15
	if !enabled {
		return 5, "已禁用"
	}
	if running {
		return 15, "运行中"
	}
	return 0, "已停止"
}

func scoreScanSuccess(rate float64) (int, string) {
	// 满分 15；尚无采样时给中性分 12（避免冷启动误报）
	if rate <= 0 {
		return 12, "暂无采样"
	}
	switch {
	case rate >= 98:
		return 15, fmt.Sprintf("%.0f%%", rate)
	case rate >= 90:
		return 13, fmt.Sprintf("%.0f%%", rate)
	case rate >= 80:
		return 10, fmt.Sprintf("%.0f%%", rate)
	case rate >= 70:
		return 7, fmt.Sprintf("%.0f%%", rate)
	case rate >= 50:
		return 4, fmt.Sprintf("%.0f%%", rate)
	default:
		return 1, fmt.Sprintf("%.0f%% 偏低", rate)
	}
}

func scoreSyncLag(behind uint64) (int, string) {
	// 满分 10
	switch {
	case behind == 0:
		return 10, "已同步"
	case behind <= 5:
		return 9, fmt.Sprintf("落后 %d 块", behind)
	case behind <= 20:
		return 7, fmt.Sprintf("落后 %d 块", behind)
	case behind <= 50:
		return 5, fmt.Sprintf("落后 %d 块", behind)
	case behind <= 200:
		return 3, fmt.Sprintf("落后 %d 块", behind)
	case behind <= 1000:
		return 1, fmt.Sprintf("落后 %d 块", behind)
	default:
		return 0, fmt.Sprintf("严重落后 %d 块", behind)
	}
}

// checkFiatChannelHealth 支付宝/微信渠道健康评分（百分制）
//
// 维度:
//   - 渠道启用    20
//   - 模式配置    25（官方/个人 + 关键参数）
//   - 收款能力    30（官方密钥完整 / 个人码钱包数）
//   - 回调就绪    15（site_url，官方模式必需）
//   - 可用性探测  10（官方：网关可达；个人：有启用钱包）
func (s *BlockchainService) checkFiatChannelHealth(chain string) map[string]interface{} {
	result := map[string]interface{}{
		"chain":      chain,
		"healthy":    false,
		"status":     "unknown",
		"score":      0,
		"grade":      "未知",
		"latency_ms": int64(0),
		"message":    "",
		"breakdown":  map[string]interface{}{},
		"passive":    true,
	}

	enabled := s.IsChainEnabled(chain)
	result["enabled"] = enabled
	result["running"] = enabled

	// 1) 启用 20
	enScore := 0
	enDetail := "已禁用"
	if enabled {
		enScore = 20
		enDetail = "已启用"
	}

	// 2) 模式 25 + 3) 收款能力 30 + 4) 回调 15 + 5) 探测 10
	mode := "personal"
	// 延迟 import 循环：通过 model 读配置键（与 payment 包一致）
	modeKey := "pay_alipay_mode"
	appKey := "pay_alipay_app_id"
	privKey := "pay_alipay_private_key"
	pubKey := "pay_alipay_public_key"
	if chain == "wechat" {
		modeKey = "pay_wechat_mode"
		appKey = "pay_wechat_app_id"
		privKey = "pay_wechat_private_key"
		pubKey = "pay_wechat_mch_id" // 微信用商户号代替公钥完成度
	}
	mode = getSystemConfigValue(modeKey, "personal")
	appID := getSystemConfigValue(appKey, "")
	priv := getSystemConfigValue(privKey, "")
	pubOrMch := getSystemConfigValue(pubKey, "")
	siteURL := getSystemConfigValue("site_url", "")

	modeScore := 0
	modeDetail := ""
	capScore := 0
	capDetail := ""
	cbScore := 0
	cbDetail := ""
	probeScore := 0
	probeDetail := ""
	var latency int64

	// 钱包数量（个人码）
	var walletCount int64
	model.GetDB().Model(&model.Wallet{}).
		Where("chain = ? AND status = 1 AND deleted_at IS NULL", chain).
		Count(&walletCount)

	if mode == "official" {
		modeScore = 25
		modeDetail = "官方商户模式"

		// 密钥完整度
		filled := 0
		if appID != "" {
			filled++
		}
		if priv != "" {
			filled++
		}
		if pubOrMch != "" {
			filled++
		}
		// 微信还要 serial / api v3
		if chain == "wechat" {
			if getSystemConfigValue("pay_wechat_api_v3_key", "") != "" {
				filled++
			}
			if getSystemConfigValue("pay_wechat_serial_no", "") != "" {
				filled++
			}
			// max 5 items for wechat
			capScore = filled * 6 // 0-30
			if capScore > 30 {
				capScore = 30
			}
			capDetail = fmt.Sprintf("官方参数 %d/5 已填", filled)
		} else {
			capScore = filled * 10 // 0-30
			capDetail = fmt.Sprintf("官方参数 %d/3 已填", filled)
		}

		// site_url 回调
		if siteURL != "" && (strings.HasPrefix(siteURL, "https://") || strings.HasPrefix(siteURL, "http://")) {
			cbScore = 15
			cbDetail = "site_url 已配置"
		} else {
			cbScore = 0
			cbDetail = "缺少 site_url（官方回调必需）"
		}

		// 探测：参数齐全则给分；尝试轻量连通
		if appID != "" && priv != "" {
			start := time.Now()
			ok, msg := probeOfficialGateway(chain)
			latency = time.Since(start).Milliseconds()
			result["latency_ms"] = latency
			if ok {
				probeScore = 10
				probeDetail = msg
			} else {
				probeScore = 3
				probeDetail = msg
			}
		} else {
			probeScore = 0
			probeDetail = "密钥未配齐，跳过探测"
		}
	} else {
		modeScore = 18
		modeDetail = "个人收款码模式"

		if walletCount > 0 {
			capScore = 30
			if walletCount == 1 {
				capScore = 24
			}
			capDetail = fmt.Sprintf("%d 个可用收款码", walletCount)
		} else {
			capScore = 0
			capDetail = "无可用收款码钱包"
		}

		// 个人码不强制 site_url
		cbScore = 12
		cbDetail = "个人码无需官方回调"
		if siteURL != "" {
			cbScore = 15
			cbDetail = "site_url 已配置"
		}

		if walletCount > 0 && enabled {
			probeScore = 10
			probeDetail = "有可用收款码"
		} else if walletCount > 0 {
			probeScore = 6
			probeDetail = "有收款码但渠道禁用"
		} else {
			probeScore = 0
			probeDetail = "无收款能力"
		}
	}

	total := enScore + modeScore + capScore + cbScore + probeScore
	if total > 100 {
		total = 100
	}
	grade, status := healthGrade(total)
	if !enabled {
		status = "disabled"
		grade = "已禁用"
	}

	result["score"] = total
	result["grade"] = grade
	result["status"] = status
	result["healthy"] = total >= 60 && enabled
	if !enabled {
		result["healthy"] = true
	}
	result["message"] = fmt.Sprintf("综合评分 %d/100（%s）· %s", total, grade, modeDetail)
	result["mode"] = mode
	result["wallet_count"] = walletCount
	result["breakdown"] = map[string]interface{}{
		"enabled":  map[string]interface{}{"score": enScore, "max": 20, "label": "渠道启用", "detail": enDetail},
		"mode":     map[string]interface{}{"score": modeScore, "max": 25, "label": "收款模式", "detail": modeDetail},
		"capacity": map[string]interface{}{"score": capScore, "max": 30, "label": "收款能力", "detail": capDetail},
		"callback": map[string]interface{}{"score": cbScore, "max": 15, "label": "回调就绪", "detail": cbDetail},
		"probe":    map[string]interface{}{"score": probeScore, "max": 10, "label": "可用性", "detail": probeDetail},
	}
	return result
}

func getSystemConfigValue(key, def string) string {
	var cfg model.SystemConfig
	if err := model.GetDB().Where(`"key" = ?`, key).First(&cfg).Error; err != nil {
		return def
	}
	if cfg.Value == "" {
		return def
	}
	return cfg.Value
}

// probeOfficialGateway 轻量探测官方网关（不真正下单）
func probeOfficialGateway(chain string) (bool, string) {
	if chain == "alipay" {
		// 访问网关首页/根路径，能建立 TLS 即认为网络可达
		url := "https://openapi.alipay.com/gateway.do"
		if getSystemConfigValue("pay_alipay_sandbox", "0") == "1" {
			url = "https://openapi-sandbox.dl.alipaydev.com/gateway.do"
		}
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, "构造请求失败"
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return false, "网关不可达: " + err.Error()
		}
		resp.Body.Close()
		// 支付宝网关 GET 通常返回 200 或业务错误页，只要连通即可
		if resp.StatusCode >= 200 && resp.StatusCode < 500 {
			return true, fmt.Sprintf("支付宝网关可达 (HTTP %d)", resp.StatusCode)
		}
		return false, fmt.Sprintf("网关异常 HTTP %d", resp.StatusCode)
	}
	if chain == "wechat" {
		url := "https://api.mch.weixin.qq.com"
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return false, "构造请求失败"
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return false, "微信网关不可达: " + err.Error()
		}
		resp.Body.Close()
		if resp.StatusCode >= 200 && resp.StatusCode < 500 {
			return true, fmt.Sprintf("微信网关可达 (HTTP %d)", resp.StatusCode)
		}
		return false, fmt.Sprintf("网关异常 HTTP %d", resp.StatusCode)
	}
	return false, "未知渠道"
}

// CheckAllChainsHealth 检查所有链健康度
func (s *BlockchainService) CheckAllChainsHealth() map[string]interface{} {
	s.mu.RLock()
	chains := make([]string, 0, len(s.listeners)+2)
	for chain := range s.listeners {
		chains = append(chains, chain)
	}
	s.mu.RUnlock()
	chains = append(chains, "wechat", "alipay")

	results := make(map[string]interface{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	for _, chain := range chains {
		wg.Add(1)
		go func(ch string) {
			defer wg.Done()
			h := s.CheckChainHealth(ch)
			mu.Lock()
			results[ch] = h
			mu.Unlock()
		}(chain)
	}
	wg.Wait()
	return results
}

// probeEVMRPC 探测 EVM RPC 可用性并返回区块高度
func (s *BlockchainService) probeEVMRPC(rpc string, client *RPCClient) (uint64, error) {
	reqBody := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_blockNumber",
		"params":  []interface{}{},
		"id":      1,
	}
	body, _ := json.Marshal(reqBody)

	var resp *http.Response
	var err error
	if client != nil {
		resp, err = client.Post("", "application/json", bytes.NewReader(body))
	} else {
		httpReq, _ := http.NewRequest("POST", rpc, bytes.NewReader(body))
		httpReq.Header.Set("Content-Type", "application/json")
		resp, err = httpClient.Do(httpReq)
	}
	if err != nil {
		return 0, fmt.Errorf("RPC 连接失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("读取响应失败: %v", err)
	}
	var result struct {
		Result string `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("解析响应失败")
	}
	if result.Error != nil {
		return 0, fmt.Errorf("RPC 错误: %s", result.Error.Message)
	}
	if result.Result == "" {
		return 0, fmt.Errorf("空的区块高度响应")
	}
	// 解析 hex
	hexStr := strings.TrimPrefix(result.Result, "0x")
	var height uint64
	fmt.Sscanf(hexStr, "%x", &height)
	return height, nil
}

// probeTronRPC 探测 Tron RPC 可用性
func (s *BlockchainService) probeTronRPC(rpc string, client *RPCClient) (uint64, error) {
	url := strings.TrimRight(rpc, "/") + "/wallet/getnowblock"
	var resp *http.Response
	var err error
	if client != nil {
		resp, err = client.Post("/wallet/getnowblock", "application/json", bytes.NewReader([]byte("{}")))
	} else {
		resp, err = httpClient.Post(url, "application/json", bytes.NewReader([]byte("{}")))
	}
	if err != nil {
		return 0, fmt.Errorf("Tron RPC 连接失败: %v", err)
	}
	defer resp.Body.Close()
	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, fmt.Errorf("读取响应失败: %v", err)
	}
	var result struct {
		BlockHeader struct {
			RawData struct {
				Number int64 `json:"number"`
			} `json:"raw_data"`
		} `json:"block_header"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return 0, fmt.Errorf("解析 Tron 响应失败")
	}
	if result.BlockHeader.RawData.Number <= 0 {
		return 0, fmt.Errorf("无效的区块高度")
	}
	return uint64(result.BlockHeader.RawData.Number), nil
}

// IsValidChainDynamic 检查链是否有效（内置 + 数据库动态链 + 被动渠道）
func (s *BlockchainService) IsValidChainDynamic(chain string) bool {
	if chain == "wechat" || chain == "alipay" {
		return true
	}
	s.mu.RLock()
	_, ok := s.listeners[chain]
	s.mu.RUnlock()
	if ok {
		return true
	}
	var count int64
	model.GetDB().Model(&model.ChainConfig{}).Where("chain = ?", chain).Count(&count)
	return count > 0
}

// GetRegisteredChains 返回所有已注册链标识
func (s *BlockchainService) GetRegisteredChains() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	chains := make([]string, 0, len(s.listeners)+2)
	for chain := range s.listeners {
		chains = append(chains, chain)
	}
	chains = append(chains, "wechat", "alipay")
	return chains
}

// EnableChain 启用链监控
func (s *BlockchainService) EnableChain(chain string) error {
	// 处理微信、支付宝的特殊情况（被动推送渠道）
	if chain == "wechat" || chain == "alipay" {
		return s.setPassiveChannelEnabled(chain, true)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	listener, ok := s.listeners[chain]
	if !ok {
		return fmt.Errorf("unknown chain: %s", chain)
	}

	listener.mu.Lock()
	defer listener.mu.Unlock()

	if listener.enabled && listener.running {
		return nil // 已经在运行
	}

	listener.enabled = true

	// 如果服务已启动但监听器未运行，则启动它
	if !listener.running {
		listener.stopCh = make(chan struct{}) // 重新创建停止通道
		s.wg.Add(1)
		go s.runListener(chain, listener)
	}

	// 持久化启用状态
	model.GetDB().Model(&model.ChainConfig{}).Where("chain = ?", chain).Update("enabled", true)

	log.Printf("Chain %s enabled", chain)
	return nil
}

// DisableChain 禁用链监控
func (s *BlockchainService) DisableChain(chain string) error {
	// 处理微信、支付宝的特殊情况（被动推送渠道）
	if chain == "wechat" || chain == "alipay" {
		return s.setPassiveChannelEnabled(chain, false)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	listener, ok := s.listeners[chain]
	if !ok {
		return fmt.Errorf("unknown chain: %s", chain)
	}

	listener.mu.Lock()

	if !listener.enabled {
		listener.mu.Unlock()
		return nil // 已经禁用
	}

	listener.enabled = false

	// 如果监听器正在运行，停止它
	if listener.running {
		close(listener.stopCh)
	}
	listener.mu.Unlock()

	// 持久化禁用状态
	model.GetDB().Model(&model.ChainConfig{}).Where("chain = ?", chain).Update("enabled", false)

	log.Printf("Chain %s disabled", chain)
	return nil
}

// setPassiveChannelEnabled 设置被动推送渠道启用状态
func (s *BlockchainService) setPassiveChannelEnabled(channel string, enabled bool) error {
	var configKey string
	if channel == "wechat" {
		configKey = model.ConfigKeyWechatEnabled
	} else if channel == "alipay" {
		configKey = model.ConfigKeyAlipayEnabled
	} else {
		return fmt.Errorf("unknown passive channel: %s", channel)
	}

	value := "0"
	if enabled {
		value = "1"
	}

	// 更新数据库配置
	var config model.SystemConfig
	if err := model.GetDB().Where(`"key" = ?`, configKey).First(&config).Error; err != nil {
		// 不存在，创建新记录
		config = model.SystemConfig{
			Key:         configKey,
			Value:       value,
			Description: channel + " 支付启用状态",
		}
		if err := model.GetDB().Create(&config).Error; err != nil {
			return err
		}
	} else {
		// 存在，更新
		if err := model.GetDB().Model(&config).Update("value", value).Error; err != nil {
			return err
		}
	}

	log.Printf("Passive channel %s enabled: %v", channel, enabled)
	return nil
}

// IsChainEnabled 检查链是否启用
func (s *BlockchainService) IsChainEnabled(chain string) bool {
	if chain == "wechat" || chain == "alipay" {
		var cfg model.SystemConfig
		key := model.ConfigKeyWechatEnabled
		if chain == "alipay" {
			key = model.ConfigKeyAlipayEnabled
		}
		if model.GetDB().Where(`"key" = ?`, key).First(&cfg).Error == nil {
			return cfg.Value == "1" || cfg.Value == "true"
		}
		return true // 默认启用
	}

	s.mu.RLock()
	defer s.mu.RUnlock()

	listener, ok := s.listeners[chain]
	if !ok {
		return false
	}

	listener.mu.Lock()
	defer listener.mu.Unlock()
	return listener.enabled
}

// GetEnabledChains 获取所有已启用的链
func (s *BlockchainService) GetEnabledChains() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var chains []string
	for chain, listener := range s.listeners {
		listener.mu.Lock()
		if listener.enabled {
			chains = append(chains, chain)
		}
		listener.mu.Unlock()
	}
	return chains
}

// GetChainStatus 获取链状态 (简化版，用于商户查看)
func (s *BlockchainService) GetChainStatus() map[string]bool {
	s.mu.RLock()
	defer s.mu.RUnlock()

	status := make(map[string]bool)
	for chain, listener := range s.listeners {
		listener.mu.Lock()
		status[chain] = listener.enabled
		listener.mu.Unlock()
	}

	// 添加被动渠道（微信/支付宝）状态
	var wechatConfig model.SystemConfig
	if model.GetDB().Where(`"key" = ?`, model.ConfigKeyWechatEnabled).First(&wechatConfig).Error == nil {
		status["wechat"] = wechatConfig.Value == "1" || wechatConfig.Value == "true"
	} else {
		status["wechat"] = true // 默认启用
	}

	var alipayConfig model.SystemConfig
	if model.GetDB().Where(`"key" = ?`, model.ConfigKeyAlipayEnabled).First(&alipayConfig).Error == nil {
		status["alipay"] = alipayConfig.Value == "1" || alipayConfig.Value == "true"
	} else {
		status["alipay"] = true // 默认启用
	}

	return status
}

// parseTokenAmount 解析代币金额

// parseHexAmount 解析十六进制金额

// parseHexUint64 解析十六进制数字

// adjustScanInterval 动态调整扫描间隔
func (s *BlockchainService) adjustScanInterval(listener *ChainListener, transferCount int) {
	listener.mu.Lock()
	defer listener.mu.Unlock()

	// 如果发现交易，减小扫描间隔
	if transferCount > 0 {
		listener.scanInterval = listener.baseScanInterval / 2
		if listener.scanInterval < 5 {
			listener.scanInterval = 5 // 最小 5 秒
		}
	} else {
		// 没有交易，逐渐恢复到基础间隔
		listener.scanInterval = listener.baseScanInterval
	}
}

// monitorGasPrices 监控 Gas 价格
func (s *BlockchainService) monitorGasPrices() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.ctx.Done():
			return
		case <-ticker.C:
			s.updateGasPrices()
		}
	}
}

// updateGasPrices 更新 Gas 价格
func (s *BlockchainService) updateGasPrices() {
	evmChains := []string{"erc20", "bep20", "polygon", "optimism", "arbitrum", "avalanche", "base"}

	for _, chain := range evmChains {
		listener := s.listeners[chain]
		if listener == nil || !listener.enabled {
			continue
		}

		gasPrice, err := s.getGasPrice(chain, listener.rpc)
		if err != nil {
			log.Printf("[%s] Failed to get gas price: %v", chain, err)
			continue
		}

		s.gasPriceMu.Lock()
		s.gasPrices[chain] = gasPrice
		s.gasPriceMu.Unlock()

		log.Printf("[%s] Gas price updated: %.2f Gwei", chain, gasPrice)
	}
}

// getGasPrice 获取 Gas 价格
func (s *BlockchainService) getGasPrice(chain string, rpc string) (float64, error) {
	rpcClient := s.rpcClients[chain]
	if rpcClient == nil {
		return 0, fmt.Errorf("RPC client not found for %s", chain)
	}

	params := map[string]interface{}{
		"jsonrpc": "2.0",
		"method":  "eth_gasPrice",
		"params":  []interface{}{},
		"id":      1,
	}

	body, err := rpcClient.PostJSON("", params)
	if err != nil {
		return 0, err
	}

	var result struct {
		Result string `json:"result"`
	}

	if err := json.Unmarshal(body, &result); err != nil {
		return 0, err
	}

	// 解析 hex 值并转换为 Gwei
	value := new(big.Int)
	value.SetString(strings.TrimPrefix(result.Result, "0x"), 16)

	// 转换为 Gwei (1 Gwei = 10^9 Wei)
	gwei := new(big.Float).SetInt(value)
	gwei.Quo(gwei, big.NewFloat(1e9))

	gweiFloat, _ := gwei.Float64()
	return gweiFloat, nil
}

// GetGasPrice 获取 Gas 价格（公开方法）
func (s *BlockchainService) GetGasPrice(chain string) float64 {
	s.gasPriceMu.RLock()
	defer s.gasPriceMu.RUnlock()
	return s.gasPrices[chain]
}

// GetMetrics 获取监控指标
func (s *BlockchainService) GetMetrics() map[string]interface{} {
	if s.metrics == nil {
		return nil
	}
	return s.metrics.GetMetrics()
}

// GetChainMetrics 获取指定链的监控指标
func (s *BlockchainService) GetChainMetrics(chain string) map[string]interface{} {
	if s.metrics == nil {
		return nil
	}
	return s.metrics.GetChainMetrics(chain)
}

// detectReorg 检测区块重组
func (s *BlockchainService) detectReorg(listener *ChainListener, currentBlock uint64) bool {
	listener.mu.Lock()
	defer listener.mu.Unlock()

	// 如果没有历史记录，添加当前区块
	if len(listener.blockHistory) == 0 {
		listener.blockHistory = append(listener.blockHistory, currentBlock)
		return false
	}

	lastBlock := listener.blockHistory[len(listener.blockHistory)-1]

	// 如果当前区块小于或等于最后记录的区块，可能发生重组
	if currentBlock <= lastBlock {
		log.Printf("[%s] Potential reorg detected: current=%d, last=%d", 
			listener.chain, currentBlock, lastBlock)
		
		// 清空历史，重新扫描
		listener.blockHistory = []uint64{currentBlock}
		listener.lastBlock = currentBlock - uint64(listener.reorgDepth)
		if listener.lastBlock < 0 {
			listener.lastBlock = 0
		}
		return true
	}

	// 添加到历史
	listener.blockHistory = append(listener.blockHistory, currentBlock)

	// 只保留最近的 20 个区块
	if len(listener.blockHistory) > 20 {
		listener.blockHistory = listener.blockHistory[len(listener.blockHistory)-20:]
	}

	return false
}
