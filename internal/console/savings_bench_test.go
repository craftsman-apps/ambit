package console

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
)

// T11: the query-cost check. It seeds 30 days of gateway records for 1,000 people through the real insert path (the
// materialized views fill while it inserts), then runs the Usage query (which scans otel_logs) and the summary
// queries. The numbers come from system.query_log: test/stepsavings.sh prints them after this test, because the test
// user may not read it. Skipped unless AMBIT_SAVINGS_BENCH=1 (it takes minutes and a few GB of disk).

func envInt(name string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(name)); err == nil && v > 0 {
		return v
	}
	return def
}

// seedSQL writes n records of one day: 55% LLM (a third of them through the router "auto"), 40% MCP tools/call, 5%
// guard compression records. Each has the attributes of the gateway's access log (about 25 keys with values that do
// not repeat), so the map column is as heavy as in production. The rows offset..offset+n of total spread over the day.
// groups: how many groups exist; a person is in one or two of them.
func seedSQL(day string, offset, n, total, groups int) string {
	return fmt.Sprintf(`INSERT INTO otel_logs (Timestamp, LogAttributes)
WITH number + %[3]d AS i, i %% 1000 AS u, intDiv(i, 1000) AS k,
     if(u %% 3 = 0, concat('["g', toString(u %% %[4]d), '","g', toString((u * 7 + 1) %% %[4]d), '"]'), concat('["g', toString(u %% %[4]d), '"]')) AS groups,
     arrayElement(['gemma','claude-sonnet','gemini-pro','gpt-small','llama','qwen','mistral','claude-haiku'], 1 + (i %% 8)) AS model,
     cityHash64(i) AS h
SELECT toDateTime64('%[1]s 00:00:00', 9, 'UTC') + toIntervalMillisecond(intDiv((number + %[3]d) * 86400000, %[5]d)),
  map(
    'jwt.email', concat('user', toString(u), '@acme.test'), 'jwt.sub', concat('sub-', toString(u)), 'jwt.groups', groups,
    'ambit.identity', 'jwt', 'ambit.accessMode', 'enforce', 'ambit.rulesVersion', '3f9a1c2b7d10',
    'http.method', 'POST', 'http.path', if(i %% 20 < 11, '/v1/chat/completions', '/mcp'), 'http.status', '200',
    'http.response.status_code', '200', 'duration', toString(h %% 5000), 'request.id', hex(h), 'trace.id', hex(cityHash64(h)),
    'src.addr', concat('10.1.', toString(u %% 250), '.', toString(k %% 250)), 'dst.addr', '10.96.12.7:8080',
    'user_agent', concat('opencode/1.', toString(u %% 40)), 'route', if(i %% 20 < 11, 'llm', 'mcp'),
    'protocol', if(i %% 20 < 11, 'llm', if(i %% 20 < 19, 'mcp', 'guard')),
    'gen_ai.request.model', if(i %% 20 < 11, model, ''), 'gen_ai.system', if(i %% 20 < 11, 'openai', ''),
    'gen_ai.usage.input_tokens', if(i %% 20 < 11, toString(500 + h %% 30000), ''), 'gen_ai.usage.output_tokens', if(i %% 20 < 11, toString(50 + h %% 2000), ''),
    'ambit.router', if(i %% 20 < 11 AND i %% 3 = 0, 'auto', if(i %% 20 < 11, model, '')),
    'mcp.method.name', if(i %% 20 >= 11 AND i %% 20 < 19, 'tools/call', ''), 'mcp.target', arrayElement(['github','crm','jira','kubectl','search'], 1 + (i %% 5)),
    'gen_ai.tool.name', if(i %% 20 >= 11 AND i %% 20 < 19, concat('tool_', toString(i %% 30)), ''),
    'ambit.kind', if(i %% 20 = 19, 'compression', ''), 'ambit.compress.type', arrayElement(['json','table','yaml','logs','test'], 1 + (i %% 5)),
    'ambit.compress.mode', 'lossless', 'ambit.compress.bytes_in', toString(8192 + h %% 400000), 'ambit.compress.bytes_out', toString(4000 + h %% 150000),
    'ambit.compress.omitted', '0'
  )
FROM numbers(%[2]d)`, day, n, offset, groups, total)
}

