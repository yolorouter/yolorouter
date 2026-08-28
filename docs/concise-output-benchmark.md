# Concise-output benchmark

[中文版](concise-output-benchmark_zh.md)

The cost-optimization page estimates what the Concise Output switch saves
(a global system prompt that asks models to keep replies short). The
estimate is a period total for the selected time range: the output spend
and the output tokens of your priced traffic, each times a fixed
coefficient of **12.6%**. This file records how that 12.6% was measured.
The complete raw data is at the bottom.

## How it was measured

10 fixed questions (listed below), 3 rounds each, on five models:
claude-opus-4-7, deepseek-v4-flash, deepseek-v4-pro, glm-5.1,
qwen3.5-flash. Each model ran all 30 questions with the switch off, then the
switch was flipped through the console's own setting API and the same 30 ran
again. 300 calls in total, none failed, which gives 150 on/off pairs — one
per (model, question, round).

The ON runs installed exactly what the console installs when you turn the
switch on, verbatim:

> 回答请保持简洁，去掉客套话和不必要的铺垫，保留完整语法和技术细节。优先复用标准库和平台已有能力，非必要不引入新的抽象或依赖，用最短的可行改动解决问题。

Both sentences, because the switch writes both — the second one, about
preferring existing platform capabilities and the smallest viable change,
shortens code answers as much as the first one shortens prose. The console
writes them in its own language, so an English console installs the English
wording; that wording is not what was measured here.

Sampling parameters were whatever each model defaults to. For each pair,
`r = (off_tokens − on_tokens) / off_tokens`, where the token counts are
the upstream-reported `completion_tokens`. On reasoning models this
includes the thinking tokens, because those are billed too. The shipped
coefficient is the median of all 150 ratios.

Measured on 2026-08-24.

## Results

| | |
| --- | --- |
| Median (the shipped coefficient) | **+12.6%** |
| 25th / 75th percentile | −4.0% / +27.2% |
| Min / max | −192.8% / +87.2% |
| Negative pairs | 44 of 150 |

Per-model medians:

| Model | Median |
| --- | --- |
| claude-opus-4-7 | +3.6% |
| deepseek-v4-flash | +18.6% |
| deepseek-v4-pro | +27.4% |
| glm-5.1 | +11.3% |
| qwen3.5-flash | +10.6% |

Every model came out ahead, but the spread inside each one is wide and it
is not noise you can average away by looking harder. 44 of the 150 pairs are
negative — the prompt made that particular answer longer. Individual pairs
swing hard: the same model on the same question landed at −193% in one round
and +46% in another. Expect the figure to hold across a month of traffic,
not on any single request.

We ship one global number, not a per-model table. Models change faster than
such a table stays useful, and the page labels the figure as an estimate.
The number also assumes the switch is left on the prompt it installs;
replacing that text through the API with something unrelated voids it.

## The questions

Frozen; the same set is used every time. The runs were made in Chinese —
the questions below are translations, kept here so the mix is readable.
Question length and language both move the token counts, so reproducing
these numbers means sending the Chinese originals verbatim: they are in the
[Chinese edition](concise-output-benchmark_zh.md#题目). (Embedded code, SQL
and log samples are identical in both.)

1. **[codegen]** Implement in Go a function `WordCount(text string, topN int)`
   that counts case-insensitively how often each word appears in an English
   text and returns the top `topN` `(word, count)` pairs by descending
   count. Provide complete runnable code including imports and a small
   `main` example.
2. **[codegen]** Write a Python CLI script that recursively walks a given
   directory, finds files larger than 10 MB modified within the last 7
   days, and prints their paths and sizes (human-readable) in descending
   size order. Provide complete code and usage instructions.
3. **[code-explain]** Explain what this SQL does and point out possible
   performance problems and improvements:
   `SELECT u.id, u.username, u.email, COUNT(o.id) AS order_count, SUM(o.amount) AS total FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.status = 'paid' WHERE u.created_at > '2026-01-01' AND u.status <> 'deleted' GROUP BY u.id, u.username, u.email HAVING COUNT(o.id) > 5 ORDER BY order_count DESC LIMIT 100;`
4. **[code-review]** Review this Go code and list every problem you find
   (error handling, resource leaks, edge cases):
   `func readConfig(path string) ([]byte, error) { f, err := os.Open(path); if err != nil { return nil, err } b, err := io.ReadAll(f); return b, err }`
5. **[log-analysis]** Given this one-hour sample of API access logs
   (format: `time method path status latency_ms`), analyze the traffic
   pattern, surface anomalies, and give operational recommendations:
   `10:01 GET /v1/models 200 12; 10:01 POST /v1/chat/completions 200 3421; 10:02 POST /v1/chat/completions 429 5; 10:02 POST /v1/chat/completions 200 5410; 10:03 GET /v1/models 200 9; 10:03 POST /v1/chat/completions 500 78; 10:04 POST /v1/chat/completions 200 4102; 10:05 GET /health 200 1; 10:05 POST /v1/chat/completions 200 6230; 10:06 POST /v1/chat/completions 429 4; 10:07 POST /v1/chat/completions 200 3871; 10:08 GET /v1/models 200 11; 10:09 POST /v1/chat/completions 200 5540; 10:10 POST /v1/chat/completions 502 120`
6. **[summary]** Read the product introduction below and produce a
   structured summary (target users, core features, pricing model,
   differentiation — at most three bullets each): “YoloRouter is a
   developer-facing AI model routing gateway. It aggregates multiple
   upstream model services behind one OpenAI-compatible endpoint; callers
   switch models by pointing base_url at the gateway. Provider-level
   failover is built in: one model can map to several providers and
   requests degrade automatically to the next one, invisibly to the
   caller. It also provides API key management, per-account usage
   analytics and budget caps. Deployment is a single binary with SQLite
   built in, or PostgreSQL. Pricing is upstream cost + 5%, no monthly
   fee.”
