# 阶段四总结：从 docker-compose 到 Kubernetes（k3d + HPA + Rolling Update）

> **目标**：把"手改 yaml 扩容"替换成"自动扩缩容"，把"docker stop 上新版"替换成"零停机滚动发布"。这是从"项目能跑"到"生产能跑"的最后一公里。

---

## 一、整体架构变化

```
[ 阶段三 ]                            [ 阶段四 ]

docker-compose 编排:                  k3d 集群 + docker-compose 双栈:

  app1 (固定容器) ─┐                    Pod-1 ┐
  app2 (固定容器) ─┤                    Pod-2 ┼─ Deployment 管 (replicas=3)
  ...                                  Pod-3 ┘
                                       ↑
  容器名 = 角色                         随机调度到不同 node
  scale=N 手动改                        HPA 自动伸缩 (2-10)
  改 image 要 docker-compose up        kubectl set env → 30s 滚动更新
```

**关键认知转变**：

| 维度 | docker-compose | Kubernetes |
|---|---|---|
| **编排单元** | 容器 | **Pod**（1+ 容器） |
| **副本管理** | `scale=N` 手动 | **Deployment** 声明 + Controller |
| **服务发现** | 容器名 DNS | **Service**（VIP + iptables） |
| **自愈** | 重启容器 | **Reconcile loop** 持续比对 |
| **扩缩容** | 重启 | **HPA 基于指标自动** |
| **部署** | 重启镜像 | **滚动更新 + 自动回滚** |
| **核心哲学** | 命令式 | **声明式** |

---

## 二、容器/节点拓扑（27 容器）

```
[ Mac M5 24GB ]
   │
   └── Docker Desktop
         │
         ├── docker-compose 栈 (24 容器)   ← 阶段 1-3 保留（去掉 app1/app2）
         │   ├── Redis Sentinel × 6
         │   ├── MySQL 主从 + ProxySQL × 3
         │   ├── Kafka 3 broker + UI × 4
         │   ├── APISIX / Nginx / 监控 等
         │
         └── k3d 集群 (3 容器)
             ├── k3d-ha-arch-k8s-server-0  ← control plane (api/scheduler/controller)
             ├── k3d-ha-arch-k8s-agent-0   ← worker node
             └── k3d-ha-arch-k8s-agent-1   ← worker node
                 │
                 └── 跑着 K8s Pod:
                     ├── product-app Pod × 2-10 (HPA 控制)
                     └── metrics-server, traefik (k3s 内置)
```

> 注：阶段 4 只把 stateless 的 `product-app` 迁到 K8s，stateful 的数据层（MySQL/Redis/Kafka）暂留 docker-compose。StatefulSet + PV + Operator 留给阶段 5。

---

## 三、K8s 三大基础抽象（必懂）

### ① Pod —— "最小调度单位"

**为什么不直接管容器**：Sidecar 模式需要"主容器 + 辅助容器"**共享网络 + 卷 + 生命周期**。Pod 是这种"共生死容器组"的抽象。

```
Pod 内的容器：
  - 共享 network namespace（localhost 互通）
  - 可挂同一个 emptyDir / configmap volume
  - 同时调度、同时销毁
```

**生产用法**：
- 主容器 + 日志采集器（Fluent Bit）
- 主容器 + Istio Envoy sidecar
- 主容器 + Vault agent injector

### ② Deployment + Reconcile Loop —— "声明式自愈"

```
while true:
    desired = read_yaml()      # 期望状态
    actual = list_pods()        # 现实状态
    if actual != desired:
        take_action(diff)       # 收敛
    sleep(1)
```

**实测**：手动 `kubectl delete pod` 一个，**3 秒**内新 Pod 顶上：

```
Before: gzd6r (4m old), tfxpw (4m old), ns47j (4m old)
kubectl delete pod ns47j
After:  gzd6r (4m old), tfxpw (4m old), tj4ht (116s old)  ← 自动补的新 Pod
```

**面试金句**：

> K8s 自愈不是事件触发，是**持续状态比对**。它不需要知道"发生了什么"，只需要知道"现在和应该的差距"。这就是**声明式 API 的威力**。

### ③ Service ClusterIP —— "iptables 玩出来的虚拟 IP"

```
kubectl get svc → ClusterIP 10.43.x.x (虚拟，没人绑定！)
   │
   ▼
kube-proxy 在每个 node 上写 iptables:
   "凡是发往 10.43.x.x:80 的包，DNAT 改写成 Pod1/2/3 之一（随机选）"
```

**实测**：6 个请求分布在 3 个 Pod，比例 3:2:1（小样本随机）：

```bash
for i in 1..6: curl localhost:30080/
→ ns47j ×3, gzd6r ×2, tfxpw ×1
```

**面试金句**：

