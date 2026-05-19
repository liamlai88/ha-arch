# HA-Arch：从单机到多集群的高可用架构演练

> 在 Mac 本地用 docker-compose + k3d 复现**完整生产架构**，**每个阶段都有可演练的故障 + 实测数据**。从 Nginx + 单 MySQL 起步，演进到 26 容器的 docker-compose 栈、再演进到 K8s 双集群联邦。

---

## 🎯 一句话概括

> **5 个阶段、40+ 容器/Pod、42+ 组实测数据、34 个真实踩坑、6 句反直觉面试金句**——一份可直接落地的高可用架构学习路线。

---

## 📊 整体演进路线

```
阶段 1: 最小骨架                     阶段 2: HA + 监控
─────────────                       ─────────────────
 Nginx + 2 Go + Redis + MySQL  →    + Sentinel 3 节点
 5 容器，单机 17K QPS                + MySQL 主从 + ProxySQL
                                    + Prometheus + Grafana
                                    19 容器，零失败演练
                ↓
阶段 3: 网关 + MQ                    阶段 4: K8s + HPA
─────────────────                   ──────────────────
 + APISIX 限流/熔断/鉴权        →    + k3d 集群（3 节点）
 + Kafka 3 broker + 副本            + Deployment + Service
 + Order 异步削峰                    + HPA 自动扩缩 2→10
 26 容器，12000× 熔断加速            + 滚动更新 + 一键回滚
                ↓
阶段 5: 混沌 + StatefulSet           阶段 5.5+: Mesh + 多集群
─────────────────────────           ───────────────────────
 + Chaos Mesh 主动注入故障      →    + Istio 金丝雀 90/10
 + Redis StatefulSet + PVC          + 双 k3d 集群
 + 三大混沌实验                       + Failover 0 失败
 ~32 Pod，硬故障 vs 软故障边界        ~40 Pod，多集群韧性
```

---

## 📚 文档导览

| 阶段 | 文档 | 核心收获 | 反直觉金句 |
|---|---|---|---|
| **1** | [PHASE1.md](PHASE1.md) | 17K QPS 单机基线，瓶颈定位 | 调优本质是**瓶颈搬家** |
| **2** | [PHASE2.md](PHASE2.md) | Redis Sentinel / MySQL 主从 / 监控 | MySQL **难点不在切走，在回归** |
| **3** | [PHASE3.md](PHASE3.md) | APISIX 网关 / Kafka 削峰 / 多 broker | MQ **不为提速，为扛峰值** |
| **4** | [PHASE4.md](PHASE4.md) | K8s / HPA / 滚动更新 / 回滚 | 回滚是**重建容器，不是还原状态** |
| **5** | [PHASE5.md](PHASE5.md) | Chaos Mesh / StatefulSet | K8s **修硬故障，应用层修软故障** |
| **5+** | [PHASE5_ADVANCED.md](PHASE5_ADVANCED.md) | Istio Service Mesh / 双集群联邦 | **同 Service 不同入口流量不一样** |

---

## 🏆 关键数据（节选）

### 性能基线

| 指标 | 数据 |
|---|---|
| 单机 Mac M5 24GB 峰值 QPS | **17,000** |
| P95 延迟（缓存命中） | **41 ms** |
| 缓存命中率 | **99.99%** |
| DB 穿透率 | **0.0001%（万分之一）** |
| Nginx CPU 峰值 | 874%（多 worker） |

### 故障演练成功率

| 演练 | 结果 |
|---|---|
| Redis master 切换（70 万请求）| **0 失败 ✅** |
| MySQL master 切走（ProxySQL 自动重路由）| 写挂 10s / 读不受影响 |
| Pod kill（每 20s 杀一次，持续 60s）| **100% 成功率** |
| Kafka broker 挂（leader 自动迁移）| 91.6% 成功（acks=all 期间 8% 失败） |
| 多集群 failover（109 个请求）| **0 失败 ✅** |

### 网关与异步治理

| 演练 | 关键数据 |
|---|---|
| APISIX 限流（100 RPS → 10）| 1345/1500 个 429 |
| APISIX 熔断 | **38.7s → 0.003s（提速 12,000 倍）** |
| Kafka 削峰（5K RPS）| 同步崩 62% / 异步 99.94% |
| HPA 自动扩缩 | 2 → 10（3 分钟）→ 2（2 分钟） |
| Istio 金丝雀（90/10）| 集群内 90 v1 / 10 v2（**精准**） |

---

