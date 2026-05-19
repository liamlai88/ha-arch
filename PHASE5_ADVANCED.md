# 阶段五进阶：Service Mesh + 多集群联邦

> **PHASE5.md 的延伸**——前者讲 K8s 自身能力的"边界"（chaos + stateful），这份讲怎么**超越单集群**：用 sidecar mesh 治理流量、用多集群做地域容灾。

---

## 一、整体架构变化

```
[ PHASE5.md 结束时 ]                  [ 现在 ]
单 k3d 集群:                          双 k3d 集群 + Service Mesh:
  product-app × 2 (HPA)
  redis-StatefulSet × 3                Cluster A (k3d-ha-arch-k8s)
  chaos-mesh × 6                       ├── istiod 控制面
                                        ├── istio-ingressgateway
                                        ├── product-app-v1 × 2  (戴 Envoy sidecar)
                                        ├── product-app-v2 × 2  (戴 Envoy sidecar)
                                        ├── redis StatefulSet × 3
                                        └── chaos-mesh × 6

                                        Cluster B (k3d-ha-arch-k8s-b)
                                        └── product-app × 2 (无 sidecar)

                                        全局 LB（bash 脚本模拟）
                                        ├─ Primary:   :30080 (A)
                                        └─ Failover:  :30081 (B)
```

容器/Pod 增量：~10 个（istiod、ingressgateway、v2 deploy × 2、cluster B 1 节点、cluster B product × 2）。

---

## 二、子阶段 5.5：Istio Service Mesh

### 2.1 为什么需要 Service Mesh

阶段 4-5.2 已经有了 K8s 的 Service / HPA / Deployment / StatefulSet，但**这些都不够**：

| 需求 | K8s 原生 | Istio 提供 |
|---|---|---|
| 金丝雀发布（10% 到 v2）| ❌ | ✅ VirtualService weight |
| 服务间 mTLS 自动加密 | ❌ | ✅ PeerAuthentication |
| 分布式 trace | ❌ | ✅ 自动注入 trace header |
| 超时/重试/熔断统一治理 | ❌ | ✅ 单 CRD 配置全局 |
| 基于身份的访问控制 | ❌ | ✅ AuthorizationPolicy |

**核心思路**：把"应用级"问题用 **Sidecar 代理（Envoy）** 统一解决，**业务代码完全不变**。

### 2.2 Sidecar 注入机制（必懂）

```
[ 无 Service Mesh ]                 [ Istio Sidecar 注入后 ]
                                      
Pod (1/1):                            Pod (2/2):
  ┌──────┐                            ┌──────┬──────────────┐
  │ app  │                            │ app  │ istio-proxy  │
  │ :8080│                            │:8080 │  (Envoy)     │
  └──────┘                            └──────┴──────────────┘
                                      
  业务直接监听 :8080                   iptables 把流量重定向到 Envoy
                                      业务代码以为直连，实际经过 2 个 Envoy
```

**Istio 自动注入机制**：
1. 给 namespace 打 label：`istio-injection=enabled`
2. 任何新 Pod 创建时，admission webhook 拦截
3. 给 Pod 加 init container 设置 iptables 规则
4. 给 Pod 加 sidecar 容器 `istio-proxy`

**实测**：`kubectl get pods` 看到 READY 列 **从 `1/1` 变 `2/2`**，就是 sidecar 工作中。

> 💡 **K8s 1.29+ 新机制**：Istio 用 native sidecar（放在 `.spec.initContainers` + `restartPolicy: Always`），不在 `.spec.containers` 里。所以 `containers[*].name` 看不到 istio-proxy，但 READY 是 `2/2`。

### 2.3 Istio 三层架构

```
┌─────────────────────────────────┐
│  控制面 (istiod)                │
│  - 监听 CRD                     │
│  - 下发路由规则给所有 Envoy     │
│  - 颁发 mTLS 证书              │
└──────────────┬──────────────────┘
               │ xDS 协议下发
               ▼
┌─────────────────────────────────┐
│  数据面 (每 Pod 一个 Envoy)     │
│  - 实际处理每条请求             │
│  - 路由 / mTLS / metrics       │
└─────────────────────────────────┘

用户写 K8s CRD:
  VirtualService    "10% 流量到 v2"
  DestinationRule   "v1 round-robin, v2 least-conn"
  PeerAuthentication "service A 必须 mTLS"
```

### 2.4 金丝雀发布实测（杀手锏演示）

#### YAML 关键点

**两个 Deployment + 同一个 Service**：
```yaml
# product-app-v1（version: v1 label）
# product-app-v2（version: v2 label）
# 都带 app: product，Service 按 app=product 选两批
```