7. **[knowledge]** Why does TCP need a three-way handshake rather than two?
   Explain from the protocol's design goals (preventing historical
   connections, synchronizing initial sequence numbers, confirming
   two-way communication) and what breaks with two.
8. **[knowledge]** Explain the four database transaction isolation levels:
   what each solves, which anomalies remain (dirty read, non-repeatable
   read, phantom read), and MySQL InnoDB's default level and how it is
   implemented.
9. **[translate]** Translate this English technical documentation into
   Chinese, preserving terminology: “The gateway normalizes every inbound
   request into an intermediate representation before dispatching it to an
   upstream provider. This decouples the ingress protocol spoken by the
   caller from the egress protocol spoken by the provider, so a new
   provider can be added without touching any caller-side code. Streaming
   responses are relayed chunk-by-chunk with backpressure, and usage
   reported by the upstream is reconciled into the audit log at settlement
   time.”
10. **[rewrite]** Rewrite this rambling email to be concise and
    professional, keeping every key fact (time, place, agenda, prep
    work): “hi 大家好，是这样的，我们本来定在下周三下午的开会时间，因为会议室被占了，所以现在改到周四上午十点了，地点还是老地方 B 栋 301。这次会议主要想跟大家同步一下二季度的进度，然后讨论一下下个季度的计划，另外呢，麻烦大家提前把自己负责模块的数据准备好，最好是能发我一份，我在会上统一汇总，谢谢大家配合，有什么问题随时找我。”

## Raw data — all 150 pairs

`off` / `on` are the upstream-reported `completion_tokens` (thinking tokens
included); `r = (off − on) / off`. Positive `r` means the concise prompt
shortened the output.

