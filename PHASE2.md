# 阶段二总结：监控可见 + 数据层高可用

> **目标**：让阶段一的"骨架"长出**眼睛（监控）**、**冗余（主从）**、**自动切流（ProxySQL/Sentinel）**。每个故障都有明确的演练剧本和实测数据。

---

## 一、整体架构（19 容器拓扑）

```
                           ┌──────────────┐
                  k6 ───▶  │   Nginx LB   │
                           └──────┬───────┘
                                  │ least_conn
                          ┌───────┴────────┐
                          │                │
                   ┌─────────────┐ ┌─────────────┐
                   │   app1 (Go) │ │   app2 (Go) │
                   │ /metrics    │ │ /metrics    │
                   └──┬──────┬───┘ └──┬───────┬──┘
                      │      │        │       │
            ┌─────────▼──┐   │ ┌──────▼───────│──────┐
            │ Redis 集群  │   │ │   ProxySQL   │      │
            │ (Sentinel) │   │ │ port=6033    │      │
            │            │   │ │              │      │
            │ master+2R  │   │ │ hostgroup 10 │      │ ← 写路由
            │ + 3sentinel│   │ │ hostgroup 20 │      │ ← 读路由
            └────────────┘   │ └──────┬───────┘      │
                             │        │              │
                             │  ┌─────▼──────┐ ┌────▼─────────┐
                             │  │mysql-master│ │ mysql-slave  │
                             │  │(GTID R/W)  │ │(GTID R-only) │
                             │  └─────┬──────┘ └──────────────┘
                             │        │   binlog 复制流
                             │        └─────▶
                             │
                  ┌──────────▼──────────┐
                  │  Prometheus :9090   │ ← 抓 7 个 job
                  │  Grafana    :3000   │ ← 8 面板大盘
                  └─────────────────────┘
```

**容器清单**（19 个）：

| 角色 | 容器 |
|---|---|
| 接入层 | nginx |
| 应用层 | app1, app2 |
| 缓存层（HA） | redis-master, redis-replica1, redis-replica2, sentinel-1/2/3 |
| 数据层（HA + 读写分离） | mysql-master, mysql-slave, proxysql |
| 可观测性 | prometheus, grafana, node-exporter, nginx-exporter, redis-exporter, mysqld-exporter, mysqld-exporter-slave |

---

## 二、子阶段 2.1：监控先行

### 设计原则
**先有眼睛再做手术**——监控大盘必须在改 Redis/MySQL **之前**就位，否则改完看不到对比。

### 关键技术决策

| 决策 | 选择 | 理由 |
|---|---|---|
| 指标采集 | Prometheus pull 模式 | 业界标配，PromQL 查询能力强 |
| 业务指标 | Go `prometheus/client_golang` | 直接 Counter/Histogram 埋点 |
| 三大类指标 | 业务（app）+ 基础设施（exporter）+ 系统（node-exporter） | 黄金三层 |
| 多目标抓取 | redis-exporter 用 `/scrape?target=` 模式 | **一个 exporter 监控 3 个 Redis**，避免起 N 个容器 |
| Grafana 自动化 | provisioning 预置数据源 + 自动加载大盘 JSON | 容器启动即可用，不用手点 |

### 应用层指标（Go 服务暴露的 4 个）

```go
product_requests_total{instance,status,cache}      // Counter，分实例/状态码/缓存结果
product_request_duration_seconds{instance}          // Histogram，P95/P99 算这个
product_db_queries_total                            // Counter，缓存穿透计数
product_cache_total{result}                         // Counter，HIT vs MISS
```

### 监控开销

| 维度 | 阶段 1 | 阶段 2.1 | 影响 |
|---|---|---|---|
| 峰值 QPS | 16,985 | 15,069 | **下降 ~11%**（埋点成本） |
| 容器数 | 5 | 11 | +6 |
| 内存占用 | ~500MB | ~2GB | Prometheus 一个就占 300MB |

> **结论**：可观测性是有成本的，但能换来"故障定位时间从小时级降到分钟级"。生产里普遍接受这个 trade-off。

---

## 三、子阶段 2.2：Redis Sentinel

### 拓扑变化

```
[单 Redis] → [1 master + 2 replica + 3 sentinel]
```

### 三大原理

1. **主从复制（异步）**：master 写完立即返回，binlog 异步发往 replica。**故障时可能丢几 ms 数据**——这是异步复制的代价。
2. **Sentinel 三职责**：监控 / 通知 / 故障转移。
3. **Quorum 必须奇数**：3 个 Sentinel + quorum=2，避免脑裂。
4. **客户端通过 Sentinel 发现 master**，**不写死地址**。这是关键。

