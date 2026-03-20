package swap

import (
	"encoding/json"
	"fmt"
	"log"

	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ─── Config ──────────────────────────────────────────────────────────────────

// Config holds the global swap engine configuration.
type Config struct {
	Enabled       bool              `json:"enabled"`
	Exchange      string            `json:"exchange"`       // "tradeogre" (only supported exchange for now)
	APIKey        string            `json:"api_key"`
	APISecret     string            `json:"api_secret"`
	CheckIntervalMin int            `json:"check_interval_min"` // how often to check balances (default 15)
	StateFile     string            `json:"state_file"`     // path to persist swap state
	Rules         map[string]Rule   `json:"rules"`          // coin symbol → swap rule
	DepositAddrs  map[string]string `json:"deposit_addrs"`  // coin → exchange deposit address
	WithdrawAddrs map[string]string `json:"withdraw_addrs"` // coin → pool wallet for withdrawal
}

// Rule defines auto-swap behavior for a single source coin.
type Rule struct {
	Enabled     bool    `json:"enabled"`
	Destination string  `json:"destination"`    // target coin symbol (e.g. "LTC", "BTC")
	MinBalance  float64 `json:"min_balance"`    // minimum balance before triggering swap
	KeepBalance float64 `json:"keep_balance"`   // keep this much in source wallet (don't swap everything)
	MaxSlippage float64 `json:"max_slippage_pct"` // max acceptable slippage % (default 2.0)
}

// DefaultConfig returns sensible defaults.
func DefaultConfig() Config {
	return Config{
		Enabled:          false,
		Exchange:         "tradeogre",
		CheckIntervalMin: 15,
		StateFile:        "/opt/pipool/swap_state.json",
		Rules:            make(map[string]Rule),
		DepositAddrs:     make(map[string]string),
	}
}

// ─── Swap state machine ─────────────────────────────────────────────────────

// SwapState tracks the progress of a single swap operation.
type SwapState string

const (
	StateIdle          SwapState = "idle"
	StateSending       SwapState = "sending"       // sending from local wallet to exchange
	StateWaitDeposit   SwapState = "wait_deposit"   // waiting for exchange to credit
	StateSelling       SwapState = "selling"        // sell order placed on exchange
	StateBuying        SwapState = "buying"         // buy order placed (if dest != BTC)
	StateWithdrawing   SwapState = "withdrawing"    // withdrawing from exchange to dest wallet
	StateComplete      SwapState = "complete"
	StateFailed        SwapState = "failed"
)

// SwapJob represents one in-progress swap operation.
type SwapJob struct {
	ID            string    `json:"id"`
	SourceCoin    string    `json:"source_coin"`
	DestCoin      string    `json:"dest_coin"`
	Amount        float64   `json:"amount"`         // amount of source coin being swapped
	State         SwapState `json:"state"`
	SellOrderUUID string    `json:"sell_order_uuid,omitempty"`
	BuyOrderUUID  string    `json:"buy_order_uuid,omitempty"`
	SellPrice     string    `json:"sell_price,omitempty"`
	BuyPrice      string    `json:"buy_price,omitempty"`
	BTCProceeds   float64   `json:"btc_proceeds,omitempty"`
	DestReceived  float64   `json:"dest_received,omitempty"`
	TxID          string    `json:"txid,omitempty"` // local wallet send txid
	Error         string    `json:"error,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	RetryCount    int       `json:"retry_count"`
}

// ─── Interfaces (import-cycle-free) ─────────────────────────────────────────

// WalletRPC abstracts the coin daemon RPC calls needed by the swap engine.
type WalletRPC interface {
	Symbol() string
	GetBalance() (confirmed, immature float64, err error)
	SendToAddress(addr string, amount float64) (txid string, err error)
}

// Alerter sends notifications about swap events.
type Alerter interface {
	SwapAlert(severity, title, detail string)
}

// ─── Engine ──────────────────────────────────────────────────────────────────

// Engine monitors wallet balances and executes auto-swaps via exchange.
type Engine struct {
	cfg     Config
	exchange Exchange
	wallets map[string]WalletRPC // coin symbol → wallet RPC
	alerter Alerter

	mu       sync.Mutex
	jobs     []*SwapJob        // active + recent completed jobs
	stopCh   chan struct{}
	history  []SwapJob         // last 50 completed swaps
}

// New creates a new swap engine.
func New(cfg Config, wallets map[string]WalletRPC, alerter Alerter) *Engine {
	e := &Engine{
		cfg:     cfg,
		wallets: wallets,
		alerter: alerter,
		stopCh:  make(chan struct{}),
	}

	if cfg.APIKey != "" && cfg.APISecret != "" {
		switch strings.ToLower(cfg.Exchange) {
		case "xeggex":
			e.exchange = NewXegClient(cfg.APIKey, cfg.APISecret)
		default: // "tradeogre" or empty
			e.exchange = NewTOExchange(cfg.APIKey, cfg.APISecret)
		}
	}

	// Load persisted state
	e.loadState()

	return e
}

// Start begins the swap monitoring loop.
func (e *Engine) Start() {
	if !e.cfg.Enabled {
		log.Println("[swap] disabled — set swap.enabled=true and configure API keys to enable")
		return
	}
	if e.exchange == nil {
		log.Println("[swap] no exchange API keys configured — auto-swap disabled")
		return
	}

	interval := time.Duration(e.cfg.CheckIntervalMin) * time.Minute
	if interval < time.Minute {
		interval = 15 * time.Minute
	}

	log.Printf("[swap] engine started — checking balances every %v", interval)

	go func() {
		// Check immediately on start
		e.tick()

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		for {
			select {
			case <-e.stopCh:
				log.Println("[swap] engine stopped")
				return
			case <-ticker.C:
				e.tick()
			}
		}
	}()
}

// Stop halts the swap engine.
func (e *Engine) Stop() {
	select {
	case <-e.stopCh:
	default:
		close(e.stopCh)
	}
}

// tick runs one cycle: check balances, advance in-flight jobs, trigger new swaps.
func (e *Engine) tick() {
	e.mu.Lock()
	defer e.mu.Unlock()

	// 1. Advance any in-flight jobs
	for _, job := range e.jobs {
		if job.State == StateComplete || job.State == StateFailed {
			continue
		}
		e.advanceJob(job)
	}

	// 2. Clean up completed/failed jobs (move to history)
	e.cleanupJobs()

	// 3. Check for new swaps to initiate
	for coin, rule := range e.cfg.Rules {
		if !rule.Enabled {
			continue
		}
		// Don't start a new swap if there's already one in flight for this coin
		if e.hasActiveJob(coin) {
			continue
		}
		e.checkAndInitiate(coin, rule)
	}

	// 4. Persist state
	e.saveState()
}

// checkAndInitiate checks wallet balance and starts a swap if threshold is met.
func (e *Engine) checkAndInitiate(coin string, rule Rule) {
	wallet, ok := e.wallets[coin]
	if !ok {
		return
	}

	confirmed, _, err := wallet.GetBalance()
	if err != nil {
		log.Printf("[swap] %s: balance check error: %v", coin, err)
		return
	}

	swapAmount := confirmed - rule.KeepBalance
	if swapAmount < rule.MinBalance {
		return // not enough to swap
	}

	// Check we have a deposit address for this coin
	depositAddr, ok := e.cfg.DepositAddrs[coin]
	if !ok || depositAddr == "" {
		log.Printf("[swap] %s: no deposit address configured — skipping", coin)
		return
	}

	// Verify market exists
	market := e.marketName(coin, rule.Destination)
	if market == "" {
		log.Printf("[swap] %s→%s: no valid market path — skipping", coin, rule.Destination)
		return
	}

	log.Printf("[swap] %s: initiating swap of %.8f %s → %s", coin, swapAmount, coin, rule.Destination)

	job := &SwapJob{
		ID:         fmt.Sprintf("%s-%d", coin, time.Now().UnixMilli()),
		SourceCoin: coin,
		DestCoin:   rule.Destination,
		Amount:     swapAmount,
		State:      StateSending,
		CreatedAt:  time.Now(),
		UpdatedAt:  time.Now(),
	}

	// Send from local wallet to exchange deposit address
	txid, err := wallet.SendToAddress(depositAddr, swapAmount)
	if err != nil {
		job.State = StateFailed
		job.Error = fmt.Sprintf("send to exchange failed: %v", err)
		log.Printf("[swap] %s: %s", coin, job.Error)
		e.alert("error", fmt.Sprintf("Auto-Swap %s Failed", coin), job.Error)
		e.jobs = append(e.jobs, job)
		return
	}

	job.TxID = txid
	job.State = StateWaitDeposit
	job.UpdatedAt = time.Now()
	e.jobs = append(e.jobs, job)

	log.Printf("[swap] %s: sent %.8f to exchange, txid=%s — waiting for deposit", coin, swapAmount, txid)
	e.alert("info", fmt.Sprintf("Auto-Swap %s Started", coin),
		fmt.Sprintf("Sent %.8f %s to TradeOgre\nTXID: `%s`\nDestination: %s", swapAmount, coin, txid, rule.Destination))
}

// advanceJob moves an in-flight swap job through its state machine.
func (e *Engine) advanceJob(job *SwapJob) {
	switch job.State {
	case StateWaitDeposit:
		e.advanceWaitDeposit(job)
	case StateSelling:
		e.advanceSelling(job)
	case StateBuying:
		e.advanceBuying(job)
	case StateWithdrawing:
		e.advanceWithdrawing(job)
	}
}

func (e *Engine) advanceWaitDeposit(job *SwapJob) {
	// Check if the deposit has been credited on the exchange
	available, err := e.exchange.GetBalance(job.SourceCoin)
	if err != nil {
		log.Printf("[swap] %s: exchange balance check error: %v", job.ID, err)
		return
	}

	if available < job.Amount*0.95 { // allow 5% for tx fees
		// Not credited yet — check age, fail if too old
		if time.Since(job.CreatedAt) > 6*time.Hour {
			job.State = StateFailed
			job.Error = "deposit not credited after 6 hours"
			job.UpdatedAt = time.Now()
			e.alert("error", fmt.Sprintf("Auto-Swap %s Failed", job.SourceCoin), job.Error)
		}
		return
	}

	// Deposit credited — place sell order
	log.Printf("[swap] %s: deposit credited (%.8f available), placing sell order", job.ID, available)
	e.placeSellOrder(job, available)
}

func (e *Engine) placeSellOrder(job *SwapJob, available float64) {
	// If source IS BTC and dest is something else, skip selling — go straight to buying
	if strings.ToUpper(job.SourceCoin) == "BTC" {
		job.BTCProceeds = available
		job.State = StateBuying
		job.UpdatedAt = time.Now()
		e.placeBuyOrder(job)
		return
	}

	market := e.exchange.MarketName(job.SourceCoin, "BTC")
	bid, _, err := e.exchange.GetTicker(market)
	if err != nil {
		job.State = StateFailed
		job.Error = fmt.Sprintf("ticker error for %s: %v", market, err)
		job.UpdatedAt = time.Now()
		e.alert("error", fmt.Sprintf("Auto-Swap %s Failed", job.SourceCoin), job.Error)
		return
	}

	if bid <= 0 {
		job.State = StateFailed
		job.Error = fmt.Sprintf("no bids on %s", market)
		job.UpdatedAt = time.Now()
		return
	}

	// Apply slight discount to bid to fill faster (0.5% below bid)
	sellPrice := bid * 0.995
	sellPriceStr := strconv.FormatFloat(sellPrice, 'f', 8, 64)
	qtyStr := strconv.FormatFloat(available, 'f', 8, 64)

	orderID, err := e.exchange.SubmitSell(market, sellPriceStr, qtyStr)
	if err != nil {
		job.State = StateFailed
		job.Error = fmt.Sprintf("sell order failed: %v", err)
		job.UpdatedAt = time.Now()
		e.alert("error", fmt.Sprintf("Auto-Swap %s Failed", job.SourceCoin), job.Error)
		return
	}

	job.SellOrderUUID = orderID
	job.SellPrice = sellPriceStr
	job.State = StateSelling
	job.UpdatedAt = time.Now()

	log.Printf("[swap] %s: sell order placed on %s — qty=%.8f price=%s uuid=%s",
		job.ID, market, available, sellPriceStr, orderID)
}

func (e *Engine) advanceSelling(job *SwapJob) {
	// Check if sell order has been filled
	filled, err := e.exchange.GetOrder(job.SellOrderUUID)
	if err != nil {
		log.Printf("[swap] %s: sell order check error: %v", job.ID, err)
		return
	}

	if filled {
		log.Printf("[swap] %s: sell order filled", job.ID)
		// Check BTC balance to see proceeds
		btcAvailable, err := e.exchange.GetBalance("BTC")
		if err != nil {
			log.Printf("[swap] %s: BTC balance check error: %v", job.ID, err)
			return
		}
		job.BTCProceeds = btcAvailable
		job.UpdatedAt = time.Now()

		if strings.ToUpper(job.DestCoin) == "BTC" {
			job.State = StateWithdrawing
			e.initiateWithdraw(job)
		} else {
			job.State = StateBuying
			e.placeBuyOrder(job)
		}
		return
	}

	// Order still pending — check if it's been too long
	if time.Since(job.UpdatedAt) > 2*time.Hour {
		log.Printf("[swap] %s: sell order stale for 2h, cancelling", job.ID)
		_ = e.exchange.CancelOrder(job.SellOrderUUID)
		job.State = StateFailed
		job.Error = "sell order not filled after 2 hours — cancelled"
		job.UpdatedAt = time.Now()
		e.alert("warn", fmt.Sprintf("Auto-Swap %s Cancelled", job.SourceCoin),
			fmt.Sprintf("Sell order was not filled after 2 hours. Funds remain on %s.", e.exchange.Name()))
	}
}

func (e *Engine) placeBuyOrder(job *SwapJob) {
	if strings.ToUpper(job.DestCoin) == "BTC" {
		// Already have BTC, just withdraw
		job.State = StateWithdrawing
		e.initiateWithdraw(job)
		return
	}

	market := e.exchange.MarketName(job.DestCoin, "BTC")
	_, ask, err := e.exchange.GetTicker(market)
	if err != nil {
		job.State = StateFailed
		job.Error = fmt.Sprintf("ticker error for %s: %v", market, err)
		job.UpdatedAt = time.Now()
		e.alert("error", fmt.Sprintf("Auto-Swap %s Failed", job.SourceCoin), job.Error)
		return
	}

	if ask <= 0 {
		job.State = StateFailed
		job.Error = fmt.Sprintf("no asks on %s", market)
		job.UpdatedAt = time.Now()
		return
	}

	buyPrice := ask * 1.005 // 0.5% above ask to fill instantly
	buyPriceStr := strconv.FormatFloat(buyPrice, 'f', 8, 64)

	// Calculate quantity: BTC proceeds / buy price = quantity of dest coin
	qty := job.BTCProceeds / buyPrice
	qty = qty * 0.998 // account for exchange fee
	qtyStr := strconv.FormatFloat(qty, 'f', 8, 64)

	orderID, err := e.exchange.SubmitBuy(market, buyPriceStr, qtyStr)
	if err != nil {
		job.State = StateFailed
		job.Error = fmt.Sprintf("buy order failed: %v", err)
		job.UpdatedAt = time.Now()
		e.alert("error", fmt.Sprintf("Auto-Swap %s→%s Failed", job.SourceCoin, job.DestCoin), job.Error)
		return
	}

	job.BuyOrderUUID = orderID
	job.BuyPrice = buyPriceStr
	job.State = StateBuying
	job.UpdatedAt = time.Now()

	log.Printf("[swap] %s: buy order placed on %s — qty=%s price=%s uuid=%s",
		job.ID, market, qtyStr, buyPriceStr, orderID)
}

func (e *Engine) advanceBuying(job *SwapJob) {
	if job.BuyOrderUUID == "" {
		e.placeBuyOrder(job)
		return
	}

	filled, err := e.exchange.GetOrder(job.BuyOrderUUID)
	if err != nil {
		log.Printf("[swap] %s: buy order check error: %v", job.ID, err)
		return
	}

	if filled {
		log.Printf("[swap] %s: buy order filled", job.ID)
		destAvail, err := e.exchange.GetBalance(job.DestCoin)
		if err == nil {
			job.DestReceived = destAvail
		}
		job.State = StateWithdrawing
		job.UpdatedAt = time.Now()
		e.initiateWithdraw(job)
		return
	}

	if time.Since(job.UpdatedAt) > 2*time.Hour {
		log.Printf("[swap] %s: buy order stale for 2h, cancelling", job.ID)
		_ = e.exchange.CancelOrder(job.BuyOrderUUID)
		job.State = StateFailed
		job.Error = "buy order not filled after 2 hours — cancelled"
		job.UpdatedAt = time.Now()
		e.alert("warn", fmt.Sprintf("Auto-Swap %s→%s Cancelled", job.SourceCoin, job.DestCoin),
			fmt.Sprintf("Buy order was not filled. BTC remains on %s.", e.exchange.Name()))
	}
}

func (e *Engine) initiateWithdraw(job *SwapJob) {
	var destAmount float64
	if strings.ToUpper(job.DestCoin) == "BTC" {
		destAmount = job.BTCProceeds
	} else {
		destAmount = job.DestReceived
	}

	// Get the destination wallet address from config
	destAddr := e.cfg.WithdrawAddrs[job.DestCoin]

	// Try automatic withdrawal via exchange API
	if destAddr != "" {
		txid, err := e.exchange.Withdraw(job.DestCoin, destAmount, destAddr)
		if err == nil {
			job.State = StateComplete
			job.UpdatedAt = time.Now()
			log.Printf("[swap] %s: withdrawal initiated — %.8f %s → %s (txid=%s)",
				job.ID, destAmount, job.DestCoin, destAddr, txid)
			e.alert("info", fmt.Sprintf("Auto-Swap %s→%s Complete", job.SourceCoin, job.DestCoin),
				fmt.Sprintf("Swapped %.8f %s → %.8f %s via %s\nWithdrawal sent to `%s`\nTXID: `%s`",
					job.Amount, job.SourceCoin, destAmount, job.DestCoin, e.exchange.Name(), destAddr, txid))
			return
		}
		// If withdrawal API failed (e.g. TradeOgre doesn't support it), fall through
		log.Printf("[swap] %s: auto-withdrawal not available: %v — manual withdrawal needed", job.ID, err)
	}

	// Fall back to manual withdrawal notification
	job.State = StateComplete
	job.UpdatedAt = time.Now()

	msg := fmt.Sprintf("Swapped %.8f %s → %.8f %s via %s\n"+
		"⚠️ **Withdraw manually** from %s to your %s wallet",
		job.Amount, job.SourceCoin, destAmount, job.DestCoin, e.exchange.Name(), e.exchange.Name(), job.DestCoin)

	log.Printf("[swap] %s: swap complete — %.8f %s → %.8f %s (manual withdrawal needed)",
		job.ID, job.Amount, job.SourceCoin, destAmount, job.DestCoin)

	e.alert("info", fmt.Sprintf("Auto-Swap %s→%s Complete", job.SourceCoin, job.DestCoin), msg)
}

func (e *Engine) advanceWithdrawing(job *SwapJob) {
	// Withdrawal was already initiated in initiateWithdraw.
	// If we're still in this state, mark complete.
	e.initiateWithdraw(job)
}

// ─── Helpers ─────────────────────────────────────────────────────────────────

// marketName returns the exchange-specific market string for trading source→dest.
// All markets are BTC-based (source→BTC or BTC→dest).
func (e *Engine) marketName(source, dest string) string {
	src := strings.ToUpper(source)
	dst := strings.ToUpper(dest)

	if src == "BTC" || dst == "BTC" {
		other := src
		if src == "BTC" {
			other = dst
		}
		return e.exchange.MarketName(other, "BTC")
	}

	// Need two hops: source → BTC, then BTC → dest
	return e.exchange.MarketName(src, "BTC") // first hop
}

func (e *Engine) hasActiveJob(coin string) bool {
	for _, j := range e.jobs {
		if j.SourceCoin == coin && j.State != StateComplete && j.State != StateFailed {
			return true
		}
	}
	return false
}

func (e *Engine) cleanupJobs() {
	var active []*SwapJob
	for _, j := range e.jobs {
		if j.State == StateComplete || j.State == StateFailed {
			e.history = append(e.history, *j)
			// Keep history bounded
			if len(e.history) > 50 {
				e.history = e.history[len(e.history)-50:]
			}
		} else {
			active = append(active, j)
		}
	}
	e.jobs = active
}

func (e *Engine) alert(severity, title, detail string) {
	if e.alerter != nil {
		e.alerter.SwapAlert(severity, title, detail)
	}
}

// ─── State persistence ──────────────────────────────────────────────────────

type persistedState struct {
	Jobs    []*SwapJob `json:"jobs"`
	History []SwapJob  `json:"history"`
}

func (e *Engine) saveState() {
	if e.cfg.StateFile == "" {
		return
	}
	state := persistedState{Jobs: e.jobs, History: e.history}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		log.Printf("[swap] state save error: %v", err)
		return
	}
	dir := filepath.Dir(e.cfg.StateFile)
	os.MkdirAll(dir, 0755)
	if err := os.WriteFile(e.cfg.StateFile, b, 0600); err != nil {
		log.Printf("[swap] state write error: %v", err)
	}
}

func (e *Engine) loadState() {
	if e.cfg.StateFile == "" {
		return
	}
	data, err := os.ReadFile(e.cfg.StateFile)
	if err != nil {
		return // file doesn't exist yet, that's fine
	}
	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		log.Printf("[swap] state load error: %v", err)
		return
	}
	e.jobs = state.Jobs
	e.history = state.History
	log.Printf("[swap] loaded state: %d active jobs, %d history entries", len(e.jobs), len(e.history))
}

// ─── Dashboard / API helpers ────────────────────────────────────────────────

// Status returns the current swap engine status for the dashboard.
func (e *Engine) Status() EngineStatus {
	e.mu.Lock()
	defer e.mu.Unlock()

	s := EngineStatus{
		Enabled:    e.cfg.Enabled,
		Exchange:   e.cfg.Exchange,
		HasAPIKeys: e.exchange != nil,
		Rules:      make(map[string]RuleStatus),
	}

	for coin, rule := range e.cfg.Rules {
		rs := RuleStatus{
			Enabled:     rule.Enabled,
			Destination: rule.Destination,
			MinBalance:  rule.MinBalance,
			KeepBalance: rule.KeepBalance,
		}
		// Find active job for this coin
		for _, j := range e.jobs {
			if j.SourceCoin == coin {
				rs.ActiveJob = j
				break
			}
		}
		s.Rules[coin] = rs
	}

	// Recent history
	start := len(e.history) - 10
	if start < 0 {
		start = 0
	}
	s.RecentSwaps = e.history[start:]

	return s
}

// EngineStatus is the dashboard-visible swap engine state.
type EngineStatus struct {
	Enabled     bool                  `json:"enabled"`
	Exchange    string                `json:"exchange"`
	HasAPIKeys  bool                  `json:"has_api_keys"`
	Rules       map[string]RuleStatus `json:"rules"`
	RecentSwaps []SwapJob             `json:"recent_swaps"`
}

// RuleStatus is the dashboard-visible state for one coin's swap rule.
type RuleStatus struct {
	Enabled     bool     `json:"enabled"`
	Destination string   `json:"destination"`
	MinBalance  float64  `json:"min_balance"`
	KeepBalance float64  `json:"keep_balance"`
	ActiveJob   *SwapJob `json:"active_job,omitempty"`
}

// UpdateRule updates or creates a swap rule for a coin. Called from dashboard API.
func (e *Engine) UpdateRule(coin string, rule Rule) {
	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cfg.Rules == nil {
		e.cfg.Rules = make(map[string]Rule)
	}
	e.cfg.Rules[coin] = rule
	e.saveState()

	log.Printf("[swap] rule updated: %s → %s (enabled=%v, min=%.4f, keep=%.4f)",
		coin, rule.Destination, rule.Enabled, rule.MinBalance, rule.KeepBalance)
}

// GetConfig returns the current swap config (for dashboard display).
func (e *Engine) GetConfig() Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.cfg
}

