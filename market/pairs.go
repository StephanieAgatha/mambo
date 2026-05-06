package market

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"time"
)

// PairsConfig represents the structure of pairs.json
type PairsConfig struct {
	Pairs []string `json:"pairs"`
}

// LoadPairs loads the pairs from pairs.json file
func LoadPairs() ([]string, error) {
	// Try to read from current directory first
	data, err := os.ReadFile("pairs.json")
	if err != nil {
		// If not found, try to read from parent directory (for tests)
		data, err = os.ReadFile("../pairs.json")
		if err != nil {
			return nil, fmt.Errorf("failed to read pairs.json: %w", err)
		}
	}

	var config PairsConfig
	if err := json.Unmarshal(data, &config); err != nil {
		return nil, fmt.Errorf("failed to parse pairs.json: %w", err)
	}

	if len(config.Pairs) == 0 {
		return nil, fmt.Errorf("no pairs found in pairs.json")
	}

	return config.Pairs, nil
}

// GetRandomPair returns a random pair from pairs.json
func GetRandomPair() (string, error) {
	pairs, err := LoadPairs()
	if err != nil {
		return "", err
	}

	// Seed random generator
	rand.Seed(time.Now().UnixNano())

	// Select random pair
	randomIndex := rand.Intn(len(pairs))
	return pairs[randomIndex], nil
}

// GetRandomPairs returns n random pairs from pairs.json (without duplicates)
func GetRandomPairs(n int) ([]string, error) {
	pairs, err := LoadPairs()
	if err != nil {
		return nil, err
	}

	if n > len(pairs) {
		n = len(pairs)
	}

	// Seed random generator
	rand.Seed(time.Now().UnixNano())

	// Shuffle pairs
	shuffled := make([]string, len(pairs))
	copy(shuffled, pairs)
	rand.Shuffle(len(shuffled), func(i, j int) {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	})

	return shuffled[:n], nil
}