## 🔧 技术栈

### 基础设施
- **容器编排**：docker-compose (1-3) + k3d (4-5)
- **K8s 发行版**：k3s（轻量级 Kubernetes）
- **服务网格**：Istio（default profile）
- **混沌工程**：Chaos Mesh（Helm 安装）

### 数据层
- **缓存**：Redis 7（单节点 → 1主2从+3 哨兵 → StatefulSet）
- **数据库**：MySQL 8.0（单节点 → 主从 + GTID）
- **代理**：ProxySQL 2.7（读写分离 + 自动故障转移）
- **消息队列**：Kafka 3.7（KRaft 模式，1→3 broker）

### 接入与治理
- **LB / 反代**：Nginx 1.27（least_conn + 多 worker）
- **API 网关**：APISIX 3.11（限流 / 鉴权 / 熔断）
- **监控**：Prometheus + Grafana + 5 个 exporter
- **应用语言**：Go 1.22（product-service + order-worker）
- **压测**：k6（多种场景）

---

## 🚀 快速开始

### 前置依赖

```bash
brew install docker-compose k3d kubectl helm istioctl k6
```

需要 Docker Desktop（建议 8GB+ 内存）。

### 阶段 1-3：docker-compose 栈

```bash
cd ~/ha-arch

# 启动 26 容器（首次约 3 分钟）
docker-compose up -d --build

# 基础验证
curl 'http://localhost:9080/product?id=1'      # 走 APISIX
open http://localhost:3000/d/ha-arch-main       # Grafana
open http://localhost:8081                      # Kafka UI
```

### 阶段 4-5：k3d K8s 栈

```bash
# 建集群
k3d cluster create ha-arch-k8s \
  --servers 1 --agents 2 \
  --port "30080:30080@loadbalancer"

# 修 kubeconfig（macOS 必需）
kubectl config set-cluster k3d-ha-arch-k8s --server=https://127.0.0.1:$(docker port k3d-ha-arch-k8s-serverlb | grep 6443 | head -1 | awk -F: '{print $NF}')

# 部署 demo app
docker build -t k8s-demo:v1 ./k8s/app
k3d image import k8s-demo:v1 -c ha-arch-k8s
kubectl apply -f k8s/manifests/

# 验证
curl http://localhost:30080/
```

### 跑压测

```bash
k6 run loadtest/stress.js              # 阶段 1 基线
k6 run loadtest/order-async-5k.js      # 阶段 3 削峰
k6 run loadtest/k8s-load.js            # 阶段 4 HPA 触发
./loadtest/chaos-monitor.sh            # 阶段 5 混沌监控
./loadtest/global-lb.sh                # 阶段 5+ 多集群 failover
```

---

## 💡 跨 5 阶段的反直觉结论（背下来就赢）

### 1. 调优本质是"瓶颈搬家"，不是"消除瓶颈"
```
加缓存       → 瓶颈从 DB    搬到 app
app 多实例    → 瓶颈从 app   搬到 Nginx
Nginx 多 worker → 瓶颈从 Nginx 搬到 内核网络栈
```
单机优化必有天花板，所以必须分布式。

### 2. MySQL HA 难点不在"切走"，在"回归"
GTID 自动复制设计很美，但任何抖动都会让旧 master 回归时碰到 `Duplicate entry 1062` 等冲突。生产工具 Orchestrator/PerconaXtraBackup 存在的意义就是**处理这些边界情况**。

### 3. MQ 不为提速，为扛峰值 + 稳定延迟
1K RPS 下同步反而比异步快（4ms vs 12ms）。**异步的舞台是 5K+ RPS + 严格 P99 SLO**——延迟恒定 vs 随负载线性恶化。

### 4. K8s 的"回滚"是重建容器，不是还原状态
保留的是 ReplicaSet 模板，**不是 Pod**。原 Pod 早删了，回滚用模板**重新创建**新 Pod。**蓝绿发布的安全性 80% 由数据库前向兼容决定**，不是 K8s 能力决定。

### 5. K8s 修硬故障，应用层修软故障
- Pod 挂 → K8s 修 ✅
- 网络延迟 / CPU 异常 / 数据腐败 → **K8s 完全管不了** ❌

真正 HA = **K8s + APISIX 熔断 + Prometheus 告警 + 应用层 timeout + 团队 oncall** 的总和。

