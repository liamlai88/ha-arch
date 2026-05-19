# 阶段三总结：API 网关 + 异步削峰 + Kafka 集群 HA

> **目标**：在阶段二（高可用数据层 + 监控）基础上，解决两类流量问题——**坏流量**（恶意刷、未授权、上游挂）靠 API 网关挡，**好流量但太多**（突发写）靠 MQ 削峰。最后把 Kafka 单节点升级成 3-broker 副本集，亲眼看 partition leader 自动迁移。

---

## 一、整体架构（26 容器拓扑）

```
                       ┌──────────────┐
              k6 ──▶   │   APISIX     │  限流 / 鉴权 / 熔断 / 路由
                       │   :9080      │
                       └──────┬───────┘
                              ▼
                       ┌──────────────┐
                       │   Nginx LB   │
                       └──────┬───────┘
                              ▼
                  ┌───────────┴───────────┐
                  │                       │
            ┌──────────┐           ┌──────────┐
            │  app1    │           │  app2    │   (/product 同步)
            │ (Go)     │           │ (Go)     │   (/order async ★)
            └────┬─────┘           └────┬─────┘   (/order/sync 对照)
                 │                      │
        ┌────────┼──────────┬───────────┼────────┐
        │        │          │           │        │
        ▼        ▼          ▼           ▼        ▼
   Redis     ProxySQL   Kafka 集群    Prometheus
  Sentinel    + 主从     3 broker
                         3 part × RF=3
                              │
                              ▼
                       ┌──────────────────────┐
                       │ order-worker × 2     │ 慢消费 200/s
                       │ (3 part / 2 worker)  │
                       └──────────┬───────────┘
                                  ▼
                              MySQL 主从
```

**容器清单**（**26 个**，比阶段二多 7 个）：

| 增量 | 容器 |
|---|---|
| API 网关 | apisix |
| MQ 集群 | kafka1, kafka2, kafka3, kafka-ui |
| 消费者 | order-worker, order-worker-2 |

---

## 二、子阶段 3.1：APISIX API 网关

### 设计原则

> **网关层做策略、Nginx 做转发**。生产可以让 APISIX 直接当 LB（替代 Nginx），但分两层有教学和审计价值。

### 关键决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 部署模式 | **Standalone (apisix.yaml)** | 学习场景免 etcd，少 1 组件 |
| Admin Key | 固定明文 (`ha-arch-admin-key`) | 演示用；生产用 Vault 注入 |
| 路由抽象 | route + upstream + plugins | 跟生产对齐 |
| 启用插件 | 显式 plugins 白名单 | 安全审计，但**会替换默认列表**（坑见后） |

### 三大演示能力 + 实测数据

#### 1. 限流（漏桶算法 limit-req）

配置：`rate: 10 burst: 5`，按 IP 限。

| 测试 | 结果 |
|---|---|
| k6 100 RPS × 15s 攻击 | accepted **155**（≈10/s×15s+burst） |
| 拒绝的 429 数量 | **1345**（90% 流量被挡） |
| **被限请求的 upstream_addr** | **空**（请求根本没到 Nginx） |
| 对照组（直打 Nginx 8080） | 30/30 全 200，无限流 |

**面试金句**：被限的请求 `upstream_addr=""`——这就是网关存在的意义，**是后端的盾牌**。

#### 2. 鉴权（key-auth + Consumer 抽象）

设计：`/secure/product` 必须带 `Apikey: secret-key-123` header，路由到 Nginx 时改写成 `/product`。

| 测试 | 结果 |
|---|---|
| 不带 token | **401** Missing API key |
| 带错误 token | **401** Invalid API key |
| 带正确 token | **200** + consumer="demo_user" 标签 |

**Prometheus 指标自动打上 consumer 标签**——可以基于 consumer 做审计和计费。

#### 3. 熔断（api-breaker）—— **阶段 3.1 的高光时刻**

配置：连续 3 次 5xx → 触发熔断，最多熔断 5 秒。

