# 阶段五总结：混沌工程 + StatefulSet（K8s 的"硬核"能力）

> **目标**：完成"我能扛各种故障"的最后两块拼图——**主动制造故障**（验证 HA 真的成立）+ **真正的有状态服务**（K8s 化数据层）。

---

## 一、整体架构变化

```
[ 阶段 4 ]                           [ 阶段 5 ]
k3d 集群:                            k3d 集群:
  product-app × 2-10 (HPA)            product-app × 2-10 (HPA)
                                      + chaos-mesh × 6 (故障注入)
                                      + redis-0,1,2 (StatefulSet + PVC)
                                      
关键能力升级:
  ✅ 主动制造故障（Chaos Mesh）
  ✅ 有状态服务 K8s 化（StatefulSet + PVC）
  ✅ 验证 K8s 自愈的"边界"——硬故障能修、软故障管不了
```

---

## 二、容器/Pod 拓扑（最终 ~32 容器/Pod）

```
[ docker-compose 栈 ]：24 容器
  ├── Redis Sentinel 集群（阶段 2 保留）
  ├── MySQL 主从 + ProxySQL（阶段 2 保留）
  ├── Kafka 3 broker + UI（阶段 3 保留）
  ├── APISIX 网关（阶段 3 保留）
  ├── Prometheus + Grafana（阶段 2 保留）
  └── 监控 exporters

[ k3d 集群 ]：3 节点
  ├── 系统 Pod
  │   ├── metrics-server（HPA 依赖）
  │   ├── traefik（k3s 内置 ingress）
  │   └── local-path-provisioner（PVC 自动建）
  ├── chaos-mesh × 6
  │   ├── chaos-controller-manager × 3 (HA)
  │   ├── chaos-daemon × 3 (DaemonSet，每 node 一个)
  │   ├── chaos-dashboard
  │   └── chaos-dns-server
  ├── 业务 Pod
  │   └── product-app × 2-10 (Deployment + HPA)
  └── 数据 Pod
      └── redis × 3 (StatefulSet + 3 PVC)
```

---

## 三、子阶段 5.1：Chaos Mesh 混沌工程

### 设计原则（必懂）

| 概念 | 解释 |
|---|---|
| **稳态（Steady State）** | "我系统平时什么样"——具体到数字（P99 < 50ms、错误率 < 0.1%、Pod 数稳定） |
| **假设（Hypothesis）** | "如果发生 X，应该有 Y 结果"——必须**可证伪** |
| **爆炸半径（Blast Radius）** | "实验影响多大范围"——**先小、可控、可终止** |

### 实现机制（必懂）

Chaos Mesh 不改你的应用，靠 Linux 内核能力：

| 故障类型 | 实现机制 | 改的位置 |
|---|---|---|
| 杀 Pod | K8s API delete | 控制面 |
| 网络延迟/丢包 | `tc qdisc netem` | Pod 的 veth 网卡 |
| 网络分区 | `iptables DROP` | Pod 的 network namespace |
| CPU 打满 | 进入 cgroup 跑 `stress-ng` | 目标容器的 cgroup |
| 磁盘 IO 延迟 | fuse 挂载层 | 文件系统层 |

部署形式：每 node 一个 DaemonSet（hostPID + privileged），可以进入任何 Pod 的命名空间。

### 三大混沌实验实测数据

#### 实验 1：PodChaos（每 20s 杀 1 个 Pod，持续 60s）

```yaml
kind: Schedule          # Chaos Mesh 2.x 用 Schedule 包装重复实验
spec:
  schedule: '@every 20s'
  type: PodChaos
  podChaos:
    action: pod-kill
    mode: one
    selector: { labelSelectors: { app: product } }
```

**实测结果**：

| 指标 | 数值 |
|---|---|
| 总请求 | 659 |
| 成功 (2xx) | **659** |
| **成功率** | **100.00%** ✅ |
| P50 延迟 | 2.2 ms |
| P95 延迟 | 3.0 ms |
| P99 延迟 | 3.4 ms |

**为什么 100% 成功率不是侥幸**：

