# KOS

KOS 是运行在 Kubernetes 上的 homelab 操作系统。本仓是 **kos-shell** 的实现：内核对象模型、
归属标记、扩展接口与 ABI 的实现骨架。设计仓（意图与承诺）为
<https://gitea.tsic.top/tsic/kos-design>，契约以 `docs/02-contracts.md` 为准。

当前阶段交付内核对象模型、归属三态、写入路径、ABI、控制面 API 与通用调和内核；不含具体对象的调和与扩展装载。

## 布局

| 路径 | 内容 |
|---|---|
| `api/` | KOS API 组标识、十类内核对象的封闭集合、双版本 scheme |
| `api/v1alpha1/` | 第一版对象类型与状态形状 |
| `api/v1beta1/` | 第二版对象类型；与 v1alpha1 同结构，是存储版本 |
| `config/crd/` | 由 Go 类型生成的 CRD 清单（`make generate`） |
| `pkg/markers/` | C-02 归属标记的键、载体与校验 |
| `pkg/abi/` | ABI 自检：kind 集合、作用域、双版本同结构、无 webhook |
| `pkg/state/` | C-07/C-08 状态轴：`Reconciled` 三值、失败条件与健康聚合 |
| `pkg/reconcile/` | C-06 通用调和循环：去重工作队列、单轮时间预算与指标 |
| `pkg/controlplane/` | 控制面 HTTP 面：资源路由、投影、统一错误包络、计划请求 |
| `pkg/write/` | 写入路径：C-03/C-04 写入者分配表、准入判定、服务端合并提交与 409 冲突 |
| `pkg/adoption/` | 归属动作：接管、释放、放弃与接管前的不可变快照 |
| `pkg/serial/` | 调和循环之外显式写入动作的按对象串行门（调和轮次由 `pkg/reconcile` 串行） |
| `tests/acceptance/TC-27/` | 读投影带观测时间，超阈值标记陈旧 |
| `tests/acceptance/TC-28/` | 同一写请求重复提交只产生一个对象 |
| `tests/acceptance/TC-29/` | 四类失败的错误包络，断线后操作仍到终态 |
| `tests/acceptance/TC-35/` | 计划请求无副作用，计划与执行一致 |
| `tests/acceptance/TC-104/` | 同一对象双版本读取的字段一致性 |
| `tests/acceptance/TC-105/` | 转换策略为无转换、集群无准入 webhook |
| `tests/acceptance/TC-25/` | `Reconciled` 的 True、False、Unknown 三值 |
| `tests/acceptance/TC-26/` | 重复调和无增量、超限单轮可被指标观察到 |
| `tests/acceptance/TC-17/` | 接管、释放与放弃 |
| `tests/acceptance/TC-18/` | 父对象调和器不写子对象 spec |
| `tests/acceptance/TC-19/` | 归属标记的载体 |
| `tests/acceptance/TC-24/` | 并发写入与角色写权限 |

## 对象模型

API 组为 `kos.tsic.top`。十类对象：Application、ApplicationDefinition、Volume、Endpoint、Provider、
SystemComponent、BackupPolicy、RestoreJob、NodeDevice、VolumeMigration。其中
ApplicationDefinition、Provider、SystemComponent、NodeDevice 为集群作用域，其余为命名空间作用域。

ApplicationDefinition 与 NodeDevice 没有 status 写者，因此没有 status 字段与 status 子资源；
其余八类带 `status.phase`、`status.health.state` 与 `Reconciled` 条件。

`spec` 字段由契约逐个加入：尚未固定字段的对象保持空 spec，实现不发明线格式。当前已兑现
C-60 的 `spec.owner`（五类带所有者的对象）与 C-59 的 `Application.spec.visibility`。

## ABI

- 同一时刻服务 `v1alpha1` 与 `v1beta1`，存储版本为 `v1beta1`；两版逐字段同结构，转换策略为
  `None`，不存在转换 webhook。
- `api/v1beta1` 是 `api/v1alpha1` 的结构副本，双份类型是刻意的：C-16 要求相邻版本同结构，
  复制让差异在代码里可见，`pkg/abi` 与 `config/crd` 的自检让它无法悄悄漂移。
- 扩展对象类属于扩展自己的 API 组，不得出现在 `kos.tsic.top`（C-19）。

## 标记

`pkg/markers` 固定 C-02 的九个键：归属（`managed-by`、`owner-uid`、`adopted-by`、
`adoption-reason`）、放弃（`declined`）、释放（`released`）、有效性（`invalid`、`stale`）。
除 `adopted-at` 用注解外一律用标签；未列入的注解不改变行为。

## 写入路径

`pkg/write` 把 C-03/C-04 的写入者分配写成表：主体、对象类与对象部位（`spec`、`status`、
标记）三者查一张表，表外即拒绝，拒绝信息带上主体与对象。用户写入走服务端合并提交，字段
管理器按主体区分，同一字段集的第二个写者拿到 409 与当前对象；不相交字段集都保留。