| Model | Question | Round | off | on | r |
| --- | --- | --- | --- | --- | --- |
| claude-opus-4-7 | q1-codegen-wordcount | 1 | 615 | 617 | -0.3% |
| claude-opus-4-7 | q1-codegen-wordcount | 2 | 590 | 592 | -0.3% |
| claude-opus-4-7 | q1-codegen-wordcount | 3 | 653 | 607 | 7.0% |
| claude-opus-4-7 | q2-codegen-walk | 1 | 1028 | 826 | 19.6% |
| claude-opus-4-7 | q2-codegen-walk | 2 | 1092 | 1017 | 6.9% |
| claude-opus-4-7 | q2-codegen-walk | 3 | 1082 | 874 | 19.2% |
| claude-opus-4-7 | q3-sql-explain | 1 | 1331 | 1220 | 8.3% |
| claude-opus-4-7 | q3-sql-explain | 2 | 1164 | 1554 | -33.5% |
| claude-opus-4-7 | q3-sql-explain | 3 | 1382 | 1364 | 1.3% |
| claude-opus-4-7 | q4-go-review | 1 | 632 | 342 | 45.9% |
| claude-opus-4-7 | q4-go-review | 2 | 502 | 1470 | -192.8% |
| claude-opus-4-7 | q4-go-review | 3 | 900 | 711 | 21.0% |
| claude-opus-4-7 | q5-log-analysis | 1 | 1014 | 773 | 23.8% |
| claude-opus-4-7 | q5-log-analysis | 2 | 859 | 1284 | -49.5% |
| claude-opus-4-7 | q5-log-analysis | 3 | 960 | 856 | 10.8% |
| claude-opus-4-7 | q6-longtext-summary | 1 | 299 | 255 | 14.7% |
| claude-opus-4-7 | q6-longtext-summary | 2 | 288 | 243 | 15.6% |
| claude-opus-4-7 | q6-longtext-summary | 3 | 257 | 256 | 0.4% |
| claude-opus-4-7 | q7-tcp-handshake | 1 | 1138 | 1127 | 1.0% |
| claude-opus-4-7 | q7-tcp-handshake | 2 | 1012 | 1052 | -4.0% |
| claude-opus-4-7 | q7-tcp-handshake | 3 | 1092 | 829 | 24.1% |
| claude-opus-4-7 | q8-isolation-levels | 1 | 1221 | 1261 | -3.3% |
| claude-opus-4-7 | q8-isolation-levels | 2 | 1122 | 1440 | -28.3% |
| claude-opus-4-7 | q8-isolation-levels | 3 | 1174 | 1059 | 9.8% |
| claude-opus-4-7 | q9-translate | 1 | 151 | 149 | 1.3% |
| claude-opus-4-7 | q9-translate | 2 | 151 | 151 | 0.0% |
| claude-opus-4-7 | q9-translate | 3 | 150 | 150 | 0.0% |
| claude-opus-4-7 | q10-rewrite-email | 1 | 152 | 148 | 2.6% |
| claude-opus-4-7 | q10-rewrite-email | 2 | 183 | 137 | 25.1% |
| claude-opus-4-7 | q10-rewrite-email | 3 | 152 | 145 | 4.6% |
| deepseek-v4-flash | q1-codegen-wordcount | 1 | 1373 | 1213 | 11.7% |
| deepseek-v4-flash | q1-codegen-wordcount | 2 | 1517 | 2011 | -32.6% |
| deepseek-v4-flash | q1-codegen-wordcount | 3 | 1735 | 3354 | -93.3% |
| deepseek-v4-flash | q2-codegen-walk | 1 | 875 | 625 | 28.6% |
| deepseek-v4-flash | q2-codegen-walk | 2 | 1254 | 1062 | 15.3% |
| deepseek-v4-flash | q2-codegen-walk | 3 | 1372 | 886 | 35.4% |
| deepseek-v4-flash | q3-sql-explain | 1 | 2385 | 1576 | 33.9% |
| deepseek-v4-flash | q3-sql-explain | 2 | 1733 | 2005 | -15.7% |
| deepseek-v4-flash | q3-sql-explain | 3 | 2152 | 3992 | -85.5% |
| deepseek-v4-flash | q4-go-review | 1 | 492 | 826 | -67.9% |
| deepseek-v4-flash | q4-go-review | 2 | 671 | 1133 | -68.9% |
| deepseek-v4-flash | q4-go-review | 3 | 1431 | 521 | 63.6% |
| deepseek-v4-flash | q5-log-analysis | 1 | 1118 | 1175 | -5.1% |
| deepseek-v4-flash | q5-log-analysis | 2 | 5522 | 722 | 86.9% |
| deepseek-v4-flash | q5-log-analysis | 3 | 4812 | 3224 | 33.0% |
| deepseek-v4-flash | q6-longtext-summary | 1 | 350 | 259 | 26.0% |
| deepseek-v4-flash | q6-longtext-summary | 2 | 545 | 1201 | -120.4% |
| deepseek-v4-flash | q6-longtext-summary | 3 | 317 | 248 | 21.8% |
| deepseek-v4-flash | q7-tcp-handshake | 1 | 1220 | 1624 | -33.1% |
| deepseek-v4-flash | q7-tcp-handshake | 2 | 1031 | 410 | 60.2% |
| deepseek-v4-flash | q7-tcp-handshake | 3 | 1367 | 459 | 66.4% |
| deepseek-v4-flash | q8-isolation-levels | 1 | 815 | 853 | -4.7% |
| deepseek-v4-flash | q8-isolation-levels | 2 | 537 | 534 | 0.6% |
| deepseek-v4-flash | q8-isolation-levels | 3 | 977 | 702 | 28.1% |
| deepseek-v4-flash | q9-translate | 1 | 419 | 244 | 41.8% |
| deepseek-v4-flash | q9-translate | 2 | 327 | 218 | 33.3% |
| deepseek-v4-flash | q9-translate | 3 | 211 | 178 | 15.6% |
| deepseek-v4-flash | q10-rewrite-email | 1 | 200 | 157 | 21.5% |
| deepseek-v4-flash | q10-rewrite-email | 2 | 161 | 197 | -22.4% |
| deepseek-v4-flash | q10-rewrite-email | 3 | 851 | 109 | 87.2% |
| deepseek-v4-pro | q1-codegen-wordcount | 1 | 1599 | 1570 | 1.8% |
| deepseek-v4-pro | q1-codegen-wordcount | 2 | 1324 | 960 | 27.5% |
| deepseek-v4-pro | q1-codegen-wordcount | 3 | 1046 | 845 | 19.2% |
| deepseek-v4-pro | q2-codegen-walk | 1 | 1453 | 1057 | 27.3% |
| deepseek-v4-pro | q2-codegen-walk | 2 | 1105 | 1679 | -51.9% |
| deepseek-v4-pro | q2-codegen-walk | 3 | 1919 | 911 | 52.5% |
| deepseek-v4-pro | q3-sql-explain | 1 | 1534 | 1504 | 2.0% |
| deepseek-v4-pro | q3-sql-explain | 2 | 2404 | 1592 | 33.8% |
| deepseek-v4-pro | q3-sql-explain | 3 | 2997 | 2626 | 12.4% |
| deepseek-v4-pro | q4-go-review | 1 | 336 | 366 | -8.9% |
| deepseek-v4-pro | q4-go-review | 2 | 908 | 442 | 51.3% |
| deepseek-v4-pro | q4-go-review | 3 | 819 | 351 | 57.1% |
| deepseek-v4-pro | q5-log-analysis | 1 | 1570 | 955 | 39.2% |
| deepseek-v4-pro | q5-log-analysis | 2 | 1086 | 844 | 22.3% |
| deepseek-v4-pro | q5-log-analysis | 3 | 928 | 791 | 14.8% |
| deepseek-v4-pro | q6-longtext-summary | 1 | 423 | 847 | -100.2% |
| deepseek-v4-pro | q6-longtext-summary | 2 | 733 | 412 | 43.8% |
| deepseek-v4-pro | q6-longtext-summary | 3 | 1095 | 793 | 27.6% |
| deepseek-v4-pro | q7-tcp-handshake | 1 | 1367 | 810 | 40.7% |
| deepseek-v4-pro | q7-tcp-handshake | 2 | 1327 | 1109 | 16.4% |
| deepseek-v4-pro | q7-tcp-handshake | 3 | 1557 | 1572 | -1.0% |
| deepseek-v4-pro | q8-isolation-levels | 1 | 1483 | 1159 | 21.8% |
| deepseek-v4-pro | q8-isolation-levels | 2 | 1075 | 559 | 48.0% |
| deepseek-v4-pro | q8-isolation-levels | 3 | 1698 | 1129 | 33.5% |
| deepseek-v4-pro | q9-translate | 1 | 984 | 518 | 47.4% |
| deepseek-v4-pro | q9-translate | 2 | 555 | 687 | -23.8% |
| deepseek-v4-pro | q9-translate | 3 | 912 | 442 | 51.5% |
| deepseek-v4-pro | q10-rewrite-email | 1 | 487 | 1009 | -107.2% |
| deepseek-v4-pro | q10-rewrite-email | 2 | 655 | 445 | 32.1% |
| deepseek-v4-pro | q10-rewrite-email | 3 | 1008 | 569 | 43.6% |
| glm-5.1 | q1-codegen-wordcount | 1 | 1580 | 1609 | -1.8% |
| glm-5.1 | q1-codegen-wordcount | 2 | 1505 | 1095 | 27.2% |
| glm-5.1 | q1-codegen-wordcount | 3 | 1710 | 1268 | 25.8% |
| glm-5.1 | q2-codegen-walk | 1 | 1288 | 968 | 24.8% |
| glm-5.1 | q2-codegen-walk | 2 | 1353 | 1197 | 11.5% |
| glm-5.1 | q2-codegen-walk | 3 | 1661 | 1217 | 26.7% |
| glm-5.1 | q3-sql-explain | 1 | 2117 | 1895 | 10.5% |
| glm-5.1 | q3-sql-explain | 2 | 2757 | 2086 | 24.3% |
| glm-5.1 | q3-sql-explain | 3 | 2274 | 2575 | -13.2% |
| glm-5.1 | q4-go-review | 1 | 767 | 660 | 14.0% |
| glm-5.1 | q4-go-review | 2 | 965 | 889 | 7.9% |
| glm-5.1 | q4-go-review | 3 | 690 | 578 | 16.2% |
| glm-5.1 | q5-log-analysis | 1 | 1721 | 1792 | -4.1% |
| glm-5.1 | q5-log-analysis | 2 | 1550 | 1379 | 11.0% |
| glm-5.1 | q5-log-analysis | 3 | 1914 | 1670 | 12.7% |
| glm-5.1 | q6-longtext-summary | 1 | 1122 | 894 | 20.3% |
| glm-5.1 | q6-longtext-summary | 2 | 1094 | 1216 | -11.2% |
| glm-5.1 | q6-longtext-summary | 3 | 1286 | 1147 | 10.8% |
| glm-5.1 | q7-tcp-handshake | 1 | 1364 | 1540 | -12.9% |
| glm-5.1 | q7-tcp-handshake | 2 | 1412 | 1369 | 3.0% |
| glm-5.1 | q7-tcp-handshake | 3 | 1847 | 1345 | 27.2% |
| glm-5.1 | q8-isolation-levels | 1 | 1377 | 1200 | 12.9% |
| glm-5.1 | q8-isolation-levels | 2 | 1735 | 1363 | 21.4% |
| glm-5.1 | q8-isolation-levels | 3 | 1344 | 1233 | 8.3% |
| glm-5.1 | q9-translate | 1 | 1206 | 1076 | 10.8% |
| glm-5.1 | q9-translate | 2 | 1503 | 1139 | 24.2% |
| glm-5.1 | q9-translate | 3 | 1042 | 1013 | 2.8% |
| glm-5.1 | q10-rewrite-email | 1 | 834 | 810 | 2.9% |
| glm-5.1 | q10-rewrite-email | 2 | 883 | 606 | 31.4% |
| glm-5.1 | q10-rewrite-email | 3 | 748 | 1044 | -39.6% |
| qwen3.5-flash | q1-codegen-wordcount | 1 | 3692 | 4426 | -19.9% |
| qwen3.5-flash | q1-codegen-wordcount | 2 | 2150 | 3372 | -56.8% |
| qwen3.5-flash | q1-codegen-wordcount | 3 | 2871 | 2172 | 24.3% |
| qwen3.5-flash | q2-codegen-walk | 1 | 3056 | 2733 | 10.6% |
| qwen3.5-flash | q2-codegen-walk | 2 | 2316 | 2739 | -18.3% |
| qwen3.5-flash | q2-codegen-walk | 3 | 1860 | 4833 | -159.8% |
| qwen3.5-flash | q3-sql-explain | 1 | 3145 | 3324 | -5.7% |
| qwen3.5-flash | q3-sql-explain | 2 | 3780 | 2697 | 28.7% |
| qwen3.5-flash | q3-sql-explain | 3 | 2566 | 3548 | -38.3% |
| qwen3.5-flash | q4-go-review | 1 | 3563 | 2171 | 39.1% |
| qwen3.5-flash | q4-go-review | 2 | 2544 | 2839 | -11.6% |
| qwen3.5-flash | q4-go-review | 3 | 3344 | 1276 | 61.8% |
| qwen3.5-flash | q5-log-analysis | 1 | 4562 | 2572 | 43.6% |
| qwen3.5-flash | q5-log-analysis | 2 | 3071 | 2545 | 17.1% |
| qwen3.5-flash | q5-log-analysis | 3 | 3287 | 2404 | 26.9% |
| qwen3.5-flash | q6-longtext-summary | 1 | 2057 | 1756 | 14.6% |
| qwen3.5-flash | q6-longtext-summary | 2 | 2405 | 1751 | 27.2% |
| qwen3.5-flash | q6-longtext-summary | 3 | 2017 | 2116 | -4.9% |
| qwen3.5-flash | q7-tcp-handshake | 1 | 3977 | 3420 | 14.0% |
| qwen3.5-flash | q7-tcp-handshake | 2 | 2694 | 1817 | 32.6% |
| qwen3.5-flash | q7-tcp-handshake | 3 | 3940 | 3751 | 4.8% |
| qwen3.5-flash | q8-isolation-levels | 1 | 1778 | 2477 | -39.3% |
| qwen3.5-flash | q8-isolation-levels | 2 | 2364 | 1860 | 21.3% |
| qwen3.5-flash | q8-isolation-levels | 3 | 2252 | 2405 | -6.8% |
| qwen3.5-flash | q9-translate | 1 | 4317 | 4367 | -1.2% |
| qwen3.5-flash | q9-translate | 2 | 2820 | 2456 | 12.9% |
| qwen3.5-flash | q9-translate | 3 | 4093 | 1571 | 61.6% |
| qwen3.5-flash | q10-rewrite-email | 1 | 1282 | 2106 | -64.3% |
| qwen3.5-flash | q10-rewrite-email | 2 | 1145 | 1934 | -68.9% |
| qwen3.5-flash | q10-rewrite-email | 3 | 2148 | 1918 | 10.7% |