```
3 个机制叠加保证零失败：
  1. readinessProbe        → Pod terminating 立刻摘出 Service endpoint
  2. iptables 实时更新     → kube-proxy 毫秒级摘除路由规则
  3. Deployment 提前补 Pod → 杀前 5 个 → 杀后 4 个 → 立刻拉 1 个新的
                             总容量从不低于 4
```

#### 实验 2：NetworkChaos（500ms 延迟 + 50ms jitter，持续 60s）

```yaml
kind: NetworkChaos
spec:
  action: delay
  mode: all
  delay: { latency: '500ms', jitter: '50ms' }
```

**实测结果**（对比实验 1）：

| 指标 | 实验 1 (Pod-kill) | **实验 2 (Net-delay)** | 倍数 |
|---|---|---|---|
| 成功率 | 100% | **100%** | 同 |
| P50 延迟 | 2.3 ms | **2.3 ms** | 同 ← 部分请求基线（窗口外） |
| **P95 延迟** | 3.0 ms | **1031 ms** | **343×** |
| **P99 延迟** | 3.4 ms | **1070 ms** | **315×** |

**为什么 P99 是 ~1s 而不是 ~500ms**：

```
NetworkChaos 默认 direction: both
   ├─ Pod ingress 加 500ms
   └─ Pod egress  加 500ms
   总 RTT ≈ 1000ms（双向都被延迟）
```

**这次实验的"金句"**：

> **延迟 ≠ 不可用**。包到达了，K8s 觉得"系统正常"，**完全不会触发任何修复**。
>
> - Pod 挂 → K8s 修 ✅
> - 包变慢 → K8s **管不了** ❌
>
> 必须靠**应用层 timeout + 熔断**（这就是阶段 3.1 APISIX api-breaker 的真正价值）。

#### 实验 3：StressChaos（单 Pod CPU 打满，持续 120s）

```yaml
kind: StressChaos
spec:
  mode: one
  stressors: { cpu: { workers: 2, load: 100 } }
  duration: '120s'
```

**实测 HPA 时间线**：

| 时刻 | CPU 利用率 | Pod 数 | 行为 |
|---|---|---|---|
| T+0 | 1% / 50% | 2 | 基线 |
| T+15s | 44% / 50% | 2 | chaos 启动，stress-ng 在 1 个 Pod 烧 CPU |
| T+30s | **250%** / 50% | 2 | 单 Pod 打到 limit，平均 250% |
| T+45s | 250% | 2→**4** | HPA 第一次扩 |
| T+60s | 167% | 4→**8** | 继续扩 |
| T+75s | 101% | 8→**10** | 撞 max |
| T+2m | 50% | 10 | 稳态（chaos 被 10 个 Pod 摊薄） |
| T+2m30s | 5% | 10 | chaos 结束，CPU 急降 |

**和阶段 4.2 HPA 演练的曲线几乎完全一样**——但触发原因完全不同！

**这次实验的"金句"**：

> **HPA 是"流量盲"的**。它只看 CPU 利用率，**完全不知道这 CPU 是怎么用掉的**。
>
> - 用户流量烧 CPU → HPA 扩容 ✅ 对的
> - **bug 烧 CPU → HPA 也扩容** ❌ 错的
>
> 生产隐患：**1 个 bug Pod 让 HPA 拉起 9 个健康 Pod"陪绑"**——CPU 平均看似正常，但 bug 没解决，还在烧钱。
>
> **真正的 SRE 解决方案**：基于业务指标的自定义 HPA（QPS、Kafka lag、错误率），不要只看 CPU。这需要 Prometheus Adapter + custom metrics API。

---

## 四、子阶段 5.2：StatefulSet —— 真正的有状态服务

### 为什么 Deployment 干不了数据库

| Deployment 的假设 | 在 stateful 场景下的灾难 |
|---|---|
| Pod 名是随机 hash | 主从配置文件里**没法写死地址** |
| Pod 重启换新名 | DNS 记录失效，**主从找不到对方** |
| 共享或不挂 PVC | **多 Pod 抢同一份数据 → 损坏** |
| 不保证启动顺序 | replica 可能在 master 前就启动 |