func TestQueryCost(t *testing.T) {
	if os.Getenv("AMBIT_SAVINGS_BENCH") != "1" {
		t.Skip("AMBIT_SAVINGS_BENCH is not 1")
	}
	a := clickhouseTestDB(t, "savings_bench")
	ctx := context.Background()
	if err := EnsureSummaries(ctx, a.DB, 1095); err != nil {
		t.Fatal(err)
	}
	perDay, block, groups := envInt("AMBIT_SAVINGS_BENCH_PER_DAY", 300_000), 20_000, envInt("AMBIT_SAVINGS_BENCH_GROUPS", 20)
	end := time.Now().UTC().Truncate(24 * time.Hour)
	start := time.Now()
	for d := 29; d >= 0; d-- {
		day := end.AddDate(0, 0, -d).Format("2006-01-02")
		for off := 0; off < perDay; off += block {
			n := min(block, perDay-off)
			// The whole day's rows are spread over 24 hours: the block's rows get their own slice of the day.
			if _, err := a.DB.ExecContext(ctx, seedSQL(day, off, n, perDay, groups)); err != nil {
				t.Fatalf("seed %s +%d: %v", day, off, err)
			}
		}
	}
	t.Logf("seeded %d records in %v", 30*perDay, time.Since(start).Round(time.Second))
	from, to := end.AddDate(0, 0, -29), end.Add(23*time.Hour+59*time.Minute)
	for i := 0; i < 3; i++ { // the first run is cold; all three are in the query log
		ctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"log_comment": "savings-old"}))
		u, err := a.Usage(ctx, from, to.Add(time.Hour), nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			t.Logf("old Usage query: %d rows", len(u))
		}
	}
	for i := 0; i < 3; i++ {
		ctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"log_comment": "savings-new"}))
		l, c, _, err := a.SummaryRows(ctx, from, to)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			t.Logf("new summary queries: %d LLM rows, %d compression rows", len(l), len(c))
		}
	}
}

// Cardinality: the summary queries with 20, 100 and 500 groups for 30 days (rows written straight into the summaries).
func TestSummaryCardinality(t *testing.T) {
	if os.Getenv("AMBIT_SAVINGS_BENCH") != "1" {
		t.Skip("AMBIT_SAVINGS_BENCH is not 1")
	}
	a := clickhouseTestDB(t, "savings_bench_card")
	ctx := context.Background()
	if err := EnsureSummaries(ctx, a.DB, 1095); err != nil {
		t.Fatal(err)
	}
	end := time.Now().UTC().Truncate(24 * time.Hour)
	for _, g := range []int{20, 100, 500} {
		for _, tbl := range []string{llmTable, compressTable} {
			if _, err := a.DB.ExecContext(ctx, "TRUNCATE TABLE "+tbl); err != nil {
				t.Fatal(err)
			}
		}
		// 30 days x groups x 15 (router, model) pairs; 30 x groups x 15 servers x 5 types.
		q1 := fmt.Sprintf(`INSERT INTO %s SELECT toDate('%s') - intDiv(number, %d*15), concat('group-', toString(intDiv(number, 15) %% %d)), if(number %% 15 < 5, 'auto', ''), concat('model-', toString(number %% 15)), 100 + number %% 900, 1000000 + number, 100000 + number FROM numbers(30*%d*15)`,
			llmTable, end.Format("2006-01-02"), g, g, g)
		q2 := fmt.Sprintf(`INSERT INTO %s SELECT toDate('%s') - intDiv(number, %d*75), concat('group-', toString(intDiv(number, 75) %% %d)), concat('server-', toString(intDiv(number, 5) %% 15)), arrayElement(['json','table','yaml','logs','test'], 1 + number %% 5), 10 + number %% 90, 5000000 + number, 2000000 + number, number %% 7 FROM numbers(30*%d*75)`,
			compressTable, end.Format("2006-01-02"), g, g, g)
		for _, q := range []string{q1, q2} {
			if _, err := a.DB.ExecContext(ctx, q); err != nil {
				t.Fatal(err)
			}
		}
		ctx := clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"log_comment": fmt.Sprintf("savings-card-%d", g)}))
		l, c, _, err := a.SummaryRows(ctx, end.AddDate(0, 0, -29), end)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d groups: %d LLM rows, %d compression rows", g, len(l), len(c))
	}
}