**故障演练**：`docker stop ha-arch-nginx` 后立刻打 8 个请求：

| 请求 | 状态 | 耗时 |
|---|---|---|
| req 1 (首次失败) | 502 | **38.7 秒** ⚠️（TCP 重传到内核 timeout） |
| req 2-3 (熔断已触发) | 503 | 0.03-0.06 秒 🚀 |
| req 4-6 (熔断持续) | 503 | 0.01-0.10 秒 |
| **req 7 (半开试探)** | 502 | 3.1 秒 👀 |
| req 8 (再熔断) | 503 | 0.003 秒 |

**核心数据**：**38.7s → 0.003s，加速 ~12,000 倍**。

**恢复测试**（启动 Nginx + 等 6s）：5/5 全 200，平均 4ms 响应。

**面试金句**：

> 熔断不让请求成功，**只让失败变得便宜**。同样是 503，**等 3 秒 vs 立即** 对调用方的雪崩防护差几个数量级。熔断 ≈ 传染病防控，不是治病。

### 限流 vs 熔断的本质区别

| | 限流 | 熔断 |
|---|---|---|
| 保护谁 | **后端** | **自己（调用方）** |
| 触发 | 入口流量太多 | 后端**已经不健康** |
| 行为 | 拒绝超额请求 | **快速失败**所有请求 |
| 类比 | 餐厅取号叫号 | 厨房着火立刻打发客人走 |

---

## 三、子阶段 3.2：Kafka 削峰

### 拓扑变化

```
client ──▶ POST /order      (async path) ──▶ Kafka ──▶ worker ──▶ MySQL
client ──▶ POST /order/sync (sync path)  ─────────────────▶ MySQL
```

### 关键技术决策

| 决策 | 选择 | 理由 |
|---|---|---|
| MQ | **Kafka (KRaft 单节点初版)** | 业界标杆；KRaft 免 ZK |
| 客户端 | **`segmentio/kafka-go`** | 纯 Go，性能好，无 cgo |
| Worker 部署 | 独立服务 | 生产典型形态（producer/consumer 解耦） |
| 消费速率 | **人为限到 200 msg/s** | 故意慢消费，看削峰效果 |
| Kafka UI | provectuslabs/kafka-ui | 浏览器看 topic/lag |
| acks | **all + Hash 分区** | 强一致 + 同 user 保序 |

### 实测数据汇总（这才是最有价值的部分）

> 注：表中 RPS 为 producer 端，DB 实际承压由 worker rate 决定。

#### 测试 1：基线（1K RPS × 10s = 10K 订单）

| 指标 | 异步 (/order) | 同步 (/order/sync) | 谁赢 |
|---|---|---|---|
| 成功率 | 100% | 100% | 平 |
| P95 延迟 | 11.61 ms | **4.01 ms** | 同步快 3 倍 |
| 客户端体验 | 慢一点 | 快 | 同步 |

**反常识发现**：1K RPS 下**同步反而比异步快**——Kafka 多一跳网络往返。

#### 测试 2：5K RPS

| 指标 | 异步 | 同步 |
|---|---|---|
| 成功率 | 100% | 100% |
| **P95 延迟** | **11.9 ms** | **19.2 ms** ⬆ |
| Kafka LAG 峰值 | ~47800 | — |

**延迟交叉点出现**：3-4K RPS 之间。**异步延迟恒定（11.9ms），同步随负载线性升高**。

#### 测试 3：连接池打满演练（DB_MAX_CONNS=5，5K RPS）

| 指标 | 同步 | 异步 |
|---|---|---|
| 成功率 | **62.20%** 💀 | **99.94%** ✅ |
| 失败数 | 13,685 + 13,791 dropped | 30 |
| **P95 延迟** | **1,910 ms** | **11.97 ms** |
| Max 延迟 | **5 秒（超时）** | 24.9 ms |
| 平均延迟 | 386 ms | 8.5 ms |

**185 倍 P95 差距**。

#### 数据故事弧（这是面试可以讲完整的）

