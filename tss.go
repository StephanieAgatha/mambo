package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
)

// --- Struktur Hyperliquid ---
type HlMeta struct {
	Universe []HlUniverseAsset `json:"universe"`
}

type HlUniverseAsset struct {
	Name       string `json:"name"`
	IsDelisted bool   `json:"isDelisted"`
}

// --- Struktur Binance ---
type BinanceSymbol struct {
	Symbol       string `json:"symbol"`
	Status       string `json:"status"`
	ContractType string `json:"contractType"`
	QuoteAsset   string `json:"quoteAsset"`
}

type BinanceExchangeInfo struct {
	Symbols []BinanceSymbol `json:"symbols"`
}

// --- Struktur Output JSON ---
type Output struct {
	Pairs           []string `json:"pairs"`
	Timeframe       string   `json:"timeframe"`
	ScanIntervalMin int      `json:"scan_interval_min"`
}

func main() {
	// 1. Ambil pasangan Hyperliquid
	hlPairs := fetchHyperliquidPairs()
	fmt.Fprintf(os.Stderr, "✔ Hyperliquid: %d pasangan\n", len(hlPairs))

	// 2. Ambil pasangan Binance Futures (USDT perpetual)
	binancePairs := fetchBinancePairs()
	fmt.Fprintf(os.Stderr, "✔ Binance: %d pasangan\n", len(binancePairs))

	// 3. Cari token yang ada di keduanya
	commonTokens := findCommonTokens(hlPairs, binancePairs)
	fmt.Fprintf(os.Stderr, "✔ Cocok: %d token\n", len(commonTokens))

	// 4. Siapkan output & simpan ke file
	output := Output{
		Pairs:           commonTokens,
		Timeframe:       "4h",
		ScanIntervalMin: 15,
	}
	err := saveToJSONFile(output, "common_pairs.json")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Gagal menyimpan file: %v\n", err)
		os.Exit(1)
	}
	fmt.Fprintln(os.Stderr, "✔ Hasil disimpan di common_pairs.json")
}

func fetchHyperliquidPairs() []string {
	payload := `{"type":"meta"}`
	resp, err := http.Post("https://api.hyperliquid.xyz/info", "application/json", strings.NewReader(payload))
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	var meta HlMeta
	if err := json.NewDecoder(resp.Body).Decode(&meta); err != nil {
		panic(err)
	}

	pairs := make([]string, 0, len(meta.Universe))
	for _, a := range meta.Universe {
		if !a.IsDelisted {
			pairs = append(pairs, a.Name)
		}
	}
	return pairs
}

func fetchBinancePairs() []string {
	resp, err := http.Get("https://fapi.binance.com/fapi/v1/exchangeInfo")
	if err != nil {
		panic(err)
	}
	defer resp.Body.Close()

	var info BinanceExchangeInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		panic(err)
	}

	pairs := make([]string, 0, len(info.Symbols))
	for _, s := range info.Symbols {
		if s.Status == "TRADING" && s.ContractType == "PERPETUAL" && s.QuoteAsset == "USDT" {
			pairs = append(pairs, s.Symbol)
		}
	}
	return pairs
}

func findCommonTokens(hlPairs, binancePairs []string) []string {
	binanceTokenSet := make(map[string]bool)
	for _, sym := range binancePairs {
		if strings.HasSuffix(sym, "USDT") {
			token := strings.TrimSuffix(sym, "USDT")
			binanceTokenSet[token] = true
		}
	}

	hlTokenSet := make(map[string]bool)
	for _, hl := range hlPairs {
		token := hl
		token = strings.TrimSuffix(token, "USDT")
		token = strings.TrimSuffix(token, "USD")
		hlTokenSet[token] = true
	}

	commonSet := make(map[string]bool)
	for token := range hlTokenSet {
		if binanceTokenSet[token] {
			commonSet[token] = true
		}
	}

	common := make([]string, 0, len(commonSet))
	for t := range commonSet {
		common = append(common, t)
	}
	sort.Strings(common)
	return common
}

func saveToJSONFile(data interface{}, filename string) error {
	file, err := os.Create(filename)
	if err != nil {
		return err
	}
	defer file.Close()

	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	return encoder.Encode(data)
}