### 6. 同 Service 不同入口流量不一样；多集群难点是数据
- 阶段 5.5：NodePort 50/50 vs sidecar 内 90/10（**入口必须戴 Envoy**）
- 阶段 5.6：多集群部署简单，难的是**跨地域数据复制**——50ms+ 延迟让强一致代价巨大

---

## 🧠 五阶段面试包装（30 秒电梯说话）

> 我搭了一套完整 HA 架构演练项目，从 Nginx + 单 MySQL 起步，演进到 26 容器的 docker-compose 栈和 K8s 双集群联邦。**每个阶段都有压测数据和故障演练实证**：
>
> - 阶段 1 摸到单机 17K QPS 天花板，定位到 Nginx 是 CPU 瓶颈
> - 阶段 2 加 Redis Sentinel + MySQL 主从，演练 70 万请求 0 失败的 Redis 故障切换
> - 阶段 3 加 APISIX + Kafka，**熔断器把失败响应时间从 38.7s 降到 0.003s（12000× 加速）**
> - 阶段 4 把无状态服务迁到 K8s，HPA 在 3 分钟内从 2 个 Pod 扩到 10 个
> - 阶段 5 用 Chaos Mesh 主动注入故障，**揭示 K8s 自愈的边界——硬故障能修、软故障管不了**
>
> 这个过程最深的认知：**HA 不是单一技术，是 K8s + 监控 + 应用层韧性 + 团队 oncall 的组合拳**。每加一个组件都是**用复杂度换可用性**的明确 trade-off。

---

## 📁 目录结构

```
ha-arch/
├── README.md                  ← 本文档（项目总览）
├── PHASE1.md ~ PHASE5_ADVANCED.md   ← 6 份阶段总结
│
├── docker-compose.yml         ← 阶段 1-3 编排（26 服务）
│
├── app/                       ← Go product-service
│   ├── main.go                ← 含 /product /order /metrics 端点
│   └── Dockerfile
│
├── order-worker/              ← Kafka 异步消费者（阶段 3）
│
├── k8s/                       ← Kubernetes 资源（阶段 4-5）
│   ├── app/                   ← 简化 demo 服务（/load 烧 CPU）
│   └── manifests/             ← Deployment / Service / HPA / Chaos / Istio
│
├── nginx/                     ← Nginx 配置
├── redis/                     ← Sentinel 配置
├── mysql/                     ← Master/Slave 配置 + init.sql
├── proxysql/                  ← 读写分离配置
├── apisix/                    ← Standalone 模式配置 + 路由规则
├── prometheus/                ← 抓取配置
├── grafana/                   ← 大盘 + 数据源
│
└── loadtest/                  ← k6 + bash 压测脚本
    ├── test.js / stress.js          ← 阶段 1 基线
    ├── limit-test.js                 ← 阶段 3 限流
    ├── order-async/sync*.js          ← 阶段 3 异步
    ├── k8s-load.js                   ← 阶段 4 HPA
    ├── chaos-monitor.sh              ← 阶段 5 混沌
    └── global-lb.sh                  ← 阶段 5+ 多集群
```

---

## 🎓 学到的元能力

不只是知识，更是**思维方式**：

1. **演进式架构思维**：先做最小可用，再加 HA，再加治理。**不要一开始就上微服务 + k8s**
2. **数据驱动的决策**：每加一个组件，都用压测对比"加之前 vs 之后"
3. **trade-off 意识**：没有"完美方案"，只有"用什么代价换什么收益"
4. **故障演练习惯**：HA 不是"我用了高可用组件就 100%"，是**主动制造故障验证假设**

---

## 🛣 未来扩展方向

每个阶段都留了"未尽事宜"，按时间投入可继续：

| 方向 | 难度 | 价值 |
|---|---|---|
| Operator 模式（CRD 编程） | 高 | 把运维知识编码进 K8s |
| GitOps (ArgoCD) | 中 | 声明式 CD，git push 即部署 |
| Karmada 联邦 | 高 | 真正的多集群控制面 |
| Custom Metrics HPA | 中 | 基于 QPS / Kafka lag 扩缩 |
| 全链路 Tracing (Jaeger) | 中 | 端到端调用链 |
| Service Mesh 流量镜像 | 中 | 生产流量影子测试 |

---

## 🙏 致谢

- 整套架构演练在 **Mac M5 MacBook Air 24GB** 上完成
- 协助工具：Claude Code（结对编程 / 文档生成）
- 参考：CNCF Landscape、Istio 官方文档、Chaos Mesh 文档

---

**📬 想交流？** PR / Issue / 直接克隆来跑都欢迎。