```
延迟 (ms)
   │              ┌─ 同步：线性恶化（连接池竞争）
2000│            ↗
   │           ↗
   │          /
 20│      ↗  
   │   ↗
 10│ ────────────────  异步：恒定 ~12ms（Kafka 不挑食）
   │
    └──────────────────── RPS
    1K  3K  5K
    交叉点
```

### 异步化的真正意义（核心金句）

> **MQ 不是为了"提速"，是为了"扛峰值 + 稳定延迟"**。
>
> - 同步系统延迟随负载剧烈波动（4ms → 1.9 秒）
> - 异步系统延迟恒定（~12ms，与 RPS 无关）
> - 生产 SLO 通常承诺 **P99 < X ms** —— 这就是异步的舞台

### 异步化的代价（trade-off）

| 代价 | 解释 | 场景 |
|---|---|---|
| **最终一致性** | 用户下单返回 202 后 DB 才慢慢写入 | 下单后立刻查"我的订单"可能看不到 |
| **失败处理复杂** | 消费失败要 DLQ、重试、幂等 | 必须设计补偿逻辑 |
| **追踪困难** | producer trace + consumer trace 要拼 | 全链路 trace 设计变复杂 |

**绝对不能异步的场景**：
- 支付授权（用户必须立刻知道结果）
- 库存扣减（不能让两个人都抢到最后 1 件）
- 登录鉴权（不能"先放进来，回头验证"）

---

## 四、子阶段 3.2 加餐：Kafka 多 Broker + 副本集

### 升级目标

```
1 broker, 1 partition, RF=1
   ↓
3 broker, 3 partitions, RF=3
+ acks=all + min.insync.replicas=2
```

### 三个核心概念

#### ① Partition（吞吐量并行单位）

- 不同 partition 可以在不同 broker 上（水平扩容）
- 单 partition 内严格保序，跨 partition 不保序
- **partition 数 = consumer group 并发上限**

#### ② Replication Factor（可用性单位）

- 每 partition 一个 Leader + (RF-1) 个 Follower
- 写只能去 Leader，Follower 异步拉取
- Leader 挂了从 ISR 选新 Leader

#### ③ ISR (In-Sync Replicas) —— **面试必考**

```
acks=all + min.insync.replicas=2 + RF=3 →
  "写入返回成功 = 至少 2 个副本已持久化"
  哪怕 1 个 broker 立刻挂，数据还在另外 1 个上，不丢
```

### 实测数据：Partition 自动均衡

| Partition | Leader | Replicas | ISR |
|---|---|---|---|
| P0 | broker 3 | [3,1,2] | [3,1,2] |
| P1 | broker 1 | [1,2,3] | [1,2,3] |
| P2 | broker 2 | [2,3,1] | [2,3,1] |

**每个 broker 各当 1 个 partition 的 leader**——Kafka 自动负载均衡。

### 消费组分配（3 partition / 2 worker）

| Worker | 拥有 partition | 数量 |
|---|---|---|
| worker-1 | **P1 + P2** | 2 |
| worker-2 | P0 | 1 |

**铁律**：一个 partition 同时只能被同组内的 1 个 consumer 消费。

### 故障演练：kill broker2（P2 的 leader）

#### 拓扑变化

| Partition | 故障前 | 故障后 |
|---|---|---|
| P0 | Leader=3, ISR=[3,1,2] | Leader=3, **ISR=[3,1]** |
| P1 | Leader=1, ISR=[1,2,3] | Leader=1, **ISR=[1,3]** |
| **P2** | **Leader=2**, ISR=[2,3,1] | **Leader=3** ★, ISR=[3,1] |

**P2 leader 自动从 2 迁到 3**——几秒内完成，零人工干预。

#### Producer 影响

| 指标 | 值 |
|---|---|
| 成功率 | **91.62%** |
| 失败数 | 799 |
| P95 延迟 | **445.34 ms**（vs 平时 11ms） |
| Max | 1.88 s |

