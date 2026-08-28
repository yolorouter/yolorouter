# 输出精简基准实验

[English](concise-output-benchmark.md)

成本优化页会给「输出精简」开关估一个节省金额（这个开关就是全局系统提示词，
要求模型回答简洁）。口径是所选时间段的总量：取该时段内已定价流量的输出费用
与输出 token 数，各乘一个固定系数 **12.6%**。本文件记录这个 12.6% 是怎么测出
来的，全部原始数据附在文末。

## 测法

10 个固定问题（见下），每题 3 轮，模型 5 个：claude-opus-4-7、
deepseek-v4-flash、deepseek-v4-pro、glm-5.1、qwen3.5-flash。每个模型先关着
开关把 30 道题跑完，然后通过后台自己的设置接口把开关打开，同样的 30 道再跑一
遍。共 300 次调用，无一失败，配成 150 对——每对对应一组（模型，题目，轮次）。

开的那次装的就是后台打开开关时写入的原文，一字未改：

> 回答请保持简洁，去掉客套话和不必要的铺垫，保留完整语法和技术细节。优先复用标准库和平台已有能力，非必要不引入新的抽象或依赖，用最短的可行改动解决问题。

两句都装，因为开关本来就两句都写。第二句讲的是「优先复用已有平台能力、用最短的
可行改动解决问题」，它压缩代码类回答的力度不比第一句压缩行文的力度小。后台按自
身语言写入，所以英文后台装的是英文措辞——那套措辞不在这次测量里。

采样参数用各模型默认值。每对算 `r = (off_tokens − on_tokens) / off_tokens`，
token 数取上游回报的 `completion_tokens`。推理模型的思考 token 也在内，因为
那部分照样计费。出厂系数取全部 150 对 r 的中位数。

测量日期：2026-08-24。

## 结果

| | |
| --- | --- |
| 中位数（出厂系数） | **+12.6%** |
| 25 / 75 分位 | −4.0% / +27.2% |
| 最小 / 最大 | −192.8% / +87.2% |
| 负值对 | 150 对中 44 对 |

按模型的中位数：

| 模型 | 中位数 |
| --- | --- |
| claude-opus-4-7 | +3.6% |
| deepseek-v4-flash | +18.6% |
| deepseek-v4-pro | +27.4% |
| glm-5.1 | +11.3% |
| qwen3.5-flash | +10.6% |

五个模型都是正的，但每个模型内部的离散度都很大，而且不是多测几次就能抹平的
噪声。150 对里有 44 对是负的——那一次提示词把回答弄长了。单对之间摆动很凶：
同一模型同一题，一轮 −193%，另一轮 +46%。这个数该按「一个月的流量」去理解，
不是按单次请求。

只发一个全局系数，不做分模型表——模型迭代比表格保鲜期快，页面上也标了这是估算。
另外这个数的前提是开关装的就是它自己写入的那段提示词；如果你通过 API 把文本换成
了不相干的内容，这个数就不适用了。

## 题目

固定不变，每次同一套。

