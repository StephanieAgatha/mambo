package journal

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"mambo/exchange"
)

const (
	positionsFile    = "positions.json"
	journalFile      = "journal.json"
	decisionLogFile  = "decision-log.json"
	executedLogFile  = "executed-log.json"
)

// OpenPosition represents an active futures position being monitored.
// Saved to positions.json so the monitor goroutine can recover after bot restart.
type OpenPosition struct {
	ID         string             `json:"id"` // unique: "pos_<unix_timestamp>"
	Pair       string             `json:"pair"`
	Direction  exchange.OrderSide `json:"direction"` // "long" / "short"
	EntryPrice float64            `json:"entry_price"`
	SizeUSD    float64            `json:"size_usd"`
	Leverage   int                `json:"leverage"`
	StopLoss   float64            `json:"stop_loss"`
	TakeProfit float64            `json:"take_profit"`
	PeakPnLPct float64            `json:"peak_pnl_pct"` // highest PnL% ever reached
	Confidence float64            `json:"confidence"`
	Strategy   string             `json:"strategy"`
	AIReason   string             `json:"ai_reason"`
	OrderID    uint64             `json:"order_id"`
	OpenedAt   string             `json:"opened_at"` // RFC3339 with timezone
	Status     string             `json:"status"`    // "open" / "filled" / "cancelled"
}

// ClosedTrade represents a completed trade written to journal.json.
type ClosedTrade struct {
	ID          string             `json:"id"`
	Pair        string             `json:"pair"`
	Direction   exchange.OrderSide `json:"direction"`
	EntryPrice  float64            `json:"entry_price"`
	ExitPrice   float64            `json:"exit_price"`
	SizeUSD     float64            `json:"size_usd"`
	Leverage    int                `json:"leverage"`
	PnLUSD      float64            `json:"pnl_usd"`
	PnLPct      float64            `json:"pnl_pct"`
	Result      string             `json:"result"`       // "WIN" / "LOSS" / "CANCELLED"
	CloseReason string             `json:"close_reason"` // "TAKE_PROFIT" / "STOP_LOSS" / "AI_CLOSE" / "HARD_RULE" / "TIMEOUT"
	Strategy    string             `json:"strategy"`
	Confidence  float64            `json:"confidence"`
	AIReason    string             `json:"ai_reason"`
	OpenedAt    string             `json:"opened_at"`
	ClosedAt    string             `json:"closed_at"`
}

// positionsStore is the JSON structure for positions.json
type positionsStore struct {
	Positions []OpenPosition `json:"positions"`
}

// journalStore is the JSON structure for journal.json
type journalStore struct {
	Trades []ClosedTrade `json:"trades"`
}

// Logger handles all persistence for open positions and trade history.
// Thread-safe via mutex — monitor goroutines may write concurrently.
type Logger struct {
	mu sync.Mutex
}

// New creates a Logger and ensures both JSON files exist.
func New() (*Logger, error) {
	l := &Logger{}

	if err := l.ensureFile(positionsFile, positionsStore{Positions: []OpenPosition{}}); err != nil {
		return nil, fmt.Errorf("journal: init positions file: %w", err)
	}

	if err := l.ensureFile(journalFile, journalStore{Trades: []ClosedTrade{}}); err != nil {
		return nil, fmt.Errorf("journal: init journal file: %w", err)
	}

	slog.Info("journal initialized",
		"positions_file", positionsFile,
		"journal_file", journalFile,
	)

	return l, nil
}

// ── Open Positions ────────────────────────────────────────────────────────────

