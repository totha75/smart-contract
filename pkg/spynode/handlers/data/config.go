package data

import (
	"fmt"
	"strings"
	"sync"

	"github.com/tokenized/smart-contract/pkg/bitcoin"
)

// Config holds all configuration for the running service.
type Config struct {
	Net            bitcoin.Network
	NodeAddress    string         // IP address of trusted external full node
	UserAgent      string         // User agent to send to external node
	StartHash      bitcoin.Hash32 // Hash of first block to start processing on initial run
	UntrustedCount int            // The number of untrusted nodes to run for double spend monitoring
	SafeTxDelay    int            // Number of milliseconds without conflict before a tx is "safe"
	ShotgunCount   int            // The number of nodes to attempt to send to when broadcasting
	Lock           sync.Mutex     // Lock for config data
}

// NewConfig returns a new Config populated from environment variables.
func NewConfig(net bitcoin.Network, host, useragent, starthash string, untrustedNodes, safeDelay,
	shotgunCount int) (Config, error) {
	result := Config{
		Net:            net,
		NodeAddress:    host,
		UserAgent:      useragent,
		UntrustedCount: untrustedNodes,
		SafeTxDelay:    safeDelay,
		ShotgunCount:   shotgunCount,
	}

	hash, err := bitcoin.NewHash32FromStr(starthash)
	if err != nil {
		return result, err
	}
	result.StartHash = *hash
	return result, nil
}

// String returns a custom string representation.
//
// This is important so we don't log sensitive config values.
func (c Config) String() string {
	pairs := map[string]string{
		"NodeAddress": c.NodeAddress,
		"UserAgent":   c.UserAgent,
		"StartHash":   c.StartHash.String(),
		"SafeTxDelay": fmt.Sprintf("%d ms", c.SafeTxDelay),
	}

	parts := []string{}

	for k, v := range pairs {
		parts = append(parts, fmt.Sprintf("%v:%v", k, v))
	}

	return fmt.Sprintf("{%v}", strings.Join(parts, " "))
}

func (c Config) Copy() Config {
	c.Lock.Lock()
	defer c.Lock.Unlock()

	result := c
	return result
}