> Service 的 ClusterIP **没有任何机器真的绑这个 IP**，纯靠 iptables 规则拦截改写。这就是为什么集群外 ping 它不通——iptables 规则只存在于集群 node 上。

---

## 四、阶段 4.1：基础部署

### Deployment + Service YAML 关键点

```yaml
# deployment.yaml
spec:
  replicas: 3
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxSurge: 1               # 更新时最多多 1 个 Pod
      maxUnavailable: 0         # 更新时不能减少 Pod
  template:
    spec:
      containers:
      - resources:
          requests:
            cpu: 100m            # ★ HPA 的"利用率"分母
        readinessProbe:           # 没 ready → Service 不分流
          httpGet: { path: /healthz, port: 8080 }
```

**关键设计点**：
- `requests.cpu: 100m`：HPA 利用率 = 实际 / requests。**不写 requests 等于 HPA 失效**
- `maxUnavailable: 0`：滚动更新时永远保留所有 ready Pod，**零停机**
- `readinessProbe`：失败的 Pod 自动从 Service 后端摘除

---

## 五、阶段 4.2：HPA 自动扩缩容（核心高潮）

### HPA 配置

```yaml
spec:
  minReplicas: 2
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50      # ★ CPU > 50m (= 100m × 50%) 就扩
  behavior:
    scaleUp:
      stabilizationWindowSeconds: 0  # 立即扩
      policies: [{type: Percent, value: 100, periodSeconds: 15}]  # 每 15s 最多翻倍
    scaleDown:
      stabilizationWindowSeconds: 60 # 等 60s 确认不是抖动
      policies: [{type: Percent, value: 50, periodSeconds: 30}]   # 每 30s 最多减半
```

### 实测扩缩容时间线 🎯

| 时刻 | CPU 利用率 | Pod 数 | 行为 |
|---|---|---|---|
| T+0 | 1% / 50% | 2 | 闲置基线 |
| T+? | **501%** / 50% | 2 | 压测开始，CPU **暴涨 10 倍超标** |
| +1min | 501% | 2→**4** | 第一次扩容（翻倍策略） |
| +2min | 334% | 4→**8** | 还在烧，再翻倍 |
| +3min | 143% | 8→**10** | 撞 maxReplicas 天花板 |
| +5min | 100% / 50% | 10 | 持续超标但已到 max |
| +6min | 53% / 50% | 10 | 接近目标 |
| **+7min** | **1%** | **10** | 压测结束，**CPU 瞬降但 Pod 不动** ⚠ |
| +8min | 1% | 10 | **等待 60 秒稳定窗口** |
| +9min | 1% | 10→**5** | **缩半**（scaleDown 50% / 30s） |
| +10min | 1% | 5→**2** | **再缩半**，触底 minReplicas |

### "扩容快、缩容慢"——为什么这是生产标配

| 阶段 | 行为 | 设计意图 |
|---|---|---|
| **扩容** | 1 分钟内翻倍，3 分钟到 max | 流量来了**立刻反应**，宁可多开几个也不让用户卡 |
| **缩容** | 流量没了等 2 分钟才动 | **怕"假摔"**——用户可能短暂离开，刚缩完又来流量就尴尬 |

业界叫这种策略 **"敏捷扩、稳重缩"**（fast scale-out, slow scale-in）。

### HPA 怎么算"该扩多少"

不是简单二选一，是**比例计算**：

```
所需 Pod 数 = ceil(当前 Pod 数 × (实际利用率 / 目标利用率))

例：当前 2 Pod，CPU=501%，目标 50%
   = ceil(2 × 10.02) = 21 个理论需要
   但 maxReplicas=10 → 卡到 10
   scaleUp policy 每次最多翻倍 → 第一步只能到 4
   → 实际看到的 2→4→8→10 是"想跳到 21 被策略一步步逼近"
```

### 撞到 maxReplicas 怎么办

实测：CPU 100% 但 Pod 数停在 10——**HPA 不会突破 max**。生产意味着：
- 告警："HPA 持续撞顶 > 5 分钟" → 人工介入（提高 max / 加机器）
- **不能让 HPA 无限扩**，否则 1 个 bug 流量把账单打爆

---

## 六、阶段 4.2 加餐：滚动更新 + 回滚

### 演练 1：v1 → v2 滚动更新

```bash
kubectl set env deployment/product-app APP_VERSION=v2
kubectl rollout status deployment/product-app
```

**整个过程 ~30 秒**：

| 时间 | ReplicaSet 状态 |
|---|---|
| T+0s | `6bbbdf9bb8`: 2/2/2 (v1), `fd4447bf` 不存在 |
| T+7s | `6bbbdf9bb8`: 0/0/0, `fd4447bf`: 2/2/2 (v2) ★ |
| 7 秒就完成切换 | **旧 RS DESIRED=0 但不删除**（保留作快照） |