1. **[代码生成]** 用 Go 实现一个函数 WordCount(text string, topN int)，统计一段英文文本中每个单词出现的次数，忽略大小写，返回按出现次数降序排列的前 topN 个 (word, count)。请给出完整可运行的代码，包含必要的 import 和一个简单的 main 示例。
2. **[代码生成]** 用 Python 写一个命令行脚本，递归遍历指定的目录，找出最近 7 天内被修改过且大小超过 10MB 的文件，按大小降序打印它们的路径和大小（人类可读格式）。请给出完整代码和使用说明。
3. **[代码解释]** 请解释下面这段 SQL 做了什么，并指出它可能存在的性能问题与改进方法：SELECT u.id, u.username, u.email, COUNT(o.id) AS order_count, SUM(o.amount) AS total FROM users u LEFT JOIN orders o ON o.user_id = u.id AND o.status = 'paid' WHERE u.created_at > '2026-01-01' AND u.status <> 'deleted' GROUP BY u.id, u.username, u.email HAVING COUNT(o.id) > 5 ORDER BY order_count DESC LIMIT 100;
4. **[代码审查]** 请审查下面这段 Go 代码，指出所有你发现的问题（错误处理、资源泄漏、边界条件等）：func readConfig(path string) ([]byte, error) { f, err := os.Open(path); if err != nil { return nil, err } b, err := io.ReadAll(f); return b, err }
5. **[日志分析]** 以下是某个 API 服务一小时内的访问日志采样（格式：时间 方法 路径 状态码 耗时ms）。请分析流量特征、发现异常并给出运营建议：10:01 GET /v1/models 200 12；10:01 POST /v1/chat/completions 200 3421；10:02 POST /v1/chat/completions 429 5；10:02 POST /v1/chat/completions 200 5410；10:03 GET /v1/models 200 9；10:03 POST /v1/chat/completions 500 78；10:04 POST /v1/chat/completions 200 4102；10:05 GET /health 200 1；10:05 POST /v1/chat/completions 200 6230；10:06 POST /v1/chat/completions 429 4；10:07 POST /v1/chat/completions 200 3871；10:08 GET /v1/models 200 11；10:09 POST /v1/chat/completions 200 5540；10:10 POST /v1/chat/completions 502 120
6. **[长文总结]** 请阅读下面这段产品介绍并输出结构化要点总结（目标用户、核心功能、定价模式、差异化优势各不超过三条）：『YoloRouter 是一个面向开发者的 AI 模型路由网关。它把多家上游模型服务聚合在一个 OpenAI 兼容接口后面，调用方只需要把 base_url 指向网关即可切换模型。网关内置了供应商级故障转移：同一个模型可以配置多个供应商映射，请求失败时自动降级到下一家，调用方无感知。它还提供 API Key 管理、按账号的用量统计和预算限额，团队可以给每个成员发独立的 key 并设置月度预算。部署形态是单个二进制文件，内置 SQLite，开箱即用，也支持 PostgreSQL。价格按上游成本加收 5% 服务费，没有月费。』
7. **[知识问答]** 为什么 TCP 建立连接需要三次握手而不是两次？请从协议设计的目标（防止历史连接、同步双方初始序列号、确认双向通信能力）出发解释，并说明两次握手会出什么问题。
8. **[知识问答]** 请解释数据库事务的四个隔离级别，每个级别分别解决了什么问题、还存在什么异常（脏读、不可重复读、幻读），并说明 MySQL InnoDB 的默认级别及其实现方式。
9. **[翻译]** 请把下面这段英文技术文档翻译成中文，保留术语准确性：『The gateway normalizes every inbound request into an intermediate representation before dispatching it to an upstream provider. This decouples the ingress protocol spoken by the caller from the egress protocol spoken by the provider, so a new provider can be added without touching any caller-side code. Streaming responses are relayed chunk-by-chunk with backpressure, and usage reported by the upstream is reconciled into the audit log at settlement time.』
10. **[改写]** 请把下面这封邮件改写得简洁专业，保留全部关键信息（时间、地点、议程、需要的准备）：『hi 大家好，是这样的，我们本来定在下周三下午的开会时间，因为会议室被占了，所以现在改到周四上午十点了，地点还是老地方 B 栋 301。这次会议主要想跟大家同步一下二季度的进度，然后讨论一下下个季度的计划，另外呢，麻烦大家提前把自己负责模块的数据准备好，最好是能发我一份，我在会上统一汇总，谢谢大家配合，有什么问题随时找我。』

## 原始数据——全部 150 对

`off` / `on` 为上游回报的 `completion_tokens`（含思考 token）；`r = (off − on) / off`。r 为正表示提示词让输出变短了。

| 模型 | 题目 | 轮次 | off | on | r |
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

## 复现

