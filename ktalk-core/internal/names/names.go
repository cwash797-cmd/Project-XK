// Package names generates realistic Russian first names for room participants.
// Each invocation returns a different name from an embedded dictionary.
// No suffixes, numbers, or other modifications are added — the result
// should look like a real person's display name.
package names

import (
	"crypto/rand"
	_ "embed"
	"math/big"
	"strings"
)

//go:embed data/firstnames
var embeddedFirstnames string

var firstNames []string //nolint:gochecknoglobals

func init() {
	firstNames = parseLines(embeddedFirstnames)
}

func parseLines(raw string) []string {
	var out []string
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			out = append(out, line)
		}
	}
	return out
}

// Generate returns a random Russian first name from the embedded dictionary.
// Each call picks independently — consecutive calls may return the same name.
func Generate() string {
	if len(firstNames) == 0 {
		return "Участник"
	}
	idx := randomIndex(len(firstNames))
	return firstNames[idx]
}

func randomIndex(limit int) int {
	if limit <= 1 {
		return 0
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	if err != nil {
		return 0
	}
	return int(n.Int64())
}