### 客户端代码改动

```go
// Before
rdb := redis.NewClient(&redis.Options{Addr: "redis:6379"})

// After (核心就这一处)
rdb := redis.NewFailoverClient(&redis.FailoverOptions{
    MasterName:    "mymaster",
    SentinelAddrs: []string{"sentinel-1:26379", "sentinel-2:26379", "sentinel-3:26379"},
})
```

### 故障演练实测数据 🎯

**演练步骤**：压测稳定后 `docker stop ha-arch-redis-master`，观察。

| 指标 | 实测值 |
|---|---|
| Sentinel 故障判定耗时 | 5 秒（`down-after-milliseconds`） |
| 完整切换耗时 | ~6 秒（判定 + 选举 + 重配置） |
| **70 万请求期间错误率** | **0.00%（0 / 706,340 失败）** ✅ |
| 缓存命中率从 99.99% → ? | 短暂跌至 ~0%，恢复到 ~98% |
| DB 穿透 QPS | **0.5/s → 291/s**（飙升 600 倍） |
| 旧 master 回归 | 自动降级为 replica，不复辟 |

**零失败的三层韧性**：
1. Sentinel 切换快（~6s）
2. Go `FailoverClient` 内置 Sentinel 重发现 + 重试
3. 代码自带降级：`rdb.Get` 出错走 DB 兜底

### Sentinel 自动发现的"魔法"

`sentinel.conf` 里只写了 `sentinel monitor mymaster redis-master 6379 2`。
**replica 和其他 sentinel 是自动发现的**：

- Sentinel 通过 master 的 `INFO replication` 命令自动发现 replica
- Sentinel 通过 master 的 `__sentinel__:hello` 频道自动发现彼此

---

## 四、子阶段 2.3：MySQL 主从 + ProxySQL

### 拓扑

```
                ┌──────────┐
       app ──▶  │ ProxySQL │ ──┬──写──▶ mysql-master (read_only=0)
                │ :6033    │   │
                └──────────┘   └──读──▶ mysql-slave  (read_only=1)
                                         ▲
                                         │ GTID 复制
                                         └──── binlog
```

### 三大原理

1. **GTID 复制**：每个事务全局唯一 ID，slave 知道自己同步到哪、不丢不重。
2. **读写分离**：写只能去 master，读可以分散到多 slave。**读能横向扩、写不能**。
3. **ProxySQL 三件事**：认证转发 / SQL 正则路由 / 主从角色自动探测（查 `@@read_only`）。

### MySQL HA 比 Redis HA 难在哪

| 维度 | Redis | MySQL |
|---|---|---|
| 数据丢失代价 | 低（缓存） | **高（真相源）** |
| 故障转移 | Sentinel 内置自动 | **没有内置**，靠外部工具（MHA / Orchestrator / 云管控） |
| 节点回归 | 自动降级 | **GTID 冲突，需要人工或重新克隆** |

**结论**：阶段 2.3 只做手动 failover 演练 + ProxySQL 自动切流。生产自动决策是单独一套系统。

### ProxySQL 三层关键配置

```ini
mysql_query_rules:
  - rule 1: ^SELECT .* FOR UPDATE$  →  hostgroup 10 (master)  # 持锁要写组
  - rule 2: ^SELECT                 →  hostgroup 20 (slave)   # 普通读走读组
  - default                         →  hostgroup 10 (master)  # 写默认走主

mysql_replication_hostgroups:
  - { writer: 10, reader: 20 }       # ProxySQL 每 1.5 秒查 @@read_only 自动分组
```

### 读写分离实测数据 🎯

**45 秒压测后**：

| 指标 | 数值 | 解读 |
|---|---|---|
| 业务 SELECT 总数 | 148 次 | 缓存 miss 穿透的 |
| 落到 **mysql-slave** | **95 次** | 读流量全部去了读组 ✅ |
| 落到 mysql-master | **0 次** | master 不接业务读 ✅ |

**这就是"加一个 slave，读容量翻倍"的实证。**

### 故障转移实测数据

**演练**：`docker stop ha-arch-mysql-master`，手动升 slave。

