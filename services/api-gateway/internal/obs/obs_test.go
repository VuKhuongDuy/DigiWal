package obs_test

import (
	"context"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"digiwal/pkg/metrics"
	"digiwal/services/api-gateway/internal/obs"
	"digiwal/services/api-gateway/internal/store"
	"digiwal/services/api-gateway/internal/testdb"

	"github.com/prometheus/client_golang/prometheus"
)

func TestMain(m *testing.M) {
	code := m.Run()
	testdb.Terminate()
	os.Exit(code)
}

func scrape(c prometheus.Collector) string {
	r := prometheus.NewRegistry()
	r.MustRegister(c)
	rec := httptest.NewRecorder()
	metrics.Handler(r).ServeHTTP(rec, httptest.NewRequest("GET", "/metrics", nil))
	return rec.Body.String()
}

func TestUsersTotalCountsUsersAndOnlyGrows(t *testing.T) {
	ctx := context.Background()
	st, err := store.Connect(ctx, testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if out := scrape(obs.Collector(st)); !strings.Contains(out, "digiwal_users_total 0") || !strings.Contains(out, "# TYPE digiwal_users_total counter") {
		t.Fatalf("no users yet:\n%s", out)
	}
	for i := 0; i < 3; i++ {
		if _, _, err := st.CreateUser(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if out := scrape(obs.Collector(st)); !strings.Contains(out, "digiwal_users_total 3") || !strings.Contains(out, `digiwal_collector_up{collector="api-gateway"} 1`) {
		t.Fatalf("three users:\n%s", out)
	}
}

// A database that cannot be read is a gap, not a zero (design invariant 4).
func TestUnreadableDatabaseIsAGap(t *testing.T) {
	st, err := store.Connect(context.Background(), testdb.New(t))
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	out := scrape(obs.Collector(st))
	if strings.Contains(out, "digiwal_users_total") || !strings.Contains(out, `digiwal_collector_up{collector="api-gateway"} 0`) {
		t.Fatalf("\n%s", out)
	}
}
