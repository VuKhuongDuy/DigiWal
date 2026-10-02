package chaos

import (
	"fmt"
	"net"
	"testing"
	"time"
)

// K-2: a Kafka broker blip must not take consumers (and the money API behind them) down.
func TestK2_KafkaRestartDoesNotKillServices(t *testing.T) {
	qa(t)
	t.Logf(">> docker restart %s", ctr("kafka"))
	dc(t, "restart", ctr("kafka"))
	time.Sleep(25 * time.Second)
	for _, s := range []string{"ledger-service", "sweeper", "history-service", "chain-watcher", "withdrawal-service"} {
		if st := dc(t, "inspect", "-f", "{{.State.Status}} exit={{.State.ExitCode}}", ctr(s)); st[:7] != "running" {
			t.Errorf("K-2 (HIGH): %s is %q 25s after a Kafka restart (consumer error -> log.Fatalf); no restart policy, so it stays down. For ledger-service that means balances/locks API unavailable until someone starts it.", s, st)
		}
	}
}

// S-1: a client that sends a partial request and goes quiet must be cut by the server (ReadHeaderTimeout).
func TestS1_IdleConnectionsAreClosed(t *testing.T) {
	qa(t)
	for name, port := range map[string]string{"wallet": "18080", "ledger": "18081", "chain-watcher": "18082", "history": "18083", "gateway": "18084", "sweeper": "18085", "withdrawal": "18086"} {
		c, err := net.Dial("tcp", "localhost:"+port)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		fmt.Fprint(c, "GET /healthz HTTP/1.1\r\nHost: x\r\nX-Slow: ")
		c.SetReadDeadline(time.Now().Add(15 * time.Second))
		_, rerr := c.Read(make([]byte, 1))
		if ne, ok := rerr.(net.Error); ok && ne.Timeout() {
			t.Errorf("S-1 (MEDIUM): %s kept a half-sent request open for 15s with no timeout (no ReadHeaderTimeout)", name)
		}
		c.Close()
	}
}
