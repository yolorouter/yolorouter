# Concise-output benchmark

[中文版](concise-output-benchmark_zh.md)

The cost-optimization page estimates what the Concise Output switch saves
(a global system prompt that asks models to keep replies short). The
estimate is an amount per 1M output tokens: the weighted output price of
your priced traffic, times a fixed coefficient of **12.6%**. This file
records how that 12.6% was measured. The complete raw data is at the
bottom.

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