// SavePosition adds or updates an open position in positions.json.
// Uses position ID to detect duplicates — safe to call multiple times.
func (l *Logger) SavePosition(pos OpenPosition) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadPositions()
	if err != nil {
		return fmt.Errorf("journal: save position %s: %w", pos.ID, err)
	}

	// update if exists, append if new
	found := false
	for i, p := range store.Positions {
		if p.ID == pos.ID {
			store.Positions[i] = pos
			found = true
			break
		}
	}
	if !found {
		store.Positions = append(store.Positions, pos)
	}

	if err := l.writePositions(store); err != nil {
		return fmt.Errorf("journal: write positions after save %s: %w", pos.ID, err)
	}

	slog.Debug("position saved",
		"id", pos.ID,
		"pair", pos.Pair,
		"direction", pos.Direction,
		"entry", pos.EntryPrice,
	)

	return nil
}

// RemovePosition deletes a position from positions.json by ID.
// Called when a position is closed (TP/SL/AI/hard rule).
func (l *Logger) RemovePosition(id string) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadPositions()
	if err != nil {
		return fmt.Errorf("journal: remove position %s: %w", id, err)
	}

	filtered := store.Positions[:0]
	for _, p := range store.Positions {
		if p.ID != id {
			filtered = append(filtered, p)
		}
	}
	store.Positions = filtered

	if err := l.writePositions(store); err != nil {
		return fmt.Errorf("journal: write positions after remove %s: %w", id, err)
	}

	slog.Debug("position removed", "id", id)
	return nil
}

// LoadPositions reads all open positions from positions.json.
// Called on bot startup to recover monitor goroutines.
func (l *Logger) LoadPositions() ([]OpenPosition, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadPositions()
	if err != nil {
		return nil, fmt.Errorf("journal: load positions: %w", err)
	}

	slog.Info("positions loaded from disk", "count", len(store.Positions))
	return store.Positions, nil
}

// UpdatePeakPnL updates the peak PnL percentage for a position.
// Called by the monitor goroutine when PnL exceeds the previous peak.
func (l *Logger) UpdatePeakPnL(id string, peakPct float64) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadPositions()
	if err != nil {
		return fmt.Errorf("journal: update peak PnL %s: %w", id, err)
	}

	for i, p := range store.Positions {
		if p.ID == id {
			store.Positions[i].PeakPnLPct = peakPct
			break
		}
	}

	if err := l.writePositions(store); err != nil {
		return fmt.Errorf("journal: write positions after peak update %s: %w", id, err)
	}

	return nil
}

// ── Trade Journal ─────────────────────────────────────────────────────────────

// LogTrade appends a closed trade to journal.json.
// Called after every position close regardless of outcome.
func (l *Logger) LogTrade(trade ClosedTrade) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadJournal()
	if err != nil {
		return fmt.Errorf("journal: log trade %s: %w", trade.ID, err)
	}

	store.Trades = append(store.Trades, trade)

	if err := l.writeJournal(store); err != nil {
		return fmt.Errorf("journal: write journal after log %s: %w", trade.ID, err)
	}

	slog.Info("trade logged",
		"id", trade.ID,
		"pair", trade.Pair,
		"result", trade.Result,
		"pnl_usd", trade.PnLUSD,
		"pnl_pct", trade.PnLPct,
		"close_reason", trade.CloseReason,
	)

	return nil
}

// GetTodayTrades returns all trades closed today (WIB timezone).
func (l *Logger) GetTodayTrades() ([]ClosedTrade, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadJournal()
	if err != nil {
		return nil, fmt.Errorf("journal: get today trades: %w", err)
	}

	// use WIB (UTC+7) — Indonesian timezone
	wib := time.FixedZone("WIB", 7*60*60)
	now := time.Now().In(wib)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, wib)

	var today []ClosedTrade
	for _, t := range store.Trades {
		closedAt, err := time.Parse(time.RFC3339, t.ClosedAt)
		if err != nil {
			continue
		}
		if closedAt.In(wib).After(todayStart) {
			today = append(today, t)
		}
	}

	return today, nil
}