**DestinationRule + VirtualService**：
```yaml
kind: DestinationRule
spec:
  host: product-svc
  subsets:
  - name: v1
    labels: {version: v1}
  - name: v2
    labels: {version: v2}

kind: VirtualService
spec:
  hosts: [product-svc]
  gateways: [mesh]              # 只对集群内流量生效
  http:
  - route:
    - destination: {host: product-svc, subset: v1}
      weight: 90
    - destination: {host: product-svc, subset: v2}
      weight: 10
```

#### 实测数据 1：NodePort 外部访问（基线）

```bash
for i in {1..100}; do
  curl -s http://localhost:30080/ | grep -o '"version":"v[12]"'
done | sort | uniq -c

→ 46 "version":"v1"
  54 "version":"v2"
```

**50/50 分布**——为什么？因为 NodePort → kube-proxy iptables 直接到 Pod，**绕过 Envoy**，按 K8s Service 随机分流。

#### 实测数据 2：集群内带 sidecar 访问（Istio 生效）

```bash
kubectl run istio-client --image=curlimages/curl:8.10.1 --restart=Never -i --rm -- \
  sh -c 'for i in $(seq 1 100); do curl -s http://product-svc/ | grep -o "\"version\":\"v[12]\""; done | sort | uniq -c'

→ 90 "version":"v1"
  10 "version":"v2"
```

**90/10 精准分流** ✅——临时 client Pod 有 sidecar（namespace 自动注入），流量被 Envoy 拦截改路由。

#### 两组对比的核心金句

> **同样的 Pod、同样的 Service，从不同入口流量分布天差地别**：
> - **NodePort**（外部）：iptables 比 Istio 早一步执行，50/50 随机
> - **Sidecar 内部**：Envoy 拦截，VirtualService 精准 90/10
>
> 生产含义：要让 Istio 治理外部流量，**必须用 Istio Ingress Gateway 替代 NodePort**——让入口也戴上 Envoy。

### 2.5 渐进切流演练

```bash
# 50/50 → 10/90 → 0/100，每次 kubectl patch 1 秒生效
kubectl patch vs product-svc-vs --type=merge -p='
spec.http[0].route:
- {destination: {host: product-svc, subset: v2}, weight: 100}'
```

**最终 0/100 实测**：100 次集群内请求全部 v2，**v1 Pod 仍然在跑**（只是不接流量）。

**金丝雀 vs 滚动更新对比表**：

| | 滚动更新 (PHASE4) | 金丝雀 (PHASE5.5) |
|---|---|---|
| **切换粒度** | 全有或全无 | **任意百分比** |
| **回滚速度** | 新 Pod 启动 30 秒+ | **改 weight，1 秒生效** |
| **A/B 测试** | ❌ | ✅ 可并存 |
| **基于 Header 路由** | ❌ | ✅（内部员工走 v2、外部走 v1） |

---

## 三、子阶段 5.6：多集群联邦（mini）

### 3.1 多集群三种"真实"形态

| 形态 | 控制面 | 数据面 | 工具 | 难度 |
|---|---|---|---|---|
| **A. 独立多集群 + 全局 LB** | 独立 | 独立 | Route53 / Cloudflare GeoDNS / F5 | **低** |
| **B. 联邦控制面** | 统一 | 独立 | Karmada / KubeFed | 高 |
| **C. Service Mesh 多集群** | mesh | 跨集群直连 | Istio multi-cluster | 极高 |

**生产现实**：
- 90% 中小厂用 **A**（GSLB / DNS 切流）
- 大厂用 **B 或 C**，但都有专职 SRE 团队
- 这次我们做 **A 的 mini 版**

### 3.2 形态 A 的两种工作模式

| 模式 | 工作方式 | 典型场景 |
|---|---|---|
| **Active-Passive** | A 主跑、B 待命，健康检查 fail 切换 | 银行 / 金融，强一致优先 |
| **Active-Active** | A B 同时接流量（按地理/比例）| 互联网 / 内容服务，UX 优先 |

我们演示的是 **Active-Passive**：A 优先，A 挂了 B 接管。

### 3.3 多集群最难的不是部署，是数据

| 数据类型 | 跨集群方案 |
|---|---|
| **完全无状态服务** | 简单：每集群独立部 |
| **缓存** | 各集群独立 Redis，或云厂商全局 Redis（如 AWS ElastiCache Global） |
| **数据库** | **极难**：跨机房延迟 50ms+，强一致 = 性能差 |
| **消息队列** | Kafka MirrorMaker 2 异步复制 |

> **真实生产**：跨地域常用"**主写次读**"——主机房写、从机房只读。**完全双写多活极少**，只有 Google Spanner、CockroachDB 这种特殊系统。

### 3.4 拓扑