`pkg/adoption` 是 C-02 的三个动作。接管写入归属标记，并在 `kos-system` 留下一条
`immutable` 的 ConfigMap 记录，内容等于接管前 spec；释放移除归属标记，对象保持当前状态且
KOS 不再写它；放弃写入 `declined`，对象留在 unmanaged，不再被列为可接管对象。

`pkg/serial` 是显式门：调和循环之外的读—改—写动作（接管、释放、放弃）按对象键串行；
调和轮次自身由 `pkg/reconcile` 的去重队列保证同一对象同时最多一轮，两层不共用锁，
跨层并发写由服务端冲突判定收口：服务端合并提交按字段集、显式更新按 `resourceVersion`，
都返回 409 与当前对象（C-05）。

## 控制面 API

浏览器与扩展只有这一个读写入口（C-12），控制面自己持有集群客户端，不把凭据交给调用方。

| 方法 | 路径 | 语义 |
|---|---|---|
| `GET` | `/api/v1/resources/{kind}` | 一页投影；`limit`、`continue`、`labelSelector`、`namespace` |
| `GET` | `/api/v1/resources/{kind}/{name}` | 单对象投影 |
| `POST` | `/api/v1/resources/{kind}` | 创建；202 已接受 + 对象引用 |
| `PUT` | `/api/v1/resources/{kind}/{name}` | 更新客户端所有的字段；需 `If-Match` 或 `metadata.resourceVersion` |
| `DELETE` | `/api/v1/resources/{kind}/{name}` | 删除；需 `If-Match` |
| `POST` | `/api/v1/plan-requests` | 三类差异、摘要与警告；不写任何对象 |
| `POST` | `/api/v1/apply-requests` | 接受期望对象集后执行；进度从对象本身读取 |

- 读投影每项带 `observedAt` 与 `stale`；陈旧阈值是 `controlplane.StaleThreshold`（2 分钟），所有来源共用。
- 写请求返回 202 与对象引用，不返回已完成；工作在接受后继续，客户端断线不影响推进。
- 同一写请求重复提交收敛到同一对象；已存在且期望状态不同时报 `object_exists`。
- 写请求带 `If-Match`（或 `metadata.resourceVersion`）；版本已变时由 API server 判冲突，返回 409
  与当前对象 `current`，不静默覆盖。
- 分页游标绑定对象类型、命名空间与过滤条件；不属于本次查询的游标被拒绝，不会静默漏数据。
- 失败响应统一为 `{"error": {...}}`，含 `code`、`message`、`stage`、`object`、`retryable`、
  `requiresUserAction`、`sideEffects`。`code` 在一个主版本内不变。
- 只服务十类内核对象。外部组的对象类在扩展装载前不可用，返回 `capability_missing`（C-19、F16）。

## 调和与状态轴

`pkg/reconcile` 提供通用循环：请求只携带对象名，每轮读取当前事实；队列对同一对象去重，
同一对象的调和因此串行。每轮有固定时间预算（C-06 的上限），以轮次上下文的 deadline
下发；调和器必须在预算内返回，忽略 ctx 的调和器会阻塞该 worker，直到它返回。超过预算的
轮次计入 `kos_reconcile_round_over_limit_total`，每轮耗时记录在
`kos_reconcile_round_duration_seconds`，panic 的轮次计入
`kos_reconcile_round_panics_total` 并作为失败重试。长时间动作应由调和器创建 Job 后返回，
由下一次观察推进，不在轮内同步等待。返回错误按退避重试；`RequeueAfter` 安排下一次观察。

`pkg/state` 提供两条正交状态轴。`Reconciled` 条件取值 True（期望状态已完整应用）、
False（仍有待处理动作或被阻塞，`reason` 给出原因）与 Unknown（无法观察）；没有条件即
Unknown，表示尚无写者观察过该对象。失败由对象自己的失败条件表达，不并入
`Reconciled`。`EvaluateHealth` 按 Unknown、Unavailable、Degraded、Healthy 的优先级
求值：无法观察不推断为 Healthy，期望副本大于零且就绪为零、必需依赖不可用或必需卷丢失
判 Unavailable，部分就绪、非必需依赖降级、下游漂移或能力保证不足判 Degraded；暂停与
维护停止按有效期望副本零判定。

## 构建与测试

```bash
make generate   # 重新生成 deepcopy 与 CRD，并保持与 Go 类型一致
make verify     # 生成物无漂移 + gofmt + go vet
make test       # 单元测试 + 验收测试（自动下载 envtest 二进制）
make acceptance # 只跑 tests/acceptance，输出用例名
```

验收测试使用 envtest 启动真实 kube-apiserver 与 etcd，安装 `config/crd` 后读写对象；
环境变量 `KUBEBUILDER_ASSETS` 指向 envtest 二进制目录，`make test` 会下载并设置。