**生产真实灾难**：用 Deployment 部 MySQL，2 副本共享 PVC → 抢锁文件 → 数据库永久损坏。

### StatefulSet 解决的四件事

```
1. 稳定 Pod 名     redis-0, redis-1, redis-2 (永不变)
2. 稳定 DNS        redis-0.redis-headless.default.svc.cluster.local
3. 稳定存储        每 Pod 独占 PVC，PVC 名 = redis-data-<pod-name>
4. 有序生命周期    启动 0→1→2，停止 2→1→0
```

### Headless Service（无头服务）

```yaml
spec:
  clusterIP: None      # ★ 关键：没 ClusterIP = headless
  selector: { app: redis }
```

**两种用法**：

| DNS 名 | 返回 |
|---|---|
| `redis-headless.default.svc.cluster.local` | **所有 Pod 的 IP 列表**（客户端自己选） |
| `redis-0.redis-headless.default.svc.cluster.local` | **redis-0 这个 Pod 的 IP**（直连） |

**对比普通 Service**：普通 Service 通过 iptables 把流量随机分流到任意后端 Pod；Headless **不做 LB，只做 DNS**——客户端通过名字**直连指定 Pod**。

### PVC 自动绑定规则

```
StatefulSet redis + volumeClaimTemplate redis-data → 自动创建：
  Pod redis-0 → PVC: redis-data-redis-0  ★ 名字算出来的，固定
  Pod redis-1 → PVC: redis-data-redis-1
  Pod redis-2 → PVC: redis-data-redis-2
```

不管 Pod 重启多少次、迁移到哪个 node，**永远找到同名 PVC 挂回去**——这就是 stateful 的本质。

### 三个核心演练实测数据

#### 演练 1：写数据 → kill Pod → 数据持久 ✅

```bash
kubectl exec redis-0 -- redis-cli SET mykey "I should survive pod kill"
kubectl delete pod redis-0
sleep 30
kubectl exec redis-0 -- redis-cli GET mykey
# → "I should survive pod kill" ✅
```

**为什么数据没丢**：
1. AOF 持久化把 SET 命令写到 `/data/appendonly.aof`
2. `/data` 是 PVC `redis-data-redis-0` 的挂载点
3. Pod 删除时**PVC 不删**
4. 新 redis-0 起来，K8s 自动找到**同名 PVC**挂回
5. Redis 启动时读 AOF 恢复数据

#### 演练 2：scale up 4 → 自动创建新 PVC ✅

```bash
kubectl scale statefulset redis --replicas=4
# → redis-3 自动创建（不是随机 hash，是确定的下一个序号）
# → PVC redis-data-redis-3 自动创建（按命名规则）
```

#### 演练 3：scale down 3 → PVC 不删（数据安全）★ 最重要

```bash
kubectl scale statefulset redis --replicas=3
kubectl get pods    # 只剩 redis-0/1/2
kubectl get pvc     # ★ 仍然 4 个 PVC（redis-data-redis-3 没被删）
```

```bash
kubectl scale statefulset redis --replicas=4
# redis-3 重新创建，挂回原 PVC
kubectl exec redis-3 -- redis-cli GET key3
# → "from redis-3" ✅ 数据完整保留
```

**为什么 K8s 故意不删 PVC**：

> **Pod 是临时的，PVC 是数据**。删 Pod 删数据是**不可逆**操作，K8s 设计上**绝不自动做**——必须用户**显式 `kubectl delete pvc`** 才真删。
>
> 这是**数据安全防线**，不是 bug。生产里你可以放心 `kubectl scale` 调整 StatefulSet 大小——**最坏情况是缩容后数据"暂时离线"，永远能扩回来恢复**。

---

## 五、踩坑记录