```
[ Mac ]
   │
   ├── k3d-ha-arch-k8s     (Cluster A, 3 节点)
   │    └── product-app (NodePort :30080)
   │
   ├── k3d-ha-arch-k8s-b   (Cluster B, 1 节点)
   │    └── product-app (NodePort :30081)
   │
   └── ./loadtest/global-lb.sh  (模拟全局 LB)
        ├─ 先试 :30080 (Primary)
        └─ 失败 fallback :30081 (Secondary)
```

### 3.5 实测 failover 数据 🎯

**演练流程**：
1. 启动 global-lb.sh（60 秒）
2. ~18 秒时执行 `k3d cluster stop ha-arch-k8s`
3. 看 LB 是否自动切

**汇总结果**：

| 指标 | 数值 |
|---|---|
| 总请求（0.5s 间隔 × 60s） | 109 |
| Cluster A served（前 ~18 秒） | **36** |
| Cluster B served (failover) | **73** |
| **Both down（失败）** | **0** ✅ |

**关键证据**：`Both down: 0`——**零失败 failover**。

### 3.6 这次实验的核心金句

> **多集群 HA 的核心不是技术，是设计准则**：
>
> 1. **业务必须无状态**（或每集群有完整数据副本）
> 2. **全局 LB 必须能快速健康检查 + 切换**（我们 bash 用 2 秒 timeout，生产用 Route 53 + 30s 频率）
> 3. **客户端要容忍最终一致性**（缓存 / session 可能丢）
>
> **生产 Route 53 / Cloudflare GeoDNS 替代我们 bash 脚本，但逻辑完全一样**：健康检查 + DNS 切流。

### 3.7 Karmada / KubeFed 多了什么（我们没做但要懂）

```
[ 联邦控制面 ]
   │
   │ 声明式："我要在 3 集群部署 product-app，副本数 2/2/3"
   │
   ├─▶ 集群 A: 自动 Deployment(replicas=2)
   ├─▶ 集群 B: 自动 Deployment(replicas=2)
   └─▶ 集群 C: 自动 Deployment(replicas=3)
```

**好处**：一份 yaml 多集群生效，运维成本下降。
**代价**：装控制面 1GB+ 内存，集群网络互通需要专门方案（Submariner）。

**面试回答**：

> "我用过形态 A（多集群 + GSLB），知道形态 B/C 是怎么回事但生产没用过。**Karmada 在阿里、字节都有落地，主要解决跨地域应用统一编排**。"

---

## 四、踩坑记录

| # | 坑 | 现象 | 根因 | 解决 |
|---|---|---|---|---|
| 1 | istioctl install 看似"卡住" | 命令长时间无输出 | 镜像在拉，没失败 | **耐心等，最终成功** |
| 2 | VirtualService 不能用 `hosts: ["*"]` 配 mesh gateway | apply 报 `wildcard host * is not allowed` | Istio 校验：mesh gateway 不允许通配符 | 只列 `product-svc` |
| 3 | sidecar 在 initContainers 看不到 | `kubectl get -o jsonpath='{...containers[*].name}'` 只显示 app | K8s 1.29+ Istio 用 native sidecar | 看 READY 列 `2/2` 即证明 |
| 4 | NodePort 流量不被 Istio 路由 | NodePort 测 50/50，不是 90/10 | NodePort → kube-proxy 绕过 Envoy | 用集群内带 sidecar 的 Pod 测试 |
| 5 | k3d 第二个集群 kubeconfig 还是 0.0.0.0 | kubectl timeout | macOS Docker Desktop 限制 | 手动改 server 为 127.0.0.1 + 实际端口 |
| 6 | 切到 cluster B 后 apply deployment 没生效 | get pods 为空 | k3d 创建新集群后 context **没自动切**，apply 到了旧集群 | 显式 `kubectl config use-context k3d-ha-arch-k8s-b` |

---

## 五、四个反直觉结论（写进简历）

### ① 同一 Service，不同入口流量分布天差地别
NodePort 50/50（iptables 随机）vs sidecar 内部 90/10（Istio 精准）。**网关流量必须戴 Envoy 才被治理**。

### ② Istio 金丝雀 ≠ 滚动更新
滚动更新是"换 Pod"，金丝雀是"换流量"。**回滚速度从 30 秒降到 1 秒**——改 weight 立刻生效。

### ③ 多集群最难的不是部署，是数据
跨地域 50ms+ 延迟让强一致代价巨大。**真实生产几乎都是"主写次读 + 异步复制"**，全球强一致只有 Spanner 这种特例。

### ④ 全局 LB 是用 DNS / IP 切的，不是用 K8s 切的
Route 53、Cloudflare GeoDNS、F5——**这层在 K8s 之外**。K8s 只管单集群内的事，**跨集群是 DNS 和健康检查的活**。

---

## 六、演练数据汇总（一表打天下）