`for i in 1..5: curl /` → 5 个全部 `"version":"v2"` ✅。

### 演练 2：一行命令回滚

```bash
kubectl rollout history deployment/product-app
# REVISION  CHANGE-CAUSE
# 1         <none>          ← v1
# 2         <none>          ← v2

kubectl rollout undo deployment/product-app   # 回滚到 revision 1
```

**~30 秒后**：所有 Pod 回到 v1，**Pod 名是新的 hash**（`mdb6z`, `5zm9v`）。

### 重要细节 ⚡（这是面试加分点）

> **回滚保留的是 ReplicaSet 模板（spec），不是原始 Pod**。原 v1 Pod 在切到 v2 时被删了；回滚用保留的 spec 创建**新 Pod**——所以 hash 是新的。
>
> 这意味着：
> - ✅ 能回滚到老配置（image / env / 资源）
> - ❌ 不能回滚 Pod 内的副作用（数据库迁移、消息发送）

**生产陷阱**：

> 蓝绿/滚动发布时 **DB schema 改动必须保证前向兼容**（v2 不删 v1 用的字段），否则回滚等于自爆。这是面试常考的"如何安全地做数据库迁移"。

---

## 七、踩坑记录

| # | 坑 | 现象 | 根因 | 解决 |
|---|---|---|---|---|
| 1 | k3d kubeconfig 用 `0.0.0.0` | `kubectl get nodes` timeout / EOF | macOS 上 0.0.0.0 在 kubectl client 端反复重试失败 | `kubectl config set-cluster ... --server=https://127.0.0.1:62958` |
| 2 | k3d 启动早期 controller 日志吓人 | `error syncing 'agent-0': address annotations not yet set` | k3d agent 启动时 node controller 在等 cloud-controller 填地址注解 | 等 30 秒自动好；不是 bug |
| 3 | 镜像在 host 但 Pod ImagePullBackOff | k8s 拉不到本地 docker 镜像 | k3d 集群在独立 Docker 网络，看不到 host docker registry | `k3d image import IMG -c CLUSTER` 显式导入 |
| 4 | HPA 永远不扩 | `kubectl get hpa` 显示 `<unknown>/50%` | Deployment 没设 `resources.requests.cpu` | 加上 requests，HPA 用它当 100% 分母 |
| 5 | 滚动更新 maxUnavailable 默认非 0 | 更新过程中容量短暂下降 | K8s 默认 maxUnavailable=25% | 显式设 `maxUnavailable: 0` 保证零停机 |
| 6 | 回滚后 Pod hash 全新 | 以为回滚="时光倒流" | K8s 只保留 RS spec，不保留 Pod | 接受这个事实，做迁移时前向兼容 |

### 第 6 个坑的深层教训

> Kubernetes 的"回滚"是**重建容器**，不是**还原状态**。Pod 内的副作用（DB schema 改动、消息已发送、缓存已失效）一旦发生就回不来。**蓝绿发布的"安全保险"实际上 80% 是数据库设计决定的，不是 K8s 能力决定的**。

---

## 八、四个反直觉结论（写进简历）

### ① Service 的 IP 是"假的"
ClusterIP 没人真的绑定，全靠 iptables 改写。**集群外 ping 不通是设计如此**。

### ② Deployment 的回滚是"重建"，不是"还原"
保留的是 spec 模板，不是 Pod 状态。**DB schema 迁移必须前向兼容**，否则回滚 = 自爆。

### ③ HPA "扩快缩慢" 是刻意的不对称
扩容立刻翻倍，缩容等 60s + 减半。**生产怕的不是流量来不及扩，是流量"假摔"导致频繁扩缩抖动**。

### ④ K8s 的"自愈"不是事件驱动，是状态比对
Reconcile loop 持续运行，**它根本不关心"为什么少了一个 Pod"，只关心"应该有 3 个、现在有 2 个、补一个"**。这就是声明式的威力。

---

## 九、关键演练数据汇总（一表打天下）

| 演练 | 数据 |
|---|---|
| **Deployment 自愈** | kill 1 Pod，**3 秒**新 Pod 顶上 |
| **Service 分流** | 6 请求 / 3 Pod（iptables 随机） |
| **HPA 扩容峰值** | CPU **501% / 50%**，Pod 数 **2 → 10**（5 倍） |
| **HPA 扩容耗时** | 3 分钟到 max |
| **HPA 缩容耗时** | 2 分钟从 10 缩到 2（含 60s 稳定窗口） |
| **滚动更新耗时** | 30 秒，**零停机** |
| **回滚耗时** | 30 秒，一行命令 |
| **资源利用率** | 单 Pod request 100m，limit 500m |

---