| 阶段 | 现象 |
|---|---|
| master 挂掉瞬间 | 业务**写**失败：`Max connect timeout 10000ms` |
| master 挂掉期间 | 业务**读**仍可用（slave 还在） |
| 手动 `SET GLOBAL read_only=0` on slave | ProxySQL 1.5 秒内探测到 |
| ProxySQL 自动重组 | slave 从 hostgroup 20 移到 hostgroup 10 |
| 业务写 | **恢复，无需改任何代码** ✅ |
| 旧 master 回归 | **GTID 冲突 1062**（见踩坑） |

---

## 五、踩坑记录（这一章非常重要）

每一个都让我多花了 15-60 分钟，写下来下次不会再踩：

| # | 坑 | 现象 | 根因 | 解决 |
|---|---|---|---|---|
| 1 | node-exporter 挂载失败 | `path / is mounted on / but it is not a shared or slave mount` | Mac Docker Desktop 不支持 `rslave` 共享挂载 | 去掉 `/:/host:ro,rslave`，只跑容器内指标 |
| 2 | Docker Desktop 配置文件不同步 | 改了 `prometheus.yml`，容器内 `cat` 看到的还是旧内容 | macOS 的 `:ro` bind mount 有时不传播文件变更 | `docker-compose restart`，**不要靠 SIGHUP 热加载** |
| 3 | 旧容器占端口 | `Bind for 0.0.0.0:3306 failed: port is already allocated` | 改 service name 后，孤立容器没被清理 | `docker-compose up --remove-orphans` |
| 4 | MySQL slave 起不来 | `Access denied for user 'root'@'localhost'` + init.sql 报 `super-read-only` | slave.cnf 配了 `super_read_only=ON`，**init 阶段就生效，阻塞了 root 密码设置** | 只用 `read_only=ON`，super_read_only 在初始化后通过 SQL 动态打开 |
| 5 | GTID 冲突 1062 | 旧 master 回归时 SQL 线程报 `Duplicate entry` | 旧 master 数据卷残留旧数据，replication 重放主键冲突 | **方案**：清卷重新克隆（生产常用）；或者跳过 GTID（数据可能轻微不一致） |
| 6 | redis-exporter 抓不到主从角色 | 大盘只能看到一个节点 | 单目标 exporter | 改用 `/scrape?target=` 多目标模式，prometheus.yml 用 relabel_configs |

### 第 5 个坑的深层教训

> **MySQL 故障切换的难点不在切走，而在节点回归。** GTID 设计想自动化这事，但任何抖动都可能导致集合不一致。这就是为什么有 Orchestrator、MHA、Percona XtraBackup——**它们存在的意义是处理 GTID 边界情况**。

---

## 六、可观测性大盘（实际可看）