在自己实例上按同样流程跑：题目固定 10 道、3 轮，先关着开关全跑一遍，再打开
开关全跑一遍，最后取配对比值的中位数。具体数字肯定对不上（模型一直在变），但
形态应该一致：中位数为正，但周围分布很宽，且有相当一部分对是负的。


## 附录：与激进风格提示词的对照复测（2026-08-28）

[caveman](https://github.com/JuliusBrussee/caveman) 是一个流传很广的「用原始
人电报体回答」技能，介绍里声称能砍掉 65% 的输出 token。为了检验开关那两句提
示词是否真的留了这么大空间没吃到，这次把基准从两组扩成三组：

- **off** —— 无系统提示词（基线重新实测，不复用 2026-08-24 的数据）
- **两句提示词** —— 后台写入的原文，同上文
- **caveman** —— caveman `SKILL.md` 全文正文（去掉 YAML frontmatter，约
  6.5 KB 英文规则），原样作为系统提示词注入。2026-08-28 取自 commit
  [`b433570`](https://github.com/JuliusBrussee/caveman/blob/b4335705d436f5110386a1c39c6d8aed5002aeeb/skills/caveman/SKILL.md)。

同样 10 道冻结题、同样 5 个模型、3 轮，共 450 次调用，无一失败。方法上有一
点差异需要说明：这次 claude-opus-4-7 的调用走的是流式——它所在的上游端点对
非流式长请求会超时；无论流式与否，`completion_tokens` 都取上游回报的 usage，
口径不变。每组 `r` 仍按（模型，题目，轮次）配对计算。按 token 加权的节省 =
`1 −（该组总量 ÷ off 总量）`，三组 `completion_tokens` 总量分别为 off
244,619、两句 206,726、caveman 236,818。

| | 两句提示词 | caveman |
| --- | --- | --- |
| 节省中位数 | **+12.8%** | **+16.4%** |
| 25 / 75 分位 | −3.4% / +31.2% | −12.8% / +41.0% |
| 最小 / 最大 | −220.3% / +90.4% | −1275.0% / +87.6% |
| 负值对 | 42 / 150 | 45 / 150 |
| 按 token 加权的节省 | **+15.5%** | **+3.2%** |

按模型的中位数：

| 模型 | 两句提示词 | caveman |
| --- | --- | --- |
| claude-opus-4-7 | +12.8% | +19.4% |
| deepseek-v4-flash | +33.9% | +10.8% |
| deepseek-v4-pro | +28.8% | +33.1% |
| glm-5.1 | +9.1% | +34.6% |
| qwen3.5-flash | −16.5% | −15.5% |

怎么读：

- 两句提示词这轮中位数 +12.8%，与四天前测得的出厂系数 12.6% 基本重合。系数
  维持不变。
- caveman 的中位数是 +16.4%——只比两句提示词高 3.6 个百分点，离 65% 相去甚
  远。这与 caveman 自己的
  [honest-numbers 页面](https://github.com/JuliusBrussee/caveman/blob/ee3199bca81d388d13ec508ccf7f7b318b905c8a/docs/HONEST-NUMBERS.md)
  一致：该页明确写着这个技能没有已发布的、经审核的输出降幅汇总数据。
- 激进风格的尾部差得多。最差的一对把 108 token 的邮件改写膨胀成 1485 token
  （−1275%）；若干推理模型的配对以同样方式爆炸，很可能是模型对着 6.5 KB 的
  规则书花了大量思考 token——而思考 token 照样计费。按实际付费的 token 加权，
  caveman 只省 3.2%，两句提示词省 15.5%。
- 风格提示词本身就是输入：每次请求多带约 6.5 KB（≈1.5k token），而两句提示
  词只有约 75 字。交互一短，这笔固定开销就能吃掉全部输出节省。

### 复测原始数据——全部 150 组三元对

`off` / `ours` / `caveman` 为上游回报的 `completion_tokens`（含思考
token）；每个 `r` 都相对同行的 `off` 计算。

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