## Reproducing

Run the frozen questions on your own instance — in the original Chinese, as
above — 3 rounds each: all of them with the switch off, then all of them
again with it on. Take the median of the pair ratios. Your numbers will not
match ours — models drift — but the shape should: a positive median with a
wide spread around it, and a meaningful minority of pairs coming out
negative.


## Appendix: rerun against an aggressive style prompt (2026-08-28)

[caveman](https://github.com/JuliusBrussee/caveman) is a widely shared
"answer in terse caveman-speak" skill whose description claims a 65% cut in
output tokens. To check whether the switch's two-sentence prompt leaves that
much on the table, the benchmark was rerun with three arms instead of two:

- **off** — no system prompt (fresh baselines, not reused from 2026-08-24)
- **two-sentence prompt** — the exact text the console writes, as above
- **caveman** — the full body of caveman's `SKILL.md` (YAML frontmatter
  stripped, ~6.5 KB of English rules), injected verbatim as the system
  prompt. Fetched 2026-08-28 from commit
  [`b433570`](https://github.com/JuliusBrussee/caveman/blob/b4335705d436f5110386a1c39c6d8aed5002aeeb/skills/caveman/SKILL.md).

Same 10 frozen questions, same 5 models, 3 rounds, 450 calls, none failed.
One methodology note: the claude-opus-4-7 calls in this rerun were streamed,
because the provider endpoint serving it timed out long non-streaming
requests; `completion_tokens` is the upstream-reported usage in every arm
either way. `r` is computed per (model, question, round) pair exactly as
before. Token-weighted saving is `1 − (arm total ÷ off total)` over each
arm's summed `completion_tokens` (244,619 off, 206,726
two-sentence, 236,818 caveman).