| # | 坑 | 现象 | 根因 | 解决 |
|---|---|---|---|---|
| 1 | Chaos Mesh install.sh 版本检查 | `version skew > +/-1` | 安装脚本严格检查 kubectl client / server 版本差 | 用 `helm install` 绕过 |
| 2 | PodChaos `spec.scheduler` 不存在 | apply 时报 `unknown field "spec.scheduler"` | Chaos Mesh 2.x 把 scheduler 移到独立 `Schedule` 资源 | 用 `kind: Schedule` 包一层 |
| 3 | NetworkChaos 看到 1s 而不是 500ms | P95/P99 ≈ 1000ms | `direction` 默认 `both`，双向各 500ms = RTT 1s | 接受这个事实，或显式设 `direction: to` |
| 4 | chaos-daemon 长时间 ContainerCreating | 4 分钟还 Pending | 镜像还在拉（网络慢） | 等几分钟，或预先 `k3d image import` |
| 5 | StatefulSet scale down 后磁盘空间没回收 | 期望"删 Pod 删数据" | K8s 故意保留 PVC | 是 feature 不是 bug，要清空间手动 `delete pvc` |
| 6 | zsh 解析 `(nil)` 报 unknown file attribute | 命令里有 `# → (nil)` 注释 | zsh 把括号当 glob 模式 | 删掉注释或加引号 |

---

## 六、K8s 自愈的边界（这一节核心结论）

阶段 5 暴露了 K8s 强大但**不万能**的边界：

| 故障类型 | K8s 能修吗 | 实测 | 谁来修 |
|---|---|---|---|
| **Pod 进程崩溃** | ✅ | livenessProbe 重启 | K8s |
| **Pod 节点漂移** | ✅ | Deployment 重建到健康 node | K8s |
| **Pod 误删** | ✅ | 3 秒新 Pod 顶上 | K8s |
| **网络延迟** | ❌ | P99 飙 500 倍但 K8s 不动 | **应用层 timeout/熔断** |
| **网络丢包** | ❌ | 包丢了 K8s 不会感知 | **应用层重试** |
| **CPU 异常占用** | ⚠️ 误诊 | HPA 当流量大去扩容 | **业务监控 + custom HPA** |
| **内存泄漏** | ⚠️ 暴力解决 | OOM kill 然后重启 | **真正解决 = 应用层修 bug** |
| **磁盘 IO 慢** | ❌ | K8s 完全感知不到 | **应用层超时 + 降级** |
| **数据腐败** | ❌ | Pod 看着正常但数据错了 | **业务层校验 + 回滚** |

**核心结论**（必背）：

> **K8s 修硬故障，应用层修软故障**。HA 不是"我用了 K8s 就 100%"，**真正的 HA 需要 K8s + APISIX + Prometheus + 应用层韧性 + 团队 oncall 的总和**。

---

## 七、阶段 5 未尽事宜（值得深入但本阶段不做）

| 主题 | 内容 | 难度 |
|---|---|---|
| **5.3 Operator 模式** | 把"运维知识"编码进 CRD（Redis Operator / MySQL Operator）| 高 |
| **5.4 GitOps (ArgoCD)** | 声明式 CD：git push 即部署，集群状态永远和 git 一致 | 中 |
| **5.5 Service Mesh (Istio)** | sidecar 代理做流量管理、可观测、安全 | 高 |
| **5.6 多集群联邦** | 跨地域部署、流量分发、故障迁移 | 高 |
| **5.7 真正生产化 Chaos** | Chaos Dashboard + 定时调度 + 自动 abort | 中 |
| **5.8 Custom Metrics HPA** | 基于 QPS / 队列长度的扩缩容（不是 CPU） | 中 |

每一个都能讲一整周。阶段 5 收尾把 1+2 做扎实就行。

---

## 八、五个阶段的反直觉结论汇总（写进简历金句）

按阶段顺序，每个阶段一句：

| # | 阶段 | 反直觉金句 |
|---|---|---|
| 1 | 基础骨架 | 调优的本质是**瓶颈搬家**，单机优化必有天花板 |
| 2 | HA + 监控 | **MySQL 故障切换难点不在切走，在节点回归**（GTID 冲突） |
| 3 | 网关 + MQ | **MQ 不为提速，为扛峰值**（1K RPS 同步比异步快 3 倍） |
| 4 | k8s + HPA | **回滚是"重建容器"，不是"还原状态"**（DB schema 必须前向兼容） |
| **5** | **混沌 + StatefulSet** | **K8s 修硬故障，应用层修软故障**——延迟不是 K8s 的责任 |

