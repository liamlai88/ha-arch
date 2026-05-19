# 阶段一总结：最小高可用骨架

> 目标：在 Mac 本地复现一套**可以演练故障、可以压测、能定位瓶颈**的最小企业架构，作为后续阶段的基线。

---
## 一、架构

```
                       ┌────────────────┐
   client ──HTTP──▶    │  Nginx :8080   │   接入层 / 负载均衡 (least_conn)
                       └────────┬───────┘
                                │
                ┌───────────────┴───────────────┐
                ▼                               ▼
        ┌──────────────┐                ┌──────────────┐
        │ app1 (Go)    │                │ app2 (Go)    │   应用层 (水平扩展 + HA)
        │  :8080       │                │  :8080       │
        └──────┬───────┘                └──────┬───────┘
               │                               │
               └───────────────┬───────────────┘
                               │
                ┌──────────────┴──────────────┐
                ▼                             ▼
        ┌──────────────┐              ┌──────────────┐
        │ Redis :6379  │              │ MySQL :3306  │
        │ (缓存层)     │              │ (持久层)      │
        └──────────────┘              └──────────────┘
```

---

## 二、组件清单与生产对应

| 容器 | 角色 | 生产对应 | 解决什么问题 |
|---|---|---|---|
| `ha-arch-nginx` | 接入层 / LB | 阿里云 SLB / AWS ALB | 单一入口、流量分发、失败剔除 |
| `ha-arch-app1` / `app2` | 应用层 × 2 | k8s Pod | 业务逻辑，HA + 水平扩展 |
| `ha-arch-redis` | 缓存层 | 阿里云 Tair / ElastiCache | 抗读流量、共享缓存 |
| `ha-arch-mysql` | 数据层 | 阿里云 RDS | 持久化、真相源 |

---

## 三、关键技术决策

| 决策点 | 选择 | 理由 |
|---|---|---|
| 编排 | docker-compose | 阶段一只要 5 个容器，k3d 是阶段四的事 |
| LB 算法 | `least_conn` | 比 round-robin 更适合长短请求混合 |
| 缓存共享 | 集中式 Redis（非进程内） | 多实例共享同一份缓存是 HA 关键 |
| Compose 命名 | `name: ha-arch` + `ha-arch-*` 容器名 | 避免和其他项目（如 Dify）混在 `docker ps` 里 |
| 故障转移 | Nginx `proxy_next_upstream` + `max_fails=2` | 实例挂了 5 秒内自动剔除 |
| 响应头埋点 | `X-Instance`、`X-Cache` | 一眼看出请求落到哪个实例、是否命中缓存 |
| 健康检查 | MySQL/Redis healthcheck + `depends_on: condition: service_healthy` | 应用等数据层 ready 再启动 |
| Go 连接池 | `SetMaxOpenConns(50)` | 防止压测打爆 MySQL 连接 |
| 缓存 TTL | 30 秒 | 故意短，方便演练 HIT/MISS 切换 |

---

## 四、踩坑记录

| 问题 | 现象 | 解决 |
|---|---|---|
| `missing go.sum entry` | `docker build` 失败 | Dockerfile 用 `go mod tidy` 替代 `go mod download` |
| zsh 把 `?` `{` 当通配符 | `curl http://.../product?id=1` 报 `no matches found` | URL 加单引号 |
| 跑了其他实验的 docker | `docker ps` 一堆 Dify 容器，看不清归属 | compose 加 `name: ha-arch`，容器统一前缀 |
| Nginx 单 worker | 13K QPS 时 CPU 干到 106%（吃满 1 核） | 加 `worker_processes auto;` → 提升 30% |

---

## 五、HA 故障演练（已验证）

```bash
docker stop ha-arch-app1
# 后续请求 X-Instance 全部变成 app2
for i in 1 2 3 4 5; do
  curl -s -D - "http://localhost:8080/product?id=1" -o /dev/null | grep X-Instance
done
docker start ha-arch-app1
# 5 秒内 LB 自动把 app1 拉回来
```

**结论**：单实例宕机对外部请求**完全无感**（除非正好打到那一瞬间），HA 第一层成立。

---

## 六、压测基线（k6，500 VU，45 秒）

### 6.1 三组对比数据

| 指标 | sleep 限速版 | 真实压力 | 真实压力 + Nginx 多 worker |
|---|---:|---:|---:|
| **QPS** | 1,795 | 13,103 | **16,985** |
| **P95 延迟** | 5.76 ms | 50.03 ms | **41.20 ms** |
| **P99 延迟** | 8.66 ms | 63.34 ms | ~85 ms (max 234ms) |
| **错误率** | 0% | 0% | **0%** |
| **缓存命中率** | 99.97% | 99.997% | **99.99%** |
| **DB 穿透** | 28 / 107K | 16 / 590K | 71 / 764K |
| **LB 偏差** | 0.14% | 0.13% | 0.56% |

### 6.2 资源占用（峰值，500 VU 真压）