**8% 失败的根因**：`acks=all` 严格等待 → broker2 切换的 metadata 刷新窗口（~2-3 秒）内 producer 还在发往旧 leader。生产解决：
- 应用层幂等 + 重试
- `metadata.max.age.ms` 调小

#### Consumer 影响

实测：**worker 持续消费、MySQL count 持续增长**。

但要诚实地说——**worker-1 的 P2 消费有 2-3 秒"小停顿"**：
- P0/P1 leader 未变 → worker-2 / worker-1 完全无感
- P2 leader 迁移期间 → worker-1 的 kafka-go 客户端要重新发现 leader、重连
- 因为速率 200/s × 3s = 600 条延迟、不是丢失，**总消费数仍在涨**

**理解 consumer HA 的关键**：consumer 不是"连 broker 集群"，是**"直连每个 partition 的 leader broker"**。挂掉 leader 才有 2-3 秒重连，挂掉 follower 完全无感。

#### 恢复（broker2 回归）

```
ISR: [3,1] → [3,1,2]    ← kafka2 重新加入 ✅
P2 Leader: 3 → 3        ← 不抢回（默认行为，避免无必要迁移）
```

要让 leader 抢回原位置：`kafka-leader-election.sh --election-type preferred`。

### 极限测试理论：杀掉 2 个 broker 会怎样

| 端 | 影响 | 原因 |
|---|---|---|
| Producer 写 | **全部失败** (`NotEnoughReplicasException`) | ISR 只剩 1 < min.insync.replicas=2 |
| Consumer 读 P0/P2 (leader=幸存的 broker3) | ✅ 仍可用 | broker3 有完整数据 |
| Consumer 读 P1 (leader=死的 broker1) | ❌ **阻塞** | quorum 不够，**选不出新 leader** |

**根因**：KRaft 控制面也用 quorum=2/3。挂 2 个 → controller 没多数派 → **不敢做任何元数据决策**。

### Quorum 公式（必背）

> **N broker 能扛 (N-1)/2 个 broker 同时挂**

| 集群规模 | 容错 | 适用场景 |
|---|---|---|
| 3 节点 | 挂 1 | 学习、单可用区 |
| **5 节点** | **挂 2** | **生产标配，跨可用区** |
| 7 节点 | 挂 3 | 金融业务，跨地域 |

**为什么必须多数派 (quorum > N/2)**：防脑裂。任意两个 quorum 集合必有交集——任意时刻最多一边凑齐 quorum，另一边主动拒绝服务，**数据永不冲突**。

---

## 五、踩坑记录（这章再次成为最重要的章节）

| # | 坑 | 现象 | 根因 | 解决 |
|---|---|---|---|---|
| 1 | APISIX plugins 白名单是"替换"不是"追加" | `proxy-rewrite` URI 改写不生效，返回 404 | `config.yaml` 里的 `plugins:` 列表替换了 APISIX 的默认插件集 | 显式把 `proxy-rewrite` 加进列表 |
| 2 | Kafka consumer 启动时序坑 | worker 启动后什么都不消费 | worker 启动时 `orders` topic 还不存在，kafka-go 订阅失败后**不会自动重试** | **重启 worker**，或预创建 topic，或在代码层加重订阅 |
| 3 | Kafka 多 broker `INCONSISTENT_CLUSTER_ID` | 3 broker 互相 reject vote | bitnami 镜像没设 `KAFKA_KRAFT_CLUSTER_ID`，每个 broker 自动生成各自的 cluster ID | 显式给**所有 broker** 设同一个 `KAFKA_KRAFT_CLUSTER_ID` |
| 4 | `replace_all` 没替换全 | 改了 1 处，剩 2 处仍然漏 | old_string 包含的注释只在第 1 个 broker 出现 | 手动 Edit 剩余 2 处，或者改用 `grep -c` 验证 |
| 5 | DB 连接池排队超时 | 5K RPS sync 模式 62% 失败 | `db.SetMaxOpenConns(5)`，Go 的 `db.Exec` 会**阻塞**等连接，达 HTTP timeout 后超时 | 这就是要演示的——异步路径完美避开 |
| 6 | acks=all 故障窗口下 8% 写入失败 | broker 切换瞬间 producer 持续报错 | producer 还在等死掉的 broker ack，metadata 没刷新 | 应用层加幂等重试 + `metadata.max.age.ms` 调小 |