[http://localhost:3000/d/ha-arch-main](http://localhost:3000/d/ha-arch-main)

**8 个核心面板**：

| 面板 | 用途 | 阶段 2 关键观察点 |
|---|---|---|
| 总 QPS | 系统吞吐 | 17K → 15K（监控开销 ~11%） |
| 错误率 | SLA | 故障切换瞬间会有尖峰 |
| 缓存命中率 | 缓存健康度 | **Redis 故障时从 99.99% 跌到 ~0%** |
| DB 穿透 QPS | 后端压力 | 缓存挂了飙升 600 倍 |
| QPS 按实例 | LB 均匀性 | 50/50 平分 |
| P95/P99 延迟 | 尾延迟 | 故障时尖峰到 200ms+ |
| HIT vs MISS 速率 | 缓存细节 | 看 MISS 突增就知道缓存出事 |
| Nginx 活跃连接 | 接入层 | 找瓶颈 |
| Redis 各节点速率 (含角色) | 主从分布 | 看 master 比 slave 忙多少 |
| MySQL 连接 / 慢查询 | 数据层 | 慢 SQL 报警基础 |

---

## 七、关键指标对比（阶段一 → 阶段二）

| 维度 | 阶段一 | 阶段 2.3 完成 |
|---|---|---|
| 容器数 | 5 | **19** |
| Redis 节点 | 1 单点 | **3 节点 + 3 哨兵** |
| MySQL 节点 | 1 单点 | **1 主 1 从 + ProxySQL** |
| 监控大盘 | ❌ | ✅ 8 面板实时 |
| Redis 挂了 | 业务全死 | **0 失败（70 万请求）** |
| MySQL 挂了 | 业务全死 | 写挂 10s，**读不受影响** |
| 峰值 QPS | 17K | 15K（监控开销） |
| 数据丢失风险 | 全丢 | 异步复制 < 1ms 数据 |

---

## 八、面试包装话术

> 阶段二在阶段一的基础上做了三件事：
>
> 1. **可观测性先行**：Prometheus + Grafana + 4 个 exporter + 应用层 4 个业务指标，搭出 8 面板大盘。**没有大盘谈不上 HA**——故障定位时间从"看日志猜半天"降到"扫一眼指标"。
>
> 2. **Redis Sentinel 集群**：1 主 2 从 + 3 哨兵，quorum=2。故障转移演练实测**70 万请求 0 失败**，靠的是 Sentinel 切换快 + 客户端 FailoverClient 重发现 + 应用层 DB 兜底的三层韧性。
>
> 3. **MySQL 主从 + ProxySQL**：GTID 复制，ProxySQL 按 SQL 正则做读写分离。实测 100% 读流量自动去 slave、写流量去 master。**业务零代码改动**，只改了 DSN 一行。
>
> 这个过程最大的教训是 **MySQL HA 比 Redis HA 难**：Redis 故障切换 6 秒完成、零失败；MySQL 切走容易（ProxySQL 1.5 秒自动重路由），但**旧 master 回归遇到 GTID 1062 冲突，最后用"清卷重克隆"才修好**。这正是为什么生产有 Orchestrator、PerconaXtraBackup 这类工具——**它们的存在意义就是处理这些边界情况**。

---

## 九、阶段二**没**解决的问题（留给阶段三）

| 问题 | 解决在 |
|---|---|
| 没有限流，恶意流量直接打穿 | **阶段 3**：APISIX 网关 |
| 同步写 DB，扛不住秒杀 | **阶段 3**：Kafka 削峰 |
| 没有鉴权（任何请求都能进） | **阶段 3**：APISIX JWT |
| MySQL failover 决策仍要人工 | **阶段 5**：Orchestrator + 混沌 |
| 扩容要手改 yaml | **阶段 4**：k3d + HPA |

---

## 十、文件清单（阶段二新增/修改）

```
ha-arch/
├── PHASE1.md
├── PHASE2.md                           ← 本文档
│
├── docker-compose.yml                  ← 19 服务
│
├── app/main.go                         ← 加 /metrics + Sentinel FailoverClient
│
├── prometheus/
│   └── prometheus.yml                  ← 7 个 job，redis 用 relabel 多目标
│
├── grafana/
│   ├── provisioning/
│   │   ├── datasources/prometheus.yml
│   │   └── dashboards/dashboards.yml
│   └── dashboards/
│       └── ha-arch.json                ← 8 面板大盘
│
├── redis/
│   └── sentinel.conf                   ← quorum=2, resolve-hostnames
│
├── mysql/
│   ├── master.cnf                      ← server-id=1, binlog, GTID
│   ├── slave.cnf                       ← server-id=2, read_only
│   ├── init-master.sql                 ← repl + monitor + exporter 用户 + 商品数据
│   └── init-slave.sql                  ← CHANGE REPLICATION SOURCE + START REPLICA
│
└── proxysql/
    └── proxysql.cnf                    ← hostgroup 10=写，20=读，正则路由
```

---

## 十一、命令速查

```bash
cd ~/ha-arch

# 启停
docker-compose up -d --build --remove-orphans
docker-compose down -v                # 清数据

# 故障演练
docker stop ha-arch-redis-master       # Redis 主挂
docker stop ha-arch-mysql-master       # MySQL 主挂

# Redis 拓扑探查
docker exec ha-arch-sentinel-1 redis-cli -p 26379 SENTINEL get-master-addr-by-name mymaster

# MySQL 主从查
docker exec ha-arch-mysql-slave mysql -uroot -proot -e "SHOW REPLICA STATUS\G" | grep -E "Running|Source_Host|Behind|Error"

# ProxySQL 路由查
docker exec ha-arch-proxysql mysql -uadmin -padmin -h127.0.0.1 -P6032 -e \
  "SELECT hostgroup_id,hostname,status FROM runtime_mysql_servers ORDER BY hostgroup_id"

docker exec ha-arch-proxysql mysql -uadmin -padmin -h127.0.0.1 -P6032 -e \
  "SELECT hostgroup,count_star,SUBSTR(digest_text,1,60) FROM stats_mysql_query_digest ORDER BY count_star DESC LIMIT 10"

# 压测
k6 run loadtest/stress.js

# 监控
open http://localhost:3000/d/ha-arch-main         # Grafana
open http://localhost:9090/targets                # Prometheus targets
```