| | two-sentence prompt | caveman |
| --- | --- | --- |
| Median saving | **+12.8%** | **+16.4%** |
| 25th / 75th percentile | −3.4% / +31.2% | −12.8% / +41.0% |
| Min / max | −220.3% / +90.4% | −1275.0% / +87.6% |
| Negative pairs | 42 of 150 | 45 of 150 |
| Token-weighted saving | **+15.5%** | **+3.2%** |

Per-model medians:

| Model | two-sentence prompt | caveman |
| --- | --- | --- |
| claude-opus-4-7 | +12.8% | +19.4% |
| deepseek-v4-flash | +33.9% | +10.8% |
| deepseek-v4-pro | +28.8% | +33.1% |
| glm-5.1 | +9.1% | +34.6% |
| qwen3.5-flash | −16.5% | −15.5% |

How to read it:

- The two-sentence prompt's median of +12.8% replicates the 12.6% factory
  coefficient measured four days earlier. The coefficient stands.
- The caveman prompt's median is +16.4% — 3.6 points above the two-sentence
  prompt and nowhere near 65%. That is consistent with caveman's own
  [honest-numbers page](https://github.com/JuliusBrussee/caveman/blob/ee3199bca81d388d13ec508ccf7f7b318b905c8a/docs/HONEST-NUMBERS.md),
  which states that no reviewed aggregate output-reduction result is
  published for the skill.
- The aggressive style has a much heavier tail. Its worst pair turned a
  108-token email rewrite into 1,485 tokens (−1275%); several reasoning-model
  pairs blew up the same way, plausibly because the model spends thinking
  tokens deliberating over the 6.5 KB rulebook — and thinking tokens are
  billed. Weighted by tokens actually paid for, caveman saved 3.2% while the
  two-sentence prompt saved 15.5%.
- The style prompt itself is input: ~6.5 KB (≈1.5k tokens) added to every
  request, versus ~75 characters for the two-sentence prompt. On short
  interactions that overhead alone can exceed the output saving.

### Rerun raw data — all 150 triples

`off` / `ours` / `caveman` are the upstream-reported `completion_tokens`
(thinking tokens included); each `r` is against `off` in the same row.

| Model | Question | Round | off | ours | r(ours) | caveman | r(caveman) |
| --- | --- | --- | --- | --- | --- | --- | --- |
| claude-opus-4-7 | q1-codegen-wordcount | 1 | 441 | 402 | 8.8% | 423 | 4.1% |
| claude-opus-4-7 | q1-codegen-wordcount | 2 | 481 | 420 | 12.7% | 404 | 16.0% |
| claude-opus-4-7 | q1-codegen-wordcount | 3 | 481 | 419 | 12.9% | 370 | 23.1% |
| claude-opus-4-7 | q2-codegen-walk | 1 | 652 | 472 | 27.6% | 653 | -0.2% |
| claude-opus-4-7 | q2-codegen-walk | 2 | 656 | 493 | 24.8% | 511 | 22.1% |
| claude-opus-4-7 | q2-codegen-walk | 3 | 785 | 453 | 42.3% | 477 | 39.2% |
| claude-opus-4-7 | q3-sql-explain | 1 | 698 | 645 | 7.6% | 581 | 16.8% |
| claude-opus-4-7 | q3-sql-explain | 2 | 900 | 670 | 25.6% | 531 | 41.0% |
| claude-opus-4-7 | q3-sql-explain | 3 | 854 | 642 | 24.8% | 555 | 35.0% |
| claude-opus-4-7 | q4-go-review | 1 | 374 | 315 | 15.8% | 320 | 14.4% |
| claude-opus-4-7 | q4-go-review | 2 | 335 | 231 | 31.0% | 309 | 7.8% |
| claude-opus-4-7 | q4-go-review | 3 | 332 | 263 | 20.8% | 352 | -6.0% |
| claude-opus-4-7 | q5-log-analysis | 1 | 871 | 616 | 29.3% | 596 | 31.6% |
| claude-opus-4-7 | q5-log-analysis | 2 | 842 | 658 | 21.9% | 622 | 26.1% |
| claude-opus-4-7 | q5-log-analysis | 3 | 739 | 693 | 6.2% | 520 | 29.6% |
| claude-opus-4-7 | q6-longtext-summary | 1 | 168 | 171 | -1.8% | 293 | -74.4% |
| claude-opus-4-7 | q6-longtext-summary | 2 | 164 | 168 | -2.4% | 299 | -82.3% |
| claude-opus-4-7 | q6-longtext-summary | 3 | 179 | 166 | 7.3% | 300 | -67.6% |
| claude-opus-4-7 | q7-tcp-handshake | 1 | 600 | 584 | 2.7% | 562 | 6.3% |
| claude-opus-4-7 | q7-tcp-handshake | 2 | 627 | 625 | 0.3% | 462 | 26.3% |
| claude-opus-4-7 | q7-tcp-handshake | 3 | 766 | 573 | 25.2% | 498 | 35.0% |
| claude-opus-4-7 | q8-isolation-levels | 1 | 857 | 637 | 25.7% | 441 | 48.5% |
| claude-opus-4-7 | q8-isolation-levels | 2 | 812 | 558 | 31.3% | 427 | 47.4% |
| claude-opus-4-7 | q8-isolation-levels | 3 | 688 | 558 | 18.9% | 648 | 5.8% |
| claude-opus-4-7 | q9-translate | 1 | 142 | 140 | 1.4% | 97 | 31.7% |
| claude-opus-4-7 | q9-translate | 2 | 155 | 145 | 6.5% | 108 | 30.3% |
| claude-opus-4-7 | q9-translate | 3 | 142 | 140 | 1.4% | 100 | 29.6% |
| claude-opus-4-7 | q10-rewrite-email | 1 | 131 | 115 | 12.2% | 116 | 11.5% |
| claude-opus-4-7 | q10-rewrite-email | 2 | 128 | 116 | 9.4% | 109 | 14.8% |
| claude-opus-4-7 | q10-rewrite-email | 3 | 131 | 115 | 12.2% | 115 | 12.2% |
| deepseek-v4-flash | q1-codegen-wordcount | 1 | 1712 | 1077 | 37.1% | 1056 | 38.3% |
| deepseek-v4-flash | q1-codegen-wordcount | 2 | 567 | 1087 | -91.7% | 4350 | -667.2% |
| deepseek-v4-flash | q1-codegen-wordcount | 3 | 2791 | 1933 | 30.7% | 1077 | 61.4% |
| deepseek-v4-flash | q2-codegen-walk | 1 | 1722 | 1490 | 13.5% | 1546 | 10.2% |
| deepseek-v4-flash | q2-codegen-walk | 2 | 8192 | 783 | 90.4% | 4340 | 47.0% |
| deepseek-v4-flash | q2-codegen-walk | 3 | 2919 | 673 | 76.9% | 1351 | 53.7% |
| deepseek-v4-flash | q3-sql-explain | 1 | 5455 | 5420 | 0.6% | 4835 | 11.4% |
| deepseek-v4-flash | q3-sql-explain | 2 | 8192 | 1220 | 85.1% | 6456 | 21.2% |
| deepseek-v4-flash | q3-sql-explain | 3 | 1065 | 1102 | -3.5% | 8376 | -686.5% |
| deepseek-v4-flash | q4-go-review | 1 | 1886 | 1162 | 38.4% | 1101 | 41.6% |
| deepseek-v4-flash | q4-go-review | 2 | 2383 | 2290 | 3.9% | 1768 | 25.8% |
| deepseek-v4-flash | q4-go-review | 3 | 1894 | 566 | 70.1% | 3923 | -107.1% |
| deepseek-v4-flash | q5-log-analysis | 1 | 3676 | 2697 | 26.6% | 3648 | 0.8% |
| deepseek-v4-flash | q5-log-analysis | 2 | 5477 | 3352 | 38.8% | 2863 | 47.7% |
| deepseek-v4-flash | q5-log-analysis | 3 | 4878 | 3420 | 29.9% | 2181 | 55.3% |
| deepseek-v4-flash | q6-longtext-summary | 1 | 1466 | 828 | 43.5% | 182 | 87.6% |
| deepseek-v4-flash | q6-longtext-summary | 2 | 966 | 467 | 51.7% | 1145 | -18.5% |
| deepseek-v4-flash | q6-longtext-summary | 3 | 2212 | 326 | 85.3% | 1036 | 53.2% |
| deepseek-v4-flash | q7-tcp-handshake | 1 | 2734 | 1026 | 62.5% | 1785 | 34.7% |
| deepseek-v4-flash | q7-tcp-handshake | 2 | 1431 | 2460 | -71.9% | 3045 | -112.8% |
| deepseek-v4-flash | q7-tcp-handshake | 3 | 1811 | 2512 | -38.7% | 1178 | 35.0% |
| deepseek-v4-flash | q8-isolation-levels | 1 | 1287 | 772 | 40.0% | 2949 | -129.1% |
| deepseek-v4-flash | q8-isolation-levels | 2 | 981 | 544 | 44.5% | 1488 | -51.7% |
| deepseek-v4-flash | q8-isolation-levels | 3 | 1166 | 1018 | 12.7% | 2993 | -156.7% |
| deepseek-v4-flash | q9-translate | 1 | 401 | 499 | -24.4% | 944 | -135.4% |
| deepseek-v4-flash | q9-translate | 2 | 470 | 517 | -10.0% | 1702 | -262.1% |
| deepseek-v4-flash | q9-translate | 3 | 364 | 1166 | -220.3% | 633 | -73.9% |
| deepseek-v4-flash | q10-rewrite-email | 1 | 760 | 181 | 76.2% | 1621 | -113.3% |
| deepseek-v4-flash | q10-rewrite-email | 2 | 1044 | 161 | 84.6% | 628 | 39.8% |
| deepseek-v4-flash | q10-rewrite-email | 3 | 108 | 106 | 1.9% | 1485 | -1275.0% |
| deepseek-v4-pro | q1-codegen-wordcount | 1 | 1579 | 1730 | -9.6% | 556 | 64.8% |
| deepseek-v4-pro | q1-codegen-wordcount | 2 | 983 | 887 | 9.8% | 466 | 52.6% |
| deepseek-v4-pro | q1-codegen-wordcount | 3 | 1352 | 2308 | -70.7% | 910 | 32.7% |
| deepseek-v4-pro | q2-codegen-walk | 1 | 1651 | 775 | 53.1% | 698 | 57.7% |
| deepseek-v4-pro | q2-codegen-walk | 2 | 1212 | 1081 | 10.8% | 743 | 38.7% |
| deepseek-v4-pro | q2-codegen-walk | 3 | 2003 | 1209 | 39.6% | 640 | 68.0% |
| deepseek-v4-pro | q3-sql-explain | 1 | 2559 | 1229 | 52.0% | 682 | 73.3% |
| deepseek-v4-pro | q3-sql-explain | 2 | 1865 | 1338 | 28.3% | 2294 | -23.0% |
| deepseek-v4-pro | q3-sql-explain | 3 | 2083 | 1692 | 18.8% | 1532 | 26.5% |
| deepseek-v4-pro | q4-go-review | 1 | 703 | 291 | 58.6% | 515 | 26.7% |
| deepseek-v4-pro | q4-go-review | 2 | 995 | 516 | 48.1% | 291 | 70.8% |
| deepseek-v4-pro | q4-go-review | 3 | 623 | 571 | 8.3% | 743 | -19.3% |
| deepseek-v4-pro | q5-log-analysis | 1 | 912 | 622 | 31.8% | 607 | 33.4% |
| deepseek-v4-pro | q5-log-analysis | 2 | 1174 | 759 | 35.3% | 972 | 17.2% |
| deepseek-v4-pro | q5-log-analysis | 3 | 1416 | 995 | 29.7% | 1239 | 12.5% |
| deepseek-v4-pro | q6-longtext-summary | 1 | 525 | 317 | 39.6% | 188 | 64.2% |
| deepseek-v4-pro | q6-longtext-summary | 2 | 255 | 203 | 20.4% | 1320 | -417.6% |
| deepseek-v4-pro | q6-longtext-summary | 3 | 690 | 1283 | -85.9% | 597 | 13.5% |
| deepseek-v4-pro | q7-tcp-handshake | 1 | 1145 | 1466 | -28.0% | 1153 | -0.7% |
| deepseek-v4-pro | q7-tcp-handshake | 2 | 1747 | 941 | 46.1% | 321 | 81.6% |
| deepseek-v4-pro | q7-tcp-handshake | 3 | 1227 | 1477 | -20.4% | 274 | 77.7% |
| deepseek-v4-pro | q8-isolation-levels | 1 | 1181 | 589 | 50.1% | 1100 | 6.9% |
| deepseek-v4-pro | q8-isolation-levels | 2 | 1308 | 787 | 39.8% | 313 | 76.1% |
| deepseek-v4-pro | q8-isolation-levels | 3 | 1493 | 651 | 56.4% | 656 | 56.1% |
| deepseek-v4-pro | q9-translate | 1 | 1014 | 664 | 34.5% | 1173 | -15.7% |
| deepseek-v4-pro | q9-translate | 2 | 475 | 589 | -24.0% | 380 | 20.0% |
| deepseek-v4-pro | q9-translate | 3 | 741 | 603 | 18.6% | 1207 | -62.9% |
| deepseek-v4-pro | q10-rewrite-email | 1 | 515 | 808 | -56.9% | 247 | 52.0% |
| deepseek-v4-pro | q10-rewrite-email | 2 | 1062 | 750 | 29.4% | 787 | 25.9% |
| deepseek-v4-pro | q10-rewrite-email | 3 | 611 | 438 | 28.3% | 169 | 72.3% |
| glm-5.1 | q1-codegen-wordcount | 1 | 1341 | 1060 | 21.0% | 341 | 74.6% |
| glm-5.1 | q1-codegen-wordcount | 2 | 1630 | 857 | 47.4% | 365 | 77.6% |
| glm-5.1 | q1-codegen-wordcount | 3 | 1548 | 1405 | 9.2% | 1111 | 28.2% |
| glm-5.1 | q2-codegen-walk | 1 | 1621 | 934 | 42.4% | 649 | 60.0% |
| glm-5.1 | q2-codegen-walk | 2 | 1299 | 1179 | 9.2% | 694 | 46.6% |
| glm-5.1 | q2-codegen-walk | 3 | 1073 | 1005 | 6.3% | 1491 | -39.0% |
| glm-5.1 | q3-sql-explain | 1 | 2173 | 1977 | 9.0% | 1555 | 28.4% |
| glm-5.1 | q3-sql-explain | 2 | 2419 | 2759 | -14.1% | 1446 | 40.2% |
| glm-5.1 | q3-sql-explain | 3 | 2042 | 2542 | -24.5% | 1830 | 10.4% |
| glm-5.1 | q4-go-review | 1 | 948 | 755 | 20.4% | 529 | 44.2% |
| glm-5.1 | q4-go-review | 2 | 984 | 677 | 31.2% | 290 | 70.5% |
| glm-5.1 | q4-go-review | 3 | 853 | 775 | 9.1% | 418 | 51.0% |
| glm-5.1 | q5-log-analysis | 1 | 2074 | 1967 | 5.2% | 897 | 56.8% |
| glm-5.1 | q5-log-analysis | 2 | 2075 | 1617 | 22.1% | 1581 | 23.8% |
| glm-5.1 | q5-log-analysis | 3 | 1804 | 2205 | -22.2% | 1501 | 16.8% |
| glm-5.1 | q6-longtext-summary | 1 | 1012 | 1185 | -17.1% | 449 | 55.6% |
| glm-5.1 | q6-longtext-summary | 2 | 1265 | 964 | 23.8% | 1072 | 15.3% |
| glm-5.1 | q6-longtext-summary | 3 | 1171 | 1111 | 5.1% | 370 | 68.4% |
| glm-5.1 | q7-tcp-handshake | 1 | 1703 | 1210 | 28.9% | 470 | 72.4% |
| glm-5.1 | q7-tcp-handshake | 2 | 1751 | 1750 | 0.1% | 720 | 58.9% |
| glm-5.1 | q7-tcp-handshake | 3 | 1398 | 1618 | -15.7% | 435 | 68.9% |
| glm-5.1 | q8-isolation-levels | 1 | 1627 | 1245 | 23.5% | 1470 | 9.6% |
| glm-5.1 | q8-isolation-levels | 2 | 1301 | 944 | 27.4% | 1269 | 2.5% |
| glm-5.1 | q8-isolation-levels | 3 | 1257 | 1536 | -22.2% | 1418 | -12.8% |
| glm-5.1 | q9-translate | 1 | 1253 | 1274 | -1.7% | 1060 | 15.4% |
| glm-5.1 | q9-translate | 2 | 985 | 1018 | -3.4% | 891 | 9.5% |
| glm-5.1 | q9-translate | 3 | 1224 | 1026 | 16.2% | 321 | 73.8% |
| glm-5.1 | q10-rewrite-email | 1 | 753 | 706 | 6.2% | 644 | 14.5% |
| glm-5.1 | q10-rewrite-email | 2 | 674 | 711 | -5.5% | 479 | 28.9% |
| glm-5.1 | q10-rewrite-email | 3 | 806 | 588 | 27.0% | 738 | 8.4% |
| qwen3.5-flash | q1-codegen-wordcount | 1 | 3595 | 3527 | 1.9% | 4379 | -21.8% |
| qwen3.5-flash | q1-codegen-wordcount | 2 | 4298 | 5162 | -20.1% | 3505 | 18.5% |
| qwen3.5-flash | q1-codegen-wordcount | 3 | 3329 | 4852 | -45.7% | 3271 | 1.7% |
| qwen3.5-flash | q2-codegen-walk | 1 | 2300 | 3570 | -55.2% | 3034 | -31.9% |
| qwen3.5-flash | q2-codegen-walk | 2 | 3983 | 1905 | 52.2% | 3532 | 11.3% |
| qwen3.5-flash | q2-codegen-walk | 3 | 3615 | 4321 | -19.5% | 2464 | 31.8% |
| qwen3.5-flash | q3-sql-explain | 1 | 3770 | 2642 | 29.9% | 6475 | -71.8% |
| qwen3.5-flash | q3-sql-explain | 2 | 3898 | 3556 | 8.8% | 3720 | 4.6% |
| qwen3.5-flash | q3-sql-explain | 3 | 4192 | 4216 | -0.6% | 2892 | 31.0% |
| qwen3.5-flash | q4-go-review | 1 | 2391 | 1553 | 35.0% | 3464 | -44.9% |
| qwen3.5-flash | q4-go-review | 2 | 1555 | 3109 | -99.9% | 2744 | -76.5% |
| qwen3.5-flash | q4-go-review | 3 | 4398 | 1517 | 65.5% | 3133 | 28.8% |
| qwen3.5-flash | q5-log-analysis | 1 | 2568 | 4502 | -75.3% | 4937 | -92.3% |
| qwen3.5-flash | q5-log-analysis | 2 | 2437 | 3284 | -34.8% | 2422 | 0.6% |
| qwen3.5-flash | q5-log-analysis | 3 | 3079 | 3707 | -20.4% | 3236 | -5.1% |
| qwen3.5-flash | q6-longtext-summary | 1 | 2150 | 2593 | -20.6% | 2048 | 4.7% |
| qwen3.5-flash | q6-longtext-summary | 2 | 1541 | 3251 | -111.0% | 2821 | -83.1% |
| qwen3.5-flash | q6-longtext-summary | 3 | 1630 | 1524 | 6.5% | 2434 | -49.3% |
| qwen3.5-flash | q7-tcp-handshake | 1 | 3221 | 2826 | 12.3% | 3170 | 1.6% |
| qwen3.5-flash | q7-tcp-handshake | 2 | 3920 | 2482 | 36.7% | 4283 | -9.3% |
| qwen3.5-flash | q7-tcp-handshake | 3 | 2579 | 3606 | -39.8% | 2247 | 12.9% |
| qwen3.5-flash | q8-isolation-levels | 1 | 2626 | 2106 | 19.8% | 2743 | -4.5% |
| qwen3.5-flash | q8-isolation-levels | 2 | 1575 | 3557 | -125.8% | 3659 | -132.3% |
| qwen3.5-flash | q8-isolation-levels | 3 | 1769 | 1351 | 23.6% | 3696 | -108.9% |
| qwen3.5-flash | q9-translate | 1 | 2408 | 2730 | -13.4% | 3979 | -65.2% |
| qwen3.5-flash | q9-translate | 2 | 1665 | 2008 | -20.6% | 3494 | -109.8% |
| qwen3.5-flash | q9-translate | 3 | 3363 | 2133 | 36.6% | 3609 | -7.3% |
| qwen3.5-flash | q10-rewrite-email | 1 | 1025 | 2193 | -114.0% | 4561 | -345.0% |
| qwen3.5-flash | q10-rewrite-email | 2 | 1362 | 2095 | -53.8% | 3078 | -126.0% |
| qwen3.5-flash | q10-rewrite-email | 3 | 2073 | 1621 | 21.8% | 5027 | -142.5% |