### 第 3 个坑的深度教训

> **KRaft 模式把元数据完全去中心化的代价**：所有 broker 必须先达成"我们是同一个集群"的共识——靠应用层确保 cluster ID 一致。**ZK 模式没这问题**，cluster ID 由 ZK 协调。

---

## 六、四个核心反直觉结论（写进简历）

### ① 网关的核心价值不是"做更多"，是"挡掉坏的"
被限的请求 `upstream_addr=""`——它根本没到达后端。**网关是后端的盾牌**，不是简单的转发层。

### ② 熔断不让请求成功，让失败便宜
**38.7s → 0.003s**——同样是 503，对调用方的雪崩防护差 12,000 倍。

### ③ MQ 不为提速，为扛峰值 + 稳定延迟
异步在 1K RPS 反而慢 3 倍。**异步的舞台是 5K+ RPS + 严格 P99 SLO**。

### ④ 分布式共识不是"剩几个能跑"，是"必须多数派同意"
5 节点挂 3 个 = 死。挂 2 个 = 活。差别在"多数派"3 票，不在"剩了 2 个还能工作"。

---

## 七、关键演练数据汇总（一表打天下）

| 演练 | 关键数字 | 学到什么 |
|---|---|---|
| **APISIX 限流** | 100→10 RPS，1345 个 429 | 漏桶算法 + 按 IP 限 |
| **APISIX 鉴权** | 401 / 200 完美区分 | consumer 抽象 + 401 也带标签 |
| **APISIX 熔断** | **38.7s → 0.003s** | 快速失败 = 雪崩防护 |
| **熔断恢复** | 5/5 200，4ms 响应 | 半开机制 + 指数补偿 |
| **Kafka 1K RPS 同步** | P95 4ms（赢） | 低 QPS 下 MQ 是冗余 |
| **Kafka 5K RPS 异步** | P95 12ms（恒定） | 异步延迟稳定性 |
| **连接池打满** | sync 62% 失败 vs async 99.94% | 真实生产事故场景 |
| **Kafka leader 迁移** | P2: 2→3，几秒完成 | partition HA |
| **Kafka 副本同步** | ISR [3,1,2] → [3,1] → [3,1,2] | RF=3 + min.isr=2 = 不丢 |
| **Producer 故障窗口** | 8.4% 失败（acks=all） | CP 的可用性代价 |
| **Consumer 故障** | 短暂 2-3s 重连 P2 | consumer 直连 leader |

---

## 八、阶段三**没**解决的问题（留给阶段四 / 五）

| 问题 | 解决在 |
|---|---|
| 扩容靠手改 yaml | **阶段 4**：k3d + HPA |
| MySQL failover 决策仍要人工 | **阶段 5**：Orchestrator |
| 没有跨地域容灾 | **阶段 5**：多地域部署 |
| 没有混沌工程 | **阶段 5**：Chaos Mesh |
| 没有真正的全链路追踪 | **阶段 5**：Jaeger 集成 |
| 流量倍数还能怎么打？ | 加 Kafka 副本 + APISIX 集群（已有 partition，待扩展 broker） |

---

## 九、面试包装话术