// GetWeekTrades returns all trades closed in the last 7 days.
func (l *Logger) GetWeekTrades() ([]ClosedTrade, error) {
	l.mu.Lock()
	defer l.mu.Unlock()

	store, err := l.loadJournal()
	if err != nil {
		return nil, fmt.Errorf("journal: get week trades: %w", err)
	}

	weekAgo := time.Now().AddDate(0, 0, -7)

	var week []ClosedTrade
	for _, t := range store.Trades {
		closedAt, err := time.Parse(time.RFC3339, t.ClosedAt)
		if err != nil {
			continue
		}
		if closedAt.After(weekAgo) {
			week = append(week, t)
		}
	}

	return week, nil
}

// GetDailyPnL returns today's total realized PnL and consecutive loss count.
// Used by BotState to enforce daily limits.
func (l *Logger) GetDailyPnL() (lossUSD float64, winUSD float64, consecLosses int, err error) {
	trades, err := l.GetTodayTrades()
	if err != nil {
		return 0, 0, 0, fmt.Errorf("journal: get daily PnL: %w", err)
	}

	// calculate consecutive losses from the end of today's trades
	for i := len(trades) - 1; i >= 0; i-- {
		if trades[i].Result == "LOSS" {
			consecLosses++
		} else {
			break
		}
	}

	for _, t := range trades {
		if t.PnLUSD < 0 {
			lossUSD += -t.PnLUSD // store as positive
		} else {
			winUSD += t.PnLUSD
		}
	}

	return lossUSD, winUSD, consecLosses, nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// GeneratePositionID creates a unique position ID using the current unix timestamp.
func GeneratePositionID() string {
	return fmt.Sprintf("pos_%d", time.Now().UnixMilli())
}

// Now returns current time formatted as RFC3339 with WIB timezone.
// Use this for all OpenedAt/ClosedAt timestamps.
func Now() string {
	wib := time.FixedZone("WIB", 7*60*60)
	return time.Now().In(wib).Format(time.RFC3339)
}

// ensureFile creates the JSON file with default content if it does not exist.
func (l *Logger) ensureFile(path string, defaultContent any) error {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		data, err := json.MarshalIndent(defaultContent, "", "  ")
		if err != nil {
			return fmt.Errorf("marshal default content for %s: %w", path, err)
		}
		if err := os.WriteFile(path, data, 0644); err != nil {
			return fmt.Errorf("create %s: %w", path, err)
		}
		slog.Debug("created journal file", "path", path)
	}
	return nil
}

func (l *Logger) loadPositions() (positionsStore, error) {
	data, err := os.ReadFile(positionsFile)
	if err != nil {
		return positionsStore{}, fmt.Errorf("read %s: %w", positionsFile, err)
	}

	var store positionsStore
	if err := json.Unmarshal(data, &store); err != nil {
		return positionsStore{}, fmt.Errorf("parse %s: %w", positionsFile, err)
	}

	if store.Positions == nil {
		store.Positions = []OpenPosition{}
	}

	return store, nil
}

func (l *Logger) writePositions(store positionsStore) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal positions: %w", err)
	}
	if err := os.WriteFile(positionsFile, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", positionsFile, err)
	}
	return nil
}

func (l *Logger) loadJournal() (journalStore, error) {
	data, err := os.ReadFile(journalFile)
	if err != nil {
		return journalStore{}, fmt.Errorf("read %s: %w", journalFile, err)
	}

	var store journalStore
	if err := json.Unmarshal(data, &store); err != nil {
		return journalStore{}, fmt.Errorf("parse %s: %w", journalFile, err)
	}

	if store.Trades == nil {
		store.Trades = []ClosedTrade{}
	}

	return store, nil
}

func (l *Logger) writeJournal(store journalStore) error {
	data, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal journal: %w", err)
	}
	if err := os.WriteFile(journalFile, data, 0644); err != nil {
		return fmt.Errorf("write %s: %w", journalFile, err)
	}
	return nil
}

// ── Decision Log ──────────────────────────────────────────────────────────────