| 演练 | 关键数据 |
|---|---|
| **Sidecar 自动注入** | Pod 1/1 → **2/2** |
| **VirtualService 90/10** | **集群内 90 v1 / 10 v2** ★ |
| **NodePort 绕过 mesh 对比** | **外部 46/54（接近 50/50 随机）** |
| **金丝雀渐进切流** | 50/50 → 0/100，**改 weight 1 秒生效** |
| **多集群 failover** | A → B 切换**0 失败 / 109 请求** ✅ |
| **failover 触发时间** | k3d cluster stop 后 **~2 秒**（脚本 timeout） |

---

## 七、面试包装话术

> 阶段 5 进阶部分做了两个"超越单集群"的能力：
>
> **5.5 Istio Service Mesh**：通过 sidecar 自动注入实现金丝雀发布。**两组关键对比数据**——外部 NodePort 访问 50/50 随机（kube-proxy iptables 早于 Istio）、集群内带 sidecar 访问精准 90/10（VirtualService 生效）——证明 Istio 治理流量必须让入口戴 Envoy。**渐进切流 50→10→0%，每次改 weight 1 秒生效，业务零改动零停机**。这是滚动更新做不到的事。
>
> **5.6 多集群 mini**：起第二个 k3d 集群，用 bash 模拟 GSLB（健康检查 + fallback）。**演练 stop Cluster A 时，109 个请求全部 0 失败 failover 到 B**。这是 Active-Passive 模式的最小实证。
>
> 这两个加起来让我理解到一个重要事实：**单集群 K8s 治理流量靠 Service，多集群靠 DNS/GSLB；单集群灰度靠 Deployment，跨集群灰度靠流量比例**。**真实生产 90% 用形态 A（独立集群 + GSLB），Karmada/KubeFed/Istio multi-cluster 是大厂专职团队的事**——SRE 面试讲清楚"我用过 A，理解 B/C 的代价"，比硬上 Karmada 显得更务实。

---

## 八、跨 5 个阶段最终金句（5 句话讲完整套架构）

| # | 阶段 | 金句 |
|---|---|---|
| 1 | 基础骨架 | 调优的本质是**瓶颈搬家**，单机优化必有天花板 |
| 2 | HA + 监控 | **MySQL 故障切换难点不在切走，在节点回归** |
| 3 | 网关 + MQ | **MQ 不为提速，为扛峰值** |
| 4 | k8s + HPA | **回滚是重建容器，不是还原状态** |
| 5.1-5.2 | 混沌 + Stateful | **K8s 修硬故障，应用层修软故障** |
| **5.5-5.6** | **Mesh + 多集群** | **同 Service 不同入口流量不一样；多集群难点是数据，不是部署** |

---

## 九、文件清单（阶段五进阶新增）

```
ha-arch/
├── PHASE1.md / PHASE2.md / PHASE3.md / PHASE4.md
├── PHASE5.md            ← 5.1 Chaos + 5.2 StatefulSet
├── PHASE5_ADVANCED.md   ← 本文档（5.5 Istio + 5.6 多集群）
│
└── k8s/manifests/
    ├── deployment-v1-v2.yaml      ← 两个独立 Deployment 共用 Service
    ├── istio-canary.yaml          ← DestinationRule + VirtualService 90/10
    └── service-b.yaml             ← Cluster B 的 NodePort 30081

└── loadtest/
    └── global-lb.sh               ← bash 模拟 GSLB（健康检查 + fallback）
```

---

## 十、命令速查

```bash
# === Istio ===
istioctl install --set profile=default -y
kubectl label namespace default istio-injection=enabled
kubectl rollout restart deployment <name>    # 重启让 Pod 戴上 sidecar

# 金丝雀路由（改 weight）
kubectl patch vs product-svc-vs --type=merge -p='
spec:
  http:
  - route:
    - destination: {host: product-svc, subset: v1}
      weight: 50
    - destination: {host: product-svc, subset: v2}
      weight: 50'

# 集群内带 sidecar 测试（VirtualService 才会生效）
kubectl run test --image=curlimages/curl --restart=Never -i --rm -- \
  sh -c 'for i in $(seq 1 100); do curl -s http://product-svc/; done'

# === 多集群 ===
k3d cluster create ha-arch-k8s-b --servers 1 --agents 0 --port "30081:30081@loadbalancer"
k3d image import k8s-demo:v1 -c ha-arch-k8s-b
kubectl config use-context k3d-ha-arch-k8s-b
kubectl apply -f k8s/manifests/deployment.yaml
kubectl apply -f k8s/manifests/service-b.yaml

# Failover 演练
./loadtest/global-lb.sh         # 全局 LB 监控
k3d cluster stop ha-arch-k8s    # 故障注入
k3d cluster start ha-arch-k8s   # 恢复

# === 清理 ===
istioctl uninstall --purge -y
k3d cluster delete ha-arch-k8s-b
```