> 阶段三在阶段一二（基础架构 + 数据层 HA）之上加了三件事：
>
> 1. **API 网关 APISIX**：限流、鉴权、熔断三大策略上线。最有意思的是熔断演练——故障时单请求延迟从 38.7 秒（TCP 内核 timeout）降到 0.003 秒（APISIX 直接返回 503）——**加速 12,000 倍**。熔断的本质不是治病，是传染病防控。
>
> 2. **Kafka 异步削峰**：新增 `/order` 异步路径 + `/order/sync` 同步对照。基线发现**1K RPS 同步反而比异步快**（4ms vs 12ms）——MQ 多一跳是冗余。但在**连接池打满场景下**（DB_MAX_CONNS=5, 5K RPS），同步 62% 失败、P95 1.9 秒；异步 99.94% 成功、P95 12ms。**MQ 的舞台是峰值 + SLO，不是低流量**。
>
> 3. **Kafka 升级 3-broker 副本集**：3 partition × RF=3 + acks=all + min.insync.replicas=2。**kill broker2 后 partition leader 自动迁移**（P2: 2→3），producer 故障窗口 8% 失败、consumer 几乎无感（只读 leader）。**5 个 broker 挂 2 个就死**——这是 quorum=多数派的硬性约束，防脑裂用。
>
> 这个过程最深的体感：**架构调优的本质是 trade-off**——acks=all 给你强一致，但故障窗口 8% 写入失败；min.insync.replicas=2 给你不丢数据，但挂 2 broker 就停机。**生产工程没有"全胜"的方案，只有"用什么代价换什么收益"的清醒决策**。

---

## 十、文件清单（阶段三新增/修改）

```
ha-arch/
├── PHASE1.md / PHASE2.md / PHASE3.md     ← 本文档
│
├── docker-compose.yml                     ← 26 服务
│
├── apisix/                                ← ★ 新建
│   ├── config.yaml                        ← Standalone 模式 + admin key + plugins 白名单
│   └── apisix.yaml                        ← 5 条路由（限流/鉴权/熔断/异步/对照）
│
├── app/
│   ├── go.mod                             ← 加 segmentio/kafka-go
│   └── main.go                            ← 新增 /order 异步 + /order/sync 同步端点
│
├── order-worker/                          ← ★ 新建消费者服务
│   ├── go.mod
│   ├── main.go                            ← Kafka reader + MySQL writer + 200/s 限速 + WORKER_ID
│   └── Dockerfile
│
├── mysql/
│   └── init-master.sql                    ← 加 orders 表
│
├── prometheus/
│   └── prometheus.yml                     ← 加 apisix job
│
└── loadtest/
    ├── limit-test.js                      ← APISIX 限流测试 (100 RPS)
    ├── order-async.js / order-async-5k.js ← 异步路径压测
    └── order-sync.js / order-sync-5k.js   ← 同步路径对照压测
```

---

## 十一、命令速查

```bash
cd ~/ha-arch

# 启动
docker-compose up -d --build

# APISIX 演示
curl -i 'http://localhost:9080/product?id=1'                    # 正常
curl -i 'http://localhost:9080/secure/product?id=1'             # 401
curl -i 'http://localhost:9080/secure/product?id=1' -H 'Apikey: secret-key-123'   # 200
k6 run loadtest/limit-test.js                                   # 限流演示

# 熔断演示
docker stop ha-arch-nginx
curl -w "%{http_code} in %{time_total}s\n" 'http://localhost:9080/product?id=1'
docker start ha-arch-nginx

# Kafka 削峰演示
docker exec ha-arch-mysql-master mysql -uroot -proot -e "TRUNCATE shop.orders"
k6 run loadtest/order-async.js                                  # 异步
k6 run loadtest/order-sync-5k.js                                # 同步 5K（带 DB_MAX_CONNS=5 时 62% 崩）

# Kafka 多 broker 演示
docker exec ha-arch-kafka1 kafka-topics.sh --bootstrap-server kafka1:9092 \
  --describe --topic orders                                     # 看 leader 分布

docker stop ha-arch-kafka2                                      # 杀 broker
docker exec ha-arch-kafka1 kafka-topics.sh --bootstrap-server kafka1:9092 \
  --describe --topic orders                                     # 看 leader 迁移
docker start ha-arch-kafka2                                     # 恢复

# 监控
open http://localhost:9080                                      # APISIX (业务流量)
open http://localhost:9091/apisix/prometheus/metrics            # APISIX 指标
open http://localhost:8081                                      # Kafka UI
open http://localhost:3000/d/ha-arch-main                       # Grafana
```