// DecisionLogEntry records every AI scoring decision during /hunt.
// Written to decision-log.json — the single source of truth for all hunt decisions.
type DecisionLogEntry struct {
	Timestamp       string  `json:"timestamp"`
	Pair            string  `json:"pair"`
	Action          string  `json:"action"` // "open_long" / "open_short" / "hold" / "wait"
	Leverage        int     `json:"leverage"`
	PositionSizeUSD float64 `json:"position_size_usd"`
	StopLoss        float64 `json:"stop_loss"`
	TakeProfit      float64 `json:"take_profit"`
	Confidence      float64 `json:"confidence"`
	Strategy        string  `json:"strategy"`
	ConfluenceCount int     `json:"confluence_count"`
	RRRatio         float64 `json:"rr_ratio"`
	Reasoning       string  `json:"reasoning"`
	Executed        bool    `json:"executed"` // true if confidence >= threshold and order placed
	OrderID         uint64  `json:"order_id,omitempty"`
	Error           string  `json:"error,omitempty"`
}

// decisionLogStore is the top-level wrapper for decision-log.json
type decisionLogStore struct {
	Entries []DecisionLogEntry `json:"entries"`
}

// CleanDecisionLog moves executed entries to executed-log.json and clears decision-log.json.
// Called on bot startup to keep the decision log small and preserve executed trades permanently.
func (l *Logger) CleanDecisionLog() error {
	l.mu.Lock()
	defer l.mu.Unlock()

	data, err := os.ReadFile(decisionLogFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("journal: read decision-log for cleanup: %w", err)
	}

	var store decisionLogStore
	if err := json.Unmarshal(data, &store); err != nil {
		slog.Warn("journal: decision-log.json corrupted during cleanup — resetting", "err", err)
		return os.WriteFile(decisionLogFile, []byte(`{"entries":[]}`), 0644)
	}

	// separate executed vs non-executed
	var executed []DecisionLogEntry
	for _, e := range store.Entries {
		if e.Executed {
			executed = append(executed, e)
		}
	}

	// append executed entries to executed-log.json
	if len(executed) > 0 {
		execStore := decisionLogStore{Entries: []DecisionLogEntry{}}
		execData, err := os.ReadFile(executedLogFile)
		if err == nil {
			json.Unmarshal(execData, &execStore)
		}
		execStore.Entries = append(execStore.Entries, executed...)

		pretty, err := json.MarshalIndent(execStore, "", "  ")
		if err != nil {
			return fmt.Errorf("journal: marshal executed-log: %w", err)
		}
		if err := os.WriteFile(executedLogFile, pretty, 0644); err != nil {
			return fmt.Errorf("journal: write executed-log: %w", err)
		}
	}

	// clear decision-log.json
	if err := os.WriteFile(decisionLogFile, []byte(`{"entries":[]}`), 0644); err != nil {
		return fmt.Errorf("journal: clear decision-log: %w", err)
	}

	slog.Info("decision-log cleaned",
		"total", len(store.Entries),
		"moved_to_executed", len(executed),
	)
	return nil
}

// AppendDecisionLog appends an entry to decision-log.json.
func (l *Logger) AppendDecisionLog(entry DecisionLogEntry) error {
	l.mu.Lock()
	defer l.mu.Unlock()

	store := decisionLogStore{Entries: []DecisionLogEntry{}}

	data, err := os.ReadFile(decisionLogFile)
	if err == nil {
		if err := json.Unmarshal(data, &store); err != nil {
			slog.Warn("journal: decision-log.json corrupted — starting fresh", "err", err)
			store.Entries = []DecisionLogEntry{}
		}
	}

	store.Entries = append(store.Entries, entry)

	pretty, err := json.MarshalIndent(store, "", "  ")
	if err != nil {
		return fmt.Errorf("journal: marshal decision log %s: %w", entry.Pair, err)
	}

	if err := os.WriteFile(decisionLogFile, pretty, 0644); err != nil {
		return fmt.Errorf("journal: write decision log %s: %w", entry.Pair, err)
	}

	return nil
}
