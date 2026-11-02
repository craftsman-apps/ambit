#!/usr/bin/env bash
# Seeds ClickHouse with DAYS days of records that look like real use, through the real insert path (the Savings and
# Usage summary views fill while it inserts). 60 people (user1..user60@example.com): most are in one group, one in ten
# is in two. LLM requests go to two priced models, a third of them through the router "auto"; guard records come from
# three tool servers. Needs the objects of test/savings-seed/objects.yaml for prices and the router. Local use only.
# Usage: KCTX=<kube context> test/savings-seed/seed.sh [namespace] [days]   (the ClickHouse password stays in the pod)
set -euo pipefail
NS=${1:-ambit-system}
DAYS=${2:-14}
for d in $(seq $((DAYS - 1)) -1 0); do
  kubectl ${KCTX:+--context $KCTX} -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "
INSERT INTO otel.otel_logs (Timestamp, LogAttributes)
WITH number AS i, cityHash64(i, $d) AS h, h % 60 + 1 AS u,
     if(u <= 30, 'eng', if(u <= 48, 'support', 'ml-research')) AS g1,
     if(u % 10 = 0, if(g1 = 'eng', 'ml-research', if(g1 = 'support', 'eng', 'support')), '') AS g2,
     if(g2 = '', concat('[\"', g1, '\"]'), concat('[\"', g1, '\",\"', g2, '\"]')) AS groups,
     toDateTime64(toDate(now('UTC')) - $d, 9, 'UTC') AS day0,
     if($d = 0, now64(9, 'UTC') - toIntervalSecond(h % 7200), day0 + toIntervalSecond(8 * 3600 + h % 36000)) AS ts,
     h % 10 AS r,
     if(r < 2, 'model-large', if(r < 5, 'model-small', if(h % 100 < 68, 'model-small', 'model-large'))) AS model
SELECT ts, map('protocol', 'llm', 'jwt.email', concat('user', toString(u), '@example.com'), 'jwt.groups', groups,
  'gen_ai.request.model', model, 'ambit.router', if(r >= 5, 'auto', model),
  'gen_ai.usage.input_tokens', toString(800 + h % 12000), 'gen_ai.usage.output_tokens', toString(100 + h % 1500))
FROM numbers(1500 + cityHash64($d) % 500)" >/dev/null
  kubectl ${KCTX:+--context $KCTX} -n "$NS" exec ambit-clickhouse-0 -- sh -c 'clickhouse-client --user ambit --password "$CLICKHOUSE_PASSWORD" -q "$0"' "
INSERT INTO otel.otel_logs (Timestamp, LogAttributes)
WITH number AS i, cityHash64(i, $d, 7) AS h, h % 60 + 1 AS u,
     if(u <= 30, 'eng', if(u <= 48, 'support', 'ml-research')) AS g1,
     if(u % 10 = 0, if(g1 = 'eng', 'ml-research', if(g1 = 'support', 'eng', 'support')), '') AS g2,
     if(g2 = '', concat('[\"', g1, '\"]'), concat('[\"', g1, '\",\"', g2, '\"]')) AS groups,
     toDateTime64(toDate(now('UTC')) - $d, 9, 'UTC') AS day0,
     if($d = 0, now64(9, 'UTC') - toIntervalSecond(h % 7200), day0 + toIntervalSecond(8 * 3600 + h % 36000)) AS ts,
     arrayElement([('crm', 'json'), ('crm', 'yaml'), ('github', 'json'), ('kubectl', 'logs')], 1 + h % 4) AS st,
     20000 + (h / 7) % 380000 AS bin
SELECT ts, map('ambit.kind', 'compression', 'mcp.target', st.1, 'ambit.compress.type', st.2, 'ambit.compress.mode', 'lossless',
  'ambit.compress.bytes_in', toString(bin), 'ambit.compress.bytes_out', toString(toUInt64(bin * (0.35 + (h % 30) / 100))),
  'ambit.compress.omitted', '0', 'jwt.groups', groups)
FROM numbers(120 + cityHash64($d, 3) % 80)" >/dev/null
done
echo "seeded $DAYS days"