## 十、阶段四**没**解决的问题（留给阶段五）

| 问题 | 解决在 |
|---|---|
| StatefulSet：把 Redis/MySQL/Kafka 也搬到 K8s | **阶段 5**：Operator / StatefulSet + PV |
| 跨地域容灾 + 多集群联邦 | **阶段 5**：多地域部署 |
| 混沌工程：随机杀 Pod / 注入网络延迟 | **阶段 5**：Chaos Mesh |
| 实际生产 CI/CD：ArgoCD GitOps | **阶段 5**：声明式发布 |
| Service Mesh：Istio 流量管理 | **阶段 5+**：sidecar 代理 |
| 自定义指标 HPA（QPS 而不是 CPU） | **阶段 5**：Prometheus Adapter + HPA |

---

## 十一、面试包装话术

> 阶段 4 把阶段 1-3 的 docker-compose 栈中无状态的 `product-app` 迁到 k3d Kubernetes 集群，演练了 K8s 三个核心能力：
>
> 1. **声明式自愈**：手动 `kubectl delete pod`，3 秒内新 Pod 自动顶上——靠的是 Deployment Controller 的 reconcile loop，**持续状态比对而非事件触发**。
>
> 2. **HPA 自动扩缩容**：50 VU 烧 CPU 3 分钟，CPU 利用率从 1% 飙到 501%，Pod 数从 2 自动扩到 max=10。压测结束后**等 60s 稳定窗口 + 每 30s 减半**，缓慢缩回 2。这就是生产"扩容快、缩容慢"的不对称设计——**怕的不是流量来不及扩，是流量假摔导致抖动**。
>
> 3. **零停机滚动更新 + 一键回滚**：`kubectl set env` 改一个变量触发滚动更新，30 秒内逐个替换 Pod，`maxUnavailable: 0` 保证零停机。回滚也是一行命令、30 秒完成。
>
> 这个过程最深的认知转变：**K8s 的"回滚"不是状态还原，是用旧 ReplicaSet 模板重建容器**。Pod 内的副作用（DB schema 迁移、消息发送）一旦发生就回不来——所以**蓝绿发布的安全性 80% 由数据库前向兼容决定，不是 K8s 能力决定**。这是 SRE 面试经常问的"如何安全地做线上迁移"的真正答案。

---

## 十二、文件清单（阶段四新增）

```
ha-arch/
├── PHASE1.md / PHASE2.md / PHASE3.md / PHASE4.md   ← 本文档
│
└── k8s/                                            ← ★ 阶段四新增
    ├── app/                                        ← 简化的 K8s demo 服务
    │   ├── main.go                                 ← /、/load?ms=N、/healthz
    │   ├── go.mod
    │   └── Dockerfile
    └── manifests/                                  ← K8s 资源定义
        ├── deployment.yaml                         ← 3 副本 + 探针 + 滚动策略
        ├── service.yaml                            ← NodePort 30080
        └── hpa.yaml                                ← 自动扩缩容规则

└── loadtest/
    └── k8s-load.js                                 ← ★ 烧 CPU 触发 HPA
```

---

## 十三、命令速查

```bash
# === 集群管理 ===
k3d cluster create ha-arch-k8s --servers 1 --agents 2 --port "30080:30080@loadbalancer"
k3d cluster list
k3d cluster delete ha-arch-k8s
kubectl config use-context k3d-ha-arch-k8s

# === 镜像导入 ===
docker build -t k8s-demo:v1 ./k8s/app
k3d image import k8s-demo:v1 -c ha-arch-k8s

# === 部署 / 查看 / 故障演练 ===
kubectl apply -f k8s/manifests/
kubectl get deploy,rs,pods,svc,hpa -l app=product
kubectl get pods -o wide                              # 看 Pod 分布在哪些 node
kubectl delete pod <name>                              # 演练自愈
kubectl logs <pod-name>
kubectl exec -it <pod-name> -- sh

# === HPA 演练 ===
kubectl get hpa product-app-hpa --watch                # 实时看扩缩容
kubectl top pods                                        # 看 Pod 资源使用
k6 run loadtest/k8s-load.js                            # 触发 CPU 负载

# === 滚动更新 / 回滚 ===
kubectl set env deployment/product-app APP_VERSION=v2  # 触发滚动更新
kubectl rollout status deployment/product-app
kubectl rollout history deployment/product-app
kubectl rollout undo deployment/product-app            # 回滚到上一版本
kubectl rollout undo deployment/product-app --to-revision=1   # 回滚到指定版本

# === 清理 ===
kubectl delete -f k8s/manifests/                       # 删资源（保留集群）
k3d cluster stop ha-arch-k8s                           # 停集群（保留状态）
k3d cluster delete ha-arch-k8s                         # 删整个集群
```