| 组件 | 单 worker Nginx | 多 worker Nginx |
|---|---:|---:|
| Nginx CPU | 🔴 **106%**（瓶颈） | 🟢 874% |
| app1 CPU | 97% | 144% → 30% |
| app2 CPU | 97% | 142% → 28% |
| Redis CPU | 26% | 42% |
| MySQL CPU | 1.5% | 1.7% |
| 各组件内存 | < 30MB（app）/ 389MB（MySQL） | 同左 |

---

## 七、瓶颈定位与"瓶颈搬家"

**反常识结论 1：DB 完全没参与**
13 万请求只穿透 16 次到 MySQL，CPU 1.5%。缓存做对了之后，DB 压根不是瓶颈。

**反常识结论 2：瓶颈在接入层，不在数据层**
单 worker Nginx CPU 跑满 1 核（106%），它才是真瓶颈。

**反常识结论 3：调优会让瓶颈搬家，不会消除**

```
加缓存       → 瓶颈从 DB    搬到 app
app 多实例    → 瓶颈从 app   搬到 Nginx
Nginx 多 worker → 瓶颈从 Nginx 搬到 内核网络栈 / 客户端
```

**面试可讲的结论**：
> 架构调优的本质是"瓶颈搬家"。单机优化总有天花板，真正提升吞吐就要横向扩容——这就是为什么生产架构必须分布式。

---

## 八、这一阶段验证了什么

| 命题 | 怎么证明 | 结论 |
|---|---|---|
| LB 在分流 | 10 个请求 5/5 分布；压测下偏差 < 0.6% | ✅ |
| 缓存生效 | 第一次 MISS、第二次 HIT | ✅ |
| 缓存是共享的 | app1 写、app2 读同一 key 也能 HIT | ✅ |
| 实例故障可恢复 | stop app1，请求自动走 app2 | ✅ |
| 单机最大吞吐 | 17K QPS @ P95 41ms | ✅ |
| 真瓶颈定位 | Nginx CPU 106% / 874% | ✅ |

---

## 九、这一阶段**没**解决的问题（留给后续）

| 问题 | 解决在 |
|---|---|
| MySQL 单点，挂了全死 | 阶段二：主从 + ProxySQL |
| Redis 单点，挂了缓存雪崩 | 阶段二：Sentinel 1主2从 |
| 缓存击穿（71 次 MISS 多于理论值） | 阶段二：singleflight / 分布式锁 |
| 没有大盘，看不到实时指标 | 阶段二：Prometheus + Grafana + 各 exporter |
| 没有限流，恶意流量直接打穿 | 阶段三：APISIX 网关 |
| 同步写 DB，扛不住秒杀 | 阶段三：Kafka 削峰 |
| 扩容靠手改 yaml | 阶段四：k3d + HPA |
| 没有跨地域容灾 | 阶段五：多地域 + 混沌工程 |

---

## 十、面试包装话术

> 我在 Mac 本地用 Docker Compose 搭了一套最小高可用骨架：Nginx + 2 Go 实例 + Redis 缓存 + MySQL。
>
> 压测下单机能稳定承载 **17K QPS，P95 41ms，零错误**，缓存命中率 99.99%，DB 穿透率万分之一。
>
> 通过响应头埋点（X-Instance、X-Cache）和 docker stats，我定位到当前瓶颈是 Nginx 的 CPU 而不是数据库——这让我意识到**架构调优的本质是瓶颈搬家**：加缓存把压力从 DB 搬到 app，多实例把压力搬到 LB，多 worker 把压力搬到内核网络栈。单机的瓶颈搬家有尽头，所以生产必须分布式。
>
> 这套架构每一层都对应真实生产组件：Nginx → SLB，Redis → Tair，MySQL → RDS。我在它上面规划了五个演进阶段，分别加入主从+Sentinel+监控大盘、API 网关+MQ、k8s 编排、混沌工程和多地域容灾。

---

## 十一、文件清单

```
ha-arch/
├── PHASE1.md                       ← 本文档
├── README.md                       ← 启动/验证/演练流程
├── docker-compose.yml              ← project=ha-arch
├── nginx/
│   └── nginx.conf                  ← worker_processes auto + least_conn
├── app/
│   ├── Dockerfile                  ← 多阶段构建（go mod tidy）
│   ├── go.mod
│   └── main.go                     ← X-Cache/X-Instance 埋点
├── mysql/
│   └── init.sql                    ← 5 条商品数据
└── loadtest/
    ├── test.js                     ← 限速版（200 VU + sleep）
    └── stress.js                   ← 真实压力（500 VU 无 sleep）
```

## 十二、一键命令速查

```bash
cd ~/ha-arch

docker-compose up -d --build         # 启动
docker-compose ps                    # 状态
docker-compose logs -f nginx         # 看日志
docker stop ha-arch-app1             # 演练故障
docker start ha-arch-app1
k6 run loadtest/stress.js            # 真实压测
docker-compose down                  # 停（保留数据卷）
docker-compose down -v               # 停 + 删卷
```