---

## 九、面试包装话术

> 阶段 5 是这套架构的最后一公里：用 Chaos Mesh 主动注入故障验证 HA、用 StatefulSet 把数据层真正搬上 K8s。
>
> **混沌实验跑了三个**：
> 1. 每 20 秒杀一个 Pod，持续 60 秒，最终**100% 成功率**——证明 Deployment + Service 的硬故障自愈机制
> 2. 注入 500ms 网络延迟，**P99 飙到 1000ms 但成功率仍 100%**——暴露 K8s 自愈的边界：**延迟不是 K8s 的责任**
> 3. 单 Pod CPU 打满，**HPA 误诊为流量大而扩容到 max=10**——揭示 HPA 是"流量盲"，生产应该用 custom metrics HPA
>
> **StatefulSet 演练了三个**：
> 1. 写数据 → 杀 Pod → 数据通过 PVC 保留 ✅
> 2. scale 上去自动创建新 PVC（命名规则 `redis-data-redis-N`）
> 3. **scale 下来 PVC 不删**——这是 K8s 故意的数据安全防线，删 Pod ≠ 删数据
>
> 这个过程最深的认知：**K8s 不是 HA 的银弹**。它修 Pod 挂、节点崩、滚动发布这类"硬故障"是世界级的；但**网络延迟、CPU 异常占用、数据腐败这类"软故障"**，K8s 完全管不了，必须靠 **APISIX 熔断 + Prometheus 告警 + 应用层 timeout + 业务监控**的组合拳。**真正的 HA = K8s + 应用层韧性 + 团队 oncall 的总和**。

---

## 十、文件清单（阶段五新增）

```
ha-arch/
├── PHASE1-5.md
│
└── k8s/
    └── manifests/
        ├── chaos-pod-kill.yaml         ← Schedule 包装 PodChaos
        ├── chaos-network-delay.yaml    ← 500ms 延迟注入
        ├── chaos-cpu-stress.yaml       ← stress-ng CPU 打满
        ├── redis-statefulset.yaml      ← 3 副本 + AOF + 1Gi PVC 模板
        └── redis-headless.yaml         ← clusterIP: None 的 Service

└── loadtest/
    └── chaos-monitor.sh                ← bash 监控成功率 + P50/95/99 延迟
```

---

## 十一、命令速查

```bash
# === Chaos Mesh ===
helm install chaos-mesh chaos-mesh/chaos-mesh -n chaos-mesh \
  --set chaosDaemon.runtime=containerd \
  --set chaosDaemon.socketPath=/run/k3s/containerd/containerd.sock

kubectl apply -f k8s/manifests/chaos-pod-kill.yaml      # Pod 随机杀
kubectl apply -f k8s/manifests/chaos-network-delay.yaml # 网络延迟
kubectl apply -f k8s/manifests/chaos-cpu-stress.yaml    # CPU 打满

kubectl get podchaos networkchaos stresschaos schedule  # 看实验状态
kubectl delete schedule pod-kill-schedule               # 终止重复实验
./loadtest/chaos-monitor.sh                             # 实时监控

# === StatefulSet ===
kubectl apply -f k8s/manifests/redis-headless.yaml
kubectl apply -f k8s/manifests/redis-statefulset.yaml

kubectl get pods -l app=redis            # 看稳定命名 redis-0/1/2
kubectl get pvc                          # 看 PVC 自动绑定
kubectl get pv                           # 看 PV 自动创建

# 直连指定 Pod
kubectl exec redis-0 -- redis-cli SET k v
kubectl exec redis-0 -- redis-cli GET k

# 通过 Headless DNS 直连
kubectl run tmp --rm -it --image=redis:7-alpine --restart=Never -- \
  redis-cli -h redis-0.redis-headless GET k

# scale + 数据持久演练
kubectl scale statefulset redis --replicas=4
kubectl scale statefulset redis --replicas=3   # PVC 不删！
kubectl scale statefulset redis --replicas=4   # 数据恢复

# 真要清数据
kubectl delete pvc redis-data-redis-3
```
